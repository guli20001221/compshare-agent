package server

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/observability"
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
// Graceful shutdown: when ctx cancels we (1) flip shuttingDown so
// /readyz returns 503, (2) give load balancers up to 30s to stop sending
// new traffic, (3) shutdown the HTTP server (which closes idle conns and
// waits for in-flight handlers). New WS dials during drain receive a 503.
func (s *Server) Run(ctx context.Context) error {
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
		s.shuttingDown.Store(true)
		shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return s.httpServer.Shutdown(shutCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// RunWithListener is a test hook: same as Run but listens on a caller-
// supplied net.Listener so httptest can pick a random port and dial it.
// Production callers use Run.
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
		s.shuttingDown.Store(true)
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.httpServer.Shutdown(shutCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
