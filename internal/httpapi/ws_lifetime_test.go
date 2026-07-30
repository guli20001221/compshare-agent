package httpapi

import (
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
)

// The socket must outlive the work it carries. These two budgets were independent constants until
// 2026-07-30, and a live frontend run showed what that costs: agent.ssh_ops.timeout was 12m, the
// connection deadline was a flat 10m, and an in-instance repair was cut off at exactly 10:00.0 with
// the user seeing only "[NetworkError] 连接已关闭". By then the lane had already replaced an
// application directory on the box — so the turn that was killed was the one that had changed the
// most, and nothing was delivered saying so.
//
// The assertion is the INVARIANT (lifetime > lane budget), not a specific number, so raising either
// budget cannot quietly reintroduce the contradiction.
func TestWSConnLifetimeOutlivesTheLaneBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		lane time.Duration
		want time.Duration
	}{
		{"no lane configured keeps the wedged-connection floor", 0, minWSConnLifetime},
		{"a lane well inside the floor changes nothing", 5 * time.Minute, minWSConnLifetime},
		// The exact shape of the live failure.
		{"a lane budget near the floor extends the socket", 12 * time.Minute, 14 * time.Minute},
		// No silent clamp: a ceiling that cuts a longer lane off would be the same bug again.
		{"a long lane budget is honoured, not capped", 30 * time.Minute, 32 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handlers{cfg: &config.Config{}}
			h.cfg.Agent.SSHOps.Timeout = tc.lane
			got := h.wsConnLifetime()
			if got != tc.want {
				t.Fatalf("wsConnLifetime() = %v, want %v", got, tc.want)
			}
			if tc.lane > 0 && got <= tc.lane {
				t.Fatalf("socket (%v) must outlive the lane budget (%v)", got, tc.lane)
			}
		})
	}
}

// NOT covered, said plainly rather than left for someone to discover: the CALL SITE. Reverting
// HandleWS to `context.WithTimeout(..., minWSConnLifetime)` leaves every assertion here green,
// because they exercise the function and not the deadline the socket actually gets. Observing the
// real deadline needs a test-only lifetime override plus a >10-minute integration test, which is
// more machinery than this bug is worth — so treat these as pinning the RULE, not its wiring.

// A handler with no config at all must not panic and must keep the floor: the CLI/test paths
// construct Handlers without one.
func TestWSConnLifetimeWithoutConfig(t *testing.T) {
	if got := (&Handlers{}).wsConnLifetime(); got != minWSConnLifetime {
		t.Fatalf("wsConnLifetime() = %v, want %v", got, minWSConnLifetime)
	}
}
