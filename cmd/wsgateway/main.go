// Command wsgateway is a LOCAL DEV gateway simulator for WebSocket 联调.
//
// Why this exists: the production console gateway authenticates the logged-in
// user from a cookie and injects identity HTTP headers (X-Company-Id,
// X-Organization-Id, X-User-Email, X-Request-Id) onto the WebSocket upgrade
// before forwarding to the agent. A browser's `new WebSocket()` CANNOT set
// custom headers, so the agent (which reads identity from those headers) would
// reject a direct browser connection with 400. This proxy stands in for that
// gateway on localhost: it accepts the browser's header-less WS upgrade, then
// dials the local agent WITH the identity headers injected, and pumps frames
// both ways. It does NOT verify any cookie — identity is fixed by flags, so it
// is for local 联调 ONLY, never production.
//
// Usage:
//
//	# 1. start the real agent server (separate shell), e.g. on :8080
//	# 2. start this gateway sim pointing at it:
//	go run ./cmd/wsgateway -listen :8090 -agent ws://127.0.0.1:8080 \
//	    -company 66391350 -org 64404856 -email compshare-test@ucloud.cn
//	# 3. point the frontend CONFIG_URL.WS at ws://localhost:8090
//
// The frontend dials ws://localhost:8090/?Action=CreateCSAgentWS; this proxy
// forwards to <agent>/?Action=CreateCSAgentWS with the identity headers added.
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

// Keep this in sync with the agent WebSocket limit. Screenshot uploads are sent
// as data URLs, so the frame is larger than the raw image bytes.
const maxWSMessageBytes int64 = 20 * 1024 * 1024

func main() {
	var (
		listen  = flag.String("listen", ":8090", "address for the browser to connect to")
		agent   = flag.String("agent", "ws://127.0.0.1:8080", "agent server WS base URL to forward to")
		company = flag.String("company", "66391350", "X-Company-Id to inject (top_organization_id)")
		org     = flag.String("org", "64404856", "X-Organization-Id to inject (organization_id)")
		email   = flag.String("email", "compshare-test@ucloud.cn", "X-User-Email to inject (optional)")
	)
	flag.Parse()

	gw := &gateway{
		agentBase: strings.TrimRight(*agent, "/"),
		company:   *company,
		org:       *org,
		email:     *email,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", gw.handle)

	log.Printf("[wsgateway] DEV gateway sim listening on %s", *listen)
	log.Printf("[wsgateway]   browser → %s  ⇒  forwards to %s with injected identity", *listen, gw.agentBase)
	log.Printf("[wsgateway]   inject: X-Company-Id=%s X-Organization-Id=%s X-User-Email=%s", gw.company, gw.org, gw.email)
	log.Printf("[wsgateway]   point the frontend CONFIG_URL.WS at ws://localhost%s", *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatalf("[wsgateway] serve: %v", err)
	}
}

type gateway struct {
	agentBase           string
	company, org, email string
}

func (g *gateway) handle(w http.ResponseWriter, r *http.Request) {
	// Accept the browser's header-less upgrade. The browser is same-origin only
	// in dev via the frontend's WS URL; allow any origin here because this is a
	// localhost dev tool (NOT production).
	browserConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		log.Printf("[wsgateway] browser accept failed: %v", err)
		return
	}
	browserConn.SetReadLimit(maxWSMessageBytes)
	defer browserConn.CloseNow()

	// Forward to the agent, preserving the query (Action=CreateCSAgentWS, etc.)
	// and INJECTING the identity headers the production gateway would add.
	agentURL := g.agentBase + "/"
	if r.URL.RawQuery != "" {
		agentURL += "?" + r.URL.RawQuery
	}
	hdr := http.Header{}
	hdr.Set("X-Company-Id", g.company)
	hdr.Set("X-Organization-Id", g.org)
	if g.email != "" {
		hdr.Set("X-User-Email", g.email)
	}
	if rid := r.Header.Get("X-Request-Id"); rid != "" {
		hdr.Set("X-Request-Id", rid)
	}

	ctx := r.Context()
	agentConn, _, err := websocket.Dial(ctx, agentURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		log.Printf("[wsgateway] agent dial failed (%s): %v", agentURL, err)
		_ = browserConn.Close(websocket.StatusInternalError, "agent dial failed")
		return
	}
	agentConn.SetReadLimit(maxWSMessageBytes)
	defer agentConn.CloseNow()
	log.Printf("[wsgateway] connection open: browser ⇄ %s", agentURL)

	// Pump frames in both directions. When either side closes, cancel the other.
	pumpCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var once sync.Once
	stop := func() { once.Do(cancel) }

	go pump(pumpCtx, browserConn, agentConn, "browser→agent", stop)
	pump(pumpCtx, agentConn, browserConn, "agent→browser", stop)
}

// pump copies messages from src to dst until an error or context cancel.
func pump(ctx context.Context, src, dst *websocket.Conn, dir string, stop func()) {
	defer stop()
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("[wsgateway] %s read end: %v", dir, err)
			}
			return
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			if ctx.Err() == nil {
				log.Printf("[wsgateway] %s write end: %v", dir, err)
			}
			return
		}
	}
}

var _ = io.EOF // reserved for future framed logging
