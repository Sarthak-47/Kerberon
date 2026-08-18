package escalate

import (
	"context"
	"sort"
	"time"

	"github.com/Sarthak-47/kerberon/internal/config"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/schedule"
)

// OverrideReader supplies the covers in force when a step fires.
type OverrideReader interface {
	OverridesInWindow(ctx context.Context, scheduleName string, from, to time.Time) ([]core.Override, error)
}

// ScheduleTargets resolves policy targets against the live configuration.
//
// Schedule targets are resolved here, at the moment a step fires, which is why
// this is an interface the engine calls rather than data the engine holds.
type ScheduleTargets struct {
	schedules map[string]*schedule.Schedule
	teams     map[string][]string
	overrides OverrideReader
}

// NewScheduleTargets builds the resolver from configuration.
func NewScheduleTargets(cfg *config.Config, schedules map[string]*schedule.Schedule, overrides OverrideReader) *ScheduleTargets {
	teams := make(map[string][]string, len(cfg.Teams))
	for _, t := range cfg.Teams {
		teams[t.Name] = append([]string(nil), t.Members...)
	}
	return &ScheduleTargets{schedules: schedules, teams: teams, overrides: overrides}
}

// ResolveTargets returns everyone a step should page, de-duplicated.
//
// A user appearing through two targets — named directly and also on call — is
// paged once, not twice.
func (s *ScheduleTargets) ResolveTargets(ctx context.Context, at time.Time, targets []Target) ([]string, error) {
	seen := map[string]bool{}
	var out []string

	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}

	for _, t := range targets {
		switch t.Kind {
		case core.TargetUser:
			add(t.Name)

		case core.TargetTeam:
			members := append([]string(nil), s.teams[t.Name]...)
			// Sorted so a team page has a stable order, which keeps the
			// incident timeline readable across runs.
			sort.Strings(members)
			for _, m := range members {
				add(m)
			}

		case core.TargetSchedule:
			sched, ok := s.schedules[t.Name]
			if !ok {
				continue
			}
			var overrides []core.Override
			if s.overrides != nil {
				var err error
				overrides, err = s.overrides.OverridesInWindow(ctx, t.Name, at, at.Add(time.Nanosecond))
				if err != nil {
					return nil, err
				}
			}
			if user, ok := schedule.NewResolver(sched, overrides).At(at); ok {
				add(user)
			}
			// A schedule with nobody on call contributes nobody. The engine
			// treats that as a coverage gap and says so loudly.
		}
	}
	return out, nil
}

// ConfigContacts looks up a user's address on a channel.
type ConfigContacts struct {
	byUser map[string]map[core.Channel]string
}

// NewConfigContacts builds the contact book from configuration.
func NewConfigContacts(cfg *config.Config) *ConfigContacts {
	byUser := make(map[string]map[core.Channel]string, len(cfg.Users))
	for _, u := range cfg.Users {
		m := make(map[core.Channel]string, len(u.Contacts))
		for ch, dest := range u.Contacts {
			m[core.Channel(ch)] = dest
		}
		byUser[u.ID] = m
	}
	return &ConfigContacts{byUser: byUser}
}

// Destination returns where to reach a user on a channel.
func (c *ConfigContacts) Destination(userID string, ch core.Channel) (string, bool) {
	m, ok := c.byUser[userID]
	if !ok {
		return "", false
	}
	dest, ok := m[ch]
	return dest, ok && dest != ""
}
