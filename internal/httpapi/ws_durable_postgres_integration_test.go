package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/turncoord"
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
		"0001_init.sql", "0003_add_session_context_version.sql",
		"0005_create_turn_execution.sql", "0006_create_turn_protocol.sql",
		"0007_add_turn_recovery_context.sql",
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
}

func (e *durableWSEngine) SetSessionState(state engine.SessionState, version int) {
	e.state, e.version = state, version
}

func (e *durableWSEngine) SessionStateSnapshot() (engine.SessionState, int, bool) {
	return e.state, e.version, true
}

func (e *durableWSEngine) ChatWithOptions(ctx context.Context, _ string, _ func(engine.StepEvent), _ engine.ChatOptions) (string, error) {
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

func newDurableWSPGServer(t *testing.T, db *sql.DB, factory turncoord.EngineFactory, recognizer OCRRecognizer) (*httptest.Server, *turncoord.Coordinator, store.Session) {
	t.Helper()
	sessions := store.NewSessionStore(db)
	messages := store.NewMessageStore(db)
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
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/", h.HandleWS)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv, coordinator, session
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
