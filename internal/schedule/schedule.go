package schedule

import (
	"fmt"
	"sort"
	"time"

	"github.com/Sarthak-47/kerberon/internal/config"
	"github.com/Sarthak-47/kerberon/internal/core"
)

// anchorRef is the fixed reference from which rotation position is counted.
//
// A rotation must land on the same participant regardless of when the process
// started or which window is being rendered, so position is derived from a
// constant rather than from "now" or from the start of the query range.
var anchorRef = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

// LayerType distinguishes a rotating layer from a fixed restriction window.
type LayerType string

const (
	LayerRotation LayerType = "rotation"
	// LayerRestriction is accepted by config but not yet resolved; see
	// Schedule.Layers.
	LayerRestriction LayerType = "restriction"
)

// Layer is one band of a schedule.
type Layer struct {
	Name         string
	Type         LayerType
	Participants []string
	// Rotation is "daily" or "weekly".
	Rotation string
	// HandoffDay applies to weekly rotations.
	HandoffDay    time.Weekday
	HandoffHour   int
	HandoffMinute int
	Location      *time.Location

	// anchor is the first handoff at or after anchorRef, precomputed so
	// rotation position is a subtraction rather than a walk.
	anchor time.Time
}

// Schedule resolves who is on call.
//
// Layers are evaluated in configuration order and the first that produces
// someone wins, so a specific band can be written above a general one. Spec
// section 7.1 orders resolution overrides first, then restriction layers, then
// the base rotation; overrides are applied by Resolver, above all layers.
type Schedule struct {
	Name     string
	Team     string
	Location *time.Location
	Layers   []Layer
}

// NewLayer prepares a layer for resolution.
func NewLayer(l Layer) (Layer, error) {
	if l.Location == nil {
		return Layer{}, fmt.Errorf("layer %q has no timezone", l.Name)
	}
	if len(l.Participants) == 0 {
		return Layer{}, fmt.Errorf("layer %q has no participants", l.Name)
	}

	switch l.Rotation {
	case "weekly":
		l.anchor = NextWeekly(anchorRef, l.HandoffDay, l.HandoffHour, l.HandoffMinute, l.Location)
	case "daily":
		l.anchor = NextDaily(anchorRef, l.HandoffHour, l.HandoffMinute, l.Location)
	case "":
		if l.Type == LayerRotation {
			return Layer{}, fmt.Errorf("layer %q is a rotation but names no rotation period", l.Name)
		}
	default:
		return Layer{}, fmt.Errorf("layer %q has unknown rotation %q", l.Name, l.Rotation)
	}
	return l, nil
}

// FromConfig builds every schedule in a configuration, keyed by name.
func FromConfig(cfg *config.Config) (map[string]*Schedule, error) {
	out := make(map[string]*Schedule, len(cfg.Schedules))

	for _, cs := range cfg.Schedules {
		loc, err := time.LoadLocation(cs.Timezone)
		if err != nil {
			return nil, fmt.Errorf("schedule %q: timezone %q: %w", cs.Name, cs.Timezone, err)
		}

		s := &Schedule{Name: cs.Name, Team: cs.Team, Location: loc}
		for _, cl := range cs.Layers {
			layerLoc := loc
			if cl.Handoff.Timezone != "" {
				layerLoc, err = time.LoadLocation(cl.Handoff.Timezone)
				if err != nil {
					return nil, fmt.Errorf("schedule %q layer %q: timezone %q: %w",
						cs.Name, cl.Name, cl.Handoff.Timezone, err)
				}
			}

			hour, minute, err := parseHHMM(cl.Handoff.Time)
			if err != nil && cl.Type == config.LayerRotation {
				return nil, fmt.Errorf("schedule %q layer %q: %w", cs.Name, cl.Name, err)
			}

			day, err := parseWeekday(cl.Handoff.Day)
			if err != nil && cl.Rotation == "weekly" {
				return nil, fmt.Errorf("schedule %q layer %q: %w", cs.Name, cl.Name, err)
			}

			layer, err := NewLayer(Layer{
				Name:          cl.Name,
				Type:          LayerType(cl.Type),
				Participants:  append([]string(nil), cl.Participants...),
				Rotation:      cl.Rotation,
				HandoffDay:    day,
				HandoffHour:   hour,
				HandoffMinute: minute,
				Location:      layerLoc,
			})
			if err != nil {
				return nil, fmt.Errorf("schedule %q: %w", cs.Name, err)
			}
			s.Layers = append(s.Layers, layer)
		}
		out[cs.Name] = s
	}
	return out, nil
}

func parseHHMM(s string) (hour, minute int, err error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid handoff time %q, want 24-hour HH:MM", s)
	}
	return t.Hour(), t.Minute(), nil
}

func parseWeekday(s string) (time.Weekday, error) {
	for d := time.Sunday; d <= time.Saturday; d++ {
		if equalFold(d.String(), s) {
			return d, nil
		}
	}
	return time.Sunday, fmt.Errorf("invalid handoff day %q", s)
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// ─── Handoffs and rotation position ───────────────────────────────────────

// nextHandoff is the first handoff strictly after t.
func (l Layer) nextHandoff(t time.Time) time.Time {
	if l.Rotation == "daily" {
		return NextDaily(t, l.HandoffHour, l.HandoffMinute, l.Location)
	}
	return NextWeekly(t, l.HandoffDay, l.HandoffHour, l.HandoffMinute, l.Location)
}

// prevHandoff is the latest handoff at or before t.
func (l Layer) prevHandoff(t time.Time) time.Time {
	// Step back far enough to guarantee a handoff lies in between, then walk
	// forward. Weekly needs nine days of slack, daily two.
	back := 9
	if l.Rotation == "daily" {
		back = 2
	}
	cur := l.nextHandoff(t.AddDate(0, 0, -back-1))
	for {
		next := l.nextHandoff(cur)
		if next.After(t) {
			return cur
		}
		cur = next
	}
}

// civilDays counts whole calendar days between two instants as seen in loc.
//
// Counting calendar days rather than dividing a duration is what keeps
// rotation position correct across DST: the day a transition happens is 23 or
// 25 hours long, and dividing by 24 would drift.
func civilDays(from, to time.Time, loc *time.Location) int {
	a := from.In(loc)
	b := to.In(loc)
	au := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	bu := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return int(bu.Sub(au) / (24 * time.Hour))
}

// position is which participant holds the shift beginning at handoff.
func (l Layer) position(handoff time.Time) int {
	days := civilDays(l.anchor, handoff, l.Location)
	period := days
	if l.Rotation == "weekly" {
		period = days / 7
	}
	n := len(l.Participants)
	// Go's % keeps the sign of the dividend, and dates before the anchor are
	// legitimate when rendering a historical calendar.
	return ((period % n) + n) % n
}

// userAt returns the participant on call at t, if this layer covers it.
func (l Layer) userAt(t time.Time) (string, bool) {
	if l.Type != LayerRotation || len(l.Participants) == 0 {
		return "", false
	}
	return l.Participants[l.position(l.prevHandoff(t))], true
}

// boundaries lists this layer's transition instants within (from, to].
func (l Layer) boundaries(from, to time.Time) []time.Time {
	if l.Type != LayerRotation {
		return nil
	}
	var out []time.Time
	for h := l.nextHandoff(from); !h.After(to); h = l.nextHandoff(h) {
		out = append(out, h)
	}
	return out
}

// ─── Resolution ───────────────────────────────────────────────────────────

// Resolver answers on-call questions for one schedule, layering overrides on
// top.
//
// Overrides are state rather than config — a last-minute swap should not need a
// git commit — so they are supplied at resolve time rather than baked in.
type Resolver struct {
	schedule  *Schedule
	overrides []core.Override
}

// NewResolver pairs a schedule with the overrides in force.
func NewResolver(s *Schedule, overrides []core.Override) *Resolver {
	// Later-created overrides win where two cover the same instant, so sorting
	// by creation makes the outcome deterministic rather than dependent on the
	// order rows came back.
	sorted := append([]core.Override(nil), overrides...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
	})
	return &Resolver{schedule: s, overrides: sorted}
}

// At returns who is on call at t.
//
// It is derived from Intervals rather than the other way round: a point lookup
// cannot see a gap, and building gap detection on sampled points would let a
// short hole at a DST boundary pass unnoticed. See docs/DECISIONS.md D6.
func (r *Resolver) At(t time.Time) (string, bool) {
	if user, ok := r.overrideAt(t); ok {
		return user, true
	}
	for _, l := range r.schedule.Layers {
		if user, ok := l.userAt(t); ok {
			return user, true
		}
	}
	return "", false
}

// overrideAt returns the override covering t, latest-created winning.
func (r *Resolver) overrideAt(t time.Time) (string, bool) {
	var (
		user  string
		found bool
	)
	for _, o := range r.overrides {
		if o.ScheduleName != r.schedule.Name {
			continue
		}
		// Half-open, matching Interval, so back-to-back overrides do not
		// both claim the boundary instant.
		if !t.Before(o.StartsAt) && t.Before(o.EndsAt) {
			user, found = o.UserID, true
		}
	}
	return user, found
}

// Intervals returns who is on call across [from, to), as contiguous spans.
//
// This is the primitive. Every boundary that could change the answer — layer
// handoffs and override edges — becomes a candidate, the answer is evaluated
// once per resulting span, and adjacent spans with the same person are merged.
// A period nobody covers simply does not appear, so a coverage gap is a hole in
// the output rather than something a caller has to go looking for.
func (r *Resolver) Intervals(from, to time.Time) []core.Interval {
	if !to.After(from) {
		return nil
	}

	points := []time.Time{from}
	for _, l := range r.schedule.Layers {
		points = append(points, l.boundaries(from, to)...)
	}
	for _, o := range r.overrides {
		if o.ScheduleName != r.schedule.Name {
			continue
		}
		if o.StartsAt.After(from) && o.StartsAt.Before(to) {
			points = append(points, o.StartsAt)
		}
		if o.EndsAt.After(from) && o.EndsAt.Before(to) {
			points = append(points, o.EndsAt)
		}
	}
	points = append(points, to)

	sort.Slice(points, func(i, j int) bool { return points[i].Before(points[j]) })

	var out []core.Interval
	for i := 0; i+1 < len(points); i++ {
		start, end := points[i], points[i+1]
		if !end.After(start) {
			continue // duplicate boundary
		}
		user, ok := r.At(start)
		if !ok {
			continue // nobody is on call; leave a hole
		}
		// Merge with the previous span when the same person continues and the
		// spans touch.
		if n := len(out); n > 0 && out[n-1].UserID == user && out[n-1].End.Equal(start) {
			out[n-1].End = end
			continue
		}
		out = append(out, core.Interval{Start: start, End: end, UserID: user})
	}
	return out
}

// Gaps returns the periods in [from, to) that nobody covers.
//
// An unnoticed gap means an unpaged incident, so this is surfaced by
// kerberon validate and by the UI rather than left for someone to discover at
// three in the morning.
func (r *Resolver) Gaps(from, to time.Time) []core.Interval {
	covered := r.Intervals(from, to)

	var gaps []core.Interval
	cursor := from
	for _, iv := range covered {
		if iv.Start.After(cursor) {
			gaps = append(gaps, core.Interval{Start: cursor, End: iv.Start})
		}
		if iv.End.After(cursor) {
			cursor = iv.End
		}
	}
	if cursor.Before(to) {
		gaps = append(gaps, core.Interval{Start: cursor, End: to})
	}
	return gaps
}
