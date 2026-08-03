package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"testing"
	"time"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkflowScopesRetainReadsAndOnlySelectedWrite(t *testing.T) {
	tests := []struct {
		operation string
		reason    string
		tool      string
	}{
		{"CreateCustomImageWorkflow", scopeReasonCreateCustomImage, "RequestCreateCustomImage"},
		{"CloneCustomImageWorkflow", scopeReasonCloneCustomImage, "RequestCloneCustomImage"},
		{"ReinstallInstanceWorkflow", scopeReasonReinstallInstance, "RequestReinstallInstance"},
	}

	for _, tc := range tests {
		t.Run(tc.operation, func(t *testing.T) {
			scope, ok := workflowToolScope(tc.operation)
			require.True(t, ok)
			assert.Equal(t, tools.ToolScopeNamed, scope.Mode)
			assert.Equal(t, tc.reason, scope.Reason)

			want := append([]string(nil), centralAgentToolNames(false, true)...)
			want = append(want, tc.tool)
			assert.ElementsMatch(t, want, scope.Names)
			assert.ElementsMatch(t, want, toolNames(centralAgentToolWindowForScope(true, true, scope)))
			assert.Contains(t, scope.Names, imageListToolName, "read-only catalog/recommendation tools stay usable")
			for _, other := range []string{"RequestCreateInstance", "RequestCreateCustomImage", "RequestCloneCustomImage", "RequestReinstallInstance"} {
				if other != tc.tool {
					assert.NotContains(t, scope.Names, other)
				}
			}
		})
	}
}

func TestImageBrowseKeepsFullWindowAndAllSources(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetIntentScopedTools(true, 100)
	eng.initializeModelToolWindowScope()
	for _, source := range []string{"community", "platform", "custom", "shared"} {
		eng.advanceModelToolWindowScope(imageListToolName, map[string]any{"source": source})
	}

	assert.Equal(t, tools.ToolScopeMutableFull, eng.modelToolWindowScopeThisTurn.Mode)
	assert.Equal(t, scopeReasonAwaitingTypedIntent, eng.modelToolWindowScopeThisTurn.Reason)
	window := eng.modelToolWindow(false)
	assert.Equal(t, centralAgentToolNames(true, false), toolNames(window))
	for _, tool := range window {
		if tool.Function == nil || tool.Function.Name != imageListToolName {
			continue
		}
		params := tool.Function.Parameters.(map[string]any)
		properties := params["properties"].(map[string]any)
		source := properties["source"].(map[string]any)
		assert.ElementsMatch(t, []string{"platform", "community", "custom", "shared"}, source["enum"])
		return
	}
	t.Fatal("image list tool missing from full window")
}

func TestToolWindowScopeLeavesGuidedCreateAndPendingWorkflowFull(t *testing.T) {
	_, ok := workflowToolScope("CreateInstanceWorkflow")
	assert.False(t, ok, "guided CreateInstance remains on its already-proven full/card path")

	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetIntentScopedTools(true, 100)
	eng.sessionStateHydrated = true
	eng.sessionState.ContextFrame = ContextFrame{
		Kind:           ContextFrameKindWorkflowTask,
		Status:         ContextFrameStatusPending,
		Workflow:       "CloneCustomImageWorkflow",
		ProducedAtUnix: time.Now().Unix(),
		TTLSeconds:     ContextFrameTTLSeconds,
	}

	// A carried ContextFrame can be an incomplete proposal and is not a signed
	// continuation token. A new user message must retain every normal tool.
	eng.initializeModelToolWindowScope()
	assert.Equal(t, tools.ToolScopeMutableFull, eng.modelToolWindowScopeThisTurn.Mode)
	assert.Equal(t, scopeReasonAwaitingTypedIntent, eng.modelToolWindowScopeThisTurn.Reason)
	assert.Equal(t, centralAgentToolNames(true, false), toolNames(eng.modelToolWindow(false)))
}

func TestToolWindowScopeRejectsUnadvertisedTool(t *testing.T) {
	exec := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, exec, nil)
	eng.modelToolWindowScopeThisTurn, _ = workflowToolScope("CreateCustomImageWorkflow")

	outOfScope := eng.executeTool(context.Background(), toolCall("out", "RequestCloneCustomImage", `{}`), noopStep)
	result, ok := tools.ParseAgentToolResult(outOfScope)
	require.True(t, ok)
	assert.Equal(t, tools.AgentToolStatusFailed, result.Status)
	assert.Equal(t, "TOOL_NOT_IN_SCOPE", result.Error.Code)
	assert.Empty(t, exec.calls, "the rejection must happen before any executor call")
}

func TestReinstallScopePreservesCustomAndSharedWorkflowInputs(t *testing.T) {
	scope, ok := workflowToolScope("ReinstallInstanceWorkflow")
	require.True(t, ok)
	window := centralAgentToolWindowForScope(true, false, scope)
	for _, tool := range window {
		if tool.Function == nil || tool.Function.Name != "RequestReinstallInstance" {
			continue
		}
		params := tool.Function.Parameters.(map[string]any)
		properties := params["properties"].(map[string]any)
		source := properties["ImageSource"].(map[string]any)
		assert.ElementsMatch(t, []string{"platform", "community", "custom", "sharing", "shared"}, source["enum"])
		return
	}
	t.Fatal("reinstall proposal missing from its scoped window")
}

func TestToolWindowScopeAlsoRejectsNamedWritesWhenSessionIsReadOnly(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetMutatingToolsEnabled(false)
	eng.modelToolWindowScopeThisTurn, _ = workflowToolScope("CreateCustomImageWorkflow")

	result := eng.executeTool(context.Background(), toolCall("write", "RequestCreateCustomImage", `{}`), noopStep)
	parsed, ok := tools.ParseAgentToolResult(result)
	require.True(t, ok)
	assert.Equal(t, tools.AgentToolStatusFailed, parsed.Status)
	assert.Equal(t, "TOOL_NOT_IN_SCOPE", parsed.Error.Code)
	assert.NotContains(t, eng.completionToolNames(eng.completionToolWindowScope()), "RequestCreateCustomImage")
}

func TestToolWindowScopeAdvancesFromAdvertisedWorkflowAndResetsOnNewTurn(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("clone", "RequestCloneCustomImage", `{}`)}},
		{Content: "请补充源镜像和目标可用区。"},
	}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	eng.SetIntentScopedTools(true, 100)
	var trace observability.TurnCompletionTrace
	eng.SetTurnCompletionObserver(func(got observability.TurnCompletionTrace) { trace = got })

	_, err := eng.Chat(context.Background(), "把镜像复制到另一个可用区", noopStep)
	require.NoError(t, err)
	require.Len(t, model.calls, 2)
	assert.Equal(t, centralAgentToolNames(true, false), toolNames(model.calls[0].Tools))
	wantLater := append([]string(nil), centralAgentToolNames(false, false)...)
	wantLater = append(wantLater, "RequestCloneCustomImage")
	assert.ElementsMatch(t, wantLater, toolNames(model.calls[1].Tools))
	assert.Equal(t, "named", trace.ToolScope)
	assert.Equal(t, "last_outbound_agent_tool_window", trace.ToolScopePhase)
	assert.Equal(t, scopeReasonCloneCustomImage, trace.ToolScopeReason)
	assert.ElementsMatch(t, wantLater, trace.ToolNames)

	eng.initializeModelToolWindowScope()
	assert.Equal(t, tools.ToolScopeMutableFull, eng.modelToolWindowScopeThisTurn.Mode)
	assert.Equal(t, scopeReasonAwaitingTypedIntent, eng.modelToolWindowScopeThisTurn.Reason)
}

func TestGuidedCreateProposalKeepsFullWindow(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetIntentScopedTools(true, 100)
	eng.initializeModelToolWindowScope()

	eng.advanceModelToolWindowScope("RequestCreateInstance", map[string]any{})
	assert.Equal(t, tools.ToolScopeMutableFull, eng.modelToolWindowScopeThisTurn.Mode)
	assert.Equal(t, scopeReasonAwaitingTypedIntent, eng.modelToolWindowScopeThisTurn.Reason)
	assert.Equal(t, centralAgentToolNames(true, false), toolNames(eng.modelToolWindow(false)))
}

func TestGuidedCreateCardPathKeepsFullWindow(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("create", "RequestCreateInstance", `{}`)}},
	}}
	eng := NewWithDeps(model, guidedIntakeExecutor(), func(string, map[string]any) bool { return true })
	eng.SetIntentScopedTools(true, 100)
	formShown := false

	_, err := eng.ChatWithOptions(context.Background(), "创建一台虚机", noopStep, ChatOptions{
		GuidedCreate: true,
		ConfirmEditsFunc: func(_ string, _ map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
			formShown = form != nil
			return workflow.ConfirmResolution{Confirmed: false}
		},
	})
	require.NoError(t, err)
	require.True(t, formShown, "the existing guided create card must still be rendered")
	require.Len(t, model.calls, 1)
	assert.Equal(t, centralAgentToolNames(true, false), toolNames(model.calls[0].Tools))
	assert.Equal(t, tools.ToolScopeMutableFull, eng.modelToolWindowScopeThisTurn.Mode)
	assert.Equal(t, scopeReasonAwaitingTypedIntent, eng.modelToolWindowScopeThisTurn.Reason)
}

func TestToolWindowScopeDoesNotAdvanceForHiddenReadOnlyWrite(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetMutatingToolsEnabled(false)
	eng.SetIntentScopedTools(true, 100)
	eng.initializeModelToolWindowScope()

	eng.advanceModelToolWindowScope("RequestCreateCustomImage", map[string]any{})
	assert.Equal(t, tools.ToolScopeReadOnlyFull, eng.modelToolWindowScopeThisTurn.Mode)
	assert.Equal(t, scopeReasonAwaitingTypedIntent, eng.modelToolWindowScopeThisTurn.Reason)
}

func TestToolWindowScopeRolloutIsDeterministicAndFailsClosed(t *testing.T) {
	const subject = "opaque-subject"
	digest := sha256.Sum256([]byte(subject))
	wantBucket := int(binary.BigEndian.Uint64(digest[:8]) % 100)
	assert.Equal(t, wantBucket, scopedToolsRolloutBucket(subject))

	eng := &Engine{intentScopedToolsEnabled: true, rateLimitSubject: subject}
	eng.intentScopedToolsRolloutPercent = 0
	assert.False(t, eng.scopedToolsRolloutSelected())
	eng.intentScopedToolsRolloutPercent = 100
	assert.True(t, eng.scopedToolsRolloutSelected())
	if wantBucket > 0 {
		eng.intentScopedToolsRolloutPercent = wantBucket
		assert.False(t, eng.scopedToolsRolloutSelected())
	}
	if wantBucket < 99 {
		eng.intentScopedToolsRolloutPercent = wantBucket + 1
		assert.True(t, eng.scopedToolsRolloutSelected())
	}

	eng.rateLimitSubject = governance.AnonymousSubjectKey
	eng.intentScopedToolsRolloutPercent = 37
	assert.False(t, eng.scopedToolsRolloutSelected(), "anonymous sessions remain on full tools during a partial rollout")
}

func TestToolWindowScopeKnowledgeOnlyRemainsTheStricterAuthority(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.modelToolWindowScopeThisTurn, _ = workflowToolScope("CreateCustomImageWorkflow")
	eng.knowledgeOnlyThisTurn = true

	scope := eng.completionToolWindowScope()
	assert.Equal(t, tools.ToolScopeReadOnlyFull, scope.Mode)
	assert.Equal(t, scopeReasonKnowledgeOnly, scope.Reason)
	names := eng.completionToolNames(scope)
	assert.Contains(t, names, "SearchKnowledge")
	assert.NotContains(t, names, "RequestCreateCustomImage")
	assert.Equal(t, toolNames(centralAgentKnowledgeToolWindow()), toolNames(eng.modelToolWindow(true)))
	assert.True(t, eng.modelToolCallAllowed("SearchKnowledge"))
	assert.False(t, eng.modelToolCallAllowed("RequestCreateCustomImage"))
}

func TestToolWindowScopeIsPerSessionAndPerTurn(t *testing.T) {
	deps := &SharedDeps{
		LLMClient:                       &mockLLM{},
		ExternalExecutor:                &mockExecutor{},
		IntentScopedToolsEnabled:        true,
		IntentScopedToolsRolloutPercent: 100,
	}
	left := NewSession(deps, SessionOptions{Subject: "left", MutatingToolsEnabled: true})
	right := NewSession(deps, SessionOptions{Subject: "right", MutatingToolsEnabled: true})
	left.initializeModelToolWindowScope()
	left.advanceModelToolWindowScope("RequestCloneCustomImage", map[string]any{})
	require.Equal(t, tools.ToolScopeNamed, left.modelToolWindowScopeThisTurn.Mode)

	right.initializeModelToolWindowScope()
	assert.Equal(t, tools.ToolScopeMutableFull, right.modelToolWindowScopeThisTurn.Mode)
	left.initializeModelToolWindowScope()
	assert.Equal(t, tools.ToolScopeMutableFull, left.modelToolWindowScopeThisTurn.Mode)
}

func TestToolWindowScopeConfigurationCopiesIntoEachSession(t *testing.T) {
	eng := NewSession(&SharedDeps{
		LLMClient:                       &mockLLM{},
		ExternalExecutor:                &mockExecutor{},
		IntentScopedToolsEnabled:        true,
		IntentScopedToolsRolloutPercent: 25,
	}, SessionOptions{MutatingToolsEnabled: true})
	assert.True(t, eng.intentScopedToolsEnabled)
	assert.Equal(t, 25, eng.intentScopedToolsRolloutPercent)

	failClosed := NewSession(&SharedDeps{
		LLMClient:                       &mockLLM{},
		ExternalExecutor:                &mockExecutor{},
		IntentScopedToolsEnabled:        true,
		IntentScopedToolsRolloutPercent: 101,
	}, SessionOptions{MutatingToolsEnabled: true})
	assert.True(t, failClosed.intentScopedToolsEnabled)
	assert.Zero(t, failClosed.intentScopedToolsRolloutPercent)
	assert.False(t, failClosed.scopedToolsRolloutSelected())
}
