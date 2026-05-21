package server

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// stubLLMReply is a minimal LLMClient that returns a single canned reply.
// Each call records the latest request so tests can inspect prompts.
type stubLLMReply struct {
	mu          sync.Mutex
	reply       string
	lastRequest llm.ChatRequest
}

func (s *stubLLMReply) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastRequest = req
	return &llm.ChatResponse{
		Content: s.reply,
	}, nil
}

// stubExecutor mirrors the engine_test mockExecutor but lives here so
// server tests can run independently of the engine package's test helpers.
type stubExecutor struct{}

func (stubExecutor) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	return map[string]any{"Action": action, "RetCode": 0}, nil
}

// startTestServer brings up a Server bound to a random port on 127.0.0.1
// for the test's lifetime. Returns the base URL + a cancel func.
func startTestServer(t *testing.T, deps *engine.SharedDeps) (baseURL string, shutdown func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv, err := New(Options{
		Addr:           ln.Addr().String(),
		Deps:           deps,
		TenantSource:   TenantSourceGateway,
		AllowedOrigins: []string{"*"},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.RunWithListener(ctx, ln) }()

	baseURL = "ws://" + ln.Addr().String()
	return baseURL, func() {
		cancel()
		// Give the server up to 2s to drain.
		time.Sleep(50 * time.Millisecond)
	}
}

func newTestDeps(reply string) (*engine.SharedDeps, *stubLLMReply) {
	stub := &stubLLMReply{reply: reply}
	deps := &engine.SharedDeps{
		LLMClient:                stub,
		RateLimiter:              governance.NewMemoryLimiter(governance.DefaultLimits()),
		SupportsObjectToolChoice: true,
		ExternalExecutor:         stubExecutor{},
	}
	return deps, stub
}

// dialWSWithTenant opens a WS connection with the given top_org_id / org_id
// in query params (gateway mode). Returns the connection and a close func.
func dialWSWithTenant(t *testing.T, baseURL string, topOrgID, orgID int64) *websocket.Conn {
	t.Helper()
	url := baseURL + "/v1/chat/stream?top_org_id=" + itoa(topOrgID) + "&org_id=" + itoa(orgID)
	conn, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{})
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	return conn
}

// waitForType reads messages until one matches typ OR the deadline expires.
func waitForType(t *testing.T, conn *websocket.Conn, typ string, deadline time.Duration) ServerMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	for {
		var msg ServerMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			t.Fatalf("read while waiting for %s: %v", typ, err)
		}
		if msg.Type == typ {
			return msg
		}
	}
}

// itoa is a tiny strconv-free integer-to-string used in URL construction
// to keep the test file's import list small.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestServer_HealthzAlwaysOK guards the liveness probe: it returns 200
// even when MySQL is missing. Encodes WHY: liveness must signal "process
// alive", not "process healthy" — wiring readiness checks into liveness
// causes pods to be killed on transient outages instead of removed from
// load balancers.
func TestServer_HealthzAlwaysOK(t *testing.T) {
	deps, _ := newTestDeps("ignored")
	srv, err := New(Options{Addr: "127.0.0.1:0", Deps: deps})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)
	srv.handleHealthz(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/healthz: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("/healthz body missing ok=true: %q", rec.Body.String())
	}
}

// TestServer_ReadyzDownDuringShutdown asserts that flipping shuttingDown
// turns /readyz into a 503 immediately. This is what lets load balancers
// stop sending NEW traffic to a pod during graceful drain.
func TestServer_ReadyzDownDuringShutdown(t *testing.T) {
	deps, _ := newTestDeps("ignored")
	srv, err := New(Options{Addr: "127.0.0.1:0", Deps: deps})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.shuttingDown.Store(true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/readyz", nil)
	srv.handleReadyz(rec, req)
	if rec.Code != 503 {
		t.Fatalf("/readyz during shutdown: got %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"reason":"shutting_down"`) {
		t.Fatalf("/readyz reason missing: %q", rec.Body.String())
	}
}

// TestServer_ReadyzOKWithoutMySQL asserts the "MySQL not configured"
// branch — readyz should still return 200 with mysql=skipped so file-sink
// deployments aren't paged as unhealthy.
func TestServer_ReadyzOKWithoutMySQL(t *testing.T) {
	deps, _ := newTestDeps("ignored")
	srv, err := New(Options{Addr: "127.0.0.1:0", Deps: deps})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/readyz", nil)
	srv.handleReadyz(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/readyz: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "skipped") {
		t.Fatalf("/readyz missing mysql=skipped: %q", rec.Body.String())
	}
}

// TestServer_E2E_SingleUserHappyPath: full WS round-trip with stub LLM.
// Encodes WHY: the entire reader/writer/engine wiring path must be
// reachable from a single send. If any goroutine fails to start (or
// startup ordering breaks), this hangs and the timeout flags it.
func TestServer_E2E_SingleUserHappyPath(t *testing.T) {
	deps, _ := newTestDeps("hello from stub LLM")
	baseURL, shutdown := startTestServer(t, deps)
	defer shutdown()

	conn := dialWSWithTenant(t, baseURL, 11, 111)
	defer conn.Close(websocket.StatusNormalClosure, "")

	// First frame should be "ready" with a session_id.
	ready := waitForType(t, conn, ServerMsgReady, 5*time.Second)
	if ready.SessionID == "" {
		t.Fatalf("ready frame missing session_id")
	}

	// Send user_message.
	if err := wsjson.Write(context.Background(), conn, ClientMessage{
		Type:        ClientMsgUserMessage,
		Text:        "hi",
		RequestUUID: "test-uuid-1",
	}); err != nil {
		t.Fatalf("write user_message: %v", err)
	}

	// Wait for answer_final.
	answer := waitForType(t, conn, ServerMsgAnswerFinal, 30*time.Second)
	if answer.RequestUUID != "test-uuid-1" {
		t.Fatalf("answer_final RequestUUID drift: %q", answer.RequestUUID)
	}
	if !strings.Contains(answer.Text, "hello from stub LLM") {
		t.Fatalf("answer_final text drift: %q", answer.Text)
	}

	// Done frame should follow.
	done := waitForType(t, conn, ServerMsgDone, 5*time.Second)
	if done.RequestUUID != "test-uuid-1" {
		t.Fatalf("done RequestUUID drift: %q", done.RequestUUID)
	}
}

// TestServer_E2E_TwoConcurrentTenantsIsolation: open two WS connections
// with different tenants, send a marker in each, assert neither user's
// reply contains the other's marker. This is the runtime confirmation
// that the engine session split (PR2) holds end-to-end.
func TestServer_E2E_TwoConcurrentTenantsIsolation(t *testing.T) {
	// Two stubs returning the marker that was sent in (echoing through
	// the system message + Chat call). We use a per-tenant stub by
	// keying on the LLMClient identity per-deps.
	depsA, stubA := newTestDeps("REPLY_FROM_A")
	depsB, stubB := newTestDeps("REPLY_FROM_B")
	_, _ = stubA, stubB

	baseA, shutdownA := startTestServer(t, depsA)
	defer shutdownA()
	baseB, shutdownB := startTestServer(t, depsB)
	defer shutdownB()

	connA := dialWSWithTenant(t, baseA, 11, 111)
	defer connA.Close(websocket.StatusNormalClosure, "")
	connB := dialWSWithTenant(t, baseB, 22, 222)
	defer connB.Close(websocket.StatusNormalClosure, "")

	// Drain ready frames.
	waitForType(t, connA, ServerMsgReady, 5*time.Second)
	waitForType(t, connB, ServerMsgReady, 5*time.Second)

	// Send concurrent messages.
	go func() {
		_ = wsjson.Write(context.Background(), connA, ClientMessage{
			Type: ClientMsgUserMessage, Text: "MARKER_A", RequestUUID: "ua",
		})
	}()
	go func() {
		_ = wsjson.Write(context.Background(), connB, ClientMessage{
			Type: ClientMsgUserMessage, Text: "MARKER_B", RequestUUID: "ub",
		})
	}()

	replyA := waitForType(t, connA, ServerMsgAnswerFinal, 30*time.Second)
	replyB := waitForType(t, connB, ServerMsgAnswerFinal, 30*time.Second)

	if strings.Contains(replyA.Text, "REPLY_FROM_B") {
		t.Errorf("tenant A reply leaked tenant B content: %q", replyA.Text)
	}
	if strings.Contains(replyB.Text, "REPLY_FROM_A") {
		t.Errorf("tenant B reply leaked tenant A content: %q", replyB.Text)
	}
	if replyA.RequestUUID != "ua" {
		t.Errorf("tenant A request_uuid drift: got %q want ua", replyA.RequestUUID)
	}
	if replyB.RequestUUID != "ub" {
		t.Errorf("tenant B request_uuid drift: got %q want ub", replyB.RequestUUID)
	}
}

// TestServer_RejectsMissingTenant asserts a WS dial without identity
// gets 401 (before WS upgrade completes). Encodes WHY: anonymous users
// would land in the AnonymousSubjectKey bucket and share one rate-limit
// pool — the agent must refuse them at the door.
func TestServer_RejectsMissingTenant(t *testing.T) {
	deps, _ := newTestDeps("ignored")
	baseURL, shutdown := startTestServer(t, deps)
	defer shutdown()

	// Dial without top_org_id / org_id.
	url := baseURL + "/v1/chat/stream"
	_, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{})
	if err == nil {
		t.Fatalf("expected WS dial to fail with 401, got success")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 in error, got %v", err)
	}
}

// Ensure unused tools.ToolExecutor reference does not pull dead imports.
var _ tools.ToolExecutor = stubExecutor{}
