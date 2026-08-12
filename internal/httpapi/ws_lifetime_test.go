package httpapi

import (
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
)

// The socket must outlive the work it carries. These two budgets were independent constants until
// 2026-07-30, and a live frontend run showed what that costs: agent.ssh_ops.timeout was 12m, the
// connection deadline was a flat 10m, and an in-instance repair was cut off at exactly 10:00.0 with
// the user seeing only "[NetworkError] 连接已关闭". By then the lane had already replaced an
// application directory on the box — so the turn that was killed was the one that had changed the
// most, and nothing was delivered saying so.
//
// The assertion is the INVARIANT (machine lifetime > lane budget), not a specific number, so raising
// either budget cannot quietly reintroduce the contradiction.
func TestWSMachineLifetimeOutlivesTheLaneBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		lane time.Duration
		want time.Duration
	}{
		{"no lane configured keeps the wedged-connection floor", 0, minWSMachineLifetime},
		{"a lane well inside the floor changes nothing", 5 * time.Minute, minWSMachineLifetime},
		// The exact shape of the live failure.
		{"a lane budget near the floor extends the socket", 12 * time.Minute, 14 * time.Minute},
		// No silent clamp: a ceiling that cuts a longer lane off would be the same bug again.
		{"a long lane budget is honoured, not capped", 30 * time.Minute, 32 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handlers{cfg: &config.Config{}}
			h.cfg.Agent.SSHOps.Timeout = tc.lane
			got := h.wsMachineLifetime()
			if got != tc.want {
				t.Fatalf("wsMachineLifetime() = %v, want %v", got, tc.want)
			}
			if tc.lane > 0 && got <= tc.lane {
				t.Fatalf("machine lifetime (%v) must outlive the lane budget (%v)", got, tc.lane)
			}
		})
	}
}

// The 2026-08-12 half of the same rule: a socket sized only for MACHINE work kills turns that spend
// their time waiting for a PERSON.
//
// wsLaneSlack was 2 minutes and its own comment charged the operator's consent cards to it. That
// was survivable at a 60s card only by accident; production traces over 30 days show turns carrying
// five cards, so raising the card budget to 120s puts up to 10 minutes of human time inside a
// 2-minute allowance. The failure would not have looked like a timeout — the socket would simply
// close mid-repair, which is the 2026-07-30 bug with a different clock driving it.
//
// The assertion is again the invariant, deliberately restating the sum from the two policy
// constants rather than from wsInteractionAllowance, so redefining the allowance in terms of
// something other than "cards x card budget" fails here instead of silently shrinking the room.
func TestWSConnLifetimeCoversMachineWorkPlusEveryCardsHumanTime(t *testing.T) {
	humanTime := time.Duration(wsMaxConfirmationsPerTurn) * confirmWaitTimeout

	for _, lane := range []time.Duration{0, 5 * time.Minute, 12 * time.Minute, 30 * time.Minute} {
		h := &Handlers{cfg: &config.Config{}}
		h.cfg.Agent.SSHOps.Timeout = lane

		machine := h.wsMachineLifetime()
		got := h.wsConnLifetime()

		if want := machine + humanTime; got != want {
			t.Fatalf("lane=%v: wsConnLifetime() = %v, want machine(%v) + human(%v) = %v",
				lane, got, machine, humanTime, want)
		}
		// The concrete shape of the reported incident: a repair that uses its whole lane budget
		// AND stops for its cards must still fit.
		if lane > 0 && got <= lane+humanTime {
			t.Fatalf("lane=%v: socket (%v) must outlive the lane budget plus every card's wait (%v)",
				lane, got, lane+humanTime)
		}
	}
}

// Human time must not be derived from the machine budget. agent.ssh_ops.timeout states how long a
// harness may work inside an instance; it says nothing about how long a person may take to read a
// card, and folding one into the other is what made a careful reader look like a slow subprocess.
// Changing only the lane budget must therefore leave the interaction allowance untouched.
func TestInteractionAllowanceDoesNotMoveWithTheLaneBudget(t *testing.T) {
	short := &Handlers{cfg: &config.Config{}}
	short.cfg.Agent.SSHOps.Timeout = time.Minute
	long := &Handlers{cfg: &config.Config{}}
	long.cfg.Agent.SSHOps.Timeout = time.Hour

	shortAllowance := short.wsConnLifetime() - short.wsMachineLifetime()
	longAllowance := long.wsConnLifetime() - long.wsMachineLifetime()

	if shortAllowance != longAllowance {
		t.Fatalf("interaction allowance moved with the lane budget: %v vs %v", shortAllowance, longAllowance)
	}
	if shortAllowance != wsInteractionAllowance {
		t.Fatalf("interaction allowance = %v, want %v", shortAllowance, wsInteractionAllowance)
	}
}

// The allowance has to hold every card a turn can actually show, or it is the old 2-minute slack
// with a bigger number. Sized from measured production turns (five cards was the largest observed
// over 30 days of agent_traces on 2026-08-12), with one card of headroom.
func TestInteractionAllowanceHoldsTheLargestObservedCardRun(t *testing.T) {
	const largestObservedCardsPerTurn = 5
	if wsMaxConfirmationsPerTurn <= largestObservedCardsPerTurn {
		t.Fatalf("wsMaxConfirmationsPerTurn = %d leaves no headroom over the %d cards seen in production",
			wsMaxConfirmationsPerTurn, largestObservedCardsPerTurn)
	}
	if want := largestObservedCardsPerTurn * confirmWaitTimeout; wsInteractionAllowance <= want {
		t.Fatalf("wsInteractionAllowance = %v cannot hold %d cards at %v each (%v)",
			wsInteractionAllowance, largestObservedCardsPerTurn, confirmWaitTimeout, want)
	}
}

// NOT covered, said plainly rather than left for someone to discover: the CALL SITE. Reverting
// HandleWS to `context.WithTimeout(..., minWSMachineLifetime)` leaves every assertion here green,
// because they exercise the function and not the deadline the socket actually gets. Observing the
// real deadline needs a test-only lifetime override plus a >10-minute integration test, which is
// more machinery than this bug is worth — so treat these as pinning the RULE, not its wiring.

// A handler with no config at all must not panic and must keep the floor: the CLI/test paths
// construct Handlers without one.
func TestWSConnLifetimeWithoutConfig(t *testing.T) {
	if got := (&Handlers{}).wsMachineLifetime(); got != minWSMachineLifetime {
		t.Fatalf("wsMachineLifetime() = %v, want %v", got, minWSMachineLifetime)
	}
	if got := (&Handlers{}).wsConnLifetime(); got != minWSMachineLifetime+wsInteractionAllowance {
		t.Fatalf("wsConnLifetime() = %v, want %v", got, minWSMachineLifetime+wsInteractionAllowance)
	}
}

// The engine phrases the timeout message with its own copy of the card budget, because engine must
// not import a transport package. That copy has to say the same number the transport enforces, or
// the reply tells a user to answer within a window that is not the one the server is counting.
func TestInstanceOpsConfirmWindowMatchesTheTransport(t *testing.T) {
	if engine.InstanceOpsConfirmWindow != confirmWaitTimeout {
		t.Fatalf("engine quotes a %v confirmation window, transport enforces %v",
			engine.InstanceOpsConfirmWindow, confirmWaitTimeout)
	}
}
