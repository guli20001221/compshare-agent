package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/coder/websocket"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/opscontext"
	"github.com/compshare-agent/internal/store"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

type interruptedDiagnosisLLM struct{}

func (interruptedDiagnosisLLM) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{ToolCalls: []openai.ToolCall{{
		ID: "diagnosis-call", Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{Name: "DiagnoseInstanceInternals", Arguments: `{"UHostId":"uhost-1","Task":"排查服务状态"}`},
	}}}, nil
}

type interruptedDiagnosisRunner struct {
	cancel   context.CancelFunc
	progress []engine.InstanceOpsProgress
	calls    int
}

func (r *interruptedDiagnosisRunner) Run(_ context.Context, req engine.InstanceOpsRequest, onProgress func(engine.InstanceOpsProgress)) (engine.InstanceOpsVerdict, error) {
	r.calls++
	for _, progress := range r.progress {
		if progress.Kind == engine.InstanceOpsProgressAgentSession {
			progress.AgentSessionConversationAnchor = req.Context.BridgeConversationAnchor
		}
		onProgress(progress)
	}
	if r.cancel != nil {
		r.cancel()
	}
	return engine.InstanceOpsVerdict{}, errors.New("sshops: harness exited")
}

func TestChatInterruptedDiagnosisPersistsObservedWorkAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "delivered_failure_notice", true: "canceled_runner"}[canceled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			exit := 0
			const jobID = "job-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			const sessionID = "11111111-1111-4111-8111-111111111111"
			const workdirID = "22222222-2222-4222-8222-222222222222"
			const secret = "interruption-secret-0123456789"
			runner := &interruptedDiagnosisRunner{progress: []engine.InstanceOpsProgress{
				{Kind: engine.InstanceOpsProgressAgentSession, AgentSessionID: sessionID, AgentSessionWorkdirID: workdirID,
					AgentSessionContract: opscontext.AgentSessionContract, AgentSessionModel: "gpt-5.6-terra"},
				{Kind: engine.InstanceOpsProgressBackgroundJob, JobID: jobID, JobState: "running", JobPurpose: "安装所需依赖"},
				{Kind: engine.InstanceOpsProgressCommand, Command: "nvidia-smi", Tier: "read_only", Disposition: "ran", ExitCode: &exit},
				{Kind: engine.InstanceOpsProgressCommand, Command: "systemctl start app", Tier: "mutating", Disposition: "ran", ExitCode: &exit},
				{Kind: engine.InstanceOpsProgressCommand, Command: "systemctl restart app", Tier: "mutating", Disposition: "failed"},
				{Kind: engine.InstanceOpsProgressCommand, Command: "curl -H 'Authorization: Bearer " + secret + "' http://127.0.0.1/status", Tier: "mutating", Disposition: "refused"},
			}}
			if canceled {
				runner.cancel = cancel
			}
			eng := engine.NewWithDeps(interruptedDiagnosisLLM{}, chatExecutor{}, denyConfirm)
			eng.SetMutatingToolsEnabled(true)
			eng.SetInstanceOps(runner)
			eng.RehydrateHistory(nil)
			writer := &captureTraceWriter{}
			messages := &recordingMessages{}
			sessions := &mockSessions{byID: map[string]store.Session{"interrupted": {
				ID: "interrupted", TopOrganizationID: 1, OrganizationID: 2, CreatedAt: time.Now(), UpdatedAt: time.Now(),
			}}}
			h := NewHandlers(&config.Config{Agent: config.AgentConfig{
				LLM:  config.LLMConfig{Model: "offline-diagnosis"},
				HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
				STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
			}}, sessions, messages, mockFeedback{}, fakePool{eng: eng}, writer)
			base := BaseRequest{Action: "SendCSAgentChat", RequestUUID: "diagnosis-test"}
			base.Owner = store.Owner{TopOrganizationID: 1, OrganizationID: 2}
			prep, apiErr := h.prepareChat(context.Background(), base, "interrupted", "请排查 uhost-1 的服务状态", "")
			require.Nil(t, apiErr)
			defer prep.release()
			sink := &recordingSink{}
			h.chatStream(ctx, sink, base, prep)
			require.Equal(t, 1, runner.calls)
			require.Len(t, writer.records, 1)
			require.Equal(t, 1, sessions.updateContextCalls, "the existing continuation envelope is persisted once")
			persisted, err := engine.ParsePersistedContext(sessions.byID["interrupted"].Context)
			require.NoError(t, err)
			require.Equal(t, jobID, persisted.AgentSessionState.PersistedInstanceOpsJob.JobID)
			require.Equal(t, sessionID, persisted.AgentSessionState.PersistedInstanceOpsAgent.SessionID)
			require.Equal(t, workdirID, persisted.AgentSessionState.PersistedInstanceOpsAgent.WorkdirID)
			require.Len(t, persisted.AgentSessionState.PersistedInstanceOpsAgent.ConversationAnchor, 64)
			trace := writer.records[0]
			if canceled {
				require.Equal(t, "aborted", messages.patch.Status)
				require.Equal(t, observability.TerminatedByUserCancel, trace.Outcome.TerminatedBy)
				require.Equal(t, observability.AbortCauseClientDisconnect, trace.Outcome.AbortCause)
				require.Equal(t, "failure", trace.Outcome.ResponseContract)
				require.False(t, sink.has("done"))
				require.False(t, sink.has("token"), "a canceled transport cannot receive a new fallback reply")
				require.Contains(t, messages.patch.Content, abortedAssistantMessage)
				require.Contains(t, messages.patch.Content, "本轮对实例 uhost-1")
				require.Contains(t, messages.patch.Content, "已执行 2 条命令")
				require.Contains(t, messages.patch.Content, "其中 1 条可能修改实例状态")
				require.NotContains(t, messages.patch.Content, "已确认执行")
				require.NotContains(t, messages.patch.Content, "经确认执行")
				require.Contains(t, messages.patch.Content, "systemctl start app")
				require.Contains(t, messages.patch.Content, "可能已修改实例，结果不确定")
				require.Contains(t, messages.patch.Content, "可能不完整")
				require.Contains(t, messages.patch.Content, jobID)
				require.Contains(t, messages.patch.Content, "不会重新启动")
				require.NotContains(t, messages.patch.Content, secret)
				require.NotEmpty(t, eng.InstanceOpsInterruptionSummary(), "persistence must not consume the existing next-turn notice")
			} else {
				require.Equal(t, "ok", messages.patch.Status)
				require.Equal(t, observability.TerminatedByDone, trace.Outcome.TerminatedBy)
				require.Empty(t, trace.Outcome.AbortCause)
				require.Contains(t, messages.patch.Content, "排查未能完成")
				require.True(t, sink.has("done"))
			}
		})
	}
}

type disconnectedDiagnosisRunner struct {
	started chan struct{}
	calls   int
}

func (r *disconnectedDiagnosisRunner) Run(ctx context.Context, _ engine.InstanceOpsRequest, onProgress func(engine.InstanceOpsProgress)) (engine.InstanceOpsVerdict, error) {
	r.calls++
	exit := 0
	onProgress(engine.InstanceOpsProgress{Kind: engine.InstanceOpsProgressCommand,
		Command: "systemctl start app", Tier: "mutating", Disposition: "ran", ExitCode: &exit})
	close(r.started)
	<-ctx.Done()
	// A killed subprocess commonly returns its exit error, not context.Canceled.
	return engine.InstanceOpsVerdict{}, errors.New("sshops: harness exited")
}

type releaseNotifyingPool struct {
	eng      *engine.Engine
	released chan struct{}
}

func (p releaseNotifyingPool) Lease(context.Context, store.Owner, string) (*engine.Engine, func(), error) {
	return p.eng, func() { close(p.released) }, nil
}

func TestWS_DiagnosisDisconnectPersistsSummaryBeforeReleasingEngine(t *testing.T) {
	srv, messages, handlers := wsTestHandlers(t, interruptedDiagnosisLLM{}, denyConfirm)
	eng := handlers.pool.(fakePool).eng
	runner := &disconnectedDiagnosisRunner{started: make(chan struct{})}
	eng.SetMutatingToolsEnabled(true)
	eng.SetInstanceOps(runner)
	released := make(chan struct{})
	handlers.pool = releaseNotifyingPool{eng: eng, released: released}
	writer := &captureTraceWriter{}
	handlers.traceWriter = writer
	conn := dialWS(t, srv, gatewayHeaders())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, conn.Write(ctx, websocket.MessageText,
		[]byte(`{"Action":"SendCSAgentChat","SessionId":"sess-1","Message":"请排查 uhost-1 的服务状态"}`)))
	select {
	case <-runner.started:
	case <-ctx.Done():
		t.Fatal("diagnosis did not start")
	}
	require.NoError(t, conn.CloseNow())
	select {
	case <-released:
	case <-ctx.Done():
		t.Fatal("disconnected turn did not persist and release its engine")
	}
	// The release channel synchronizes all reads below with the chat goroutine;
	// no polling of the unsynchronized test message/trace stores is necessary.
	require.Equal(t, 1, runner.calls)
	require.Equal(t, "aborted", messages.patch.Status)
	require.Contains(t, messages.patch.Content, "本轮对实例 uhost-1")
	require.Contains(t, messages.patch.Content, "systemctl start app")
	require.Contains(t, messages.patch.Content, "无法确认")
	require.Len(t, writer.records, 1)
	require.Equal(t, observability.TerminatedByUserCancel, writer.records[0].Outcome.TerminatedBy)
	require.Equal(t, observability.AbortCauseClientDisconnect, writer.records[0].Outcome.AbortCause)
	require.Equal(t, 1, handlers.sessions.(*mockSessions).updateContextCalls)
}

type interruptedTurnMessages struct {
	recordingMessages
	metadata   json.RawMessage
	metaCtxErr error
}

type interruptedKnowledgeRetriever struct{ calls int }

func (r *interruptedKnowledgeRetriever) RetrieveContext(context.Context, string, string) knowledge.RetrievalResult {
	r.calls++
	chunk := knowledge.KBChunk{ChunkID: "resume-pod-ports", KBVersion: "kb.test", Title: "Pod 端口", Content: "Pod 的 TCP 7860 端口须单独配置映射。"}
	return knowledge.RetrievalResult{Enabled: true, KBVersion: "kb.test", Hits: []knowledge.KBChunk{chunk},
		HitItems: []knowledge.RetrievalHit{{Chunk: chunk, Score: 90, Kept: true}}}
}

type interruptedKnowledgeLLM struct {
	resume scriptedChatLLM
	cancel context.CancelFunc
	round  int
}

func (m *interruptedKnowledgeLLM) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	m.round++
	switch m.round {
	case 1:
		return &llm.ChatResponse{ToolCalls: []openai.ToolCall{{ID: "search-before-interruption", Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: `{"query":"Pod TCP 7860 端口"}`}}}}, nil
	case 2:
		return &llm.ChatResponse{Content: `{"answer_question":"Pod TCP 7860 端口","search_queries":["Pod TCP 7860 端口"]}`}, nil
	case 3:
		m.cancel()
		return nil, ctx.Err()
	default:
		return m.resume.Chat(ctx, req)
	}
}

func TestInterruptedKnowledgeContinuationStripsCitationsAcrossStreamAndPersistence(t *testing.T) {
	const question = "请查文档解释 Pod 的 TCP 7860 端口，不执行操作"
	const draft = "上轮目标是 Pod 的 TCP 7860 端口，须单独配置映射[[resume-pod-ports]]。"
	const answer = "上轮目标是 Pod 的 TCP 7860 端口，须单独配置映射。"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := &interruptedKnowledgeLLM{cancel: cancel, resume: scriptedChatLLM{content: draft}}
	retriever := &interruptedKnowledgeRetriever{}
	hot := engine.NewWithDeps(model, chatExecutor{}, denyConfirm)
	hot.RehydrateHistory(nil)
	hot.SetKnowledgeRetriever(retriever)
	_, err := hot.Chat(ctx, question, func(engine.StepEvent) {})
	require.ErrorIs(t, err, context.Canceled)
	metadata, _ := hot.LastTurnTranscript()
	require.NotEmpty(t, metadata)
	require.Equal(t, 1, retriever.calls)
	coldModel := &scriptedChatLLM{content: draft}
	cold := engine.NewWithDeps(coldModel, chatExecutor{}, denyConfirm)
	cold.SetKnowledgeRetriever(retriever)
	cold.RehydrateHistory([]engine.HistoryMessage{
		{Role: "user", Content: question}, {Role: "assistant", Transcript: metadata},
	})
	for _, tc := range []struct {
		name  string
		eng   *engine.Engine
		model *scriptedChatLLM
	}{{"hot", hot, &model.resume}, {"cold", cold, coldModel}} {
		t.Run(tc.name, func(t *testing.T) {
			messages := &interruptedTurnMessages{}
			sess := store.Session{ID: "resume-" + tc.name, TopOrganizationID: 1, OrganizationID: 2, CreatedAt: time.Now(), UpdatedAt: time.Now()}
			h := newChatTestHandlersWith(t, tc.eng, &mockSessions{byID: map[string]store.Session{sess.ID: sess}})
			h.messages = messages
			writer := &captureTraceWriter{}
			h.traceWriter = writer
			base := BaseRequest{Action: "SendCSAgentChat", RequestUUID: "resume-" + tc.name}
			base.Owner = store.Owner{TopOrganizationID: 1, OrganizationID: 2}
			prep, apiErr := h.prepareChat(context.Background(), base, sess.ID, "继续刚才还没答完的问题", "")
			require.Nil(t, apiErr)
			defer prep.release()
			sink := &recordingSink{}
			h.chatStream(context.Background(), sink, base, prep)

			var streamed strings.Builder
			var done string
			for _, event := range sink.events {
				switch event.Event {
				case "token":
					streamed.WriteString(event.Data.(tokenEvent).Text)
				case "done":
					done = event.Data.(doneEvent).Content
				}
			}
			require.Equal(t, answer, done)
			require.Equal(t, done, streamed.String())
			require.Equal(t, done, messages.patch.Content)
			require.Equal(t, "ok", messages.patch.Status)
			require.Equal(t, 1, retriever.calls, "continuation replays evidence without rerunning retrieval")
			replayed := false
			for _, message := range tc.model.messages {
				if message.Role == openai.ChatMessageRoleTool && message.ToolCallID == "search-before-interruption" {
					require.Contains(t, message.Content, "resume-pod-ports")
					replayed = true
				}
			}
			require.True(t, replayed, "the prior observation still reaches the model")
			require.Len(t, writer.records, 1)
			require.Equal(t, "unavailable", writer.records[0].Outcome.GroundingOutcome)
			require.Empty(t, writer.records[0].Outcome.GroundingCitationScope)
			state, _, _ := tc.eng.SessionStateSnapshot()
			require.Empty(t, state.VerifiedEvidence, "display cleanup must not promote unvalidated prior evidence")
		})
	}
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
