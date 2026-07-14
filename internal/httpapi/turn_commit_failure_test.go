package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startsAnInstanceLLM asks for a mutating workflow on the first round, then answers.
// Mutating actions in this codebase are the *Workflow tools (tools/policies.go: L1 == a name
// ending in "Workflow"), so this is what a real write turn looks like.
type startsAnInstanceLLM struct{ ran bool }

func (m *startsAnInstanceLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if !m.ran {
		m.ran = true
		return &llm.ChatResponse{ToolCalls: []openai.ToolCall{{
			ID:   "tc-1",
			Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{
				Name:      "StartInstanceWorkflow",
				Arguments: `{"UHostId":"uhost-1exampleaa0N"}`,
			},
		}}}, nil
	}
	if req.OnTextDelta != nil {
		req.OnTextDelta("已开机")
	}
	return &llm.ChatResponse{Content: "已开机"}, nil
}

// startExecutor lets the workflow find its instance and complete. A SUCCESSFUL execution is the
// precondition for the engine to record the action: a blocked, rate-limited or unconfirmed one
// changed nothing out in the world and must not be recorded.
type startExecutor struct{}

func (startExecutor) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	if action == "DescribeCompShareInstance" {
		return map[string]any{"UHostSet": []any{map[string]any{
			"UHostId": "uhost-1exampleaa0N", "State": "Stopped", "GpuType": "4090", "Name": "demo",
		}}}, nil
	}
	return map[string]any{"RetCode": 0}, nil
}

// ---------------------------------------------------------------------------
// What a turn that CANNOT be saved is allowed to do.
//
// The first version of this commit contract introduced a bug worse than the one
// it fixed. On a commit error it "cleaned up" by calling UpdateAssistant with
// AssistantPatch{Status: "error"} — whose Content field is the empty string, and
// whose SQL is `SET content = $1` with no conditions. So:
//
//	the transaction COMMITS in Postgres
//	  -> the ack is lost / times out on our side
//	  -> we see an error and retry
//	  -> the retry's CAS loses to our OWN successful write
//	  -> we declare the turn unsaved
//	  -> and we blank the correct answer we had just saved.
//
// The code destroyed the exact thing it existed to protect. Four rules come out
// of that, and each has a gate below:
//
//	1. ASK, don't assume. Before believing a commit error, read the row back.
//	2. A failure path may write the STATUS, never the CONTENT.
//	3. An unsaved answer must not survive in the hot engine.
//	4. "Please retry" is a lie on a turn that already created an instance.
// ---------------------------------------------------------------------------

// rowMessages is a message store that behaves like the real one: Append inserts,
// UpdateAssistant overwrites content (including with the empty string, exactly as
// the SQL does), and GetWithOwnerCheck reads back what is actually stored.
//
// The realism is the point. A mock that ignored Content could not express the bug
// at all, and a test that cannot express the bug cannot gate it.
type rowMessages struct {
	mu   sync.Mutex
	rows map[string]store.Message
}

func newRowMessages() *rowMessages { return &rowMessages{rows: map[string]store.Message{}} }

func (m *rowMessages) Append(_ context.Context, msg store.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[msg.ID] = msg
	return nil
}

func (m *rowMessages) UpdateAssistant(_ context.Context, _ store.Owner, msgID string, patch store.AssistantPatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.rows[msgID]
	row.Content = patch.Content // unconditional, like `SET content = $1`
	row.Status = patch.Status
	m.rows[msgID] = row
	return nil
}

func (m *rowMessages) MarkAssistantOutcome(_ context.Context, _ store.Owner, msgID string, status string, _ *string, _, _ *int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.rows[msgID]
	row.Status = status // content is untouched — the method cannot even name it
	m.rows[msgID] = row
	return nil
}

func (m *rowMessages) ListBySession(context.Context, string, int, string) ([]store.Message, string, error) {
	return nil, "", nil
}

func (m *rowMessages) GetWithOwnerCheck(_ context.Context, _ store.Owner, msgID string) (store.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[msgID]
	if !ok {
		return store.Message{}, errors.New("no such message")
	}
	return row, nil
}

func (m *rowMessages) assistantRow(t *testing.T) store.Message {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.Role == "assistant" {
			return r
		}
	}
	t.Fatal("no assistant row was ever inserted")
	return store.Message{}
}

// lateAckTurns is the committer whose write SUCCEEDS and whose acknowledgement is
// then lost — the failure that produced the data destruction. It applies the patch
// to the store for real, and only then returns an error.
type lateAckTurns struct {
	messages *rowMessages
	err      error
}

func (c lateAckTurns) CommitTurn(ctx context.Context, owner store.Owner, _ string, msgID string,
	patch store.AssistantPatch, _ json.RawMessage, _ int) (int, error) {
	_ = c.messages.UpdateAssistant(ctx, owner, msgID, patch) // the write lands...
	return 0, c.err                                          // ...and the caller never hears about it
}

// deadTurns never commits anything.
type deadTurns struct{ err error }

func (c deadTurns) CommitTurn(context.Context, store.Owner, string, string,
	store.AssistantPatch, json.RawMessage, int) (int, error) {
	return 0, c.err
}

// capturingTraces records what the turn was FILED as, which is a different question from what the
// user was told.
type capturingTraces struct {
	mu      sync.Mutex
	records []observability.TraceRecord
}

func (w *capturingTraces) Append(rec observability.TraceRecord) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.records = append(w.records, rec)
	return nil
}
func (w *capturingTraces) EmitStep(observability.StepTrace) error { return nil }
func (w *capturingTraces) Dir() string                            { return "" }
func (w *capturingTraces) Close(context.Context) error            { return nil }

func (w *capturingTraces) only(t *testing.T) observability.TraceRecord {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	require.Len(t, w.records, 1, "exactly one turn should have been traced")
	return w.records[0]
}

func failureTestHandlers(t *testing.T, eng *engine.Engine, msgs *rowMessages, turns store.TurnCommitter, pool EnginePool) *Handlers {
	t.Helper()
	return failureTestHandlersTracing(t, eng, msgs, turns, pool, nil)
}

func failureTestHandlersTracing(t *testing.T, _ *engine.Engine, msgs *rowMessages, turns store.TurnCommitter, pool EnginePool, traces observability.Writer) *Handlers {
	t.Helper()
	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			Meta: config.MetaConfig{MaxInputLength: 4000},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		&mockSessions{byID: map[string]store.Session{
			"sess-x": {ID: "sess-x", TopOrganizationID: 1, OrganizationID: 2,
				CreatedAt: time.Now(), UpdatedAt: time.Now()},
		}},
		msgs,
		mockFeedback{},
		pool,
		traces,
	)
	h.turns = turns
	return h
}

func chatOnce(t *testing.T, h *Handlers) *recordingSink {
	t.Helper()
	sink, _ := runChatJSON(t, h,
		`{"Action":"SendCSAgentChat","SessionId":"sess-x","Message":"hi","request_uuid":"r1","top_organization_id":1,"organization_id":2}`)
	return sink
}

// confirmingSink is a recordingSink that says YES to the confirmation card, from another
// goroutine, the way a live user's browser does.
//
// It is needed because the HTTP path REPLACES the engine's ConfirmFunc with its own broker-backed
// one (engine.go: "Per-turn ConfirmFunc override"), so a mutating turn driven through the handler
// really does stop and wait for a human. Handing the engine a confirm-everything function, as the
// engine's own tests do, would not exercise the path the server actually runs — and the first
// draft of this test, which did exactly that, sat for 60 seconds waiting for a confirmation
// nobody was going to send, and then recorded nothing.
type confirmingSink struct {
	recordingSink
	broker    *ConfirmBroker
	sessionID string
	owner     store.Owner
}

func (s *confirmingSink) WriteEvent(event string, data any) error {
	if event == "confirmation" {
		if ev, ok := data.(confirmationEvent); ok {
			id := ev.ConfirmationID
			go func() {
				_ = s.broker.Resolve(id, s.sessionID, s.owner, ConfirmDecision{Confirmed: true})
			}()
		}
	}
	return s.recordingSink.WriteEvent(event, data)
}

// chatOnceConfirming runs a turn end to end through the real handler, with a client that approves
// the confirmation card.
func chatOnceConfirming(t *testing.T, h *Handlers, message string) *recordingSink {
	t.Helper()
	owner := store.Owner{TopOrganizationID: 1, OrganizationID: 2}
	base := BaseRequest{Action: "SendCSAgentChat", RequestUUID: "r1", Owner: owner}

	prep, apiErr := h.prepareChat(context.Background(), base, "sess-x", message, "")
	require.Nil(t, apiErr)
	defer prep.release()

	sink := &confirmingSink{broker: h.confirmBroker, sessionID: "sess-x", owner: owner}
	h.chatStream(context.Background(), sink, base, prep)
	return &sink.recordingSink
}

// GATE 1 — a commit whose acknowledgement was lost must NOT be reported as a
// failure, and above all must not be "cleaned up".
//
// This is the data-destruction bug, reproduced end to end: the write landed, we
// got an error anyway. The handler must read the row back, discover the truth,
// and report done.
//
// Mutation: delete the turnAlreadyLanded check and this fails twice over — the
// user is told the turn was lost, and the saved answer is blanked.
func TestChat_CommitLandedButTheAckWasLost_TheAnswerSurvivesAndTheTurnIsDone(t *testing.T) {
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	msgs := newRowMessages()
	h := failureTestHandlers(t, eng, msgs, lateAckTurns{messages: msgs, err: errors.New("connection reset")}, fakePool{eng: eng})

	sink := chatOnce(t, h)

	row := msgs.assistantRow(t)
	assert.Equal(t, "你好", row.Content,
		"the answer WAS committed — a lost acknowledgement must never be allowed to erase it")
	assert.Equal(t, "ok", row.Status,
		"and the row must not be flipped to error over a turn that actually succeeded")
	assert.True(t, sink.has("done"),
		"the turn is saved, so the client must be told so — reporting failure would make the user re-ask "+
			"a question that was answered and recorded")
	assert.False(t, sink.has("error"))
}

// GATE 2 — a turn that genuinely did not commit is reported, and the failure path
// records the STATUS without touching the content.
//
// Mutation: swap MarkAssistantOutcome back to UpdateAssistant on the failure path
// and gate 1 breaks (the answer is blanked); this one pins the other half — that
// the failure is still recorded, so the row does not sit at "pending" forever.
func TestChat_CommitTrulyFailed_TheTurnIsReportedNotSaved(t *testing.T) {
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	msgs := newRowMessages()
	h := failureTestHandlers(t, eng, msgs, deadTurns{err: errors.New("db is down")}, fakePool{eng: eng})

	sink := chatOnce(t, h)

	assert.False(t, sink.has("done"),
		"`done` unlocks the client's input box; sending it for a turn the server has no record of "+
			"is how the NEXT turn ends up looking like amnesia")
	require.True(t, sink.has("error"))

	row := msgs.assistantRow(t)
	assert.Equal(t, "error", row.Status, "the outcome must still be recorded")
	assert.Empty(t, row.Content, "nothing was committed, so there is nothing to preserve here")
}

// blindRowMessages is a rowMessages whose read-back FAILS. It models the nastiest ordering there
// is: the commit landed, the ack was lost, AND the reconcile query we would use to discover that
// also fails (the store is having a bad minute — which is, after all, why the commit "failed").
type blindRowMessages struct{ *rowMessages }

func (m *blindRowMessages) GetWithOwnerCheck(context.Context, store.Owner, string) (store.Message, error) {
	return store.Message{}, errors.New("store unavailable")
}

// GATE 2b — the failure path may never write content, even when it runs against a row that
// already holds a committed answer.
//
// GATE 1 covers the common case by asking the database. This covers the case where the database
// will not answer: we cannot know the turn landed, so we correctly report it as not saved — and we
// must STILL not destroy it. That is the whole reason MarkAssistantOutcome exists as a separate
// method with no Content parameter, rather than "an AssistantPatch we are careful to fill in
// correctly". Care is not a mechanism.
//
// Mutation: swap MarkAssistantOutcome back to UpdateAssistant on the failure path and the
// committed answer is blanked — the exact data destruction the first version of this PR shipped.
func TestChat_TheFailurePathCannotBlankACommittedAnswer_EvenWhenItCannotTell(t *testing.T) {
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	rows := newRowMessages()
	msgs := &blindRowMessages{rowMessages: rows}
	h := failureTestHandlersTracing(t, eng, rows, lateAckTurns{messages: rows, err: errors.New("connection reset")}, fakePool{eng: eng}, nil)
	h.messages = msgs // the handler reads through the blind store; the committer still writes to rows

	sink := chatOnce(t, h)

	assert.True(t, sink.has("error"),
		"we could not confirm the commit, so we must not claim success")

	row := rows.assistantRow(t)
	assert.Equal(t, "你好", row.Content,
		"the answer WAS committed. We could not prove it, we correctly refused to claim success — "+
			"and we must still not have destroyed it. A failure path that cannot name `content` "+
			"cannot overwrite it.")
}

// GATE 3 — a turn that could not be saved must not leave its answer in the hot
// engine.
//
// The engine still holds the reply in its in-memory history. Left in the pool, the
// session forks: the next HOT turn sees an answer the database does not have,
// while a cold rebuild — after an eviction, a restart, a deploy — does not. The
// same session then tells two different stories, decided by the LRU.
//
// Mutation: delete the h.pool.Invalidate call and this fails.
func TestChat_AnUnsavedAnswerIsNotLeftInTheHotEngine(t *testing.T) {
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	msgs := newRowMessages()
	pool := &recordingPool{eng: eng}
	h := failureTestHandlers(t, eng, msgs, deadTurns{err: errors.New("db is down")}, pool)

	chatOnce(t, h)

	assert.Equal(t, []string{"sess-x"}, pool.invalidated,
		"the engine holds an answer the database does not — it must be thrown away, or the next "+
			"hot turn and every cold rebuild will disagree about what was said")
}

// GATE 3b — a turn that was not saved must not be FILED as a success.
//
// finishTrace(nil) used to run BEFORE the commit, so every unsaved turn went into the trace as a
// clean success. That is not a cosmetic defect: the traces are the evidence base for the intent
// A/B work and for the session-loss attribution, and a dataset that records failures as successes
// is worse than no dataset — it will confidently tell us the thing we are trying to measure does
// not happen.
//
// Mutation: move finishTrace(nil) back above commitTurn and this fails.
func TestChat_TraceRecordsTheTurnAsFailedWhenItWasNotSaved(t *testing.T) {
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	msgs := newRowMessages()
	traces := &capturingTraces{}
	h := failureTestHandlersTracing(t, eng, msgs, deadTurns{err: errors.New("db is down")}, fakePool{eng: eng}, traces)

	chatOnce(t, h)

	rec := traces.only(t)
	assert.True(t, rec.EngineHardBlock.Hit,
		"the turn was not saved, and the trace must say so — filing it as a success poisons the "+
			"dataset every later measurement of this bug depends on")
	assert.Equal(t, observability.HardBlockCategoryChatError, rec.EngineHardBlock.Category)
}

// GATE 4 — a turn that EXECUTED something must not be told to retry.
//
// "本轮未保存，请重试" is a safe thing to say when nothing happened. Said to a user
// whose instance was just created, it is an instruction to create a second one.
// The two cases carry different error CODES so a frontend cannot collapse them
// into one branch by accident.
//
// Mutation: drop the MutatingActionsThisTurn branch and the code comes back as
// plain TurnNotSaved — the retry-and-double-charge path.
func TestChat_CommitFailedAfterAMutatingAction_DoesNotTellTheUserToRetry(t *testing.T) {
	// A REAL mutating execution, not a test hook poked into the engine. The engine records the
	// action at its own tool-execution choke point; this test drives that path and then asserts
	// on what the HTTP layer does with the fact. Anything less would gate the branch without
	// gating the thing the branch depends on.
	eng := engine.NewWithDeps(
		&startsAnInstanceLLM{},
		tools.ToolExecutor(startExecutor{}),
		func(string, map[string]any) bool { return true }, // the user confirms
	)
	eng.SetMutatingToolsEnabled(true)
	eng.RehydrateHistory(nil)

	msgs := newRowMessages()
	h := failureTestHandlers(t, eng, msgs, deadTurns{err: errors.New("db is down")}, fakePool{eng: eng})

	// The instance is named in the user's own words: workflowTargetIsTrusted (correctly) refuses
	// to act on a target the user never asked for, so a bare "hi" would never reach the workflow.
	sink := chatOnceConfirming(t, h, "开机 uhost-1exampleaa0N")

	require.NotEmpty(t, eng.MutatingActionsThisTurn(),
		"precondition: the turn really did execute a mutating action")

	var code, message string
	for _, ev := range sink.events {
		if ev.Event != "error" {
			continue
		}
		e, ok := ev.Data.(streamErrorEvent)
		require.True(t, ok)
		code, message = e.Code, e.Message
	}

	assert.Equal(t, ErrTurnNotSavedAfterAction.Code, code,
		"a turn that already created an instance must not be reported with the same code as a turn "+
			"where nothing happened — the client cannot tell them apart otherwise")
	assert.NotContains(t, message, "请重试",
		"telling this user to retry is telling them to create a second instance")
	assert.Contains(t, message, "请勿重试")
}
