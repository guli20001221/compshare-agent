package engine

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// An ID-only message followed by a prose acknowledgement must survive both a
// live engine and a persisted-history rebuild. The next "continue" is resolved
// by the central Agent reading that conversation, not a second target parser.
func TestColdTypedIDSurvivesAProseOnlyAcknowledgement(t *testing.T) {
	for _, cold := range []bool{false, true} {
		t.Run(map[bool]string{false: "hot", true: "cold"}[cold], func(t *testing.T) {
			const instanceID = "cpod-1uivn2vwu842"
			runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "排查完成", Ran: 1}}
			model := &mockLLM{responses: []llm.ChatResponse{
				{Content: "已记录当前实例。"},
				{ToolCalls: []openai.ToolCall{toolCall("diagnose", "DiagnoseInstanceInternals",
					"{\"UHostId\":\"cpod-1uivn2vwu842\",\"Task\":\"只读核查 GPU\"}")}},
			}}
			eng := NewWithDeps(model, &mockExecutor{}, nil)
			eng.SetInstanceOps(runner)
			eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
			require.True(t, eng.registry.NeedsRefresh(time.Now()))

			reply, err := eng.Chat(context.Background(), instanceID, noopStep)
			require.NoError(t, err)
			require.Zero(t, runner.calls)
			state, version, _ := eng.SessionStateSnapshot()
			require.Empty(t, state.SelectedInstanceID, "a prose acknowledgement is not an actual observation or confirmed workflow")

			if cold {
				rebuilt := NewWithDeps(model, &mockExecutor{}, nil)
				rebuilt.SetInstanceOps(runner)
				rebuilt.SetSessionState(state, version+1)
				rebuilt.RehydrateHistory([]HistoryMessage{
					{Role: openai.ChatMessageRoleUser, Content: instanceID},
					{Role: openai.ChatMessageRoleAssistant, Content: reply},
				})
				eng = rebuilt
			}
			_, err = eng.Chat(context.Background(), "继续", noopStep)
			require.NoError(t, err)
			require.Equal(t, 1, runner.calls)
			require.Equal(t, instanceID, runner.lastReq.InstanceID)
			require.Len(t, model.calls, 2)
			var history []openai.ChatCompletionMessage
			for _, message := range model.calls[1].Messages {
				if message.Role != openai.ChatMessageRoleSystem {
					history = append(history, message)
				}
			}
			require.Len(t, history, 3)
			require.Equal(t, instanceID, history[0].Content)
			require.Equal(t, reply, history[1].Content)
			require.Equal(t, "继续", history[2].Content)
			require.Equal(t, instanceID, runner.lastReq.Context.ConversationHistory[0].Content)
			state, _, _ = eng.SessionStateSnapshot()
			require.Equal(t, instanceID, state.SelectedInstanceID)
			require.Equal(t, SelectedInstanceSourceObserved, state.SelectedInstanceSource)
		})
	}
}

// A previous confirmed target is context, not permission to replace a different
// exact target the model resolves from a newer conversational reference.
func TestWorkflowTargetAfterProseOnlySwitchKeepsModelID(t *testing.T) {
	for _, cold := range []bool{false, true} {
		t.Run(map[bool]string{false: "hot", true: "cold"}[cold], func(t *testing.T) {
			const priorID, nextID = "uhost-a", "uhost-b"
			model := &mockLLM{responses: []llm.ChatResponse{
				{Content: "接下来讨论这台实例。"},
				{ToolCalls: []openai.ToolCall{toolCall("stop", "RequestStopInstance", `{"UHostId":"uhost-b"}`)}},
				{Content: "操作已取消。"},
			}}
			var describedIDs []string
			executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
				switch action {
				case "DescribeCompShareInstance":
					var id string
					switch ids := args["UHostIds"].(type) {
					case []string:
						require.Len(t, ids, 1)
						id = ids[0]
					case []any:
						require.Len(t, ids, 1)
						id, _ = ids[0].(string)
					}
					require.NotEmpty(t, id)
					describedIDs = append(describedIDs, id)
					return map[string]any{"UHostSet": []any{map[string]any{
						"UHostId": id, "State": "Running", "Zone": "cn-wlcb-01",
					}}}, nil
				case "DescribeCompShareSupportZone":
					return map[string]any{"ZoneInfo": []any{map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"}}}, nil
				default:
					return map[string]any{"RetCode": 0}, nil
				}
			}}
			var confirmedIDs []string
			confirm := func(action string, args map[string]any) bool {
				require.Equal(t, "StopInstanceWorkflow", action)
				id, _ := args["UHostId"].(string)
				confirmedIDs = append(confirmedIDs, id)
				return false
			}
			eng := NewWithDeps(model, executor, confirm)
			eng.SetMutatingToolsEnabled(true)
			eng.SetSessionState(SessionState{
				SchemaVersion: SessionStateSchemaCurrent, SelectedInstanceID: priorID,
				SelectedInstanceSource: SelectedInstanceSourceUser, SelectedInstanceAtUnix: time.Now().Unix(),
			}, 1)
			syncTwoInstances(t, eng)
			reply, err := eng.Chat(context.Background(), nextID, noopStep)
			require.NoError(t, err)
			state, version, _ := eng.SessionStateSnapshot()
			require.Equal(t, priorID, state.SelectedInstanceID, "ordinary dialogue must not mint a new user_selected record")

			if cold {
				rebuilt := NewWithDeps(model, executor, confirm)
				rebuilt.SetMutatingToolsEnabled(true)
				rebuilt.SetSessionState(state, version+1)
				rebuilt.RehydrateHistory([]HistoryMessage{
					{Role: openai.ChatMessageRoleUser, Content: nextID},
					{Role: openai.ChatMessageRoleAssistant, Content: reply},
				})
				eng = rebuilt
			}
			_, err = eng.Chat(context.Background(), "停止它", noopStep)
			require.NoError(t, err)
			require.Equal(t, []string{nextID}, confirmedIDs, "the confirmation card must retain the model's target, not the old confirmed instance")
			require.NotEmpty(t, describedIDs)
			for _, id := range describedIDs {
				require.Equal(t, nextID, id, "existence and workflow reads must check the proposed target")
			}
			require.NotContains(t, executor.calls, "StopCompShareInstance", "declining the card must still prevent the write")
			require.Contains(t, renderTestMessages(model.calls[1].Messages), nextID)
			require.Contains(t, renderTestMessages(model.calls[1].Messages), "停止它")
			state, _, _ = eng.SessionStateSnapshot()
			require.Equal(t, priorID, state.SelectedInstanceID, "a declined model proposal does not replace the prior confirmed selection")
		})
	}
}
