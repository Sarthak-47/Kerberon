//go:build linux || darwin

// Chaos test for the durable timer scheduler.
//
// The claim this project is built to justify is: kill the process at arbitrary
// points during an escalation, restart it, and every step fires exactly once —
// never zero times, never twice. Zero means a missed page; twice means waking
// someone at 4am for nothing. Both are real failures.
//
// The test runs an escalation chain in a subprocess, SIGKILLs it at randomised
// moments, restarts it, and finally asserts that each step left exactly one
// audit row.
//
// Unix-only: Windows has no faithful SIGKILL, and TerminateProcess does not
// exercise the same failure mode. See docs/DECISIONS.md D10. Delays are
// milliseconds rather than minutes, which is why escalation delays must be
// configurable rather than hardcoded.
//
// This harness is exempt from the clock rule because a fake clock cannot span
// a real process restart: the worker is SIGKILLed and a new process starts with
// no in-memory state to advance. Measuring real elapsed time is the point.
//
//kerberon:allow-clock-file
package timer_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/store"
	"github.com/Sarthak-47/kerberon/internal/timer"
)

const (
	envWorker = "KERBERON_CHAOS_WORKER"
	envDB     = "KERBERON_CHAOS_DB"
	envSteps  = "KERBERON_CHAOS_STEPS"

	// stepDelay keeps a full chain short enough to run hundreds of times.
	stepDelay = 15 * time.Millisecond
)

// TestMain lets the test binary re-execute itself as the chaos worker. The
// worker is a real process with a real scheduler over a real database, so
// SIGKILL exercises the same recovery path a production crash would.
func TestMain(m *testing.M) {
	if os.Getenv(envWorker) != "" {
		runChaosWorker()
		return
	}
	os.Exit(m.Run())
}

// stepPayload identifies which escalation step a timer represents.
type stepPayload struct {
	Step int `json:"step"`
}

// runChaosWorker drives an escalation chain until killed or finished.
func runChaosWorker() {
	dbPath := os.Getenv(envDB)
	totalSteps := 0
	fmt.Sscanf(os.Getenv(envSteps), "%d", &totalSteps)
	if dbPath == "" || totalSteps <= 0 {
		fmt.Fprintln(os.Stderr, "chaos worker: missing configuration")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "chaos worker: open:", err)
		os.Exit(2)
	}
	defer db.Close()

	clk := clock.Real()
	sched := timer.New(db, clk, timer.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Retry fast: a killed process must resume promptly.
		RetryBackoff:    5 * time.Millisecond,
		MaxRetryBackoff: 20 * time.Millisecond,
		MaxIdleWait:     20 * time.Millisecond,
	})

	// The escalation effect. Everything here is a database write, applied in
	// the transaction that also marks the timer complete, which is what makes
	// the whole step atomic (D1).
	sched.Register(core.TimerEscalate, timer.HandlerFunc(
		func(ctx context.Context, tx *sql.Tx, t core.Timer) error {
			var p stepPayload
			if err := json.Unmarshal([]byte(t.Payload), &p); err != nil {
				return err
			}

			// One audit row per step. Counting these is how the parent proves
			// exactly-once.
			detail, _ := json.Marshal(p)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO events (incident_id, kind, detail, created_at)
				VALUES (?, 'escalated', ?, ?)`,
				t.IncidentID, string(detail), clk.Now().Unix()); err != nil {
				return err
			}

			// Advance the incident, as a real escalation would.
			if _, err := tx.ExecContext(ctx,
				`UPDATE incidents SET current_step = ? WHERE id = ?`,
				p.Step+1, t.IncidentID); err != nil {
				return err
			}

			// Schedule the next step in the same transaction.
			if p.Step+1 < totalSteps {
				next, err := json.Marshal(stepPayload{Step: p.Step + 1})
				if err != nil {
					return err
				}
				_, err = store.InsertTimer(ctx, tx, core.Timer{
					IncidentID: t.IncidentID,
					Kind:       core.TimerEscalate,
					FireAt:     clk.Now().Add(stepDelay),
					Payload:    string(next),
					CreatedAt:  clk.Now(),
				})
				return err
			}
			return nil
		}))

	_ = sched.Run(ctx)
}

// ─── Parent ───────────────────────────────────────────────────────────────

// chaosDB creates a migrated database seeded with one incident and the first
// escalation timer.
func chaosDB(t *testing.T, dir string) (string, int64) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(dir, "chaos.db")

	db, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var incidentID int64
	err = db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			INSERT INTO incidents
				(group_key, team, policy, severity, title, status, created_at, last_alert_at)
			VALUES ('chaos', 'platform', 'p', 'critical', 'chaos', 'triggered', 1, 1)`)
		if err != nil {
			return err
		}
		incidentID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		payload, err := json.Marshal(stepPayload{Step: 0})
		if err != nil {
			return err
		}
		_, err = store.InsertTimer(ctx, tx, core.Timer{
			IncidentID: incidentID,
			Kind:       core.TimerEscalate,
			FireAt:     time.Now().Add(stepDelay),
			Payload:    string(payload),
			CreatedAt:  time.Now(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return path, incidentID
}

// stepCounts returns how many audit rows exist per escalation step.
func stepCounts(t *testing.T, dbPath string) map[int]int {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatalf("open for verification: %v", err)
	}
	defer db.Close()

	rows, err := db.Read().QueryContext(ctx,
		`SELECT detail FROM events WHERE kind = 'escalated'`)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer rows.Close()

	counts := map[int]int{}
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		var p stepPayload
		if err := json.Unmarshal([]byte(detail), &p); err != nil {
			t.Fatalf("decode event detail %q: %v", detail, err)
		}
		counts[p.Step]++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events: %v", err)
	}
	return counts
}

// startWorker launches the chaos worker subprocess.
func startWorker(t *testing.T, dbPath string, steps int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		envWorker+"=1",
		envDB+"="+dbPath,
		fmt.Sprintf("%s=%d", envSteps, steps),
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	return cmd
}

// runOneChaosCycle kills and restarts the worker repeatedly, then lets it
// finish, and reports the per-step counts.
func runOneChaosCycle(t *testing.T, rng *rand.Rand, steps, kills int) map[int]int {
	t.Helper()

	root := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create .tmp: %v", err)
	}
	dir, err := os.MkdirTemp(root, "chaos-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	dbPath, _ := chaosDB(t, dir)

	for i := 0; i < kills; i++ {
		cmd := startWorker(t, dbPath, steps)
		// Kill at a randomised point, so over many iterations the kill lands
		// everywhere in the escalation sequence including mid-transaction.
		jitter := time.Duration(rng.Intn(int(stepDelay*3/time.Millisecond))) * time.Millisecond
		time.Sleep(jitter)
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_ = cmd.Wait()
	}

	// Let it run to completion.
	cmd := startWorker(t, dbPath, steps)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if counts := stepCounts(t, dbPath); len(counts) == steps {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = cmd.Process.Signal(syscall.SIGKILL)
	_ = cmd.Wait()

	return stepCounts(t, dbPath)
}

// The headline test. Every escalation step must fire exactly once across
// repeated kill/restart cycles.
func TestChaosEscalationFiresExactlyOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test skipped in -short mode")
	}

	const steps = 6

	// Iterations are modest by default so the suite stays fast; CI runs a
	// larger nightly sweep. Spec section 11 targets 1,000.
	iterations := 15
	if v := os.Getenv("KERBERON_CHAOS_ITERATIONS"); v != "" {
		fmt.Sscanf(v, "%d", &iterations)
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for i := 0; i < iterations; i++ {
		counts := runOneChaosCycle(t, rng, steps, 1+rng.Intn(3))

		for step := 0; step < steps; step++ {
			switch n := counts[step]; {
			case n == 0:
				t.Fatalf("iteration %d: step %d never fired - a page was missed (counts: %v)",
					i, step, counts)
			case n > 1:
				t.Fatalf("iteration %d: step %d fired %d times - someone was paged twice (counts: %v)",
					i, step, n, counts)
			}
		}
		if len(counts) != steps {
			t.Fatalf("iteration %d: %d distinct steps fired, want %d (counts: %v)",
				i, len(counts), steps, counts)
		}
	}
}

// A process killed and restarted with no further kills must still complete the
// whole chain, which is the plain recovery case.
func TestChaosRecoveryCompletesTheChain(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test skipped in -short mode")
	}

	const steps = 4
	rng := rand.New(rand.NewSource(1))
	counts := runOneChaosCycle(t, rng, steps, 1)

	for step := 0; step < steps; step++ {
		if counts[step] != 1 {
			t.Errorf("step %d fired %d times, want exactly 1 (counts: %v)",
				step, counts[step], counts)
		}
	}
}
