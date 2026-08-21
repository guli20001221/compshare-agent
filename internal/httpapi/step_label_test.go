package httpapi

import (
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The console prints one line per step, keyed on the step frame's Action. Those
// Actions are internal tool names assembled from several sources, and a name
// with no display label reaches the user as raw English.
//
// These two tests are the reason the label map lives in Go: they enumerate the
// sources programmatically, so adding a tool anywhere fails here instead of
// shipping. They intentionally do not assert specific label text; wording is a
// product call, coverage is the invariant.

// Source 1+2+4: everything the model itself can be offered — tools.Registry
// entries exposed to the agent, the ReadCapability_ family, internal constants,
// and the generated Request<Operation> proposal tools.
func TestStepActionLabelCoversModelVisibleTools(t *testing.T) {
	names := engine.ModelVisibleToolNames()
	if len(names) < 20 {
		t.Fatalf("model-visible tool enumeration collapsed to %d names (%v) — the gate would pass vacuously", len(names), names)
	}
	var missing []string
	for _, name := range names {
		if stepActionLabel(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("model-visible tools with no console label: %v\n"+
			"add them to stepActionLabels in step_label.go — without one the user sees the raw English name",
			missing)
	}
}

// Source 3: the registry tools that deterministic handlers call. These are not
// in the model's window, but the planner-handler path emits the raw API action
// as a step, so the console shows them too.
func TestStepActionLabelCoversRegistryTools(t *testing.T) {
	if len(tools.Registry) < 20 {
		t.Fatalf("tools.Registry collapsed to %d entries — the gate would pass vacuously", len(tools.Registry))
	}
	var missing []string
	for _, tool := range tools.Registry {
		if tool.Function == nil || tool.Function.Name == "" {
			continue
		}
		if stepActionLabel(tool.Function.Name) == "" {
			missing = append(missing, tool.Function.Name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("registered tools with no console label: %v\nadd them to stepActionLabels in step_label.go", missing)
	}
}

// Request<Operation> labels are derived, not listed. This asserts the derivation
// actually fires for a generated name and stays anchored to the workflow's own
// label — if proposalToolName's naming scheme changes, this fails rather than
// silently falling back to "".
func TestStepActionLabelDerivesProposalToolsFromTheirWorkflow(t *testing.T) {
	if got, want := stepActionLabel("RequestCreateInstance"), "发起"+stepActionLabel("CreateInstanceWorkflow")+"请求"; got != want {
		t.Errorf("RequestCreateInstance label = %q, want %q", got, want)
	}
	// Derivation must not invent labels for a workflow that does not exist.
	if got := stepActionLabel("RequestFabricatedThing"); got != "" {
		t.Errorf("unknown proposal tool got label %q, want empty so the console can fall back", got)
	}
	// Every generated proposal tool in the live window must resolve — this is
	// what catches "a write op was added but its workflow label was not".
	for _, name := range engine.ModelVisibleToolNames() {
		if !strings.HasPrefix(name, "Request") {
			continue
		}
		if !strings.HasPrefix(stepActionLabel(name), "发起") {
			t.Errorf("proposal tool %s did not derive a label from its workflow (got %q)", name, stepActionLabel(name))
		}
	}
}

// An unknown action must degrade to no Label at all rather than to some
// placeholder — the console falls back to its own map, and a placeholder would
// override it with something worse.
func TestStepActionLabelIsEmptyForUnknownActions(t *testing.T) {
	for _, action := range []string{"", "BrandNewTool", "ReadCapability_not_a_real_intent"} {
		if got := stepActionLabel(action); got != "" {
			t.Errorf("stepActionLabel(%q) = %q, want empty", action, got)
		}
	}
}

// The map above is worthless if the frame never carries it. This drives the real
// SSE dispatch path and asserts the wire actually gains Label next to Action —
// the console reads Label, so a correct map plus an unwired frame is invisible.
func TestDispatchChatStepFrameCarriesTheActionLabel(t *testing.T) {
	llmFake := &toolTurnLLM{}
	eng := engine.NewWithDeps(llmFake, toolTurnExecutor{}, denyConfirm)
	eng.RehydrateHistory(nil)

	sess := store.Session{
		ID:                "sess-step-label",
		TopOrganizationID: 1,
		OrganizationID:    2,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	sessions := &mockSessions{byID: map[string]store.Session{sess.ID: sess}}
	h := newChatTestHandlersWith(t, eng, sessions)

	sink, _ := dispatchChatTurn(t, h, sess.ID, "show instances")
	body := sink.body()

	// The fake LLM calls DescribeCompShareInstance; its frame must be labelled.
	require.Contains(t, body, `"Action":"DescribeCompShareInstance"`)
	assert.Contains(t, body, `"Label":"`+stepActionLabel("DescribeCompShareInstance")+`"`)
	// Label sits next to Action, not in place of it — the console still keys on
	// Action for its own behavior (STEP_DETAIL_ACTIONS et al.).
	assert.Contains(t, body, `"Action":"DescribeCompShareInstance","Label":"查询实例信息"`)
}
