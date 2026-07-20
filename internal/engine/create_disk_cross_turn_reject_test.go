package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// crossTurnDiskInstance is the source instance the CreateDisk workflow prices
// against. It carries a full GPU spec (GpuType/GPU/CPU/Memory) because
// addSourceInstanceSpecForDiskPrice refuses to price a disk without it — so a
// price step that reaches the confirmation card is genuine, not an early error.
func crossTurnDiskInstance() map[string]any {
	return map[string]any{"UHostSet": []any{
		map[string]any{
			"UHostId": "uhost-test", "Name": "test-gpu", "State": "Stopped",
			"Region": "cn-sh2", "Zone": "cn-sh2-02", "GpuType": "4090",
			"GPU": float64(1), "CPU": float64(16), "Memory": float64(65536),
			"ChargeType": "Postpay",
		},
	}}
}

// TestCreateDiskCrossTurnResumeRejectMakesZeroWrites is the permanent regression
// gate for the FirstDecision retirement (2026-07). The whole justification for
// deleting the forced-first-decision hop was that the free ReAct loop must carry a
// multi-turn write task across turns without a first-hop mechanism hijacking the
// second turn or slamming the write window shut after the first hop. This locks the
// exact scenario end to end, through the real per-turn Chat entry point (the seam
// FirstDecision used to hook), over ONE engine and TWO turns:
//
//	Turn 1  "给 uhost-test 加一个数据盘"  → the Agent proposes CreateDisk with no Size;
//	        the resolver reports Size missing and the engine PARKS a CreateDiskWorkflow
//	        task frame (only Size outstanding). No mutating call happens.
//	Turn 2  "200G"                      → the Agent re-proposes CreateDisk now carrying
//	        Size; the resolver readies it, the workflow prices the disk and reaches the
//	        confirmation card, the user REJECTS, and NOTHING is written.
//
// Non-vacuous by construction: the executor returns a real success for
// CreateAndAttachCompshareDisk (so a write would be recorded if it fired), and the
// test asserts the price step DID run (proving turn 2 reached the card, not an early
// error) while the create API did NOT. A first-hop regression that dropped the turn-1
// park or short-circuited turn 2 would break one of these assertions.
//
// Size is supplied as a JSON number because the RequestCreateDisk tool schema types
// Size as a number (a capacity field, internal/actionresolver/catalog.go); that is
// what the real model emits and what the deterministic resolver accepts.
func TestCreateDiskCrossTurnResumeRejectMakesZeroWrites(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":    crossTurnDiskInstance(),
		"GetCompShareInstancePrice":    {"PriceDetails": []any{map[string]any{"Disks": float64(0.8)}}},
		"CreateAndAttachCompshareDisk": {"UDiskId": "udisk-new"},
	}}
	rejectConfirm := func(string, map[string]any) bool { return false }
	mock := &mockLLM{responses: []llm.ChatResponse{
		// Turn 1: propose CreateDisk with the target but no Size.
		{ToolCalls: []openai.ToolCall{toolCall("t1", "RequestCreateDisk", `{"UHostId":"uhost-test"}`)}},
		{Content: "请问数据盘要多大？"},
		// Turn 2: re-propose CreateDisk now carrying Size (a number, per the schema).
		{ToolCalls: []openai.ToolCall{toolCall("t2", "RequestCreateDisk", `{"UHostId":"uhost-test","Size":200}`)}},
		{Content: "（此文本不应出现：拒绝确认后引擎自写未执行回复）"},
	}}

	eng := NewWithDeps(mock, executor, rejectConfirm)
	// Hydrate the session: setContextFrame is a no-op on an unhydrated engine, so a
	// cross-turn task only parks once the session state exists (as it always does on
	// the server path after RehydrateHistory).
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	// --- Turn 1: the incomplete proposal parks a task, writes nothing. ---
	_, err := eng.Chat(context.Background(), "给 uhost-test 加一个数据盘", noopStep)
	require.NoError(t, err)
	assert.NotContains(t, executor.calls, "CreateAndAttachCompshareDisk",
		"an incomplete create-disk proposal must never reach the mutating call")

	frame := eng.sessionState.ContextFrame
	assert.Equal(t, ContextFrameKindWorkflowTask, frame.Kind,
		"a missing-Size create-disk proposal must park a workflow task to resume next turn")
	assert.Equal(t, "CreateDiskWorkflow", frame.Workflow)
	assert.Contains(t, frame.MissingSlots, "Size",
		"the parked task must record that Size is the outstanding slot")

	// --- Turn 2: supplying the size resumes to the card; the reject writes nothing. ---
	reply, err := eng.Chat(context.Background(), "200G", noopStep)
	require.NoError(t, err)

	assert.Contains(t, executor.calls, "GetCompShareInstancePrice",
		"turn 2 must price the disk and reach the confirmation card, not fail early")
	assert.NotContains(t, executor.calls, "CreateAndAttachCompshareDisk",
		"a rejected confirmation must make ZERO real writes")
	assert.Contains(t, reply, "未执行",
		"a rejected write must be narrated honestly as not-executed")
	assert.NotContains(t, reply, "已取消",
		"a not-granted confirm must not falsely claim the user cancelled")
}
