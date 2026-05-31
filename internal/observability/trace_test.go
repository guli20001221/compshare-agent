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
	for _, emptyBlock := range []string{`"planner":`, `"rate_limit":`, `"retrieval":`, `"renderer":`, `"outcome":`} {
		if strings.Contains(lines[0], emptyBlock) {
			t.Fatalf("minimal trace line should omit empty optional block %s: %s", emptyBlock, lines[0])
		}
	}
	if first.RateLimit.Checked || first.RateLimit.Allowed || first.RateLimit.Class != "" || first.RateLimit.RetryAfterMS != 0 {
		t.Fatalf("default rate limit trace = %#v, want zero values", first.RateLimit)
	}
}

func TestSparseTraceRecordMissingOptionalBlocksStillReadable(t *testing.T) {
	data := []byte(`{"schema_version":"trace.v0.2","trace_id":"trace-sparse","turn_id":"turn-1","turn_index":1,"timestamp":"2026-05-08T12:00:00Z","user_msg_hash":"sha256:user"}`)
	var record TraceRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("unmarshal sparse v0.2 trace: %v", err)
	}
	if record.SchemaVersion != "trace.v0.2" || record.TraceID != "trace-sparse" {
		t.Fatalf("sparse trace identity = %#v", record)
	}
	if record.Planner.Enabled || record.RateLimit.Checked || record.Retrieval.Enabled || record.Outcome.TotalTokens != 0 {
		t.Fatalf("sparse optional fields should read as zero values: planner=%#v rate=%#v retrieval=%#v outcome=%#v", record.Planner, record.RateLimit, record.Retrieval, record.Outcome)
	}
}

func TestSchemaVersionIsV04(t *testing.T) {
	if SchemaVersion != "trace.v0.4" {
		t.Fatalf("SchemaVersion = %q, want trace.v0.4", SchemaVersion)
	}
}

func TestRetrievalTraceV03FieldsMarshal(t *testing.T) {
	trace := RetrievalTrace{
		Enabled:         true,
		KBVersion:       "kb.stage2b.w0.2026-05-14",
		QueryRaw:        "实例一直卡初始化怎么办",
		QueryNormalized: "实例 初始化失败",
		QueryExpansions: []string{"实例启动失败", "卡初始化"},
		Hits:            2,
		HitItems: []RetrievalHit{
			{ChunkID: "w0-init_failure-error-code-a1b2c3d4", Score: 0.78, Kept: true},
			{ChunkID: "w0-billing_rule-arrears-aabbccdd", Score: 0.41, Kept: false},
		},
		RefusedReason:        "weak_evidence",
		WeakEvidence:         true,
		HybridMode:           "bm25_fallback",
		HybridFallbackReason: "embedding_timeout",
		EmbeddingLatencyMS:   int64Ptr(4987),
	}

	data, err := json.Marshal(trace)
	if err != nil {
		t.Fatalf("marshal RetrievalTrace: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`"enabled":true`,
		`"kb_version":"kb.stage2b.w0.2026-05-14"`,
		`"query_raw":"实例一直卡初始化怎么办"`,
		`"query_normalized":"实例 初始化失败"`,
		`"query_expansions":["实例启动失败","卡初始化"]`,
		`"hits":2`,
		`"hit_items":[{"chunk_id":"w0-init_failure-error-code-a1b2c3d4","score":0.78,"kept":true},{"chunk_id":"w0-billing_rule-arrears-aabbccdd","score":0.41,"kept":false}]`,
		`"refused_reason":"weak_evidence"`,
		`"weak_evidence":true`,
		`"hybrid_mode":"bm25_fallback"`,
		`"hybrid_fallback_reason":"embedding_timeout"`,
		`"embedding_latency_ms":4987`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("retrieval trace JSON missing %s: %s", want, text)
		}
	}
}

// HybridMode and HybridFallbackReason must omit from JSON when empty so old
// traces and the retrieval-disabled path don't suddenly grow new keys.
// EmbeddingLatencyMS (*int64) must omit when nil so bm25_only / empty-pool
// paths stay clean. A *0 (real 0ms) would still emit — that's the whole
// point of using *int64 instead of int64+omitempty.
func TestRetrievalTraceHybridFieldsOmitEmpty(t *testing.T) {
	trace := RetrievalTrace{
		Enabled:   true,
		KBVersion: "kb.stage2b.w0.2026-05-14",
		Hits:      0,
	}
	data, err := json.Marshal(trace)
	if err != nil {
		t.Fatalf("marshal RetrievalTrace: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "hybrid_mode") {
		t.Fatalf("empty HybridMode must omit from JSON: %s", text)
	}
	if strings.Contains(text, "hybrid_fallback_reason") {
		t.Fatalf("empty HybridFallbackReason must omit from JSON: %s", text)
	}
	if strings.Contains(text, "embedding_latency_ms") {
		t.Fatalf("nil EmbeddingLatencyMS must omit from JSON: %s", text)
	}
}

// A pointer to 0 (real 0ms round-trip — reserved for future client-side
// cache hits) must NOT be omitted. Distinguishing nil ("never invoked")
// from *0 ("invoked, <1ms") is the load-bearing reason this field is
// *int64 instead of int64+omitempty.
func TestRetrievalTraceEmbeddingLatencyZeroIsNotOmitted(t *testing.T) {
	trace := RetrievalTrace{
		Enabled:            true,
		KBVersion:          "kb.test",
		EmbeddingLatencyMS: int64Ptr(0),
	}
	data, err := json.Marshal(trace)
	if err != nil {
		t.Fatalf("marshal RetrievalTrace: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, `"embedding_latency_ms":0`) {
		t.Fatalf("*0 EmbeddingLatencyMS must serialize as 0, got: %s", text)
	}
}

func int64Ptr(v int64) *int64 { return &v }

// HybridMode alone must be enough to mark the retrieval block as observed —
// otherwise a bm25_only retriever's trace would be silently dropped from the
// trace record (and ops would lose visibility into "retriever ran but had
// nothing to surface").
func TestRetrievalTraceObservedByHybridModeAlone(t *testing.T) {
	for _, mode := range []string{"bm25_only", "hybrid_cosine", "bm25_fallback"} {
		t.Run(mode, func(t *testing.T) {
			trace := RetrievalTrace{HybridMode: mode}
			if !traceRetrievalObserved(trace) {
				t.Fatalf("traceRetrievalObserved(HybridMode=%q only) = false, want true", mode)
			}
		})
	}
	t.Run("fallback_reason_only", func(t *testing.T) {
		trace := RetrievalTrace{HybridFallbackReason: "embedding_timeout"}
		if !traceRetrievalObserved(trace) {
			t.Fatal("traceRetrievalObserved(HybridFallbackReason only) = false, want true")
		}
	})
	t.Run("embedding_latency_only", func(t *testing.T) {
		trace := RetrievalTrace{EmbeddingLatencyMS: int64Ptr(1234)}
		if !traceRetrievalObserved(trace) {
			t.Fatal("traceRetrievalObserved(EmbeddingLatencyMS only) = false, want true")
		}
	})
	t.Run("embedding_latency_pointer_to_zero", func(t *testing.T) {
		// *0 (invoked but <1ms) must still mark the block as observed,
		// otherwise future client-cache hits would silently lose the
		// retrieval trace.
		trace := RetrievalTrace{EmbeddingLatencyMS: int64Ptr(0)}
		if !traceRetrievalObserved(trace) {
			t.Fatal("traceRetrievalObserved(EmbeddingLatencyMS=*0) = false, want true")
		}
	})
}

func TestRetrievalHitScoreZeroMarshalsAsZero(t *testing.T) {
	hit := RetrievalHit{
		ChunkID: "w0-init_failure-error-code-a1b2c3d4",
		Score:   0,
		Kept:    true,
	}

	data, err := json.Marshal(hit)
	if err != nil {
		t.Fatalf("marshal RetrievalHit: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`"chunk_id":"w0-init_failure-error-code-a1b2c3d4"`,
		`"score":0`,
		`"kept":true`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("retrieval hit JSON missing %s: %s", want, text)
		}
	}
}

func TestRetrievalTraceNewFieldsMarkBlockObserved(t *testing.T) {
	cases := []struct {
		name  string
		trace RetrievalTrace
	}{
		{name: "query raw", trace: RetrievalTrace{QueryRaw: "怎么收费"}},
		{name: "query normalized", trace: RetrievalTrace{QueryNormalized: "计费 规则"}},
		{name: "query expansions", trace: RetrievalTrace{QueryExpansions: []string{"扣费规则"}}},
		{name: "hit items", trace: RetrievalTrace{HitItems: []RetrievalHit{{ChunkID: "w0-billing_rule-aabbccdd", Score: 0.67, Kept: true}}}},
		{name: "refused reason", trace: RetrievalTrace{RefusedReason: "no_evidence"}},
		{name: "weak evidence", trace: RetrievalTrace{WeakEvidence: true}},
		{name: "ranking error candidate", trace: RetrievalTrace{RankingErrorCandidate: true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !traceRetrievalObserved(tc.trace) {
				t.Fatalf("traceRetrievalObserved(%#v) = false, want true", tc.trace)
			}
			data, err := json.Marshal(TraceRecord{
				SchemaVersion: SchemaVersion,
				TraceID:       "trace-1",
				TurnID:        "turn-1",
				TurnIndex:     1,
				Timestamp:     "2026-05-14T00:00:00Z",
				UserMsgHash:   "sha256:user",
				Retrieval:     tc.trace,
			})
			if err != nil {
				t.Fatalf("marshal TraceRecord: %v", err)
			}
			if !strings.Contains(string(data), `"retrieval":`) {
				t.Fatalf("trace record should include retrieval for %s: %s", tc.name, data)
			}
		})
	}
}

func TestRetrievalTraceDefaultsKeepSlicesIterable(t *testing.T) {
	record := TraceRecord{}.withDefaults(time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC))
	if record.Retrieval.QueryExpansions == nil {
		t.Fatalf("QueryExpansions default = nil, want empty slice")
	}
	if record.Retrieval.HitItems == nil {
		t.Fatalf("HitItems default = nil, want empty slice")
	}
	if len(record.Retrieval.QueryExpansions) != 0 || len(record.Retrieval.HitItems) != 0 {
		t.Fatalf("retrieval defaults should be empty: %#v", record.Retrieval)
	}
}

func TestWriterMirrorsRankingErrorCandidates(t *testing.T) {
	now := time.Date(2026, 5, 15, 8, 0, 0, 0, time.UTC)
	writer, err := NewWriter(WriterOptions{Dir: t.TempDir(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	err = writer.Append(TraceRecord{
		TraceID:     "trace-ranking",
		TurnID:      "turn-1",
		TurnIndex:   1,
		UserMsgHash: "sha256:user",
		Retrieval: RetrievalTrace{
			Enabled:               true,
			RankingErrorCandidate: true,
			RefusedReason:         "retry_no_cite",
		},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	line := readLines(t, filepath.Join(writer.Dir(), "2026-05-15", "ranking-error-candidates.jsonl"))[0]
	if !strings.Contains(line, `"ranking_error_candidate":true`) || !strings.Contains(line, `"retry_no_cite"`) {
		t.Fatalf("ranking-error mirror line = %s", line)
	}
}

func TestRetrievalTraceHashingStableUnderNilVsEmpty(t *testing.T) {
	nilSlices := RetrievalTrace{}
	emptySlices := RetrievalTrace{
		QueryExpansions: []string{},
		HitItems:        []RetrievalHit{},
	}

	leftHash, err := HashTracePayload(nilSlices)
	if err != nil {
		t.Fatalf("HashTracePayload(nilSlices): %v", err)
	}
	rightHash, err := HashTracePayload(emptySlices)
	if err != nil {
		t.Fatalf("HashTracePayload(emptySlices): %v", err)
	}
	if leftHash != rightHash {
		t.Fatalf("nil and empty retrieval slices should hash the same: %s vs %s", leftHash, rightHash)
	}
}

func TestRedactQueryDerivedFieldsRedactsStaffNames(t *testing.T) {
	trace := RetrievalTrace{
		QueryRaw:        "请张慧帮我看一下实例启动失败",
		QueryNormalized: "张慧 实例 启动失败",
		QueryExpansions: []string{"实例启动失败", "找张慧处理"},
	}

	RedactQueryDerivedFields(&trace)

	if trace.QueryRaw != "[REDACTED]" || trace.QueryNormalized != "[REDACTED]" {
		t.Fatalf("query fields not redacted: %#v", trace)
	}
	if got := strings.Join(trace.QueryExpansions, "|"); got != "实例启动失败|[REDACTED]" {
		t.Fatalf("query expansions = %q", got)
	}
}

func TestOutcomeTraceOmitsUnavailableCounters(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := NewWriter(WriterOptions{Dir: t.TempDir(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := writer.Append(TraceRecord{
		TraceID:     "trace-outcome",
		TurnID:      "turn-1",
		TurnIndex:   1,
		UserMsgHash: "sha256:user",
		Outcome: OutcomeTrace{
			TotalLatencyMS: 123,
		},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	line := readLines(t, filepath.Join(writer.Dir(), "agent-trace-2026-05-08.jsonl"))[0]
	if !strings.Contains(line, `"outcome":{"total_latency_ms":123}`) {
		t.Fatalf("outcome latency missing from trace line: %s", line)
	}
	for _, unavailable := range []string{
		`"total_tokens":`,
		`"attempted_hallucinated_count":`,
		`"escaped_hallucinated_count":`,
		`"kb_conflict_count":`,
	} {
		if strings.Contains(line, unavailable) {
			t.Fatalf("trace line should omit unavailable outcome field %s: %s", unavailable, line)
		}
	}
}

func TestTraceV01FixtureStillReadable(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "trace_v0_1_minimal.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var record TraceRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("unmarshal v0.1 fixture: %v", err)
	}
	if record.SchemaVersion != "trace.v0.1" || record.ToolCalls[0].Action != "DescribeCompShareInstance" {
		t.Fatalf("v0.1 fixture read as %#v", record)
	}
	if record.ToolCalls[0].Capped != "" || record.ToolCalls[0].RequestedTargets != 0 {
		t.Fatalf("missing v0.2 cap fields should read as zero values: %#v", record.ToolCalls[0])
	}
}

func TestTraceV02FixtureIncludesRuntimeAndCapFields(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "trace_v0_2_cap_fields.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var record TraceRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("unmarshal v0.2 fixture: %v", err)
	}
	if record.SchemaVersion != "trace.v0.2" || record.Runtime.PlannerMode != "shadow" ||
		len(record.Runtime.CutoverIntents) != 1 || record.Runtime.CutoverIntents[0] != "monitor" {
		t.Fatalf("runtime fixture = %#v schema=%q", record.Runtime, record.SchemaVersion)
	}
	call := record.ToolCalls[0]
	if call.Capped != ToolCappedTargets || call.RequestedTargets != 21 || call.ExecutedTargets != 0 ||
		call.WindowSeconds != 3600 || call.CapReason == "" {
		t.Fatalf("cap fields = %#v", call)
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

func TestCleanupDeletesOnlyExpiredTraceFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 8, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(dir, "agent-trace-2026-04-07.jsonl")) // 31 days old: delete
	writeFile(t, filepath.Join(dir, "agent-trace-2026-04-08.jsonl")) // exactly 30 days: keep
	writeFile(t, filepath.Join(dir, "agent-trace-2026-05-08.jsonl")) // current: keep
	writeFile(t, filepath.Join(dir, "agent-trace-2026-04-01.txt"))   // wrong suffix: keep
	writeFile(t, filepath.Join(dir, "not-agent-trace-2026-04-01.jsonl"))
	if err := os.Mkdir(filepath.Join(dir, "agent-trace-2026-01-01.jsonl"), 0o700); err != nil {
		t.Fatalf("mkdir trace-like dir: %v", err)
	}

	if err := Cleanup(dir, 30, now); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	assertPathMissing(t, filepath.Join(dir, "agent-trace-2026-04-07.jsonl"))
	for _, name := range []string{
		"agent-trace-2026-04-08.jsonl",
		"agent-trace-2026-05-08.jsonl",
		"agent-trace-2026-04-01.txt",
		"not-agent-trace-2026-04-01.jsonl",
		"agent-trace-2026-01-01.jsonl",
	} {
		assertPathExists(t, filepath.Join(dir, name))
	}
}

func TestCleanupUsesDefaultRetentionForNonPositiveDays(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 8, 10, 0, 0, 0, time.UTC)
	writeFile(t, filepath.Join(dir, "agent-trace-2026-04-07.jsonl"))

	if err := Cleanup(dir, 0, now); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	assertPathMissing(t, filepath.Join(dir, "agent-trace-2026-04-07.jsonl"))
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

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertPathExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("expected %s to be deleted", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
	}
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

// TaskTier (ADR-002 reserved schema slot) must serialize when populated
// and stay absent when empty (omitempty). The "empty" branch protects
// legacy consumers parsing pre-ADR-002 traces from seeing an unexpected
// "task_tier":"" key. Populator is B2-B4 territory; this test only
// covers the schema contract added in B1.
func TestTraceRecord_TaskTier_Serialization(t *testing.T) {
	base := TraceRecord{
		SchemaVersion: SchemaVersion,
		TraceID:       "trace-1",
		TurnID:        "turn-1",
		TurnIndex:     1,
		Timestamp:     "2026-05-29T00:00:00Z",
		UserMsgHash:   "sha256:user",
	}

	t.Run("empty TaskTier is omitted", func(t *testing.T) {
		data, err := json.Marshal(base)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(data), `"task_tier"`) {
			t.Fatalf("empty TaskTier should be omitted, got: %s", data)
		}
	})

	t.Run("populated TaskTier appears in JSON", func(t *testing.T) {
		rec := base
		rec.TaskTier = "knowledge"
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(data), `"task_tier":"knowledge"`) {
			t.Fatalf("populated TaskTier should serialize, got: %s", data)
		}
	})
}

// RealizedTier (B4a derived dispatch tier) must serialize when populated and
// stay absent when empty (omitempty), same contract as TaskTier. The "empty"
// branch protects consumers parsing pre-B4a traces from an unexpected
// "realized_tier":"" key, and is also the on-the-wire encoding of "tier not
// observable for this turn" (attribution-observable-only).
func TestTraceRecord_RealizedTier_Serialization(t *testing.T) {
	base := TraceRecord{
		SchemaVersion: SchemaVersion,
		TraceID:       "trace-1",
		TurnID:        "turn-1",
		TurnIndex:     1,
		Timestamp:     "2026-05-29T00:00:00Z",
		UserMsgHash:   "sha256:user",
	}

	t.Run("empty RealizedTier is omitted", func(t *testing.T) {
		data, err := json.Marshal(base)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(data), `"realized_tier"`) {
			t.Fatalf("empty RealizedTier should be omitted, got: %s", data)
		}
	})

	t.Run("populated RealizedTier appears in JSON", func(t *testing.T) {
		rec := base
		rec.RealizedTier = RealizedTierAgent
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(data), `"realized_tier":"agent"`) {
			t.Fatalf("populated RealizedTier should serialize, got: %s", data)
		}
	})
}

// TestDeriveRealizedTier pins the priority-ordered derivation. The cases that
// matter for correctness (not just coverage): a retrieval *fallback* that
// continued into ReAct must read as agent (the path that actually ran), NOT
// knowledge by status name; and a turn with no observable dispatch signal must
// read as "" (unknown), NOT default-to-agent — otherwise hard-block/canned
// refusals would inflate the agent traffic share.
//
// NOTE: this table hardcodes the cutover_status string literals because
// observability cannot import internal/intent (intent imports observability —
// import cycle). It therefore tests the derivation ALGORITHM, not the binding to
// the real enum VALUES. The value binding (which fails if a CutoverStatus
// constant is renamed in handler.go) lives in intent.TestCutoverStatusBindsToRealizedTier,
// which can reference the constants directly. Both must be kept in sync when the
// enum changes (internal/intent/handler.go:42-58).
func TestDeriveRealizedTier(t *testing.T) {
	reactCall := []ToolCallTrace{{Source: ToolSourceMainReAct}}
	knowledgeCall := []ToolCallTrace{{Source: ToolSourceKnowledgeLocal}}
	cases := []struct {
		name   string
		record TraceRecord
		want   string
	}{
		{"cutover dispatched -> fast",
			TraceRecord{Planner: PlannerTrace{CutoverStatus: "dispatched"}}, RealizedTierFast},
		{"cutover selection_required -> fast (clarify prompt, no ReAct)",
			TraceRecord{Planner: PlannerTrace{CutoverStatus: "selection_required"}}, RealizedTierFast},
		{"cutover dispatched_retrieval -> knowledge",
			TraceRecord{Planner: PlannerTrace{CutoverStatus: "dispatched_retrieval"}}, RealizedTierKnowledge},
		{"cutover dispatched_agent -> agent (B8.3 deploy_model arm)",
			TraceRecord{Planner: PlannerTrace{CutoverStatus: "dispatched_agent"}}, RealizedTierAgent},
		{"no cutover but retrieval hits -> knowledge",
			TraceRecord{Retrieval: RetrievalTrace{Enabled: true, Hits: 2}}, RealizedTierKnowledge},
		{"main_react tool fired -> agent",
			TraceRecord{ToolCalls: reactCall}, RealizedTierAgent},
		{"retrieval enabled but 0 hits, ReAct ran -> agent",
			TraceRecord{Retrieval: RetrievalTrace{Enabled: true, Hits: 0}, ToolCalls: reactCall}, RealizedTierAgent},
		{"retrieval fallback continued into ReAct -> agent (path that ran, not status name)",
			TraceRecord{Planner: PlannerTrace{CutoverStatus: "fallback_retrieval_miss"}, ToolCalls: reactCall}, RealizedTierAgent},
		{"low-confidence fallback into ReAct -> agent",
			TraceRecord{Planner: PlannerTrace{CutoverStatus: "fallback_low_confidence"}, ToolCalls: reactCall}, RealizedTierAgent},
		{"failure_after_tool but retrieval hits present -> knowledge",
			TraceRecord{Planner: PlannerTrace{CutoverStatus: "failure_after_tool"}, Retrieval: RetrievalTrace{Enabled: true, Hits: 1}}, RealizedTierKnowledge},
		{"no observable signal -> unknown (not default-agent)",
			TraceRecord{}, ""},
		{"hard-block canned reply, no dispatch signal -> unknown",
			TraceRecord{EngineHardBlock: EngineHardBlockTrace{Hit: true, Category: "account_billing"}}, ""},
		{"knowledge_local tool alone (no cutover, no hits) -> unknown",
			TraceRecord{ToolCalls: knowledgeCall}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.record.DeriveRealizedTier(); got != tc.want {
				t.Fatalf("DeriveRealizedTier() = %q, want %q", got, tc.want)
			}
		})
	}
}
