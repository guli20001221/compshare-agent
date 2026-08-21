package httpapi

import (
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/workflow"
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

// The allowance has to be sized from what the CODE permits, not from what users happened to do.
//
// This started as "holds the largest observed card run", asserting headroom over the five cards
// seen in 30 days of production traffic. That was measuring the wrong thing: guided create has
// eleven wizard steps and allows three re-asks after edits, so fourteen cards has always been
// reachable. A budget justified by traffic and described as a system bound is how the budget
// quietly becomes the thing that fails.
//
// Deriving it also keeps it honest as the wizard changes: adding a step moves the bound.
func TestInteractionAllowanceIsSizedFromTheCodeBound(t *testing.T) {
	if wsMaxConfirmationsPerTurn != workflow.MaxConfirmationsPerWorkflowTurn {
		t.Fatalf("transport card count %d must be the workflow's own bound %d, not a local guess",
			wsMaxConfirmationsPerTurn, workflow.MaxConfirmationsPerWorkflowTurn)
	}
	// The number production actually produced, kept only to state the gap the old constant hid.
	const largestObservedCardsPerTurn = 5
	if wsMaxConfirmationsPerTurn <= largestObservedCardsPerTurn {
		t.Fatalf("the code bound (%d) is not above the largest observed run (%d) — one of the two is wrong",
			wsMaxConfirmationsPerTurn, largestObservedCardsPerTurn)
	}
	if want := time.Duration(wsMaxConfirmationsPerTurn) * confirmWaitTimeout; wsInteractionAllowance != want {
		t.Fatalf("wsInteractionAllowance = %v, want %d cards x %v = %v",
			wsInteractionAllowance, wsMaxConfirmationsPerTurn, confirmWaitTimeout, want)
	}
}

// NOT covered, said plainly rather than left for someone to discover: the CALL SITE. Reverting
// HandleWS to `context.WithTimeout(..., minWSMachineLifetime)` leaves every assertion here green,
// because they exercise the function and not the deadline the socket actually gets. Observing the
// real deadline needs a test-only lifetime override plus a >10-minute integration test, which is
// more machinery than this bug is worth — so treat these as pinning the RULE, not its wiring.

// A handler with no config at all must not panic and must keep the floor: test paths
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
