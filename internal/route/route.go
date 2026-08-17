// Package route decides which team and escalation policy an alert belongs to,
// and carries the grouping parameters that decision implies.
//
// Matching follows Alertmanager's semantics rather than inventing new ones:
// routes are evaluated in configuration order and the first match wins, so an
// operator can put specific rules above general ones and reason about the
// result by reading top to bottom (spec section 6.3).
package route

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Sarthak-47/kerberon/internal/alert"
	"github.com/Sarthak-47/kerberon/internal/config"
	"github.com/Sarthak-47/kerberon/internal/core"
)

// Route is a resolved routing rule.
type Route struct {
	// Name is stable across restarts and participates in the group key. Two
	// routes never share an incident even for identical labels.
	Name   string
	Match  map[string]string
	Team   string
	Policy string

	GroupBy        []string
	GroupWait      time.Duration
	GroupInterval  time.Duration
	ResolveGrace   time.Duration
	VolatileLabels []string
}

// GroupKey is the incident identity for an alert on this route.
func (r Route) GroupKey(labels core.Labels) string {
	return alert.GroupKey(r.Name, r.GroupBy, labels)
}

// Fingerprint identifies an alert on this route, ignoring its volatile labels.
func (r Route) Fingerprint(labels core.Labels) string {
	return alert.Fingerprint(labels, r.VolatileLabels)
}

// Router matches alerts to routes.
type Router struct {
	routes []Route
}

// New builds a Router from configuration. Defaults have already been applied by
// config.Load, so the durations here are the effective ones.
func New(cfg *config.Config) *Router {
	rs := make([]Route, 0, len(cfg.Routes))
	for i, c := range cfg.Routes {
		rs = append(rs, Route{
			Name:           routeName(c, i),
			Match:          copyMap(c.Match),
			Team:           c.Team,
			Policy:         c.Policy,
			GroupBy:        append([]string(nil), c.GroupBy...),
			GroupWait:      c.GroupWait.Std(),
			GroupInterval:  c.GroupInterval.Std(),
			ResolveGrace:   c.ResolveGrace.Std(),
			VolatileLabels: append([]string(nil), c.VolatileLabels...),
		})
	}
	return &Router{routes: rs}
}

// routeName returns the configured name, or derives a stable one.
//
// The index is deliberately not used: reordering routes in the config would
// then change every group key, splitting open incidents in two and paging
// people again for something already being handled. Deriving from the match
// criteria, team and policy means only a meaningful edit changes identity.
func routeName(c config.Route, index int) string {
	if n := strings.TrimSpace(c.Name); n != "" {
		return n
	}

	keys := make([]string, 0, len(c.Match))
	for k := range c.Match {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte("="))
		h.Write([]byte(c.Match[k]))
		h.Write([]byte{0})
	}
	h.Write([]byte(c.Team))
	h.Write([]byte{0})
	h.Write([]byte(c.Policy))

	// Short, readable, and long enough that a collision is not a practical
	// concern for the handful of routes a config holds.
	return "route-" + hex.EncodeToString(h.Sum(nil))[:12]
}

// Match returns the first route whose criteria the labels satisfy.
//
// An alert matching nothing is not routed and therefore never pages anyone,
// which is precisely the failure this product exists to prevent. Callers must
// surface that loudly rather than dropping it; see ErrNoRoute.
func (r *Router) Match(labels core.Labels) (Route, bool) {
	for _, rt := range r.routes {
		if matches(rt.Match, labels) {
			return rt, true
		}
	}
	return Route{}, false
}

// Routes returns every configured route, in evaluation order.
func (r *Router) Routes() []Route { return r.routes }

// ErrNoRoute reports an alert that matched no route. It carries the labels so
// the operator can see what arrived and fix the config.
type ErrNoRoute struct {
	Labels core.Labels
}

func (e *ErrNoRoute) Error() string {
	return fmt.Sprintf("no route matched alert with labels %s; it would never page anyone",
		formatLabels(e.Labels))
}

// matches reports whether every criterion is satisfied. Matching is exact:
// Alertmanager's regex form is deliberately not supported in v1, since the spec
// specifies only equality and a regex that fails to match is a silent
// non-page.
//
// An empty criteria map matches everything, which is how a catch-all route is
// written. config validation rejects that, so it can only arise from a Router
// built by hand.
func matches(criteria, labels core.Labels) bool {
	for k, want := range criteria {
		if labels[k] != want {
			return false
		}
	}
	return true
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func formatLabels(l core.Labels) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, l[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
