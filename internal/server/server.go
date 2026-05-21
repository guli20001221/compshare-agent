package server

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/observability"

	"nhooyr.io/websocket"
)

// Server is the WebSocket entry point for console-deployed agents. One
// process hosts many concurrent tenants; each WS connection gets its own
// engine session built from a shared engine.SharedDeps. Cross-tenant
// isolation is enforced by per-session Engine state (see plan §3 + the
// reflection check test in PR2).
type Server struct {
	addr           string
	deps           *engine.SharedDeps
	traceSink      observability.Writer
	msgRecorder    *MessageRecorder
	model          string
	tenantSource   TenantSource
	allowedOrigins []string

	shuttingDown atomic.Bool

	// db is used by handleReadyz to ping MySQL. nil = MySQL not configured
	// (CLI/file-sink mode), readyz reports "mysql":"skipped".
	db *sql.DB

	httpServer *http.Server

	// activeConns tracks every accepted WS connection so graceful shutdown
	// can actively close them with StatusGoingAway (1001). Without this,
	// httpServer.Shutdown waits up to 30s for WS handlers to return, but
	// the reader inside handleWS is blocked on wsjson.Read and never sees
	// ctx cancellation — so Shutdown reliably times out and we TCP-RST.
	// Clients then see a generic abnormal close instead of the explicit
	// going-away signal that tells them "reconnect", not "retry hard".
	// Lifecycle: handleWS Store on Accept, Delete on exit (registered via
	// trackConn helper so the defer is single-line).
	activeConns sync.Map // map[string]*websocket.Conn keyed by connectionID

	// lbDrainDelay is how long the server waits AFTER flipping shuttingDown
	// (so /readyz returns 503) BEFORE actively closing connections. Lets a
	// load balancer notice the readiness flip and stop routing new traffic
	// first. Default 1s for Run (production); RunWithListener sets it to 0
	// because tests have no LB and 1s slows test cleanup.
	lbDrainDelay time.Duration
}

// Options configures Server. Addr / Deps are required; TraceSink is the
// optional MySQL/file/multi writer the per-session Engine pipes traces
// into; MsgRecorder is the optional agent_messages writer (A5); AllowedOrigins
// MUST be set in production to defeat WS-CSRF.
type Options struct {
	Addr           string
	Deps           *engine.SharedDeps
	TraceSink      observability.Writer
	MsgRecorder    *MessageRecorder
	Model          string
	TenantSource   TenantSource
	AllowedOrigins []string
	DB             *sql.DB
}

// New constructs a Server. It does NOT start listening; call Run.
func New(opts Options) (*Server, error) {
	if opts.Addr == "" {
		return nil, errors.New("server.New: Addr is empty")
	}
	if opts.Deps == nil {
		return nil, errors.New("server.New: Deps is nil")
	}
	src := opts.TenantSource
	if src == "" {
		src = TenantSourceGateway
	}
	return &Server{
		addr:           opts.Addr,
		deps:           opts.Deps,
		traceSink:      opts.TraceSink,
		msgRecorder:    opts.MsgRecorder,
		model:          opts.Model,
		tenantSource:   src,
		allowedOrigins: opts.AllowedOrigins,
		db:             opts.DB,
	}, nil
}

// Run starts the HTTP server, registers WS + health routes, blocks until
// ctx is cancelled (typically SIGTERM), then drains gracefully.
//
// Graceful shutdown sequence:
//  1. Flip shuttingDown → /readyz returns 503.
//  2. Sleep lbDrainDelay (1s in prod) so the load balancer notices and
//     stops routing new traffic before we close existing conns.
//  3. closeAllConns with StatusGoingAway (1001) — tells WS clients to
//     reconnect, not retry hard. Without this, httpServer.Shutdown waits
//     ~30s and times out (handleWS readers don't honor ctx until conn
//     closes), then we TCP-RST and clients see an opaque abnormal close.
//  4. httpServer.Shutdown(30s) drains the now-closing handlers.
//
// New WS dials during drain receive 503 from the shuttingDown check at
// the top of handleWS.
func (s *Server) Run(ctx context.Context) error {
	s.lbDrainDelay = 1 * time.Second
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/v1/chat/stream", s.handleWS)

	s.httpServer = &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		ln, err := net.Listen("tcp", s.addr)
		if err != nil {
			errCh <- err
			return
		}
		errCh <- s.httpServer.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		return s.gracefulShutdown(30*time.Second, 5*time.Second)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// RunWithListener is a test hook: same as Run but listens on a caller-
// supplied net.Listener so httptest can pick a random port and dial it.
// Production callers use Run. lbDrainDelay stays at 0 (the zero value) so
// tests don't pay an extra 1s per shutdown.
func (s *Server) RunWithListener(ctx context.Context, ln net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/v1/chat/stream", s.handleWS)

	s.httpServer = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.httpServer.Serve(ln)
	}()
	select {
	case <-ctx.Done():
		return s.gracefulShutdown(5*time.Second, 2*time.Second)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// gracefulShutdown runs the 4-step drain (flip → wait → close conns →
// http Shutdown). Shared by Run / RunWithListener so the order can't
// drift. closeTimeout caps the parallel-close phase so a few slow
// clients can't pin shutdown for 5s each.
func (s *Server) gracefulShutdown(httpTimeout, closeTimeout time.Duration) error {
	s.shuttingDown.Store(true)
	if s.lbDrainDelay > 0 {
		time.Sleep(s.lbDrainDelay)
	}
	s.closeAllConns("server shutting down", closeTimeout)
	shutCtx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	return s.httpServer.Shutdown(shutCtx)
}

// trackConn registers an accepted WS conn so graceful shutdown can reach
// it, and returns the cleanup func the caller must defer to deregister
// on every exit path (including panic). Returns a closure so the call
// site stays a single `defer s.trackConn(id, c)()` line.
func (s *Server) trackConn(id string, conn *websocket.Conn) func() {
	s.activeConns.Store(id, conn)
	return func() { s.activeConns.Delete(id) }
}

// closeAllConns closes every tracked WS conn with StatusGoingAway (1001 =
// reconnect, not 1011 = back off) IN PARALLEL with a total cap. Serial
// close would be O(N × 5s) because nhooyr's Conn.Close waits up to 5s for
// the peer ack — one slow client could pin shutdown indefinitely.
//
// After totalTimeout, any still-handshaking conn gets CloseNow (no wait,
// TCP-level slam) so shutdown can proceed.
//
// Race: a conn that passed handleWS's shuttingDown check before the flip
// but hasn't reached trackConn.Store yet won't be seen here; it drains
// via httpServer.Shutdown afterward.
func (s *Server) closeAllConns(reason string, totalTimeout time.Duration) {
	var wg sync.WaitGroup
	s.activeConns.Range(func(_, value any) bool {
		conn, ok := value.(*websocket.Conn)
		if !ok {
			return true
		}
		wg.Add(1)
		go func(c *websocket.Conn) {
			defer wg.Done()
			_ = c.Close(websocket.StatusGoingAway, reason)
		}(conn)
		return true
	})

	// Bound how long we wait for the parallel Close handshakes here. If
	// some conns are still mid-handshake at the cap we just return; the
	// goroutines complete on their own at nhooyr's 5s peer-ack timeout
	// (in parallel, so this doesn't grow with N) and httpServer.Shutdown
	// will wait for the now-closing handlers. Note: CloseNow does NOT
	// help here — it blocks on the same internal mutex that the in-flight
	// Close is holding, so it would just serialize what we just parallelized.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(totalTimeout):
	}
}
