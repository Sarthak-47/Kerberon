// Command kerberon is an on-call and paging system that ships as a single
// binary with no external dependencies.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"syscall"
	"time"

	// The timezone database is compiled into the binary. Without this,
	// rotations break on minimal container images and on Windows, which ship
	// no tzdata. Spec section 7.2.
	_ "time/tzdata"

	"github.com/Sarthak-47/kerberon/internal/api"
	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/config"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/group"
	"github.com/Sarthak-47/kerberon/internal/ingest"
	"github.com/Sarthak-47/kerberon/internal/route"
	"github.com/Sarthak-47/kerberon/internal/schedule"
	"github.com/Sarthak-47/kerberon/internal/store"
	"github.com/Sarthak-47/kerberon/internal/timer"
)

// Set via -ldflags at release time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `kerberon - on-call and paging in a single binary

Usage:
  kerberon <command> [flags]

Commands:
  serve       Run the server
  validate    Check a config file and report every problem found
  migrate     Create or upgrade the database schema
  oncall      Print who is on call
  version     Print version information

Run "kerberon <command> -h" for the flags a command accepts.
`

func main() {
	// SIGTERM is what a container runtime or systemd sends; without it the
	// process is killed outright and an in-flight timer tick is lost.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		// Validation failures are already formatted as a multi-line report;
		// prefixing them with "kerberon:" would just add noise.
		var verrs *config.Errors
		if errors.As(err, &verrs) {
			fmt.Fprintln(os.Stderr, err)
		} else {
			fmt.Fprintln(os.Stderr, "kerberon:", err)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}

	switch cmd := args[0]; cmd {
	case "version", "-v", "--version":
		printVersion()
		return nil

	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil

	case "validate":
		return cmdValidate(args[1:])

	case "migrate":
		return cmdMigrate(ctx, args[1:])

	case "oncall":
		return cmdOncall(ctx, args[1:])

	case "serve":
		return cmdServe(ctx, args[1:])

	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}

// configFlags registers the shared --config flag.
func configFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	path := fs.String("config", "kerberon.yaml", "path to the configuration file")
	return fs, path
}

func cmdValidate(args []string) error {
	fs, path := configFlags("validate")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: kerberon validate [flags]

Checks a configuration file and reports every problem found, with line numbers.
Exits non-zero if anything is wrong, so it can gate a pull request in CI.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}

	// A schedule that leaves a hole means an incident nobody is paged for, so
	// this fails the command rather than warning.
	gaps, err := schedule.CheckCoverage(cfg, clock.Real().Now(), schedule.CoverageWindow)
	if err != nil {
		return err
	}
	if len(gaps) > 0 {
		fmt.Fprintf(os.Stderr, "%s: %d coverage gap(s) in the next %d days\n",
			*path, len(gaps), int(schedule.CoverageWindow.Hours()/24))
		for i, g := range gaps {
			if i == 10 {
				fmt.Fprintf(os.Stderr, "  ... and %d more\n", len(gaps)-10)
				break
			}
			fmt.Fprintf(os.Stderr, "  %s\n", g)
		}
		return errors.New("schedules do not provide continuous coverage")
	}

	fmt.Printf("%s is valid\n", *path)
	fmt.Printf("  %d user(s), %d team(s), %d schedule(s), %d polic(ies), %d route(s)\n",
		len(cfg.Users), len(cfg.Teams), len(cfg.Schedules),
		len(cfg.EscalationPolicies), len(cfg.Routes))
	fmt.Printf("  database: %s\n", cfg.Database.Path)
	fmt.Printf("  coverage: no gaps in the next %d days\n",
		int(schedule.CoverageWindow.Hours()/24))
	return nil
}

func cmdOncall(ctx context.Context, args []string) error {
	fs, path := configFlags("oncall")
	scheduleName := fs.String("schedule", "", "schedule to resolve (default: all)")
	atFlag := fs.String("at", "", "RFC3339 instant to resolve (default: now)")
	days := fs.Int("days", 0, "instead of one instant, print the rota for this many days")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: kerberon oncall [flags]

Prints who is on call. Useful on its own and for other tooling.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}

	at := clock.Real().Now()
	if *atFlag != "" {
		at, err = time.Parse(time.RFC3339, *atFlag)
		if err != nil {
			return fmt.Errorf("invalid --at %q: want RFC3339, e.g. 2026-08-17T09:00:00Z", *atFlag)
		}
	}

	schedules, err := schedule.FromConfig(cfg)
	if err != nil {
		return err
	}

	// Overrides live in the database, so they are only applied when one is
	// available. Reporting without them would be quietly wrong on the exact
	// day somebody arranged a swap, so their absence is stated rather than
	// glossed over.
	//
	// This is a read-only command: it neither creates the database nor
	// migrates it, since doing either as a side effect of asking a question
	// would be surprising.
	var db *store.DB
	if _, statErr := os.Stat(cfg.Database.Path); statErr != nil {
		fmt.Fprintf(os.Stderr,
			"warning: %s does not exist, so overrides are not applied\n", cfg.Database.Path)
	} else if d, err := store.Open(ctx, cfg.Database.Path, store.Options{}); err != nil {
		fmt.Fprintf(os.Stderr,
			"warning: could not open %s, so overrides are not applied: %v\n", cfg.Database.Path, err)
	} else {
		defer d.Close()
		if v, err := d.SchemaVersion(ctx); err != nil || v == 0 {
			fmt.Fprintf(os.Stderr,
				"warning: %s has no schema yet (run kerberon migrate), so overrides are not applied\n",
				cfg.Database.Path)
		} else {
			db = d
		}
	}

	names := make([]string, 0, len(schedules))
	for name := range schedules {
		if *scheduleName == "" || name == *scheduleName {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return fmt.Errorf("no schedule named %q", *scheduleName)
	}
	sort.Strings(names)

	for _, name := range names {
		s := schedules[name]

		from, to := at, at.Add(time.Duration(*days)*24*time.Hour)
		if *days <= 0 {
			to = at.Add(time.Nanosecond)
		}

		var overrides []core.Override
		if db != nil {
			overrides, err = db.OverridesInWindow(ctx, name, from, to)
			if err != nil {
				return err
			}
		}
		r := schedule.NewResolver(s, overrides)

		if *days <= 0 {
			user, ok := r.At(at)
			if !ok {
				fmt.Printf("%-24s NOBODY IS ON CALL at %s\n", name, at.Format(time.RFC3339))
				continue
			}
			fmt.Printf("%-24s %s\n", name, user)
			continue
		}

		fmt.Printf("%s (%s)\n", name, s.Location)
		for _, iv := range r.Intervals(from, to) {
			fmt.Printf("  %s  ->  %s  %s\n",
				iv.Start.In(s.Location).Format("2006-01-02 15:04 MST"),
				iv.End.In(s.Location).Format("2006-01-02 15:04 MST"),
				iv.UserID)
		}
		for _, g := range r.Gaps(from, to) {
			fmt.Printf("  %s  ->  %s  *** NOBODY ON CALL ***\n",
				g.Start.In(s.Location).Format("2006-01-02 15:04 MST"),
				g.End.In(s.Location).Format("2006-01-02 15:04 MST"))
		}
	}
	return nil
}

func cmdMigrate(ctx context.Context, args []string) error {
	fs, path := configFlags("migrate")
	dbPath := fs.String("db", "", "database path (overrides the config file)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: kerberon migrate [flags]

Creates or upgrades the database schema. Safe to run repeatedly: migrations
already applied are skipped.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	target := *dbPath
	if target == "" {
		cfg, err := config.Load(*path)
		if err != nil {
			return err
		}
		target = cfg.Database.Path
	}

	db, err := store.Open(ctx, target, store.Options{})
	if err != nil {
		return err
	}
	defer db.Close()

	applied, err := db.Migrate(ctx)
	if err != nil {
		return err
	}

	version, err := db.SchemaVersion(ctx)
	if err != nil {
		return err
	}

	if len(applied) == 0 {
		fmt.Printf("%s is already at schema version %d\n", db.Path(), version)
		return nil
	}
	fmt.Printf("%s migrated to schema version %d\n", db.Path(), version)
	for _, m := range applied {
		fmt.Printf("  applied %04d_%s\n", m.Version, m.Name)
	}
	return nil
}

func cmdServe(ctx context.Context, args []string) error {
	fs, path := configFlags("serve")
	logFormat := fs.String("log-format", "text", "log output format: text or json")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: kerberon serve [flags]

Runs the server: the ingest API, the durable timer scheduler, and the grouping
engine. Migrations are applied at startup.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	var handler slog.Handler
	switch *logFormat {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, nil)
	case "text":
		handler = slog.NewTextHandler(os.Stderr, nil)
	default:
		return fmt.Errorf("unknown log format %q; want text or json", *logFormat)
	}
	log := slog.New(handler)

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}

	db, err := store.Open(ctx, cfg.Database.Path, store.Options{})
	if err != nil {
		return err
	}
	defer db.Close()

	applied, err := db.Migrate(ctx)
	if err != nil {
		return err
	}
	if len(applied) > 0 {
		log.Info("applied migrations", "count", len(applied))
	}

	clk := clock.Real()
	sched := timer.New(db, clk, timer.Options{Logger: log})
	router := route.New(cfg)

	// Phase 5 replaces this with the escalation engine. Until then a due page
	// is recorded on the incident timeline so the behaviour is observable, and
	// it is logged at warn level so nobody mistakes this for a working pager.
	onPageDue := func(ctx context.Context, tx *sql.Tx, inc core.Incident) error {
		log.Warn("incident is due to page, but notification delivery is not implemented yet",
			"incident_id", inc.ID, "team", inc.Team, "policy", inc.Policy,
			"title", inc.Title, "alert_count", inc.AlertCount)
		return store.InsertEvent(ctx, tx, inc.ID, core.EventNotified,
			`{"note":"delivery not implemented until Phase 5"}`, clk.Now())
	}

	engine := group.New(db, clk, router, sched, group.Options{
		OnPageDue: onPageDue,
		Logger:    log,
	})

	ingestSrv, err := ingest.New(engine, clk, ingest.Options{
		Token:  cfg.Server.IngestToken,
		Logger: log,
	})
	if err != nil {
		return err
	}

	schedules, err := schedule.FromConfig(cfg)
	if err != nil {
		return err
	}
	apiSrv := api.New(ingestSrv, schedules, clk, api.Options{
		Overrides: db,
		Logger:    log,
	})

	httpSrv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           apiSrv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// The scheduler and the HTTP server run until ctx is cancelled.
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()

	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		if err := sched.Run(runCtx); err != nil {
			log.Error("timer scheduler stopped with an error", "error", err)
		}
	}()

	serveErr := make(chan error, 1)
	go func() {
		log.Info("kerberon listening", "addr", cfg.Server.Listen, "database", db.Path())
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serveErr:
		stopRun()
		<-schedDone
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	}

	// Stop accepting new work, then let the scheduler finish the tick it is on.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown was not clean", "error", err)
	}
	stopRun()
	<-schedDone

	log.Info("stopped")
	return nil
}

func printVersion() {
	fmt.Printf("kerberon %s\n", version)
	fmt.Printf("  commit: %s\n", commit)
	fmt.Printf("  built:  %s\n", date)
	fmt.Printf("  go:     %s\n", runtime.Version())
	fmt.Printf("  target: %s/%s\n", runtime.GOOS, runtime.GOARCH)
}
