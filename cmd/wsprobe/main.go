// Command wsprobe is a standalone smoke client for the agent's chat WebSocket.
//
// It dials the WS endpoint exactly as the platform gateway does — identity in
// the upgrade HTTP headers (X-Company-Id / X-Organization-Id / X-Request-Id),
// Action=CreateCSAgentWS in the query — then sends a SendCSAgentChat frame and
// prints every frame it receives. This isolates "is the agent's WS path打通"
// without needing the frontend or the real gateway.
//
// Client mode (default):
//
//	go run ./cmd/wsprobe -url ws://localhost:8080 -session SESS -message "你好"
//	go run ./cmd/wsprobe -url ws://HOST:PORT -message "关机 uhost-xxx" -confirm yes
//
// Mock-server mode — stands up a fake agent that speaks the same frame protocol
// (meta → token → done, with a confirmation round-trip when the message
// contains "confirm"). Useful for testing the probe itself, or for a frontend
// dev to develop against without a real backend:
//
//	go run ./cmd/wsprobe -mock :8089
//	# then, in another shell:
//	go run ./cmd/wsprobe -url ws://localhost:8089 -message "confirm please" -confirm yes
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func main() {
	var (
		mockAddr = flag.String("mock", "", "run a fake-agent WS server on this addr (e.g. :8089) instead of dialing")
		url      = flag.String("url", "ws://localhost:8080", "agent WS base URL (Action=CreateCSAgentWS is appended)")
		session  = flag.String("session", "probe-sess", "SessionId to send")
		message  = flag.String("message", "你好", "chat Message to send")
		confirm  = flag.String("confirm", "", "auto-reply to a confirmation frame: 'yes' or 'no' (empty = do not reply)")
		company  = flag.String("company", "1", "X-Company-Id header (top_organization_id)")
		org      = flag.String("org", "2", "X-Organization-Id header (organization_id)")
		project  = flag.String("project", "", "ProjectId to include in the chat frame")
		reqID    = flag.String("request-id", "wsprobe-1", "X-Request-Id header")
	)
	flag.Parse()

	if *mockAddr != "" {
		runMock(*mockAddr)
		return
	}
	runClient(clientOpts{
		url:     *url,
		session: *session,
		message: *message,
		confirm: strings.ToLower(*confirm),
		company: *company,
		org:     *org,
		project: *project,
		reqID:   *reqID,
	})
}

type clientOpts struct {
	url, session, message, confirm, company, org, project, reqID string
}

func runClient(o clientOpts) {
	dialURL := o.url
	if !strings.Contains(dialURL, "Action=") {
		sep := "?"
		if strings.Contains(dialURL, "?") {
			sep = "&"
		}
		dialURL = strings.TrimRight(dialURL, "/") + sep + "Action=CreateCSAgentWS"
	}

	h := http.Header{}
	h.Set("X-Company-Id", o.company)
	h.Set("X-Organization-Id", o.org)
	h.Set("X-Request-Id", o.reqID)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	fmt.Printf("→ dialing %s\n   headers: X-Company-Id=%s X-Organization-Id=%s X-Request-Id=%s\n",
		dialURL, o.company, o.org, o.reqID)
	conn, resp, err := websocket.Dial(ctx, dialURL, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		fmt.Printf("✘ handshake FAILED (http status %d): %v\n", status, err)
		fmt.Println("   → WS not打通 at the handshake layer. Check: server reachable? identity headers injected? GET / routed to HandleWS?")
		os.Exit(1)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	fmt.Println("✔ handshake OK — socket open")

	chat := map[string]any{
		"Action":    "SendCSAgentChat",
		"SessionId": o.session,
		"Message":   o.message,
	}
	if o.project != "" {
		chat["ProjectId"] = o.project
	}
	if err := writeJSON(ctx, conn, chat); err != nil {
		fmt.Printf("✘ failed to send chat frame: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("→ sent SendCSAgentChat: %q\n\n", o.message)

	var tokens strings.Builder
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			fmt.Printf("\n✘ read ended: %v\n", err)
			os.Exit(1)
		}
		var f map[string]any
		if err := json.Unmarshal(data, &f); err != nil {
			fmt.Printf("   [non-JSON frame] %s\n", data)
			continue
		}
		event, _ := f["event"].(string)
		switch event {
		case "meta":
			fmt.Printf("● meta        RequestId=%v MessageId=%v\n", f["RequestId"], f["MessageId"])
		case "step":
			fmt.Printf("● step        [%v] %v %v\n", f["Index"], f["Type"], f["Action"])
		case "token":
			t, _ := f["Text"].(string)
			tokens.WriteString(t)
			fmt.Printf("● token       %q\n", t)
		case "confirmation":
			fmt.Printf("● confirmation Action=%v ConfirmationId=%v\n", f["Action"], f["ConfirmationId"])
			if o.confirm == "yes" || o.confirm == "no" {
				cid, _ := f["ConfirmationId"].(string)
				reply := map[string]any{
					"Action":         "ConfirmCSAgentAction",
					"SessionId":      o.session,
					"ConfirmationId": cid,
					"Confirmed":      o.confirm == "yes",
				}
				if err := writeJSON(ctx, conn, reply); err != nil {
					fmt.Printf("✘ failed to send confirm: %v\n", err)
					os.Exit(1)
				}
				fmt.Printf("→ sent ConfirmCSAgentAction Confirmed=%v (same socket)\n", o.confirm == "yes")
			} else {
				fmt.Println("   (no -confirm flag; not replying — the turn will time out waiting)")
			}
		case "done":
			fmt.Printf("\n✔ done — full reply: %q\n", firstNonEmptyStr(f["Content"], tokens.String()))
			fmt.Println("✔ WS path打通: handshake + chat stream" + confirmSuffix(o.confirm))
			return
		case "error":
			fmt.Printf("\n✘ error frame: Code=%v Message=%v\n", f["Code"], f["Message"])
			os.Exit(1)
		default:
			fmt.Printf("● [%s] %s\n", event, data)
		}
	}
}

func confirmSuffix(confirm string) string {
	if confirm == "yes" || confirm == "no" {
		return " + confirmation round-trip"
	}
	return ""
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, raw)
}

func firstNonEmptyStr(vals ...any) string {
	for _, v := range vals {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// runMock stands up a fake agent that speaks the agent's outbound frame
// protocol. It is NOT the real agent (no engine, no LLM, no auth) — only enough
// to exercise a client/frontend against the wire contract.
func runMock(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Company-Id") == "" {
			http.Error(w, "missing X-Company-Id", http.StatusBadRequest)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()

		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var f map[string]any
		_ = json.Unmarshal(data, &f)
		msg, _ := f["Message"].(string)
		log.Printf("[mock] received SendCSAgentChat: %q", msg)

		_ = writeJSON(ctx, conn, map[string]any{"event": "meta", "RequestId": f["request_uuid"], "MessageId": "mock-msg-1", "SessionId": f["SessionId"]})

		if strings.Contains(strings.ToLower(msg), "confirm") {
			_ = writeJSON(ctx, conn, map[string]any{
				"event": "confirmation", "Action": "StartCompShareInstance",
				"ConfirmationId": "mock-confirm-1", "TimeoutSeconds": 60,
				"Summary": map[string]any{"UHostId": "uhost-mock"},
			})
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var cf map[string]any
			_ = json.Unmarshal(data, &cf)
			ok, _ := cf["Confirmed"].(bool)
			log.Printf("[mock] received ConfirmCSAgentAction: Confirmed=%v", ok)
			reply := "已取消"
			if ok {
				reply = "已确认，正在执行"
			}
			_ = writeJSON(ctx, conn, map[string]any{"event": "token", "Text": reply})
			_ = writeJSON(ctx, conn, map[string]any{"event": "done", "Content": reply})
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		}

		for _, tok := range []string{"你", "好", "，", "这是 mock 回复"} {
			_ = writeJSON(ctx, conn, map[string]any{"event": "token", "Text": tok})
		}
		_ = writeJSON(ctx, conn, map[string]any{"event": "done", "Content": "你好，这是 mock 回复"})
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		log.Printf("[mock] fake-agent WS server on %s (Action=CreateCSAgentWS). Ctrl-C to stop.", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[mock] serve: %v", err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
