package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// "If the agent could not read your context, it says so."
//
// Before this change, a session whose persisted state could not be parsed was
// wiped, logged, and answered anyway. From the user's seat that is
// indistinguishable from the agent forgetting which instance they had been
// talking about for the last five turns — which is exactly the complaint this
// whole line of work exists to remove.
//
// Six gates, each pinning a different way the fix can be broken:
//
//	1. malformed  → the user is told
//	2. unknown    → the user is told, AND the row is still never written
//	3. healthy    → the user is told NOTHING (or the notice is worthless noise)
//	4. the notice survives a reload (it is in the transcript, not just the wire)
//	5. the notice never enters the prompt (a model-authored warning is not a
//	   guarantee — it gets paraphrased, softened, or dropped)
//	6. the notice does not swallow the model's actual answer
//
// ---------------------------------------------------------------------------

// nonStreamingLLM returns content WITHOUT emitting any OnTextDelta, which is
// what forces chatStream's "!tokenEmitted" one-shot fallback. Gate 6 needs it:
// the notice is written with sw.WriteEvent directly and must NOT set
// tokenEmitted, or that fallback is suppressed and the answer is never sent.
type nonStreamingLLM struct{}

func (nonStreamingLLM) Chat(_ context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{
		Content: "这是模型的真实回答",
		Usage:   llm.TokenUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
	}, nil
}

// noticeTestHandlers is newChatTestHandlers with a caller-supplied LLM.
func noticeTestHandlers(t *testing.T, sess store.Session, client engine.LLMClient) (*Handlers, *mockSessions, *recordingMessages) {
	t.Helper()
	eng := engine.NewWithDeps(client, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	sessions := &mockSessions{byID: map[string]store.Session{sess.ID: sess}}
	messages := &recordingMessages{}

	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			Meta: config.MetaConfig{MaxInputLength: 4000},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		sessions,
		messages,
		mockFeedback{},
		fakePool{eng: eng},
		nil,
	)
	return h, sessions, messages
}

// streamedText concatenates every token frame, i.e. exactly what the user's
// screen accumulates over the turn.
func streamedText(s *recordingSink) string {
	var b strings.Builder
	for _, e := range s.events {
		if e.Event != "token" {
			continue
		}
		if tok, ok := e.Data.(tokenEvent); ok {
			b.WriteString(tok.Text)
		}
	}
	return b.String()
}

func doneContent(t *testing.T, s *recordingSink) string {
	t.Helper()
	for _, e := range s.events {
		if e.Event != "done" {
			continue
		}
		d, ok := e.Data.(doneEvent)
		require.True(t, ok, "done frame carried an unexpected payload type")
		return d.Content
	}
	t.Fatal("no done frame was emitted")
	return ""
}

func brokenSession(id string) store.Session {
	return store.Session{
		ID:                id,
		TopOrganizationID: 1,
		OrganizationID:    2,
		Context:           json.RawMessage(`{not valid`),
		ContextVersion:    4,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
}

func futureSession(id string) store.Session {
	return store.Session{
		ID:                id,
		TopOrganizationID: 1,
		OrganizationID:    2,
		Context:           json.RawMessage(`{"agent_session_state":{"schema_version":"9.0"},"client_context":{"app":"console"}}`),
		ContextVersion:    4,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
}

// Gate 1. A broken context row must produce a visible notice, not just a log
// line. Mutation: drop `contextNotice = noticeSessionStateReset` from the
// default branch of prepareChat and this fails.
func TestChat_MalformedContext_TellsTheUser(t *testing.T) {
	h, _, _ := noticeTestHandlers(t, brokenSession("sess-bad-notice"), chatLLM{})

	sink, _ := dispatchChatTurn(t, h, "sess-bad-notice", "刚才那台机器怎么样了")

	assert.Contains(t, streamedText(sink), noticeSessionStateReset,
		"a turn that could not read the session's state must SAY SO — silently answering "+
			"from a wiped context is the bug, not a degradation")
}

// Gate 2. An unknown schema_version must ALSO be disclosed — and must still
// never write the row. Both halves matter and they pull in opposite directions,
// which is why they are asserted together: the obvious way to "fix" the silence
// is to make this path heal itself like the malformed one, and that would
// overwrite a newer binary's state during a rollback.
//
// Mutation-verified: making this branch identical to the malformed branch
// (SetSessionState + sessionStatePersistable = true) turns this test red, and
// the diff it prints is the disaster itself — schema_version "9.0" rewritten to
// "4.0" and the console's client_context erased.
//
// Honest scope note: flipping `sessionStatePersistable = true` ALONE does NOT
// turn this red, because the unknown branch never calls SetSessionState, so the
// persist path's `if hydrated` check stops the write on its own. The boolean is
// defence-in-depth here, not the load-bearing guard. What IS load-bearing — and
// what this test actually pins — is that the unknown branch hydrates nothing.
func TestChat_UnknownSchema_TellsTheUser_AndStillNeverPersists(t *testing.T) {
	h, sessions, _ := noticeTestHandlers(t, futureSession("sess-future-notice"), chatLLM{})
	before := sessions.byID["sess-future-notice"].Context

	sink, _ := dispatchChatTurn(t, h, "sess-future-notice", "继续")

	assert.Contains(t, streamedText(sink), noticeSessionStateUnreadable,
		"a rolled-back binary that cannot read the row must tell the user it is answering without the state")
	assert.Equal(t, 0, sessions.updateContextCalls,
		"the row was written by a NEWER binary and is INTACT — writing our understanding of it over the top "+
			"turns a reversible rollback into permanent loss")
	assert.JSONEq(t, string(before), string(sessions.byID["sess-future-notice"].Context),
		"the row must be byte-for-byte untouched")
}

// Gate 3. The negative control. Without it, "always prepend the notice" passes
// gates 1, 2, 4, 5 and 6 — and every healthy turn in production would open with
// a scary warning. A notice that is always shown carries no information.
func TestChat_HealthyContext_SaysNothingAboutContext(t *testing.T) {
	h, _, msgs := noticeTestHandlers(t, store.Session{
		ID:                "sess-healthy",
		TopOrganizationID: 1,
		OrganizationID:    2,
		ContextVersion:    0,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}, chatLLM{})

	sink, _ := dispatchChatTurn(t, h, "sess-healthy", "hi")

	got := streamedText(sink)
	assert.NotContains(t, got, noticeSessionStateReset)
	assert.NotContains(t, got, noticeSessionStateUnreadable)
	assert.NotContains(t, msgs.patch.Content, "⚠️",
		"a healthy turn must carry no degradation notice anywhere — in the stream or the transcript")
}

// Gate 4. The notice must be part of the turn's stored content, not only the
// live wire. If it is only streamed, the user refreshes the page, the notice is
// gone, and the transcript shows a confident context-free answer with nothing
// explaining why — which is worse than never having warned them.
func TestChat_TheNotice_IsStoredInTheTranscript_NotJustStreamed(t *testing.T) {
	h, _, msgs := noticeTestHandlers(t, brokenSession("sess-persist-notice"), chatLLM{})

	sink, _ := dispatchChatTurn(t, h, "sess-persist-notice", "hi")

	assert.True(t, strings.HasPrefix(msgs.patch.Content, noticeSessionStateReset),
		"the persisted assistant message must lead with the notice, so a page reload still shows it")
	assert.True(t, strings.HasPrefix(doneContent(t, sink), noticeSessionStateReset),
		"the done frame's Content must agree with the token stream and the transcript — "+
			"a client that renders done.Content instead of accumulating tokens must not lose the notice")
}

// Gate 5. The notice must never be handed to the model.
//
// This is the rule the repo has already paid for once: a warning routed through
// the LLM is a warning the LLM may reword, soften, or silently drop. "Tell the
// user" is a guarantee, and a guarantee cannot be implemented by asking a model
// to remember. Mutation: pass prep.contextNotice into ChatOptions/ImageContext
// or prepend it to prep.message, and this fails.
func TestChat_TheNotice_NeverReachesTheModel(t *testing.T) {
	capture := &captureLLM{}
	h, _, _ := noticeTestHandlers(t, brokenSession("sess-no-leak"), capture)

	_, _ = dispatchChatTurn(t, h, "sess-no-leak", "hi")

	require.NotEmpty(t, capture.messages, "the model must actually have been called")
	for i, m := range capture.messages {
		assert.NotContains(t, m.Content, noticeSessionStateReset,
			"message[%d] (role=%s) carries the degradation notice into the prompt — "+
				"the notice is the handler's guarantee, not a request to the model", i, m.Role)
		assert.NotContains(t, m.Content, "⚠️",
			"message[%d] (role=%s) leaked handler-authored warning text into the prompt", i, m.Role)
	}
}

// Gate 6. The notice must not swallow the answer.
//
// chatStream only emits the reply as a single fallback chunk when the model
// streamed nothing (`!tokenEmitted`). The notice is written straight to the
// sink for exactly this reason — if it went through the same path that sets
// tokenEmitted, this non-streaming turn would deliver the warning and no
// answer. Mutation: set tokenEmitted = true when writing the notice, and this
// fails with the reply missing.
func TestChat_ContextNotice_DoesNotSuppressTheModelsAnswer(t *testing.T) {
	h, _, msgs := noticeTestHandlers(t, brokenSession("sess-both"), nonStreamingLLM{})

	sink, _ := dispatchChatTurn(t, h, "sess-both", "hi")

	got := streamedText(sink)
	assert.Contains(t, got, noticeSessionStateReset, "the notice must be delivered")
	assert.Contains(t, got, "这是模型的真实回答",
		"the model's answer must ALSO be delivered — warning the user is not a reason to withhold the reply")
	assert.Contains(t, msgs.patch.Content, "这是模型的真实回答",
		"and the answer must be stored, not just streamed")
}
