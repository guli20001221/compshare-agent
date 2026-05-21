package server

import (
	"context"
	"database/sql"
	"errors"
	"log"
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

	// activeConns lets graceful shutdown actively close in-flight WS
	// conns with 1001 (handleWS readers don't observe ctx until conn
	// closes; without this Shutdown times out and we TCP-RST).
	// Lifecycle: Store on Accept, Delete on exit via trackConn.
	activeConns sync.Map // map[string]*websocket.Conn

	// lbDrainDelay: wait after flipping shuttingDown so the LB sees
	// readyz=503 before we yank live conns. 1s for Run; 0 for tests.
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

// Run listens, registers routes, blocks until ctx cancels, then runs
// gracefulShutdown (see helper).
//
// Known limitation: TenantSourceGateway parseTenant trusts URL query
// params before falling back to headers. Production use requires a
// trusted gateway that strips client-supplied query before forwarding,
// or a header/token-based identity path. Surfaced at startup via the
// log.Printf below.
func (s *Server) Run(ctx context.Context) error {
	log.Printf("WARNING: TenantSourceGateway currently trusts URL query "+
		"params for tenant identity. Production use requires a trusted "+
		"gateway that strips client-supplied query before forwarding, or "+
		"a header/token-based identity path. tenant_source=%s",
		s.tenantSource)
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

// gracefulShutdown: flip readyz=503 → lbDrainDelay → parallel-close
// active WS conns (capped by closeTimeout) → http.Shutdown(httpTimeout).
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

// trackConn registers a WS conn for graceful shutdown; returns the
// deregister closure for `defer s.trackConn(id, c)()`.
func (s *Server) trackConn(id string, conn *websocket.Conn) func() {
	s.activeConns.Store(id, conn)
	return func() { s.activeConns.Delete(id) }
}

// closeAllConns sends 1001 (StatusGoingAway = reconnect, not 1011 = back
// off) to every tracked conn IN PARALLEL with a cap. Serial close = O(N
// × 5s) because nhooyr's Conn.Close waits 5s for peer ack. CloseNow does
// NOT help — it blocks on the same internal mutex Close holds — so the
// only escape from a slow peer is the cap; the leaked goroutine finishes
// on its own at nhooyr's 5s timeout.
//
// Race: a conn that passed shuttingDown but hasn't yet hit trackConn.Store
// won't be seen here; it drains via httpServer.Shutdown.
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

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(totalTimeout):
	}
}
