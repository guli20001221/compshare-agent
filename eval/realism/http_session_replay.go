package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type sourceSession struct {
	SID       string          `json:"sid"`
	CreatedAt string          `json:"created_at,omitempty"`
	CaseID    string          `json:"case_id,omitempty"`
	Messages  []sourceMessage `json:"messages"`
}

type sourceMessage struct {
	Role    string `json:"role"`
	Status  string `json:"status,omitempty"`
	Content string `json:"content"`
	Turn    int    `json:"turn,omitempty"`
}

type replayResult struct {
	SourceSID     string       `json:"source_sid"`
	CreatedAt     string       `json:"created_at,omitempty"`
	CaseID        string       `json:"case_id"`
	HTTPSessionID string       `json:"http_session_id,omitempty"`
	UserTurns     int          `json:"user_turns"`
	Turns         []turnResult `json:"turns"`
	FinalReply    string       `json:"final_reply,omitempty"`
	Error         string       `json:"error,omitempty"`
	DurationMS    int64        `json:"duration_ms"`
}

type inputReplayRow struct {
	CaseID    string       `json:"case_id"`
	SID       string       `json:"sid"`
	CreatedAt string       `json:"created_at,omitempty"`
	Turns     []turnResult `json:"turns"`
}

type turnResult struct {
	Index              int                 `json:"index"`
	SourceTurn         int                 `json:"source_turn,omitempty"`
	User               string              `json:"user"`
	Reply              string              `json:"reply,omitempty"`
	ErrorCode          string              `json:"error_code,omitempty"`
	ErrorMessage       string              `json:"error_message,omitempty"`
	RequestUUID        string              `json:"request_uuid"`
	MessageID          string              `json:"message_id,omitempty"`
	EventCounts        map[string]int      `json:"event_counts"`
	Steps              []stepBrief         `json:"steps,omitempty"`
	Confirmations      []confirmationBrief `json:"confirmations,omitempty"`
	ConfirmationCount  int                 `json:"confirmation_count"`
	AutoConfirmedValue *bool               `json:"auto_confirmed_value,omitempty"`
	Usage              map[string]any      `json:"usage,omitempty"`
	LatencyMS          int64               `json:"latency_ms"`
	ServerLatencyMS    int                 `json:"server_latency_ms,omitempty"`
	TtftMS             int                 `json:"ttft_ms,omitempty"`
}

type stepBrief struct {
	Index   int    `json:"index"`
	Type    string `json:"type,omitempty"`
	Action  string `json:"action,omitempty"`
	Message string `json:"message,omitempty"`
}

type confirmationBrief struct {
	Action         string         `json:"action,omitempty"`
	Summary        map[string]any `json:"summary,omitempty"`
	TimeoutSeconds int            `json:"timeout_seconds,omitempty"`
	FormStep       map[string]any `json:"form_step,omitempty"`
	FormFields     []formField    `json:"form_fields,omitempty"`
}

type formField struct {
	Key          string       `json:"key,omitempty"`
	Label        string       `json:"label,omitempty"`
	Value        string       `json:"value,omitempty"`
	OptionCount  int          `json:"option_count,omitempty"`
	Disabled     bool         `json:"disabled,omitempty"`
	OptionsBrief []formOption `json:"options_brief,omitempty"`
}

type formOption struct {
	Value    string `json:"value,omitempty"`
	Label    string `json:"label,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func main() {
	input := flag.String("input", "", "sanitized sessions JSONL")
	out := flag.String("out", "eval/reports/chat_resolution_judge_2026-06-13/http_replay_current_637_2026-06-19/raw_replay.jsonl", "output JSONL")
	base := flag.String("base", "http://127.0.0.1:18080", "HTTP base URL")
	project := flag.String("project", "org-cwy2qk", "ProjectId sent to server")
	topOrg := flag.String("top-org", "2384301", "X-Company-Id / top_organization_id")
	org := flag.String("org", "2384302", "X-Organization-Id / organization_id")
	userEmail := flag.String("user-email", "codex-http-replay@example.invalid", "X-User-Email")
	start := flag.Int("start", 0, "0-based session offset")
	limit := flag.Int("limit", 0, "max sessions; 0 means all")
	timeout := flag.Duration("timeout", 240*time.Second, "per-turn timeout")
	confirm := flag.Bool("confirm", false, "auto confirmation value for confirmation frames")
	sleep := flag.Duration("sleep", 250*time.Millisecond, "sleep between turns")
	featuresCSV := flag.String("features", "confirm_form_v1,guided_create_v1", "comma-separated SendCSAgentChat Features")
	flag.Parse()

	if *input == "" {
		fatalf("missing -input")
	}
	sessions, err := loadSessions(*input)
	if err != nil {
		fatalf("load sessions: %v", err)
	}
	origCount := len(sessions)
	if *start > 0 {
		if *start >= len(sessions) {
			fatalf("-start %d beyond %d sessions", *start, len(sessions))
		}
		sessions = sessions[*start:]
	}
	if *limit > 0 && *limit < len(sessions) {
		sessions = sessions[:*limit]
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fatalf("mkdir out: %v", err)
	}
	f, err := os.Create(*out)
	if err != nil {
		fatalf("create out: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)

	client := &http.Client{Timeout: 30 * time.Second}
	features := splitCSV(*featuresCSV)
	end := *start + len(sessions)
	fmt.Fprintf(os.Stderr, "loaded %d sessions from %s; running range [%d,%d); base=%s features=%v confirm=%v\n",
		origCount, *input, *start, end, *base, features, *confirm)
	for i, sess := range sessions {
		absoluteIndex := *start + i
		t0 := time.Now()
		caseID := fmt.Sprintf("S%03d", absoluteIndex+1)
		userTurns := userMessages(sess.Messages)
		res := replayResult{
			SourceSID: sess.SID,
			CreatedAt: sess.CreatedAt,
			CaseID:    firstNonEmpty(sess.CaseID, caseID),
			UserTurns: len(userTurns),
		}
		caseID = res.CaseID
		sessionID, err := createSession(client, *base, *project, *topOrg, *org, sess.SID, caseID)
		if err != nil {
			res.Error = "create_session: " + err.Error()
			res.DurationMS = time.Since(t0).Milliseconds()
			_ = enc.Encode(res)
			fmt.Fprintf(os.Stderr, "[%03d/%03d] %s %s create_session ERROR: %v\n", absoluteIndex+1, origCount, caseID, sess.SID, err)
			continue
		}
		res.HTTPSessionID = sessionID
		for turnIdx, msg := range userTurns {
			tr := runTurn(*base, *project, *topOrg, *org, *userEmail, sessionID, caseID, sess.SID, turnIdx+1, msg, *timeout, *confirm, features)
			res.Turns = append(res.Turns, tr)
			if tr.Reply != "" {
				res.FinalReply = tr.Reply
			}
			if tr.ErrorCode != "" {
				res.Error = fmt.Sprintf("turn_%d: %s %s", turnIdx+1, tr.ErrorCode, tr.ErrorMessage)
				break
			}
			time.Sleep(*sleep)
		}
		res.DurationMS = time.Since(t0).Milliseconds()
		if err := enc.Encode(res); err != nil {
			fatalf("write result: %v", err)
		}
		_ = f.Sync()
		status := "ok"
		if res.Error != "" {
			status = "ERR"
		}
		fmt.Fprintf(os.Stderr, "[%03d/%03d] %s %s %s turns=%d/%d ms=%d final=%s\n",
			absoluteIndex+1, origCount, caseID, sess.SID, status, len(res.Turns), len(userTurns), res.DurationMS, oneLine(res.FinalReply, 120))
	}
}

func loadSessions(path string) ([]sourceSession, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var out []sourceSession
	sc := bufio.NewScanner(file)
	buf := make([]byte, 0, 1024*1024)
	sc.Buffer(buf, 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		sess, err := decodeSourceSession([]byte(line), len(out)+1)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, sc.Err()
}

func decodeSourceSession(raw []byte, index int) (sourceSession, error) {
	var sess sourceSession
	if err := json.Unmarshal(raw, &sess); err != nil {
		return sourceSession{}, err
	}
	if len(sess.Messages) > 0 {
		if sess.CaseID == "" {
			sess.CaseID = fmt.Sprintf("S%03d", index)
		}
		return sess, nil
	}
	var replay inputReplayRow
	if err := json.Unmarshal(raw, &replay); err != nil {
		return sourceSession{}, err
	}
	if len(replay.Turns) == 0 {
		return sess, nil
	}
	sess = sourceSession{
		SID:       replay.SID,
		CreatedAt: replay.CreatedAt,
		CaseID:    replay.CaseID,
		Messages:  make([]sourceMessage, 0, len(replay.Turns)),
	}
	for _, turn := range replay.Turns {
		if strings.TrimSpace(turn.User) == "" {
			continue
		}
		sourceTurn := turn.SourceTurn
		if sourceTurn == 0 {
			sourceTurn = turn.Index
		}
		sess.Messages = append(sess.Messages, sourceMessage{
			Role:    "user",
			Content: turn.User,
			Turn:    sourceTurn,
		})
	}
	if sess.CaseID == "" {
		sess.CaseID = fmt.Sprintf("S%03d", index)
	}
	return sess, nil
}

func userMessages(messages []sourceMessage) []sourceMessage {
	out := make([]sourceMessage, 0, len(messages)/2+1)
	for _, msg := range messages {
		if strings.EqualFold(msg.Role, "user") && strings.TrimSpace(msg.Content) != "" {
			out = append(out, msg)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func createSession(client *http.Client, base, project, topOrg, org, sourceSID, caseID string) (string, error) {
	body := map[string]any{
		"Action":              "CreateCSAgentSession",
		"ProjectId":           project,
		"top_organization_id": topOrg,
		"organization_id":     org,
		"request_uuid":        "replay637-" + caseID + "-create",
		"Title":               "637 replay " + caseID + " " + sourceSID,
	}
	raw, _ := json.Marshal(body)
	resp, err := client.Post(strings.TrimRight(base, "/")+"/", "application/json", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("http %d: %s", resp.StatusCode, string(data))
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return "", err
	}
	if ret, _ := obj["RetCode"].(float64); ret != 0 {
		return "", fmt.Errorf("retcode %v: %s", obj["RetCode"], firstString(obj["Message"]))
	}
	sid := firstString(obj["SessionId"])
	if sid == "" {
		return "", fmt.Errorf("empty SessionId: %s", string(data))
	}
	return sid, nil
}

func runTurn(base, project, topOrg, org, userEmail, sessionID, caseID, sourceSID string, turn int, msg sourceMessage, timeout time.Duration, confirm bool, features []string) turnResult {
	reqID := fmt.Sprintf("replay637-%s-t%d", caseID, turn)
	tr := turnResult{
		Index:       turn,
		SourceTurn:  msg.Turn,
		User:        msg.Content,
		RequestUUID: reqID,
		EventCounts: map[string]int{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	wsURL := toWSURL(base, project)
	headers := http.Header{}
	headers.Set("X-Company-Id", topOrg)
	headers.Set("X-Organization-Id", org)
	headers.Set("X-Request-Id", reqID)
	if userEmail != "" {
		headers.Set("X-User-Email", userEmail)
	}
	start := time.Now()
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		tr.ErrorCode = "ws_handshake"
		tr.ErrorMessage = fmt.Sprintf("http %d: %v", status, err)
		tr.LatencyMS = time.Since(start).Milliseconds()
		return tr
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	frame := map[string]any{
		"Action":       "SendCSAgentChat",
		"SessionId":    sessionID,
		"ProjectId":    project,
		"Message":      msg.Content,
		"request_uuid": reqID,
	}
	if len(features) > 0 {
		frame["Features"] = features
	}
	if err := writeJSON(ctx, conn, frame); err != nil {
		tr.ErrorCode = "ws_write"
		tr.ErrorMessage = err.Error()
		tr.LatencyMS = time.Since(start).Milliseconds()
		return tr
	}

	var tokens strings.Builder
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			tr.ErrorCode = "ws_read"
			tr.ErrorMessage = err.Error()
			tr.LatencyMS = time.Since(start).Milliseconds()
			if tr.Reply == "" {
				tr.Reply = tokens.String()
			}
			return tr
		}
		var f map[string]any
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		event := firstString(f["event"])
		if event == "" {
			event = "unknown"
		}
		tr.EventCounts[event]++
		switch event {
		case "meta":
			tr.MessageID = firstString(f["MessageId"])
		case "step":
			tr.Steps = append(tr.Steps, stepBrief{
				Index:   intNumber(f["Index"]),
				Type:    firstString(f["Type"]),
				Action:  firstString(f["Action"]),
				Message: firstString(f["Message"]),
			})
		case "token":
			tokens.WriteString(firstString(f["Text"]))
		case "confirmation":
			tr.ConfirmationCount++
			tr.Confirmations = append(tr.Confirmations, summarizeConfirmation(f))
			v := confirm
			tr.AutoConfirmedValue = &v
			cid := firstString(f["ConfirmationId"])
			reply := map[string]any{
				"Action":         "ConfirmCSAgentAction",
				"SessionId":      sessionID,
				"ConfirmationId": cid,
				"Confirmed":      confirm,
			}
			_ = writeJSON(ctx, conn, reply)
		case "done":
			if content := firstString(f["Content"]); content != "" {
				tr.Reply = content
			} else {
				tr.Reply = tokens.String()
			}
			tr.Usage = mapAny(f["Usage"])
			tr.ServerLatencyMS = intNumber(f["LatencyMs"])
			tr.TtftMS = intNumber(f["TtftMs"])
			tr.LatencyMS = time.Since(start).Milliseconds()
			return tr
		case "error":
			tr.ErrorCode = firstString(f["Code"])
			tr.ErrorMessage = firstString(f["Message"])
			tr.Reply = tokens.String()
			tr.LatencyMS = time.Since(start).Milliseconds()
			return tr
		}
	}
}

func summarizeConfirmation(f map[string]any) confirmationBrief {
	cb := confirmationBrief{
		Action:         firstString(f["Action"]),
		Summary:        redactMap(mapAny(f["Summary"])),
		TimeoutSeconds: intNumber(f["TimeoutSeconds"]),
	}
	form := mapAny(f["Form"])
	if len(form) == 0 {
		return cb
	}
	cb.FormStep = redactMap(mapAny(form["Step"]))
	fields, _ := form["Fields"].([]any)
	for _, raw := range fields {
		m := mapAny(raw)
		field := formField{
			Key:         firstString(m["Key"]),
			Label:       firstString(m["Label"]),
			Value:       firstString(m["Value"]),
			OptionCount: len(arrayAny(m["Options"])),
			Disabled:    boolValue(m["Disabled"]),
		}
		for i, optRaw := range arrayAny(m["Options"]) {
			if i >= 8 {
				break
			}
			opt := mapAny(optRaw)
			field.OptionsBrief = append(field.OptionsBrief, formOption{
				Value:    firstString(opt["Value"]),
				Label:    firstString(opt["Label"]),
				Disabled: boolValue(opt["Disabled"]),
				Reason:   firstString(opt["Reason"]),
			})
		}
		cb.FormFields = append(cb.FormFields, field)
	}
	return cb
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, raw)
}

func toWSURL(base, project string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = "/"
	q := u.Query()
	q.Set("Action", "CreateCSAgentWS")
	if project != "" {
		q.Set("ProjectId", project)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func splitCSV(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func firstString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func intNumber(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

func boolValue(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

func mapAny(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func arrayAny(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}

func redactMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "password") || strings.Contains(lower, "passwd") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "key") {
			out[k] = "[REDACTED]"
			continue
		}
		switch vv := v.(type) {
		case map[string]any:
			out[k] = redactMap(vv)
		default:
			out[k] = vv
		}
	}
	return out
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= n {
		return s
	}
	rs := []rune(s)
	return string(rs[:n]) + "..."
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
