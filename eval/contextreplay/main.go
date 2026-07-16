// Command contextreplay replays a small set of sanitized production context
// failures through the shipping HTTP + durable WebSocket v2 protocol. It is an
// evaluation client, not a product chat entry point.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type fixture struct {
	Version int          `json:"version"`
	Cases   []replayCase `json:"cases"`
}

type replayCase struct {
	Name             string       `json:"name"`
	SourceRef        string       `json:"source_ref"`
	IdentityMode     string       `json:"identity_mode,omitempty"`
	RequiresInstance bool         `json:"requires_instance,omitempty"`
	Turns            []replayTurn `json:"turns"`
}

type replayTurn struct {
	Message string      `json:"message"`
	Expect  expectation `json:"expect,omitempty"`
}

type expectation struct {
	ReplyAny     []string `json:"reply_any,omitempty"`
	ReplyNone    []string `json:"reply_none,omitempty"`
	ActionAny    []string `json:"action_any,omitempty"`
	ActionNone   []string `json:"action_none,omitempty"`
	Confirmation string   `json:"confirmation,omitempty"`
}

type options struct {
	httpURL, gatewayWS, directWS, project, company, org, email string
	timeout                                                    time.Duration
}

type turnResult struct {
	Reply         string
	Actions       []string
	Confirmations int
	TurnID        string
	LastSeq       int64
	Committed     bool
}

type caseReport struct {
	Name          string   `json:"name"`
	SourceRef     string   `json:"source_ref"`
	Passed        bool     `json:"passed"`
	Failures      []string `json:"failures,omitempty"`
	TurnCount     int      `json:"turn_count"`
	Actions       []string `json:"actions,omitempty"`
	Confirmations int      `json:"confirmations"`
}

type report struct {
	Protocol       string       `json:"protocol"`
	StartedAt      time.Time    `json:"started_at"`
	FinishedAt     time.Time    `json:"finished_at"`
	DiscoveredHost bool         `json:"discovered_test_instance"`
	Passed         int          `json:"passed"`
	Failed         int          `json:"failed"`
	Cases          []caseReport `json:"cases"`
}

func main() {
	var (
		casesPath = flag.String("cases", "eval/contextreplay/cases.json", "sanitized real-record fixture")
		httpURL   = flag.String("http", "http://127.0.0.1:7429/", "shipping HTTP endpoint")
		gatewayWS = flag.String("ws", "ws://127.0.0.1:8090/", "browser-facing WebSocket gateway")
		directWS  = flag.String("direct-ws", "ws://127.0.0.1:7429/", "agent WebSocket endpoint for identity-failure case")
		project   = flag.String("project", "org-cwy2qk", "ProjectId")
		company   = flag.String("company", "66391350", "test top organization")
		org       = flag.String("org", "64404856", "test organization")
		email     = flag.String("email", "compshare-test@ucloud.cn", "test user email")
		out       = flag.String("out", "", "optional summary JSON output (contains no replies or resource ids)")
		caseNames = flag.String("case", "", "optional comma-separated case names for focused reruns")
		timeout   = flag.Duration("turn-timeout", 3*time.Minute, "per-turn timeout")
	)
	flag.Parse()

	opts := options{httpURL: *httpURL, gatewayWS: *gatewayWS, directWS: *directWS, project: *project, company: *company, org: *org, email: *email, timeout: *timeout}
	data, err := os.ReadFile(*casesPath)
	fatalIf(err)
	var suite fixture
	fatalIf(json.Unmarshal(data, &suite))
	if suite.Version != 1 || len(suite.Cases) != 10 {
		fatalIf(fmt.Errorf("fixture must contain version 1 and exactly 10 cases"))
	}

	selected, err := selectCases(suite.Cases, *caseNames)
	fatalIf(err)
	started := time.Now().UTC()
	instanceID := ""
	if casesRequireInstance(selected) {
		var discoverErr error
		instanceID, discoverErr = discoverInstance(opts)
		if discoverErr != nil {
			fmt.Fprintf(os.Stderr, "warning: no test instance discovered: %v\n", discoverErr)
		}
	}

	summary := report{Protocol: "HTTP CreateCSAgentSession + WebSocket v2", StartedAt: started, DiscoveredHost: instanceID != ""}
	for _, testCase := range selected {
		caseResult := runCase(opts, testCase, instanceID)
		summary.Cases = append(summary.Cases, caseResult)
		if caseResult.Passed {
			summary.Passed++
			fmt.Printf("PASS %-52s actions=%v confirmations=%d\n", caseResult.Name, caseResult.Actions, caseResult.Confirmations)
		} else {
			summary.Failed++
			fmt.Printf("FAIL %-52s %s\n", caseResult.Name, strings.Join(caseResult.Failures, "; "))
		}
	}
	summary.FinishedAt = time.Now().UTC()
	if *out != "" {
		encoded, err := json.MarshalIndent(summary, "", "  ")
		fatalIf(err)
		fatalIf(os.WriteFile(*out, append(encoded, '\n'), 0o600))
	}
	fmt.Printf("\nsummary: %d passed, %d failed\n", summary.Passed, summary.Failed)
	if summary.Failed != 0 {
		os.Exit(1)
	}
}

func selectCases(all []replayCase, raw string) ([]replayCase, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return all, nil
	}
	wanted := map[string]struct{}{}
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			wanted[name] = struct{}{}
		}
	}
	var selected []replayCase
	for _, testCase := range all {
		if _, ok := wanted[testCase.Name]; ok {
			selected = append(selected, testCase)
			delete(wanted, testCase.Name)
		}
	}
	if len(wanted) != 0 {
		unknown := make([]string, 0, len(wanted))
		for name := range wanted {
			unknown = append(unknown, name)
		}
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown replay cases: %s", strings.Join(unknown, ", "))
	}
	return selected, nil
}

func casesRequireInstance(cases []replayCase) bool {
	for _, testCase := range cases {
		if testCase.RequiresInstance {
			return true
		}
	}
	return false
}

func runCase(opts options, testCase replayCase, instanceID string) caseReport {
	result := caseReport{Name: testCase.Name, SourceRef: testCase.SourceRef, Passed: true, TurnCount: len(testCase.Turns)}
	if testCase.RequiresInstance && instanceID == "" {
		result.Passed = false
		result.Failures = []string{"test account has no discoverable instance"}
		return result
	}
	identity := identityFor(opts, testCase.IdentityMode)
	sessionID, err := createSession(opts, identity, "R3 "+testCase.Name)
	if err != nil {
		result.Passed = false
		result.Failures = []string{"create session: " + err.Error()}
		return result
	}
	for index, turn := range testCase.Turns {
		message := strings.ReplaceAll(turn.Message, "$INSTANCE", instanceID)
		turn.Expect = substituteExpectation(turn.Expect, instanceID)
		turnResult, err := executeTurn(opts, identity, sessionID, fmt.Sprintf("r3-%s-%d", testCase.Name, index+1), message, turn.Expect.Confirmation)
		if err != nil {
			result.Passed = false
			result.Failures = append(result.Failures, fmt.Sprintf("turn %d: %v", index+1, err))
			break
		}
		result.Actions = append(result.Actions, turnResult.Actions...)
		result.Confirmations += turnResult.Confirmations
		for _, failure := range checkExpectation(turn.Expect, turnResult) {
			result.Passed = false
			result.Failures = append(result.Failures, fmt.Sprintf("turn %d: %s", index+1, failure))
		}
	}
	if err := verifyCommittedHistory(opts, identity, sessionID, len(testCase.Turns)*2); err != nil {
		result.Passed = false
		result.Failures = append(result.Failures, "history: "+err.Error())
	}
	result.Actions = uniqueSorted(result.Actions)
	return result
}

type identity struct{ company, org, email string }

func identityFor(opts options, mode string) identity {
	if mode == "unbound" {
		return identity{company: "922337203685477000", org: "922337203685477001", email: "context-replay-unbound@example.invalid"}
	}
	return identity{company: opts.company, org: opts.org, email: opts.email}
}

func createSession(opts options, id identity, title string) (string, error) {
	company, org, err := numericIdentity(id)
	if err != nil {
		return "", err
	}
	body := map[string]any{"Action": "CreateCSAgentSession", "ProjectId": opts.project, "Title": title, "top_organization_id": company, "organization_id": org, "user_email": id.email}
	var response map[string]any
	if err := postJSON(opts.httpURL, body, &response); err != nil {
		return "", err
	}
	if ret, _ := response["RetCode"].(float64); ret != 0 {
		return "", fmt.Errorf("RetCode=%v Message=%v", response["RetCode"], response["Message"])
	}
	sessionID, _ := response["SessionId"].(string)
	if sessionID == "" {
		return "", errors.New("empty SessionId")
	}
	return sessionID, nil
}

func discoverInstance(opts options) (string, error) {
	id := identityFor(opts, "")
	sessionID, err := createSession(opts, id, "R3 discover test instance")
	if err != nil {
		return "", err
	}
	result, err := executeTurn(opts, id, sessionID, "r3-discover-instance", "我有哪些实例", "forbidden")
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile(`(?i)\b(?:uhost|cpod)-[a-z0-9]+\b`)
	match := re.FindString(result.Reply)
	if match == "" {
		return "", errors.New("resource query returned no instance id")
	}
	return match, nil
}

func executeTurn(opts options, id identity, sessionID, clientTurnID, message, confirmationMode string) (turnResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	wsURL := opts.gatewayWS
	direct := false
	if id.company != opts.company || id.org != opts.org {
		wsURL, direct = opts.directWS, true
	}
	conn, err := dial(ctx, wsURL, id, direct)
	if err != nil {
		return turnResult{}, err
	}
	defer conn.CloseNow()
	frame := map[string]any{
		"Action": "SendCSAgentChat", "ProtocolVersion": 2, "ProjectId": opts.project,
		"SessionId": sessionID, "ClientTurnId": clientTurnID, "Message": message,
		"Features": []string{"turn_replay_v2", "confirm_form_v1", "guided_create_v1"},
	}
	if err := writeFrame(ctx, conn, frame); err != nil {
		return turnResult{}, err
	}
	return observeTurn(ctx, opts, id, conn, sessionID, confirmationMode)
}

func observeTurn(ctx context.Context, opts options, id identity, conn *websocket.Conn, sessionID, confirmationMode string) (turnResult, error) {
	var result turnResult
	var tokens strings.Builder
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return result, fmt.Errorf("read before committed terminal: %w", err)
		}
		var frame map[string]any
		if err := json.Unmarshal(raw, &frame); err != nil {
			return result, err
		}
		event, _ := frame["event"].(string)
		if turnID, _ := frame["TurnId"].(string); turnID != "" {
			result.TurnID = turnID
		}
		if seq, ok := frame["Seq"].(float64); ok && int64(seq) > result.LastSeq {
			result.LastSeq = int64(seq)
		}
		switch event {
		case "token":
			if text, _ := frame["Text"].(string); text != "" {
				tokens.WriteString(text)
			}
		case "step":
			if action, _ := frame["Action"].(string); action != "" {
				result.Actions = append(result.Actions, action)
			}
		case "confirmation":
			result.Confirmations++
			if confirmationMode == "forbidden" {
				return result, errors.New("unexpected confirmation")
			}
			key, _ := frame["InteractionKey"].(string)
			if key == "" || result.TurnID == "" {
				return result, errors.New("confirmation missing durable identity")
			}
			if err := resolveConfirmation(ctx, opts, id, sessionID, result.TurnID, key, false); err != nil {
				return result, err
			}
		case "done":
			result.Reply, _ = frame["Content"].(string)
			if result.Reply == "" {
				result.Reply = tokens.String()
			}
			result.Committed = true
			return result, nil
		case "error":
			return result, fmt.Errorf("%v: %v", frame["Code"], frame["Message"])
		case "aborted":
			return result, errors.New("turn aborted")
		}
	}
}

func resolveConfirmation(ctx context.Context, opts options, id identity, sessionID, turnID, key string, confirmed bool) error {
	wsURL := opts.gatewayWS
	direct := false
	if id.company != opts.company || id.org != opts.org {
		wsURL, direct = opts.directWS, true
	}
	conn, err := dial(ctx, wsURL, id, direct)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	return writeFrame(ctx, conn, map[string]any{
		"Action": "ConfirmCSAgentAction", "ProtocolVersion": 2, "ProjectId": opts.project,
		"SessionId": sessionID, "TurnId": turnID, "InteractionKey": key, "Confirmed": confirmed,
	})
}

func dial(ctx context.Context, base string, id identity, direct bool) (*websocket.Conn, error) {
	separator := "?"
	if strings.Contains(base, "?") {
		separator = "&"
	}
	url := strings.TrimRight(base, "/") + "/" + separator + "Action=CreateCSAgentWS"
	headers := http.Header{}
	if direct {
		headers.Set("X-Company-Id", id.company)
		headers.Set("X-Organization-Id", id.org)
		headers.Set("X-User-Email", id.email)
		headers.Set("X-Request-Id", fmt.Sprintf("context-replay-%d", time.Now().UnixNano()))
	}
	conn, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return nil, fmt.Errorf("websocket dial status %d: %w", status, err)
	}
	return conn, nil
}

func verifyCommittedHistory(opts options, id identity, sessionID string, minimum int) error {
	company, org, err := numericIdentity(id)
	if err != nil {
		return err
	}
	body := map[string]any{"Action": "GetCSAgentSession", "ProjectId": opts.project, "SessionId": sessionID, "Limit": 100, "top_organization_id": company, "organization_id": org, "user_email": id.email}
	var response struct {
		RetCode  int `json:"RetCode"`
		Messages []struct {
			Role, Content, Status string
		} `json:"Messages"`
	}
	if err := postJSON(opts.httpURL, body, &response); err != nil {
		return err
	}
	if response.RetCode != 0 {
		return fmt.Errorf("RetCode=%d", response.RetCode)
	}
	if len(response.Messages) < minimum {
		return fmt.Errorf("got %d messages, want at least %d", len(response.Messages), minimum)
	}
	for _, message := range response.Messages {
		if message.Status != "ok" {
			return fmt.Errorf("message role=%s status=%s", message.Role, message.Status)
		}
	}
	return nil
}

func numericIdentity(id identity) (int64, int64, error) {
	company, err := strconv.ParseInt(id.company, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid top organization id: %w", err)
	}
	org, err := strconv.ParseInt(id.org, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid organization id: %w", err)
	}
	return company, org, nil
}

func checkExpectation(expect expectation, result turnResult) []string {
	var failures []string
	if len(expect.ReplyAny) > 0 && !matchesAny(result.Reply, expect.ReplyAny) {
		failures = append(failures, "reply matched none of reply_any")
	}
	for _, pattern := range expect.ReplyNone {
		if regexp.MustCompile("(?is)" + pattern).MatchString(result.Reply) {
			failures = append(failures, "reply matched forbidden pattern "+pattern)
		}
	}
	if len(expect.ActionAny) > 0 && !containsAny(result.Actions, expect.ActionAny) {
		failures = append(failures, "actions contained none of action_any")
	}
	for _, forbidden := range expect.ActionNone {
		if containsAny(result.Actions, []string{forbidden}) {
			failures = append(failures, "forbidden action "+forbidden)
		}
	}
	if expect.Confirmation == "forbidden" && result.Confirmations != 0 {
		failures = append(failures, "unexpected confirmation")
	}
	if !result.Committed {
		failures = append(failures, "turn was not committed")
	}
	return failures
}

func substituteExpectation(in expectation, instanceID string) expectation {
	out := in
	out.ReplyAny = append([]string(nil), in.ReplyAny...)
	for index := range out.ReplyAny {
		out.ReplyAny[index] = strings.ReplaceAll(out.ReplyAny[index], "$INSTANCE", regexp.QuoteMeta(instanceID))
	}
	return out
}

func matchesAny(value string, patterns []string) bool {
	for _, pattern := range patterns {
		if regexp.MustCompile("(?is)" + pattern).MatchString(value) {
			return true
		}
	}
	return false
}

func containsAny(values, expected []string) bool {
	for _, value := range values {
		for _, candidate := range expected {
			if strings.EqualFold(value, candidate) {
				return true
			}
		}
	}
	return false
}

func uniqueSorted(values []string) []string {
	seen := map[string]struct{}{}
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			seen[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func postJSON(url string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "text/plain")
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

func writeFrame(ctx context.Context, conn *websocket.Conn, frame any) error {
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, raw)
}

func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
