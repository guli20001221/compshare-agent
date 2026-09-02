package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/store"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

type interruptedTurnMessages struct {
	recordingMessages
	metadata   json.RawMessage
	metaCtxErr error
}

func (m *interruptedTurnMessages) UpdateAssistantMetadata(ctx context.Context, _ store.Owner, _ string, raw json.RawMessage) error {
	m.metadata = append(json.RawMessage(nil), raw...)
	m.metaCtxErr = ctx.Err()
	return nil
}

type interruptedTurnLLM struct {
	cancel context.CancelFunc
	err    error
	final  bool
	round  int
}

func (m *interruptedTurnLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	m.round++
	if m.round == 1 {
		return &llm.ChatResponse{ToolCalls: []openai.ToolCall{{
			ID: "read-before-interruption", Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "ReadCapability_resource_info", Arguments: `{}`},
		}}}, nil
	}
	if m.cancel != nil {
		m.cancel()
	}
	if m.final {
		return &llm.ChatResponse{Content: "未交付的最终回答"}, nil
	}
	return nil, m.err
}

func TestChatStreamInterruptionPersistsToolTranscriptAndNeverFinishesSuccess(t *testing.T) {
	for _, tc := range []struct {
		name             string
		cancel           bool
		final            bool
		err              error
		wantStatus       string
		wantTerminatedBy string
	}{
		{name: "cancelled_stream_with_nil_chat_error", cancel: true, final: true, wantStatus: "aborted", wantTerminatedBy: observability.TerminatedByUserCancel},
		{name: "cancelled_after_read", cancel: true, err: context.Canceled, wantStatus: "aborted", wantTerminatedBy: observability.TerminatedByUserCancel},
		{name: "model_error_after_read", err: errors.New("model unavailable"), wantStatus: "error", wantTerminatedBy: observability.TerminatedByError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			model := &interruptedTurnLLM{err: tc.err, final: tc.final}
			if tc.cancel {
				model.cancel = cancel
			}
			eng := engine.NewWithDeps(model, toolTurnExecutor{}, denyConfirm)
			eng.RehydrateHistory(nil)
			messages := &interruptedTurnMessages{}
			sess := store.Session{ID: "interrupted", TopOrganizationID: 1, OrganizationID: 2, CreatedAt: time.Now(), UpdatedAt: time.Now()}
			h := newChatTestHandlersWith(t, eng, &mockSessions{byID: map[string]store.Session{sess.ID: sess}})
			h.messages = messages
			writer := &captureTraceWriter{}
			h.traceWriter = writer
			base := BaseRequest{Action: "SendCSAgentChat", RequestUUID: "request-interrupted"}
			base.Owner = store.Owner{TopOrganizationID: 1, OrganizationID: 2}
			prep, apiErr := h.prepareChat(context.Background(), base, sess.ID, "查看我的实例", "")
			require.Nil(t, apiErr)
			defer prep.release()
			sink := &recordingSink{}
			h.chatStream(ctx, sink, base, prep)

			require.Equal(t, tc.wantStatus, messages.patch.Status)
			require.False(t, sink.has("done"))
			require.Len(t, writer.records, 1)
			require.Equal(t, tc.wantTerminatedBy, writer.records[0].Outcome.TerminatedBy)
			require.Equal(t, "failure", writer.records[0].Outcome.ResponseContract)
			if tc.cancel {
				require.Equal(t, observability.AbortCauseClientDisconnect, writer.records[0].Outcome.AbortCause)
				require.False(t, sink.has("token"), "cancelled delivery must not leak a late candidate answer")
			}
			require.NoError(t, messages.metaCtxErr, "metadata persistence uses a detached context")
			require.NotEmpty(t, messages.metadata, "tools observed before interruption must remain recoverable")
			projected := engine.ProjectTranscript(engine.ParseTranscriptMetadata(messages.metadata))
			require.NotEmpty(t, projected)
			toolResults := 0
			for _, message := range projected {
				require.NotContains(t, message.Content, "未交付的最终回答")
				require.NotContains(t, message.Content, abortedAssistantMessage)
				if message.Role == openai.ChatMessageRoleTool {
					toolResults++
					require.Equal(t, "read-before-interruption", message.ToolCallID)
					require.Contains(t, message.Content, "uhost-e2e")
				}
			}
			require.Equal(t, 1, toolResults)
		})
	}
}
