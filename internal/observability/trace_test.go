package observability

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriterAppendWritesOneJSONLinePerRecord(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	writer, err := NewWriter(WriterOptions{Dir: t.TempDir(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	for i := 1; i <= 2; i++ {
		if err := writer.Append(TraceRecord{TurnID: "turn", TurnIndex: i}); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}

	path := filepath.Join(writer.Dir(), "agent-trace-2026-05-08.jsonl")
	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), lines)
	}

	var first TraceRecord
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("trace line is not valid JSON: %v", err)
	}
	if first.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version = %q, want %q", first.SchemaVersion, SchemaVersion)
	}
	if first.Timestamp != now.Format(time.RFC3339) {
		t.Fatalf("timestamp = %q, want %q", first.Timestamp, now.Format(time.RFC3339))
	}
	if first.Planner.Slots.TargetRefs == nil || first.Planner.Slots.Metrics == nil {
		t.Fatalf("planner slots arrays must be present as empty arrays, got %#v", first.Planner.Slots)
	}
	if first.Renderer.InputToolCallIDs == nil || first.Renderer.InputToolArgHashes == nil {
		t.Fatalf("renderer input arrays must be present as empty arrays, got %#v", first.Renderer)
	}
}

func TestHashTracePayloadIsStableAcrossMapOrder(t *testing.T) {
	left := map[string]any{
		"b": "two",
		"a": map[string]any{"y": 2, "x": 1},
	}
	right := map[string]any{
		"a": map[string]any{"x": 1, "y": 2},
		"b": "two",
	}

	leftHash, err := HashTracePayload(left)
	if err != nil {
		t.Fatalf("HashTracePayload(left): %v", err)
	}
	rightHash, err := HashTracePayload(right)
	if err != nil {
		t.Fatalf("HashTracePayload(right): %v", err)
	}
	if leftHash != rightHash {
		t.Fatalf("hashes differ for same logical payload: %s vs %s", leftHash, rightHash)
	}
}

func TestHashTracePayloadRedactsBeforeHashing(t *testing.T) {
	payload := readJSONMap(t, filepath.Join("testdata", "secret_payload.json"))
	payload["Authorization"] = "Bearer " + strings.Repeat("a", 25)
	payload["Nested"] = map[string]any{"Authorization": "Bearer " + strings.Repeat("c", 25)}

	canonical, err := canonicalTraceJSON(payload)
	if err != nil {
		t.Fatalf("canonicalTraceJSON: %v", err)
	}
	canonicalText := string(canonical)
	for _, secret := range []string{
		"pk",
		"sk",
		strings.Repeat("a", 25),
		strings.Repeat("c", 25),
		"pw",
		"npw",
		"123.45",
		"10.11.12.13",
	} {
		if strings.Contains(canonicalText, secret) {
			t.Fatalf("canonical trace JSON leaked %q: %s", secret, canonicalText)
		}
	}
	for _, public := range []string{
		`"Action":"DescribeCompShareInstance"`,
		`"Limit":10`,
	} {
		if !strings.Contains(canonicalText, public) {
			t.Fatalf("canonical trace JSON over-redacted public field %q: %s", public, canonicalText)
		}
	}

	hash, err := HashTracePayload(payload)
	if err != nil {
		t.Fatalf("HashTracePayload: %v", err)
	}
	if !strings.HasPrefix(hash, "sha256:") {
		t.Fatalf("hash = %q, want sha256 prefix", hash)
	}
}

func TestWriterAppendDoesNotLeakSecretsInTraceLine(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := NewWriter(WriterOptions{Dir: t.TempDir(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	args := map[string]any{
		"Password":      "Password123!",
		"PrivateKey":    "private-key-value",
		"Balance":       "998.76",
		"PublicIP":      "192.168.1.2",
		"Authorization": "Bearer " + strings.Repeat("b", 25),
	}
	argsHash, err := HashTracePayload(args)
	if err != nil {
		t.Fatalf("HashTracePayload(args): %v", err)
	}
	resultHash, err := HashTracePayload(map[string]any{"JupyterToken": "jupyter-token-value"})
	if err != nil {
		t.Fatalf("HashTracePayload(result): %v", err)
	}

	err = writer.Append(TraceRecord{
		TraceID:     "trace-1",
		TurnID:      "turn-1",
		TurnIndex:   1,
		UserMsgHash: "sha256:user-message",
		ToolCalls: []ToolCallTrace{{
			ID:         "call-1",
			TurnIndex:  1,
			Action:     "DescribeCompShareJupyterToken",
			Source:     ToolSourceMainReAct,
			ArgsHash:   argsHash,
			Status:     ToolStatusSuccess,
			ResultHash: resultHash,
		}},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	line := readLines(t, filepath.Join(writer.Dir(), "agent-trace-2026-05-08.jsonl"))[0]
	for _, secret := range []string{"Password123!", "private-key-value", "998.76", "192.168.1.2", "jupyter-token-value", strings.Repeat("b", 25)} {
		if strings.Contains(line, secret) {
			t.Fatalf("trace line leaked %q: %s", secret, line)
		}
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open trace file: %v", err)
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan trace file: %v", err)
	}
	return lines
}

func readJSONMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return payload
}
