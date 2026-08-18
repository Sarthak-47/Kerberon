package escalate_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Sarthak-47/kerberon/internal/ack"
	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/escalate"
	"github.com/Sarthak-47/kerberon/internal/store"
	"github.com/Sarthak-47/kerberon/internal/timer"
)

const (
	t0RFC  = "2026-08-15T09:00:00Z"
	secret = "0123456789abcdef0123456789abcdef"
)

// ─── Fakes ────────────────────────────────────────────────────────────────

// resolver returns whoever the test says is on call, and records when it was
// asked, so "targets resolve at fire time" can be asserted rather than assumed.
type resolver struct {
	mu       sync.Mutex
	byTarget map[string][]string
	askedAt  []time.Time
}

func newResolver() *resolver {
	return &resolver{byTarget: map[string][]string{}}
}

func (r *resolver) set(target string, users ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byTarget[target] = users
}

func (r *resolver) ResolveTargets(_ context.Context, at time.Time, targets []escalate.Target) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.askedAt = append(r.askedAt, at)

	seen := map[string]bool{}
	var out []string
	for _, t := range targets {
		for _, u := range r.byTarget[t.String()] {
			if !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		}
	}
	return out, nil
}

// contacts maps user+channel to an address.
type contacts map[string]string

func (c contacts) Destination(userID string, ch core.Channel) (string, bool) {
	d, ok := c[userID+"/"+string(ch)]
	return d, ok
}

// ─── Harness ──────────────────────────────────────────────────────────────

type harness struct {
	t          *testing.T
	db         *store.DB
	clk        *clock.FakeClock
	sched      *timer.Scheduler
	eng        *escalate.Engine
	res        *resolver
	incidentID int64
}

func newHarness(t *testing.T, cb contacts) *harness {
	t.Helper()

	root := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create .tmp: %v", err)
	}
	dir, err := os.MkdirTemp(root, "escalate-test-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(dir, "kerberon.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &harness{t: t, db: db, clk: clock.NewFakeAt(t0RFC), res: newResolver()}
	h.sched = timer.New(db, h.clk, timer.Options{Logger: quiet})

	signer, err := ack.NewSigner(secret)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	h.eng = escalate.New(db, h.clk, h.sched, h.res, cb, signer, escalate.Options{
		ExternalURL: "https://k.example.com",
		Logger:      quiet,
	})

	err = db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			INSERT INTO incidents
				(group_key, team, policy, severity, title, status, created_at, last_alert_at, alert_count)
			VALUES ('g', 'platform', 'critical-24x7', 'critical', 'API is down', 'triggered', 1000, 1000, 3)`)
		if err != nil {
			return err
		}
		h.incidentID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatalf("seed incident: %v", err)
	}
	return h
}

func (h *harness) incident() core.Incident {
	h.t.Helper()
	inc, err := h.db.Incident(context.Background(), h.incidentID)
	if err != nil {
		h.t.Fatalf("read incident: %v", err)
	}
	return inc
}

func (h *harness) notifications() []core.Notification {
	h.t.Helper()
	out, err := h.db.NotificationsForIncident(context.Background(), h.incidentID)
	if err != nil {
		h.t.Fatalf("read notifications: %v", err)
	}
	return out
}

// begin starts escalation as the grouping engine would when group_wait closes.
func (h *harness) begin(p escalate.Policy) {
	h.t.Helper()
	ctx := context.Background()
	err := h.db.Tx(ctx, func(tx *sql.Tx) error {
		return h.eng.Begin(ctx, tx, h.incident(), p)
	})
	if err != nil {
		h.t.Fatalf("begin: %v", err)
	}
}

// runDueTimers fires everything currently due, as the scheduler would.
func (h *harness) runDueTimers() {
	h.t.Helper()
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		pending, err := h.db.NextPendingTimers(ctx, 1)
		if err != nil {
			h.t.Fatalf("pending timers: %v", err)
		}
		if len(pending) == 0 || pending[0].FireAt.After(h.clk.Now()) {
			return
		}
		if err := h.sched.RunDueForTest(ctx); err != nil {
			h.t.Fatalf("run timer: %v", err)
		}
	}
	h.t.Fatal("timers did not settle after 50 iterations")
}

func threeStepPolicy() escalate.Policy {
	return escalate.Policy{
		Name:       "critical-24x7",
		Repeat:     1,
		AckTimeout: escalate.Duration(30 * time.Minute),
		Steps: []escalate.Step{
			{
				Delay:    0,
				Targets:  []escalate.Target{{Kind: core.TargetSchedule, Name: "primary"}},
				Channels: []core.Channel{core.ChannelNtfy},
			},
			{
				Delay:    escalate.Duration(5 * time.Minute),
				Targets:  []escalate.Target{{Kind: core.TargetSchedule, Name: "secondary"}},
				Channels: []core.Channel{core.ChannelNtfy},
			},
			{
				Delay:    escalate.Duration(10 * time.Minute),
				Targets:  []escalate.Target{{Kind: core.TargetTeam, Name: "platform"}},
				Channels: []core.Channel{core.ChannelNtfy, core.ChannelEmail},
			},
		},
	}
}

func standardContacts() contacts {
	return contacts{
		"sarthak/ntfy":  "https://ntfy.sh/sarthak",
		"sarthak/email": "sarthak@example.com",
		"priya/ntfy":    "https://ntfy.sh/priya",
		"priya/email":   "priya@example.com",
		"arun/ntfy":     "https://ntfy.sh/arun",
		"arun/email":    "arun@example.com",
	}
}

// ─── Firing ───────────────────────────────────────────────────────────────

func TestFirstStepPagesThePrimary(t *testing.T) {
	h := newHarness(t, standardContacts())
	h.res.set("schedule:primary", "sarthak")

	h.begin(threeStepPolicy())

	ns := h.notifications()
	if len(ns) != 1 {
		t.Fatalf("enqueued %d pages, want 1", len(ns))
	}
	if ns[0].TargetUser != "sarthak" || ns[0].Channel != core.ChannelNtfy {
		t.Errorf("page = %+v", ns[0])
	}
	if ns[0].Destination != "https://ntfy.sh/sarthak" {
		t.Errorf("destination = %q", ns[0].Destination)
	}
	if ns[0].State != core.NotifPending {
		t.Errorf("state = %q, want pending", ns[0].State)
	}
}

// The body must carry a working ack link, since that is the whole
// authentication story for acknowledging.
func TestPageCarriesAVerifiableAckLink(t *testing.T) {
	h := newHarness(t, standardContacts())
	h.res.set("schedule:primary", "sarthak")
	h.begin(threeStepPolicy())

	body := h.notifications()[0].Body
	idx := -1
	for i := 0; i+7 < len(body); i++ {
		if body[i:i+8] == "https://" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("no link in the page body:\n%s", body)
	}
	link := body[idx:]

	signer, err := ack.NewSigner(secret)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	path := link[len("https://k.example.com"):]
	parsed, err := ack.ParseLinkPath(path)
	if err != nil {
		t.Fatalf("parse %q: %v", link, err)
	}
	if !signer.Verify(parsed.IncidentID, parsed.UserID, 0, parsed.Token) {
		t.Error("the ack link in the page does not verify")
	}
	if parsed.IncidentID != h.incidentID || parsed.UserID != "sarthak" {
		t.Errorf("link points at %+v", parsed)
	}
}

func TestEscalationAdvancesThroughSteps(t *testing.T) {
	h := newHarness(t, standardContacts())
	h.res.set("schedule:primary", "sarthak")
	h.res.set("schedule:secondary", "priya")
	h.res.set("team:platform", "sarthak", "priya", "arun")

	h.begin(threeStepPolicy())
	if got := len(h.notifications()); got != 1 {
		t.Fatalf("after step 0: %d pages, want 1", got)
	}

	// Step 1 fires five minutes later.
	h.clk.Advance(5 * time.Minute)
	h.runDueTimers()
	ns := h.notifications()
	if len(ns) != 2 {
		t.Fatalf("after step 1: %d pages, want 2", len(ns))
	}
	if ns[1].TargetUser != "priya" {
		t.Errorf("step 1 paged %q, want priya", ns[1].TargetUser)
	}
	if h.incident().CurrentStep != 1 {
		t.Errorf("current_step = %d, want 1", h.incident().CurrentStep)
	}

	// Step 2 pages the whole team on two channels: three users times two.
	h.clk.Advance(10 * time.Minute)
	h.runDueTimers()
	if got := len(h.notifications()); got != 2+6 {
		t.Fatalf("after step 2: %d pages, want 8", got)
	}
}

// Targets resolve when the step fires, not when the incident opened, so an
// incident spanning a handoff pages whoever is on call then.
func TestTargetsResolveAtFireTimeNotAtCreation(t *testing.T) {
	h := newHarness(t, standardContacts())
	h.res.set("schedule:primary", "sarthak")
	h.res.set("schedule:secondary", "priya")

	h.begin(threeStepPolicy())

	// A handoff happens between step 0 and step 1.
	h.res.set("schedule:secondary", "arun")

	h.clk.Advance(5 * time.Minute)
	h.runDueTimers()

	ns := h.notifications()
	if ns[len(ns)-1].TargetUser != "arun" {
		t.Errorf("step 1 paged %q; targets were resolved too early",
			ns[len(ns)-1].TargetUser)
	}
}

// A user with no address on a channel is skipped, not fatal: the other
// recipients of the same page still need to hear about it.
func TestUserMissingAChannelIsSkippedNotFatal(t *testing.T) {
	cb := standardContacts()
	delete(cb, "priya/email")

	h := newHarness(t, cb)
	h.res.set("team:platform", "sarthak", "priya")

	h.begin(escalate.Policy{
		Name: "p",
		Steps: []escalate.Step{{
			Targets:  []escalate.Target{{Kind: core.TargetTeam, Name: "platform"}},
			Channels: []core.Channel{core.ChannelNtfy, core.ChannelEmail},
		}},
	})

	ns := h.notifications()
	// sarthak on both, priya on ntfy only.
	if len(ns) != 3 {
		t.Fatalf("enqueued %d pages, want 3", len(ns))
	}
	for _, n := range ns {
		if n.TargetUser == "priya" && n.Channel == core.ChannelEmail {
			t.Error("paged priya on a channel she has no address for")
		}
	}
}

// A step resolving to nobody is a coverage gap arriving at the worst moment.
// Escalation must keep going rather than stopping there.
func TestStepWithNobodyOnCallStillAdvances(t *testing.T) {
	h := newHarness(t, standardContacts())
	// Nothing set for the primary; the secondary is covered.
	h.res.set("schedule:secondary", "priya")

	h.begin(threeStepPolicy())
	if got := len(h.notifications()); got != 0 {
		t.Fatalf("paged %d despite nobody being on call", got)
	}

	h.clk.Advance(5 * time.Minute)
	h.runDueTimers()
	ns := h.notifications()
	if len(ns) != 1 || ns[0].TargetUser != "priya" {
		t.Fatalf("escalation did not continue past the empty step: %v", ns)
	}
}

// ─── Acknowledgement ──────────────────────────────────────────────────────

func TestAcknowledgeStopsEscalation(t *testing.T) {
	h := newHarness(t, standardContacts())
	h.res.set("schedule:primary", "sarthak")
	h.res.set("schedule:secondary", "priya")

	h.begin(threeStepPolicy())
	before := len(h.notifications())

	if err := h.eng.Acknowledge(context.Background(), h.incidentID, "sarthak", core.AckViaLink); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}

	inc := h.incident()
	if inc.Status != core.IncidentAcknowledged {
		t.Fatalf("status = %q, want acknowledged", inc.Status)
	}
	if inc.AcknowledgedBy != "sarthak" || inc.AcknowledgedAt == nil {
		t.Errorf("ack not recorded on the incident: %+v", inc)
	}

	// No further escalation, well past when the next step was due. The window
	// stops short of ack_timeout on purpose: resuming after that is correct
	// behaviour and has its own test, so including it here would conflate the
	// two.
	h.clk.Advance(20 * time.Minute)
	h.runDueTimers()
	if got := len(h.notifications()); got != before {
		t.Errorf("escalation continued after an ack: %d then %d pages", before, got)
	}
}

// Acknowledging is not resolving. Someone answering the page is not the same
// as the problem being over.
func TestAcknowledgeDoesNotResolve(t *testing.T) {
	h := newHarness(t, standardContacts())
	h.res.set("schedule:primary", "sarthak")
	h.begin(threeStepPolicy())

	if err := h.eng.Acknowledge(context.Background(), h.incidentID, "sarthak", core.AckViaLink); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	if got := h.incident().Status; got == core.IncidentResolved {
		t.Error("acknowledging resolved the incident")
	}
	if h.incident().ResolvedAt != nil {
		t.Error("acknowledging set resolved_at")
	}
}

// An ack on an incident that is no longer waiting must be a no-op, not a
// crash: an ack arriving just after a resolution is ordinary.
func TestAcknowledgingTwiceOrLateIsANoOp(t *testing.T) {
	h := newHarness(t, standardContacts())
	h.res.set("schedule:primary", "sarthak")
	h.begin(threeStepPolicy())
	ctx := context.Background()

	if err := h.eng.Acknowledge(ctx, h.incidentID, "sarthak", core.AckViaLink); err != nil {
		t.Fatalf("first ack: %v", err)
	}

	err := h.eng.Acknowledge(ctx, h.incidentID, "priya", core.AckViaAPI)
	if !errors.Is(err, escalate.ErrNotAcknowledgeable) {
		t.Fatalf("second ack returned %v, want ErrNotAcknowledgeable", err)
	}
	// The first acknowledger is not overwritten.
	if got := h.incident().AcknowledgedBy; got != "sarthak" {
		t.Errorf("acknowledged_by = %q, want the first acknowledger", got)
	}

	// And on a resolved incident.
	if err := h.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.ResolveIncident(ctx, tx, h.incidentID, h.clk.Now(), "auto")
		return err
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := h.eng.Acknowledge(ctx, h.incidentID, "arun", core.AckViaUI); !errors.Is(err, escalate.ErrNotAcknowledgeable) {
		t.Errorf("ack on a resolved incident returned %v, want ErrNotAcknowledgeable", err)
	}
}

// "Acknowledged and fell back asleep" is a real failure mode.
func TestAckTimeoutResumesEscalation(t *testing.T) {
	h := newHarness(t, standardContacts())
	h.res.set("schedule:primary", "sarthak")
	h.res.set("schedule:secondary", "priya")

	h.begin(threeStepPolicy())
	if err := h.eng.Acknowledge(context.Background(), h.incidentID, "sarthak", core.AckViaLink); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	before := len(h.notifications())

	// Nothing happens before the timeout.
	h.clk.Advance(29 * time.Minute)
	h.runDueTimers()
	if got := len(h.notifications()); got != before {
		t.Fatalf("escalation resumed early: %d then %d", before, got)
	}
	if h.incident().Status != core.IncidentAcknowledged {
		t.Fatal("status changed before the ack timeout elapsed")
	}

	h.clk.Advance(2 * time.Minute)
	h.runDueTimers()

	if got := h.incident().Status; got != core.IncidentTriggered {
		t.Errorf("status = %q, want triggered once ack_timeout elapsed", got)
	}
	if got := len(h.notifications()); got <= before {
		t.Errorf("no page after ack_timeout: %d then %d", before, got)
	}
}

// ─── Expiry ───────────────────────────────────────────────────────────────

// Nobody answering is critical information, not something to drop silently.
func TestIncidentExpiresWhenNobodyAnswers(t *testing.T) {
	h := newHarness(t, standardContacts())
	h.res.set("schedule:primary", "sarthak")
	h.res.set("schedule:secondary", "priya")
	h.res.set("team:platform", "arun")

	h.begin(threeStepPolicy())

	// Walk past every step, then past the final delay.
	for i := 0; i < 6; i++ {
		h.clk.Advance(15 * time.Minute)
		h.runDueTimers()
	}

	if got := h.incident().Status; got != core.IncidentExpired {
		t.Fatalf("status = %q, want expired", got)
	}

	events, err := h.db.Events(context.Background(), h.incidentID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var sawExpired bool
	for _, e := range events {
		if e.Kind == core.EventExpired {
			sawExpired = true
		}
	}
	if !sawExpired {
		t.Error("expiry was not recorded on the timeline")
	}
}

func TestRepeatRunsThePolicyAgainBeforeExpiring(t *testing.T) {
	h := newHarness(t, standardContacts())
	h.res.set("schedule:primary", "sarthak")

	p := escalate.Policy{
		Name:   "twice",
		Repeat: 2,
		Steps: []escalate.Step{{
			Delay:    escalate.Duration(time.Minute),
			Targets:  []escalate.Target{{Kind: core.TargetSchedule, Name: "primary"}},
			Channels: []core.Channel{core.ChannelNtfy},
		}},
	}
	h.begin(p)

	// One step repeated twice means two pages, and the second must not be
	// suppressed as a duplicate — the attempt group distinguishes them.
	h.clk.Advance(2 * time.Minute)
	h.runDueTimers()

	if got := len(h.notifications()); got != 2 {
		t.Fatalf("enqueued %d pages, want 2 across two passes", got)
	}
	if h.incident().Status == core.IncidentExpired {
		t.Error("expired before the repeat finished")
	}

	h.clk.Advance(5 * time.Minute)
	h.runDueTimers()
	if got := h.incident().Status; got != core.IncidentExpired {
		t.Errorf("status = %q, want expired after the repeat", got)
	}
}

// ─── Policy snapshot ──────────────────────────────────────────────────────

// An incident escalates the way it started, even if config changes underneath.
func TestPolicySnapshotIsStoredOnTheIncident(t *testing.T) {
	h := newHarness(t, standardContacts())
	h.res.set("schedule:primary", "sarthak")
	h.begin(threeStepPolicy())

	snap := h.incident().PolicySnapshot
	if snap == "" {
		t.Fatal("no policy snapshot stored")
	}
	decoded, err := escalate.DecodePolicy(snap)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Name != "critical-24x7" || len(decoded.Steps) != 3 {
		t.Errorf("snapshot = %+v", decoded)
	}
	if decoded.AckTimeout.Std() != 30*time.Minute {
		t.Errorf("ack_timeout round-tripped as %v", decoded.AckTimeout.Std())
	}
	if decoded.Steps[1].Delay.Std() != 5*time.Minute {
		t.Errorf("step delay round-tripped as %v", decoded.Steps[1].Delay.Std())
	}
}

func TestPolicyStepIndexing(t *testing.T) {
	p := escalate.Policy{
		Repeat: 2,
		Steps:  []escalate.Step{{}, {}, {}},
	}
	// Two passes of three steps.
	for i := 0; i < 6; i++ {
		_, pass, ok := p.StepAt(i)
		if !ok {
			t.Fatalf("index %d should exist", i)
		}
		if want := i / 3; pass != want {
			t.Errorf("index %d is pass %d, want %d", i, pass, want)
		}
	}
	if !p.Exhausted(6) {
		t.Error("index 6 should be past the end of two passes of three steps")
	}
	if p.Exhausted(5) {
		t.Error("index 5 is the last step of the last pass and should exist")
	}
}

func TestDecodeRejectsAnEmptyOrBrokenSnapshot(t *testing.T) {
	for _, s := range []string{"", "{}", "not json", `{"name":"p","steps":[]}`} {
		if _, err := escalate.DecodePolicy(s); err == nil {
			t.Errorf("snapshot %q should be rejected", s)
		}
	}
}
