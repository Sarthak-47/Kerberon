package schedule

import (
	"fmt"
	"sort"
	"time"

	"github.com/Sarthak-47/kerberon/internal/config"
	"github.com/Sarthak-47/kerberon/internal/core"
)

// CoverageWindow is how far ahead coverage is checked by default.
//
// Long enough to cross both DST transitions in either hemisphere, which is
// where a naive rotation develops a hole, and short enough that the check
// stays instant.
const CoverageWindow = 400 * 24 * time.Hour

// Gap is a period a schedule leaves uncovered.
type Gap struct {
	Schedule string
	Interval core.Interval
}

func (g Gap) String() string {
	return fmt.Sprintf("schedule %q has nobody on call from %s to %s (%s)",
		g.Schedule,
		g.Interval.Start.Format(time.RFC3339),
		g.Interval.End.Format(time.RFC3339),
		g.Interval.Duration())
}

// CheckCoverage reports every gap in every schedule over the window starting at
// `from`.
//
// This lives here rather than in config validation because the check needs the
// resolver, and config cannot import it without a cycle. It is called by
// kerberon validate so a hole in the rotation fails a pull request rather than
// being discovered when an incident goes unpaged.
func CheckCoverage(cfg *config.Config, from time.Time, window time.Duration) ([]Gap, error) {
	if window <= 0 {
		window = CoverageWindow
	}
	schedules, err := FromConfig(cfg)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(schedules))
	for name := range schedules {
		names = append(names, name)
	}
	sort.Strings(names)

	var gaps []Gap
	to := from.Add(window)
	for _, name := range names {
		// Overrides are deliberately not consulted. They are transient state,
		// so a rotation that only has coverage because someone happens to be
		// covering next Thursday is still a broken rotation.
		r := NewResolver(schedules[name], nil)
		for _, iv := range r.Gaps(from, to) {
			gaps = append(gaps, Gap{Schedule: name, Interval: iv})
		}
	}
	return gaps, nil
}
