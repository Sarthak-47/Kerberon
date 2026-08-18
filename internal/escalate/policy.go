// Package escalate is the state machine that decides who gets paged, when, and
// what happens when nobody answers.
//
// It never sends anything. Every effect is a database write applied inside the
// timer's transaction: advance the incident, enqueue pages to the outbox,
// schedule the next step. Dispatch workers do the sending. That is what makes
// each escalation step fire exactly once even across a crash (DECISIONS D1).
package escalate

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Sarthak-47/kerberon/internal/config"
	"github.com/Sarthak-47/kerberon/internal/core"
)

// Policy is an escalation policy as it stood when an incident opened.
//
// It is snapshotted onto the incident rather than looked up live, so an
// incident escalates the way it started even if a config reload alters or
// deletes the policy underneath it. Targets are still resolved live at
// step-fire time, so an incident spanning a handoff pages whoever is actually
// on call (DECISIONS D4, spec section 8.1).
type Policy struct {
	Name string `json:"name"`
	// Repeat is how many times the whole policy loops before the incident
	// expires. Zero means one pass.
	Repeat int `json:"repeat"`
	// AckTimeout resumes escalation if an acknowledged incident is not
	// resolved within this window — the "acknowledged and fell back asleep"
	// case. Zero disables it.
	AckTimeout Duration `json:"ack_timeout,omitempty"`
	Steps      []Step   `json:"steps"`
}

// Step is one rung of a policy.
type Step struct {
	// Delay is measured from the previous step firing, not from incident
	// creation.
	Delay    Duration       `json:"delay"`
	Targets  []Target       `json:"targets"`
	Channels []core.Channel `json:"channels"`
}

// Target is who a step pages.
type Target struct {
	Kind core.TargetKind `json:"kind"`
	Name string          `json:"name"`
}

func (t Target) String() string { return string(t.Kind) + ":" + t.Name }

// Duration serializes as a Go duration string inside the snapshot, so a stored
// policy stays readable when someone inspects the row while debugging an
// incident at 3am.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		// Tolerate a bare number of nanoseconds so a snapshot written by an
		// older build still loads rather than stranding a live incident.
		var n int64
		if err2 := json.Unmarshal(b, &n); err2 == nil {
			*d = Duration(n)
			return nil
		}
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q in policy snapshot: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// PolicyFromConfig converts a configured policy into the snapshot form.
func PolicyFromConfig(c config.EscalationPolicy) Policy {
	p := Policy{
		Name:       c.Name,
		Repeat:     c.Repeat,
		AckTimeout: Duration(c.AckTimeout.Std()),
	}
	for _, cs := range c.Steps {
		s := Step{Delay: Duration(cs.Delay.Std())}
		for _, ct := range cs.Targets {
			s.Targets = append(s.Targets, Target{Kind: ct.Kind, Name: ct.Name})
		}
		for _, ch := range cs.Channels {
			s.Channels = append(s.Channels, core.Channel(ch))
		}
		p.Steps = append(p.Steps, s)
	}
	return p
}

// Encode serializes a policy for storage on the incident.
func (p Policy) Encode() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode policy snapshot: %w", err)
	}
	return string(b), nil
}

// DecodePolicy reads a snapshot back.
func DecodePolicy(s string) (Policy, error) {
	if s == "" {
		return Policy{}, fmt.Errorf("incident has no policy snapshot")
	}
	var p Policy
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return Policy{}, fmt.Errorf("decode policy snapshot: %w", err)
	}
	if len(p.Steps) == 0 {
		return Policy{}, fmt.Errorf("policy snapshot %q has no steps", p.Name)
	}
	return p, nil
}

// TotalPasses is how many times the policy runs before the incident expires.
func (p Policy) TotalPasses() int {
	if p.Repeat < 1 {
		return 1
	}
	return p.Repeat
}

// StepAt returns the step for a flat index across all passes, along with which
// pass it belongs to.
//
// Flattening the index means the escalation engine tracks a single
// current_step integer rather than a step-and-pass pair, and a repeat is just
// running off the end of the list and coming back round.
func (p Policy) StepAt(index int) (step Step, pass int, ok bool) {
	if len(p.Steps) == 0 || index < 0 {
		return Step{}, 0, false
	}
	total := len(p.Steps) * p.TotalPasses()
	if index >= total {
		return Step{}, 0, false
	}
	return p.Steps[index%len(p.Steps)], index / len(p.Steps), true
}

// Exhausted reports whether index is past the last step of the last pass, at
// which point nobody answered and the incident expires.
func (p Policy) Exhausted(index int) bool {
	_, _, ok := p.StepAt(index)
	return !ok
}
