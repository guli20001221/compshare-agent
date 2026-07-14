package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/turncoord"
	"github.com/compshare-agent/internal/workflow"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openDurableWSPostgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("COMPSHARE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("COMPSHARE_TEST_MYSQL_DSN not set — skipping durable WebSocket PostgreSQL integration test")
	}
	if strings.Contains(dsn, "117.50.198.43") {
		t.Fatal("refusing to run durable WebSocket integration against production PostgreSQL")
	}
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	require.NoError(t, admin.Ping())
	schema := "ws_v2_it_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
		_ = admin.Close()
	})

	u, err := url.Parse(dsn)
	require.NoError(t, err)
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { _ = db.Close() })
	for _, migration := range []string{
		"0001_init.sql", "0002_create_agent_traces.sql", "0003_add_session_context_version.sql", "0004_add_agent_traces_outcome_columns.sql",
		"0005_create_turn_execution.sql", "0006_create_turn_protocol.sql",
		"0007_add_turn_recovery_context.sql", "0008_add_turn_retry_policy.sql",
	} {
		raw, readErr := os.ReadFile(filepath.Join("..", "..", "deploy", "migrations", migration))
		require.NoError(t, readErr)
		_, execErr := db.Exec(string(raw))
		require.NoError(t, execErr, "apply %s", migration)
	}
	require.NoError(t, store.VerifySchema(context.Background(), db))
	return db
}

type durableWSEngineFactory struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (f *durableWSEngineFactory) New(_ context.Context, _ store.Owner, _ string, _ engine.SessionOptions) (turncoord.TurnEngine, error) {
	f.calls.Add(1)
	return &durableWSEngine{factory: f}, nil
}

type durableWSEngine struct {
	factory *durableWSEngineFactory
	state   engine.SessionState
	version int
	hooks   engine.TraceHooks
}

type durableWSFormFactory struct {
	calls       atomic.Int32
	resolutions chan workflow.ConfirmResolution
}

func (f *durableWSFormFactory) New(_ context.Context, _ store.Owner, _ string, _ engine.SessionOptions) (turncoord.TurnEngine, error) {
	f.calls.Add(1)
	return &durableWSFormEngine{factory: f}, nil
}

type durableWSFormEngine struct {
	factory *durableWSFormFactory
	state   engine.SessionState
	version int
}

func (e *durableWSFormEngine) SetSessionState(state engine.SessionState, version int) {
	e.state, e.version = state, version
}

func (e *durableWSFormEngine) SetContinuityAdvisories(engine.ContinuityAdvisories) {}

func (e *durableWSFormEngine) SessionStateSnapshot() (engine.SessionState, int, bool) {
	return e.state, e.version, true
}

func (e *durableWSFormEngine) ChatWithOptions(ctx context.Context, _ string, _ func(engine.StepEvent), opts engine.ChatOptions) (string, error) {
	if opts.ConfirmEditsFunc == nil || !opts.GuidedCreate {
		return "", errors.New("durable editable features were not enabled")
	}
	resolution := opts.ConfirmEditsFunc("CreateInstanceWorkflow", map[string]any{"GpuType": "4090"}, testGPUForm())
	e.factory.resolutions <- resolution
	if !resolution.Confirmed {
		return "", ctx.Err()
	}
	return "created with " + resolution.Overrides["GpuType"], nil
}

func (e *durableWSEngine) SetSessionState(state engine.SessionState, version int) {
	e.state, e.version = state, version
}

func (e *durableWSEngine) SetContinuityAdvisories(engine.ContinuityAdvisories) {}

func (e *durableWSEngine) SessionStateSnapshot() (engine.SessionState, int, bool) {
	return e.state, e.version, true
}

func (e *durableWSEngine) AttachTraceHooks(hooks engine.TraceHooks) { e.hooks = hooks }

func (e *durableWSEngine) TraceSnapshot(time.Time) engine.TraceSnapshot {
	return engine.TraceSnapshot{SessionState: e.state, ContextVersion: e.version, SessionStateHydrated: true}
}

func (e *durableWSEngine) ChatWithOptions(ctx context.Context, _ string, _ func(engine.StepEvent), _ engine.ChatOptions) (string, error) {
	if e.hooks.Completion != nil {
		defer e.hooks.Completion(observability.TurnCompletionTrace{
			Class: observability.CompletionClassAgent, Reason: observability.CompletionReasonAgentLoop, ModelCalls: 1,
		})
	}
	select {
	case e.factory.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-e.factory.release:
		return "durable answer", nil
	}
}

type durableWSTraceWriter struct {
	mu      sync.Mutex
	records []observability.TraceRecord
}

func (w *durableWSTraceWriter) Append(record observability.TraceRecord) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.records = append(w.records, record)
	return nil
}
func (w *durableWSTraceWriter) Enqueue(_ observability.TenantContext, record observability.TraceRecord) error {
	return w.Append(record)
}
func (*durableWSTraceWriter) EmitStep(observability.StepTrace) error { return nil }
func (*durableWSTraceWriter) Dir() string                            { return "" }
func (*durableWSTraceWriter) Close(context.Context) error            { return nil }
func (w *durableWSTraceWriter) snapshot() []observability.TraceRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]observability.TraceRecord(nil), w.records...)
}

func newDurableWSPGServer(t *testing.T, db *sql.DB, factory turncoord.EngineFactory, recognizer OCRRecognizer) (*httptest.Server, *turncoord.Coordinator, store.Session) {
	t.Helper()
	sessions := store.NewSessionStore(db)
	session, err := sessions.Create(context.Background(), gatewayOwner, nil, nil)
	require.NoError(t, err)
	coordinator := turncoord.NewCoordinator(
		store.NewPostgresTurnStore(db), sessions, factory,
		turncoord.Options{
			ReplicaID: "ws-integration", LeaseTTL: 2 * time.Second,
			LeaseRenewInterval: 300 * time.Millisecond, InteractionPoll: 10 * time.Millisecond,
		},
	)
	t.Cleanup(coordinator.Close)
	srv := newDurableWSPGServerForCoordinator(t, db, coordinator, recognizer, false, false)
	return srv, coordinator, session
}

func newDurableWSPGServerForCoordinator(
	t *testing.T,
	db *sql.DB,
	coordinator *turncoord.Coordinator,
	recognizer OCRRecognizer,
	confirmForm, guidedCreate bool,
) *httptest.Server {
	t.Helper()
	sessions := store.NewSessionStore(db)
	messages := store.NewMessageStore(db)
	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "integration-model"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000},
			OCR:  config.OCRConfig{MaxBytes: 1024, Timeout: time.Second},
		}},
		sessions, messages, store.NewFeedbackStore(db), nil, nil,
	)
	h.SetOCRClient(recognizer)
	h.SetTurnCoordinator(coordinator)
	h.SetConfirmFormEnabled(confirmForm)
	h.SetGuidedCreateEnabled(guidedCreate)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/", h.HandleWS)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func readUntilDurableTerminal(t *testing.T, conn *websocket.Conn) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var frames []map[string]any
	for {
		_, raw, err := conn.Read(ctx)
		require.NoError(t, err)
		var frame map[string]any
		require.NoError(t, json.Unmarshal(raw, &frame))
		frames = append(frames, frame)
		if frame["event"] == "done" || frame["event"] == "error" || frame["event"] == "aborted" {
			return frames
		}
	}
}

func readUntilDurableEvent(t *testing.T, conn *websocket.Conn, wanted string) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var frames []map[string]any
	for {
		_, raw, err := conn.Read(ctx)
		require.NoError(t, err)
		var frame map[string]any
		require.NoError(t, json.Unmarshal(raw, &frame))
		frames = append(frames, frame)
		if frame["event"] == wanted {
			return frames
		}
	}
}

func TestWSDurable_PostgresLegacyAndV2ConcurrentSendExecuteOnce(t *testing.T) {
	db := openDurableWSPostgres(t)
	factory := &durableWSEngineFactory{started: make(chan struct{}, 2), release: make(chan struct{})}
	srv, _, session := newDurableWSPGServer(t, db, factory, &sequenceOCR{outputs: []string{"first OCR", "second OCR drift"}})

	legacy := dialWS(t, srv, gatewayHeaders())
	v2 := dialWS(t, srv, gatewayHeaders())
	image := "data:image/png;base64,c2FtZS1pbWFnZQ=="
	legacyFrame := `{"Action":"SendCSAgentChat","SessionId":"` + session.ID + `","ClientTurnId":"same-turn","Message":"same question","Image":"` + image + `"}`
	v2Frame := `{"Action":"SendCSAgentChat","ProtocolVersion":2,"SessionId":"` + session.ID + `","ClientTurnId":"same-turn","Message":"same question","Image":"` + image + `","Features":["turn_replay_v2"]}`
	require.NoError(t, legacy.Write(context.Background(), websocket.MessageText, []byte(legacyFrame)))
	require.NoError(t, v2.Write(context.Background(), websocket.MessageText, []byte(v2Frame)))
	assert.Equal(t, "meta", readDurableFrame(t, legacy)["event"])
	assert.Equal(t, "meta", readDurableFrame(t, v2)["event"])
	select {
	case <-factory.started:
	case <-time.After(3 * time.Second):
		t.Fatal("durable worker did not start")
	}
	close(factory.release)
	legacyFrames := readUntilDurableTerminal(t, legacy)
	v2Frames := readUntilDurableTerminal(t, v2)
	assert.Equal(t, "done", legacyFrames[len(legacyFrames)-1]["event"])
	assert.Equal(t, "done", v2Frames[len(v2Frames)-1]["event"])
	assert.Equal(t, int32(1), factory.calls.Load(), "legacy and v2 frames share one idempotent execution authority")

	turn, err := store.NewPostgresTurnStore(db).FindTurnByClientID(context.Background(), gatewayOwner, session.ID, "same-turn")
	require.NoError(t, err)
	assert.Equal(t, store.TurnStatusCommitted, turn.Status)
	history, err := store.NewMessageStore(db).ListCommittedTail(context.Background(), gatewayOwner, session.ID, 10)
	require.NoError(t, err)
	require.Len(t, history, 2)
	assert.Contains(t, history[0].Content, "same question")
	assert.Equal(t, "durable answer", history[1].Content)
}

func TestWSDurable_PostgresProductionPathWritesDurableAttemptTrace(t *testing.T) {
	db := openDurableWSPostgres(t)
	ctx := context.Background()
	sessions := store.NewSessionStore(db)
	session, err := sessions.Create(ctx, gatewayOwner, nil, nil)
	require.NoError(t, err)
	release := make(chan struct{})
	close(release)
	factory := &durableWSEngineFactory{started: make(chan struct{}, 1), release: release}
	traceWriter := &durableWSTraceWriter{}
	coordinator := turncoord.NewCoordinator(
		store.NewPostgresTurnStore(db), sessions, factory,
		turncoord.Options{
			ReplicaID: "ws-trace", LeaseTTL: 2 * time.Second,
			LeaseRenewInterval: 300 * time.Millisecond, InteractionPoll: 10 * time.Millisecond,
			TraceWriter: traceWriter,
		},
	)
	t.Cleanup(coordinator.Close)
	srv := newDurableWSPGServerForCoordinator(t, db, coordinator, nil, false, false)

	conn := dialWS(t, srv, gatewayHeaders())
	frame := `{"Action":"SendCSAgentChat","ProtocolVersion":2,"SessionId":"` + session.ID + `","ClientTurnId":"trace-over-ws","Message":"continue","Features":["turn_replay_v2"]}`
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(frame)))
	frames := readUntilDurableTerminal(t, conn)
	assert.Equal(t, "done", frames[len(frames)-1]["event"])
	require.Eventually(t, func() bool { return len(traceWriter.snapshot()) == 1 }, 3*time.Second, 20*time.Millisecond)
	record := traceWriter.snapshot()[0]
	assert.Equal(t, "committed", record.Continuity.CommitOutcome)
	assert.Equal(t, observability.CompletionClassAgent, record.Completion.Class)
	assert.NotEmpty(t, record.TurnID)
	assert.Equal(t, record.TurnID+":e1", record.TraceID)
}

func TestWSDurable_PostgresDisconnectThenResumeReplaysPersistedEvents(t *testing.T) {
	db := openDurableWSPostgres(t)
	factory := &durableWSEngineFactory{started: make(chan struct{}, 1), release: make(chan struct{})}
	srv, _, session := newDurableWSPGServer(t, db, factory, nil)

	first := dialWS(t, srv, gatewayHeaders())
	send := `{"Action":"SendCSAgentChat","ProtocolVersion":2,"SessionId":"` + session.ID + `","ClientTurnId":"detach-turn","Message":"continue after disconnect","Features":["turn_replay_v2"]}`
	require.NoError(t, first.Write(context.Background(), websocket.MessageText, []byte(send)))
	meta := readDurableFrame(t, first)
	turnID, _ := meta["TurnId"].(string)
	require.NotEmpty(t, turnID)
	select {
	case <-factory.started:
	case <-time.After(3 * time.Second):
		t.Fatal("durable worker did not start")
	}
	require.NoError(t, first.Close(websocket.StatusNormalClosure, "network detached"))
	close(factory.release)
	require.Eventually(t, func() bool {
		turn, err := store.NewPostgresTurnStore(db).GetTurn(context.Background(), gatewayOwner, turnID)
		return err == nil && turn.Status == store.TurnStatusCommitted
	}, 5*time.Second, 20*time.Millisecond)

	resume := dialWS(t, srv, gatewayHeaders())
	resumeFrame := `{"Action":"ResumeCSAgentTurn","ProtocolVersion":2,"SessionId":"` + session.ID + `","TurnId":"` + turnID + `","ClientTurnId":"detach-turn","LastSeq":0}`
	require.NoError(t, resume.Write(context.Background(), websocket.MessageText, []byte(resumeFrame)))
	frames := readUntilDurableTerminal(t, resume)
	require.GreaterOrEqual(t, len(frames), 2)
	assert.Equal(t, "meta", frames[0]["event"])
	assert.Equal(t, "done", frames[len(frames)-1]["event"])
	var lastSeq float64
	for _, frame := range frames[1:] {
		seq, ok := frame["Seq"].(float64)
		require.True(t, ok, "every replayed coordinator event has a persisted sequence")
		assert.Greater(t, seq, lastSeq)
		lastSeq = seq
	}
	assert.Equal(t, int32(1), factory.calls.Load(), "resume replays; it does not execute the model again")
}

func TestWSDurable_PostgresSameClientTurnIDDifferentImageConflicts(t *testing.T) {
	db := openDurableWSPostgres(t)
	release := make(chan struct{})
	close(release)
	factory := &durableWSEngineFactory{started: make(chan struct{}, 2), release: release}
	srv, _, session := newDurableWSPGServer(t, db, factory, &sequenceOCR{outputs: []string{"one", "two"}})

	first := dialWS(t, srv, gatewayHeaders())
	firstFrame := `{"Action":"SendCSAgentChat","ProtocolVersion":2,"SessionId":"` + session.ID + `","ClientTurnId":"image-conflict","Message":"read","Image":"data:image/png;base64,b25l"}`
	require.NoError(t, first.Write(context.Background(), websocket.MessageText, []byte(firstFrame)))
	firstResult := readUntilDurableTerminal(t, first)
	assert.Equal(t, "done", firstResult[len(firstResult)-1]["event"])

	second := dialWS(t, srv, gatewayHeaders())
	secondFrame := `{"Action":"SendCSAgentChat","ProtocolVersion":2,"SessionId":"` + session.ID + `","ClientTurnId":"image-conflict","Message":"read","Image":"data:image/png;base64,dHdv"}`
	require.NoError(t, second.Write(context.Background(), websocket.MessageText, []byte(secondFrame)))
	conflict := readDurableFrame(t, second)
	assert.Equal(t, "error", conflict["event"])
	assert.Equal(t, "Conflict", conflict["Code"])
	assert.Equal(t, int32(1), factory.calls.Load(), "the conflicting image never reaches model execution")
}

func TestWSDurable_PostgresEditableConfirmationSurvivesReconnectAndCrossReplicaResolution(t *testing.T) {
	db := openDurableWSPostgres(t)
	ctx := context.Background()
	sessions := store.NewSessionStore(db)
	session, err := sessions.Create(ctx, gatewayOwner, nil, nil)
	require.NoError(t, err)
	factory := &durableWSFormFactory{resolutions: make(chan workflow.ConfirmResolution, 1)}
	coordinatorA := turncoord.NewCoordinator(
		store.NewPostgresTurnStore(db), sessions, factory,
		turncoord.Options{
			ReplicaID: "ws-form-a", LeaseTTL: 2 * time.Second,
			LeaseRenewInterval: 300 * time.Millisecond, InteractionPoll: 10 * time.Millisecond,
		},
	)
	coordinatorB := turncoord.NewCoordinator(
		store.NewPostgresTurnStore(db), store.NewSessionStore(db), factory,
		turncoord.Options{
			ReplicaID: "ws-form-b", LeaseTTL: 2 * time.Second,
			LeaseRenewInterval: 300 * time.Millisecond, InteractionPoll: 10 * time.Millisecond,
		},
	)
	t.Cleanup(coordinatorA.Close)
	t.Cleanup(coordinatorB.Close)
	srvA := newDurableWSPGServerForCoordinator(t, db, coordinatorA, nil, true, true)
	srvB := newDurableWSPGServerForCoordinator(t, db, coordinatorB, nil, true, true)

	first := dialWS(t, srvA, gatewayHeaders())
	send := `{"Action":"SendCSAgentChat","ProtocolVersion":2,"SessionId":"` + session.ID + `","ClientTurnId":"editable-ws","Message":"create it","Features":["confirm_form_v1","guided_create_v1"]}`
	require.NoError(t, first.Write(ctx, websocket.MessageText, []byte(send)))
	meta := readDurableFrame(t, first)
	turnID, _ := meta["TurnId"].(string)
	require.NotEmpty(t, turnID)
	firstFrames := readUntilDurableEvent(t, first, "confirmation")
	firstCard := firstFrames[len(firstFrames)-1]
	firstKey, _ := firstCard["InteractionKey"].(string)
	require.Equal(t, "confirmation/0", firstKey)
	require.NotNil(t, firstCard["Form"], "both feature gates must expose the persisted editable form")
	require.NoError(t, first.Close(websocket.StatusNormalClosure, "refresh page"))

	resumed := dialWS(t, srvB, gatewayHeaders())
	resume := `{"Action":"ResumeCSAgentTurn","ProtocolVersion":2,"SessionId":"` + session.ID + `","TurnId":"` + turnID + `","ClientTurnId":"editable-ws","LastSeq":0}`
	require.NoError(t, resumed.Write(ctx, websocket.MessageText, []byte(resume)))
	resumeMeta := readDurableFrame(t, resumed)
	assert.Equal(t, turnID, resumeMeta["TurnId"])
	replayedFrames := readUntilDurableEvent(t, resumed, "confirmation")
	replayedCard := replayedFrames[len(replayedFrames)-1]
	assert.Equal(t, firstKey, replayedCard["InteractionKey"])
	assert.Equal(t, firstCard["Form"], replayedCard["Form"], "refresh must replay the original reviewed choices")

	attackerHeaders := gatewayHeaders()
	attackerHeaders.Set("X-Company-Id", "9")
	attackerHeaders.Set("X-Organization-Id", "9")
	attacker := dialWS(t, srvB, attackerHeaders)
	wrongOwner := `{"Action":"ConfirmCSAgentAction","SessionId":"` + session.ID + `","TurnId":"` + turnID + `","InteractionKey":"` + firstKey + `","Confirmed":true,"Overrides":{"GpuType":"A800"}}`
	require.NoError(t, attacker.Write(ctx, websocket.MessageText, []byte(wrongOwner)))
	wrongOwnerFrame := readDurableFrame(t, attacker)
	assert.Equal(t, "interaction_error", wrongOwnerFrame["event"])
	assert.Equal(t, "NotFound", wrongOwnerFrame["Code"])
	require.NoError(t, attacker.Close(websocket.StatusNormalClosure, "done"))

	wrongSession := `{"Action":"ConfirmCSAgentAction","SessionId":"wrong-session","TurnId":"` + turnID + `","InteractionKey":"` + firstKey + `","Confirmed":true,"Overrides":{"GpuType":"A800"}}`
	require.NoError(t, resumed.Write(ctx, websocket.MessageText, []byte(wrongSession)))
	wrongSessionFrame := readDurableFrame(t, resumed)
	assert.Equal(t, "interaction_error", wrongSessionFrame["event"])
	assert.Equal(t, "NotFound", wrongSessionFrame["Code"])

	invalid := `{"Action":"ConfirmCSAgentAction","SessionId":"` + session.ID + `","TurnId":"` + turnID + `","InteractionKey":"` + firstKey + `","Confirmed":true,"Overrides":{"GpuType":"H100"}}`
	require.NoError(t, resumed.Write(ctx, websocket.MessageText, []byte(invalid)))
	invalidFrame := readDurableFrame(t, resumed)
	assert.Equal(t, "interaction_error", invalidFrame["event"])
	assert.Equal(t, "InvalidParam", invalidFrame["Code"])
	assert.Equal(t, turnID, invalidFrame["TurnId"])
	assert.Equal(t, firstKey, invalidFrame["InteractionKey"])
	pending, err := store.NewPostgresTurnStore(db).GetInteraction(ctx, gatewayOwner, turnID, firstKey)
	require.NoError(t, err)
	assert.Equal(t, store.InteractionStatusPending, pending.Status)

	valid := `{"Action":"ConfirmCSAgentAction","SessionId":"` + session.ID + `","TurnId":"` + turnID + `","InteractionKey":"` + firstKey + `","Confirmed":true,"Overrides":{"GpuType":"A800"}}`
	require.NoError(t, resumed.Write(ctx, websocket.MessageText, []byte(valid)))
	doneFrames := readUntilDurableEvent(t, resumed, "done")
	done := doneFrames[len(doneFrames)-1]
	assert.Equal(t, "created with A800", done["Content"])
	select {
	case resolution := <-factory.resolutions:
		assert.True(t, resolution.Confirmed)
		assert.Equal(t, map[string]string{"GpuType": "A800"}, resolution.Overrides)
	case <-time.After(3 * time.Second):
		t.Fatal("engine did not receive the corrected editable resolution")
	}
	assert.Equal(t, int32(1), factory.calls.Load(), "the second replica replays and resolves; it does not fork execution")
	resolved, err := store.NewPostgresTurnStore(db).GetInteraction(ctx, gatewayOwner, turnID, firstKey)
	require.NoError(t, err)
	assert.Equal(t, store.InteractionStatusResolved, resolved.Status)
	assert.JSONEq(t, `{"confirmed":true,"overrides":{"GpuType":"A800"}}`, string(resolved.ResponsePayload))
}
