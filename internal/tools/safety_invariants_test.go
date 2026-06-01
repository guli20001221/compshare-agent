package tools

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/security"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file locks the three mutating-safety invariants as SET-WIDE assertions
// (every action in the policy/registry set, not a single sampled action), so the
// upcoming skill-executor refactor — and any atomic mutating tool added
// just-in-time when its skill lands (route phase P3b) — cannot silently break
// them. Each test iterates the whole set rather than one action, so a newly
// registered action is auto-covered the moment it appears.
//
// The existing TestSafeExecutorRejectsDestructiveActions /
// TestSafeExecutorCanDisableMutatingActions / RespectsReadOnlyFilter remain as
// targeted single-action behavior tests; these are their set-wide supersets.

// Invariant 1 — destructive is a permanent red line.
// ExecuteSafe checks L2/Destructive BEFORE the mutating-tools-flag gate, so
// enabling writes must NEVER unlock a destructive action. We assert with the flag
// ENABLED (the most permissive config) for EVERY destructive action: the flag is
// exactly the thing that must not matter here.
func TestInvariant_DestructiveActionsRefusedEvenWhenMutatingEnabled(t *testing.T) {
	policies := DefaultToolExecutionPolicies()
	checked := 0
	for action, policy := range policies {
		if policy.SecurityLevel != security.L2 && policy.Class != ActionClassDestructive {
			continue
		}
		inner := &spyExecutor{}
		safe := NewSafeToolExecutor(inner, WithMutatingToolsEnabled(true))

		_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
			Action: action,
			Args:   map[string]any{"UHostId": "uhost-1"},
			Origin: OriginDirectLLM,
		})

		require.ErrorIsf(t, err, ErrDestructiveAction,
			"destructive action %q must be refused even with mutating tools enabled", action)
		assert.Equalf(t, 0, inner.calls,
			"destructive action %q must never reach the inner executor", action)
		checked++
	}
	// Guard against the destructive set silently shrinking to empty (which would
	// make the loop vacuously pass). The four known L2 actions are Terminate
	// instance / Terminate custom image / Delete disk / Delete team.
	require.GreaterOrEqualf(t, checked, 4,
		"expected at least the 4 known L2/destructive actions, found %d — did the destructive set shrink?", checked)
}

// Invariant 2 — read-only is the shipped default.
// Every ExternalAPI mutating action must be gated OFF when the mutating-tools flag
// is false (the shipped default). Set-wide so an atomic write tool registered later
// (P3b) cannot ship reachable-by-default; it is covered the moment it is added.
func TestInvariant_MutatingActionsDisabledByDefault(t *testing.T) {
	policies := DefaultToolExecutionPolicies()
	checked := 0
	for action, policy := range policies {
		// Only ExternalAPI mutating actions reach the mutating-flag gate; workflow-
		// routed actions are stopped earlier (ErrNonExternalAction) and are driven
		// through the workflow engine, not ExecuteSafe directly.
		if policy.Class != ActionClassMutating || policy.Route != ActionRouteExternalAPI {
			continue
		}
		inner := &spyExecutor{}
		safe := NewSafeToolExecutor(inner, WithMutatingToolsEnabled(false))

		_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
			Action: action,
			Args:   map[string]any{"UHostId": "uhost-1"},
			Origin: OriginDirectLLM,
		})

		require.ErrorIsf(t, err, ErrMutatingActionDisabled,
			"mutating action %q must be disabled in the read-only default", action)
		assert.Equalf(t, 0, inner.calls,
			"mutating action %q must never reach the inner executor when disabled", action)
		checked++
	}
	require.Positivef(t, checked,
		"expected at least one ExternalAPI mutating action in the policy set, found %d", checked)
}

// Invariant 3 — the read-only registry never exposes a write tool to the LLM.
// VisibleRegistry(false) (the shipped default surface) must contain NO tool whose
// policy is workflow-routed, mutating, or destructive. This is the structural
// guarantee that registering a mutating tool (P3b atomic write tools) cannot leak
// into the default LLM surface — only flipping COMPSHARE_ENABLE_MUTATING_TOOLS does.
func TestInvariant_ReadOnlyRegistryHidesMutatingAndWorkflowTools(t *testing.T) {
	policies := DefaultToolExecutionPolicies()
	visible := VisibleRegistry(false)

	for _, tool := range visible {
		if tool.Function == nil {
			continue
		}
		name := tool.Function.Name
		policy, ok := policies[name]
		require.Truef(t, ok, "visible read-only tool %q has no execution policy", name)

		assert.NotEqualf(t, ActionRouteWorkflow, policy.Route,
			"read-only registry must not expose workflow-routed tool %q", name)
		assert.NotEqualf(t, ActionClassMutating, policy.Class,
			"read-only registry must not expose mutating tool %q", name)
		assert.NotEqualf(t, ActionClassDestructive, policy.Class,
			"read-only registry must not expose destructive tool %q", name)
	}

	// Inverse: enabling mutating exposes strictly more tools, proving the filter —
	// not mere absence from Registry — is what gates the write tools.
	require.Greaterf(t, len(VisibleRegistry(true)), len(visible),
		"mutating-enabled registry (%d) must expose strictly more tools than read-only (%d)",
		len(VisibleRegistry(true)), len(visible))
}
