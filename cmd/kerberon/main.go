// Command kerberon is an on-call and paging system that ships as a single
// binary with no external dependencies.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"

	// The timezone database is compiled into the binary. Without this,
	// rotations break on minimal container images and on Windows, which ship
	// no tzdata. Spec section 7.2.
	_ "time/tzdata"

	"github.com/Sarthak-47/kerberon/internal/config"
	"github.com/Sarthak-47/kerberon/internal/store"
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
  version     Print version information

Run "kerberon <command> -h" for the flags a command accepts.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
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

	case "serve":
		// Phase 5. See docs/ROADMAP.md.
		return errors.New("serve: not implemented yet")

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

	fmt.Printf("%s is valid\n", *path)
	fmt.Printf("  %d user(s), %d team(s), %d schedule(s), %d polic(ies), %d route(s)\n",
		len(cfg.Users), len(cfg.Teams), len(cfg.Schedules),
		len(cfg.EscalationPolicies), len(cfg.Routes))
	fmt.Printf("  database: %s\n", cfg.Database.Path)
	// Coverage-gap detection needs the schedule resolver and lands in Phase 4.
	// Saying so is better than letting a reader assume this command already
	// proves someone is on call at every instant.
	fmt.Println("  note: 24x7 coverage checking arrives with the schedule resolver (Phase 4)")
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

func printVersion() {
	fmt.Printf("kerberon %s\n", version)
	fmt.Printf("  commit: %s\n", commit)
	fmt.Printf("  built:  %s\n", date)
	fmt.Printf("  go:     %s\n", runtime.Version())
	fmt.Printf("  target: %s/%s\n", runtime.GOOS, runtime.GOARCH)
}
