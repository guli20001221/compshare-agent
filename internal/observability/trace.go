package observability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/compshare-agent/internal/policy"
	"github.com/compshare-agent/internal/security"
)

const SchemaVersion = "trace.v0.3"

const (
	ToolSourceMainReAct         = "main_react"
	ToolSourceWorkflowInternal  = "workflow_internal"
	ToolSourceDiagnosisInternal = "diagnosis_internal"
	ToolSourceKnowledgeLocal    = "knowledge_local"
	ToolSourceInitContext       = "init_context"
	ToolSourceShadowOnly        = "shadow_only"
	ToolSourcePlannerHandler    = "planner_handler"
)

const (
	ToolStatusSuccess = "success"
	ToolStatusError   = "error"
)

const (
	ToolCappedTargets   = "targets"
	ToolCappedWindow    = "window"
	ToolCappedRateLimit = "rate_limit"
)

const DefaultTraceDir = "logs"

const DefaultTraceFilePerm os.FileMode = 0o600

const DefaultTraceRetentionDays = 30

type WriterOptions struct {
	Dir string
	Now func() time.Time
}

// Writer is the trace sink abstraction. Implementations:
//   - FileWriter: append JSONL files under <dir>/<date>.jsonl (CLI default,
//     unchanged behavior).
//   - MySQLWriter: insert rows into agent_traces (server default, A4).
//
// The method name "Append" (not "Write") is deliberate: it matches the
// pre-A4 *FileWriter.Append signature so existing cmd/trace.go call sites
// (e.g. cliTraceRecorder.writer.Append at cmd/trace.go) work unchanged
// after the type-of-variable swap from *FileWriter to Writer.
//
// Dir() returns the on-disk root for file-backed implementations. Backends
// without an on-disk dir (MySQLWriter) return "" so the existing trace-dir
// cleanup logic can skip them.
//
// Close is invoked at process shutdown so MySQLWriter can drain its buffered
// queue. FileWriter has no long-lived resources and returns nil immediately.
type Writer interface {
	Append(record TraceRecord) error
	Dir() string
	Close(ctx context.Context) error
}

// FileWriter is the JSONL-on-disk implementation of Writer. Used by the CLI
// path and any environment that wants a local audit trail. Server path
// typically chooses MySQLWriter; see cmd/trace.go for the env-driven choice.
type FileWriter struct {
	dir string
	now func() time.Time
}

type TraceRecord struct {
	SchemaVersion string `json:"schema_version"`
	TraceID       string `json:"trace_id"`
	TurnID        string `json:"turn_id"`
	TurnIndex     int    `json:"turn_index"`
	Timestamp     string `json:"timestamp"`
	UserMsgHash   string `json:"user_msg_hash"`
	// TaskTier is the ADR-001 task-complexity tier (fast / knowledge /
	// agent) selected by the planner for this turn. Empty when planner is
	// disabled or pre-ADR-002. B2-B4 will populate it from planner output;
	// the field is added in B1 as a reserved schema slot so consumers
	// (analytics SQL, dashboards) can start treating its presence as the
	// signal to switch from legacy per-turn rows to tier-aware aggregation.
	// Memory: attribution-observable-only — empty means "tier not known
	// for this turn", never default-to-agent.
	TaskTier        string               `json:"task_tier,omitempty"`
	Runtime         RuntimeTrace         `json:"runtime"`
	Planner         PlannerTrace         `json:"planner"`
	EngineHardBlock EngineHardBlockTrace `json:"engine_hard_block"`
	EntityRegistry  EntityRegistryTrace  `json:"entity_registry"`
	ToolCalls       []ToolCallTrace      `json:"tool_calls"`
	Renderer        RendererTrace        `json:"renderer"`
	Freshness       FreshnessTrace       `json:"freshness"`
	RateLimit       RateLimitTrace       `json:"rate_limit"`
	Retrieval       RetrievalTrace       `json:"retrieval"`
	Outcome         OutcomeTrace         `json:"outcome"`
}

type traceRecordJSON struct {
	SchemaVersion   string                `json:"schema_version"`
	TraceID         string                `json:"trace_id"`
	TurnID          string                `json:"turn_id"`
	TurnIndex       int                   `json:"turn_index"`
	Timestamp       string                `json:"timestamp"`
	UserMsgHash     string                `json:"user_msg_hash"`
	TaskTier        string                `json:"task_tier,omitempty"`
	Runtime         *RuntimeTrace         `json:"runtime,omitempty"`
	Planner         *PlannerTrace         `json:"planner,omitempty"`
	EngineHardBlock *EngineHardBlockTrace `json:"engine_hard_block,omitempty"`
	EntityRegistry  *EntityRegistryTrace  `json:"entity_registry,omitempty"`
	ToolCalls       []ToolCallTrace       `json:"tool_calls,omitempty"`
	Renderer        *RendererTrace        `json:"renderer,omitempty"`
	Freshness       *FreshnessTrace       `json:"freshness,omitempty"`
	RateLimit       *RateLimitTrace       `json:"rate_limit,omitempty"`
	Retrieval       *RetrievalTrace       `json:"retrieval,omitempty"`
	Outcome         *OutcomeTrace         `json:"outcome,omitempty"`
}

func (r TraceRecord) MarshalJSON() ([]byte, error) {
	out := traceRecordJSON{
		SchemaVersion: r.SchemaVersion,
		TraceID:       r.TraceID,
		TurnID:        r.TurnID,
		TurnIndex:     r.TurnIndex,
		Timestamp:     r.Timestamp,
		UserMsgHash:   r.UserMsgHash,
		TaskTier:      r.TaskTier,
	}
	if traceRuntimeObserved(r.Runtime) {
		out.Runtime = &r.Runtime
	}
	if tracePlannerObserved(r.Planner) {
		out.Planner = &r.Planner
	}
	if traceEngineHardBlockObserved(r.EngineHardBlock) {
		out.EngineHardBlock = &r.EngineHardBlock
	}
	if traceEntityRegistryObserved(r.EntityRegistry) {
		out.EntityRegistry = &r.EntityRegistry
	}
	if len(r.ToolCalls) > 0 {
		out.ToolCalls = r.ToolCalls
	}
	if traceRendererObserved(r.Renderer) {
		out.Renderer = &r.Renderer
	}
	if r.Freshness.MonitorCallInCurrentTurn {
		out.Freshness = &r.Freshness
	}
	if traceRateLimitObserved(r.RateLimit) {
		out.RateLimit = &r.RateLimit
	}
	if traceRetrievalObserved(r.Retrieval) {
		out.Retrieval = &r.Retrieval
	}
	if traceOutcomeObserved(r.Outcome) {
		out.Outcome = &r.Outcome
	}
	return json.Marshal(out)
}

type RuntimeTrace struct {
	PlannerMode    string   `json:"planner_mode"`
	CutoverIntents []string `json:"cutover_intents"`
}

type PlannerTrace struct {
	Enabled       bool         `json:"enabled"`
	Model         string       `json:"model"`
	LatencyMS     int64        `json:"latency_ms"`
	InputTokens   int          `json:"input_tokens"`
	OutputTokens  int          `json:"output_tokens"`
	SchemaValid   bool         `json:"schema_valid"`
	Intent        string       `json:"intent"`
	Slots         PlannerSlots `json:"slots"`
	Confidence    float64      `json:"confidence"`
	HardBlockHint bool         `json:"hard_block_hint"`
	CutoverStatus string       `json:"cutover_status"`
}

type PlannerSlots struct {
	TargetRefs []any    `json:"target_refs"`
	Metrics    []string `json:"metrics"`
	TimeWindow any      `json:"time_window"`
}

// EngineHardBlock TriggeredBy enum values. Single-source attribution
// (no "both") — see EngineHardBlockTrace.TriggeredBy doc.
const (
	HardBlockTriggerKeyword       = "keyword"
	HardBlockTriggerPlannerIntent = "planner_intent"
	HardBlockTriggerPostLLM       = "post_llm"
	HardBlockTriggerTokenBudget   = "token_budget"
)

type EngineHardBlockTrace struct {
	Hit      bool   `json:"hit"`
	Category string `json:"category"`
	// TriggeredBy records the actually-executed stage that produced the
	// hard-block — single-source (no "both"), since short-circuited stages
	// are unobservable. Allowed values:
	//   "keyword"        — Chat() head router.PreBlock keyword match
	//   "planner_intent" — planner-classified intent (monitor_history etc.)
	//                      routed through emitMonitorHistoryHardBlock helper
	//   "post_llm"       — post-LLM gate (currently cited_contract_violation)
	// Empty when Hit=false. Joins with planner.hard_block_hint downstream
	// for cross-source analytics — the join is observability, not routing.
	TriggeredBy string `json:"triggered_by,omitempty"`
}

type EntityRegistryTrace struct {
	SnapshotID string `json:"snapshot_id"`
	AgeSeconds int64  `json:"age_seconds"`
	SyncEvent  string `json:"sync_event"`
}

type ToolCallTrace struct {
	ID               string `json:"id"`
	TurnIndex        int    `json:"turn_index"`
	Action           string `json:"action"`
	Source           string `json:"source"`
	ArgsHash         string `json:"args_hash"`
	LatencyMS        int64  `json:"latency_ms"`
	Attempts         int    `json:"attempts"`
	Status           string `json:"status"`
	ErrorClass       string `json:"error_class"`
	ResultHash       string `json:"result_hash"`
	Capped           string `json:"capped"`
	CapReason        string `json:"cap_reason"`
	RequestedTargets int    `json:"requested_targets"`
	ExecutedTargets  int    `json:"executed_targets"`
	WindowSeconds    int    `json:"window_seconds"`
}

type RendererTrace struct {
	Enabled             bool     `json:"enabled"`
	Status              string   `json:"status"`
	EnvelopeKind        string   `json:"envelope_kind"`
	InputEnvelopeHashes []string `json:"input_envelope_hashes"`
	FallbackUsed        bool     `json:"fallback_used"`
	FallbackReason      string   `json:"fallback_reason"`
	Model               string   `json:"model"`
	LatencyMS           int64    `json:"latency_ms"`
	AttributionMode     string   `json:"attribution_mode"`
	InputToolCallIDs    []string `json:"input_tool_call_ids"`
	InputToolArgHashes  []string `json:"input_tool_args_hashes"`
}

type FreshnessTrace struct {
	MonitorCallInCurrentTurn bool `json:"monitor_call_in_current_turn"`
}

type RateLimitTrace struct {
	Checked      bool   `json:"checked"`
	Allowed      bool   `json:"allowed"`
	Class        string `json:"class"`
	Action       string `json:"action"`
	Reason       string `json:"reason"`
	SubjectHash  string `json:"subject_hash"`
	RetryAfterMS int64  `json:"retry_after_ms"`
}

type RetrievalTrace struct {
	Enabled               bool           `json:"enabled"`
	KBVersion             string         `json:"kb_version"`
	QueryRaw              string         `json:"query_raw,omitempty"`
	QueryNormalized       string         `json:"query_normalized,omitempty"`
	QueryExpansions       []string       `json:"query_expansions,omitempty"`
	Hits                  int            `json:"hits"`
	HitItems              []RetrievalHit `json:"hit_items,omitempty"`
	RefusedReason         string         `json:"refused_reason,omitempty"`
	WeakEvidence          bool           `json:"weak_evidence,omitempty"`
	RankingErrorCandidate bool           `json:"ranking_error_candidate,omitempty"`
	// HybridMode mirrors internal/knowledge/retriever.RetrievalResult.HybridMode.
	// One of "bm25_only" | "hybrid_cosine" | "hybrid_rerank" | "qwen3_full"
	// | "bm25_fallback". Empty when retrieval is disabled.
	HybridMode string `json:"hybrid_mode,omitempty"`
	// HybridFallbackReason is non-empty only when HybridMode == "bm25_fallback".
	// One of "embedding_timeout" | "embedding_error" | "embedding_empty".
	HybridFallbackReason string `json:"hybrid_fallback_reason,omitempty"`
	// EmbeddingLatencyMS mirrors internal/knowledge/retriever.RetrievalResult.EmbeddingLatencyMS.
	// Pointer to distinguish three states: nil = embedder not invoked
	// (bm25_only or empty BM25 pool); *0 = real <1ms (reserved for future
	// client-side cache); *>0 = actual round-trip. Use to compute p95/p99
	// production embedding latency for principled hybridTimeout tuning.
	EmbeddingLatencyMS *int64 `json:"embedding_latency_ms,omitempty"`
	// EmbeddingModel labels which embedder produced the cosine signal.
	// Examples: "text-embedding-3-large", "qwen3-embedding-8b". Empty
	// when no embedder was invoked (bm25_only or bm25_fallback path).
	EmbeddingModel string `json:"embedding_model,omitempty"`
	// RerankerMode labels which reranker model produced the final ranking.
	// Empty when the reranker stage was not engaged (legacy hybrid_cosine,
	// bm25_only, or reranker fallback to cosine). Non-empty example:
	// "qwen3-reranker-8b". Distinguishes "reranker not configured for this
	// mode" (empty) from "reranker invoked" (model name).
	RerankerMode string `json:"reranker_mode,omitempty"`
	// RerankerLatencyMS mirrors EmbeddingLatencyMS three-state semantics
	// for the reranker stage: nil = reranker not invoked; *0 = reserved
	// for future client-side cache; *>0 = actual call round-trip ms.
	RerankerLatencyMS *int64 `json:"reranker_latency_ms,omitempty"`
	// RerankerFallbackReason is non-empty only when the reranker stage was
	// attempted but failed and the retriever fell back to the cosine top-K.
	// One of "reranker_timeout" | "reranker_error" | "reranker_empty".
	RerankerFallbackReason string `json:"reranker_fallback_reason,omitempty"`
	// CitedChunkIDs records the chunk_ids the LLM actually cited via [n]
	// markers in its final reply, in citation order (1-indexed → hit_items
	// position n-1). Populated only when the RAG path returned a cited
	// answer. Out-of-range markers (e.g. [9] when only 3 hits) are dropped.
	// Distinct from hit_items (full retrieved set with ranks/scores) — this
	// is the post-strip audit trail enabling downstream MySQL ingest to
	// reconstruct "this answer cited chunks X/Y/Z" without re-running the
	// retrieval. Citation markers are stripped from the user-facing reply
	// so the user only sees prose; this field is the only place [n] → chunk
	// mapping survives.
	CitedChunkIDs []string `json:"cited_chunk_ids,omitempty"`
}

type RetrievalHit struct {
	ChunkID string  `json:"chunk_id"`
	Score   float64 `json:"score"`
	Kept    bool    `json:"kept"`
	// RRF-only trace diagnostics. Populated only when the producing
	// retrieval mode was qwen3_rrf; omitted from JSONL for all other
	// modes via omitempty. Ranks are 1-indexed (0 = absent from that
	// input list). FusionScore is the pre-rerank RRF score, preserved
	// separately from Score because the reranker overwrites Score with
	// its relevance signal.
	BM25Rank    int     `json:"bm25_rank,omitempty"`
	DenseRank   int     `json:"dense_rank,omitempty"`
	FusionRank  int     `json:"fusion_rank,omitempty"`
	FusionScore float64 `json:"fusion_score,omitempty"`
}

func RedactQueryDerivedFields(trace *RetrievalTrace) {
	if trace == nil {
		return
	}
	trace.QueryRaw = policy.RedactQueryDerivedValue(trace.QueryRaw)
	trace.QueryNormalized = policy.RedactQueryDerivedValue(trace.QueryNormalized)
	for i, expansion := range trace.QueryExpansions {
		trace.QueryExpansions[i] = policy.RedactQueryDerivedValue(expansion)
	}
}

type OutcomeTrace struct {
	TotalLatencyMS             int64 `json:"total_latency_ms,omitempty"`
	TotalTokens                int   `json:"total_tokens,omitempty"`
	AttemptedHallucinatedCount int   `json:"attempted_hallucinated_count,omitempty"`
	// EscapedHallucinatedCount counts turns where the cited-contract
	// retry was skipped or failed. Note: turns aborted by the per-turn
	// token budget (refused_reason="token_budget") also bump this
	// counter — they couldn't AFFORD the coercion retry, not because
	// the model hallucinated. Hallucination dashboards joining on this
	// field must filter out refused_reason="token_budget" rows.
	EscapedHallucinatedCount int `json:"escaped_hallucinated_count,omitempty"`
	KBConflictCount          int `json:"kb_conflict_count,omitempty"`
}

// NewWriter constructs a FileWriter. Return type is the concrete *FileWriter
// (still satisfies the Writer interface) so callers that need
// FileWriter-specific affordances (e.g. test code reading files back from
// disk) can keep doing so without a type assertion.
func NewWriter(opts WriterOptions) (*FileWriter, error) {
	dir := opts.Dir
	if dir == "" {
		dir = DefaultTraceDir
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create trace dir: %w", err)
	}
	return &FileWriter{dir: dir, now: now}, nil
}

func (w *FileWriter) Dir() string {
	return w.dir
}

// Close is a no-op for FileWriter — each Append fsync-flushes synchronously,
// no buffered state to drain. Provided so FileWriter satisfies the Writer
// interface alongside MySQLWriter.
func (w *FileWriter) Close(_ context.Context) error { return nil }

func (w *FileWriter) Append(record TraceRecord) error {
	now := w.now()
	record = record.withDefaults(now)
	RedactQueryDerivedFields(&record.Retrieval)
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal trace record: %w", err)
	}
	path := filepath.Join(w.dir, traceFileName(now))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, DefaultTraceFilePerm)
	if err != nil {
		return fmt.Errorf("open trace file: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write trace line: %w", err)
	}
	if record.Retrieval.RankingErrorCandidate {
		if err := w.appendRankingErrorCandidate(now, data); err != nil {
			return err
		}
	}
	return nil
}

func (w *FileWriter) appendRankingErrorCandidate(now time.Time, data []byte) error {
	dir := filepath.Join(w.dir, now.Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create ranking-error trace dir: %w", err)
	}
	path := filepath.Join(dir, "ranking-error-candidates.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, DefaultTraceFilePerm)
	if err != nil {
		return fmt.Errorf("open ranking-error trace file: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write ranking-error trace line: %w", err)
	}
	return nil
}

func HashTracePayload(v any) (string, error) {
	data, err := canonicalTraceJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func canonicalTraceJSON(v any) ([]byte, error) {
	redacted := security.RedactForTrace(v)
	data, err := json.Marshal(redacted)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical trace payload: %w", err)
	}
	return data, nil
}

func traceFileName(t time.Time) string {
	return "agent-trace-" + t.Format("2006-01-02") + ".jsonl"
}

func Cleanup(dir string, retentionDays int, now time.Time) error {
	if dir == "" {
		dir = DefaultTraceDir
	}
	if retentionDays <= 0 {
		retentionDays = DefaultTraceRetentionDays
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read trace dir: %w", err)
	}
	cutoff := dateOnly(now).AddDate(0, 0, -retentionDays)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		traceDate, ok := traceFileDate(entry.Name())
		if !ok || !traceDate.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("remove expired trace file %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func traceFileDate(name string) (time.Time, bool) {
	const prefix = "agent-trace-"
	const suffix = ".jsonl"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return time.Time{}, false
	}
	dateText := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	traceDate, err := time.Parse("2006-01-02", dateText)
	if err != nil {
		return time.Time{}, false
	}
	return traceDate, true
}

func dateOnly(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, t.Location())
}

func traceRuntimeObserved(trace RuntimeTrace) bool {
	return trace.PlannerMode != "" || len(trace.CutoverIntents) > 0
}

func tracePlannerObserved(trace PlannerTrace) bool {
	return trace.Enabled ||
		trace.Model != "" ||
		trace.LatencyMS != 0 ||
		trace.InputTokens != 0 ||
		trace.OutputTokens != 0 ||
		trace.SchemaValid ||
		trace.Intent != "" ||
		len(trace.Slots.TargetRefs) > 0 ||
		len(trace.Slots.Metrics) > 0 ||
		trace.Slots.TimeWindow != nil ||
		trace.Confidence != 0 ||
		trace.HardBlockHint ||
		trace.CutoverStatus != ""
}

func traceEngineHardBlockObserved(trace EngineHardBlockTrace) bool {
	return trace.Hit || trace.Category != ""
}

func traceEntityRegistryObserved(trace EntityRegistryTrace) bool {
	return trace.SnapshotID != "" ||
		trace.AgeSeconds != 0 ||
		(trace.SyncEvent != "" && trace.SyncEvent != "unavailable")
}

func traceRendererObserved(trace RendererTrace) bool {
	return trace.Enabled ||
		trace.Status != "" ||
		trace.EnvelopeKind != "" ||
		len(trace.InputEnvelopeHashes) > 0 ||
		trace.FallbackUsed ||
		trace.FallbackReason != "" ||
		trace.Model != "" ||
		trace.LatencyMS != 0 ||
		trace.AttributionMode != "" ||
		len(trace.InputToolCallIDs) > 0 ||
		len(trace.InputToolArgHashes) > 0
}

func traceRateLimitObserved(trace RateLimitTrace) bool {
	return trace.Checked ||
		trace.Allowed ||
		trace.Class != "" ||
		trace.Action != "" ||
		trace.Reason != "" ||
		trace.SubjectHash != "" ||
		trace.RetryAfterMS != 0
}

func traceRetrievalObserved(trace RetrievalTrace) bool {
	return trace.Enabled ||
		trace.KBVersion != "" ||
		trace.QueryRaw != "" ||
		trace.QueryNormalized != "" ||
		len(trace.QueryExpansions) > 0 ||
		trace.Hits != 0 ||
		len(trace.HitItems) > 0 ||
		trace.RefusedReason != "" ||
		trace.WeakEvidence ||
		trace.RankingErrorCandidate ||
		trace.HybridMode != "" ||
		trace.HybridFallbackReason != "" ||
		trace.EmbeddingLatencyMS != nil ||
		trace.EmbeddingModel != "" ||
		trace.RerankerMode != "" ||
		trace.RerankerLatencyMS != nil ||
		trace.RerankerFallbackReason != ""
}

func traceOutcomeObserved(trace OutcomeTrace) bool {
	return trace.TotalLatencyMS != 0 ||
		trace.TotalTokens != 0 ||
		trace.AttemptedHallucinatedCount != 0 ||
		trace.EscapedHallucinatedCount != 0 ||
		trace.KBConflictCount != 0
}

func (r TraceRecord) withDefaults(now time.Time) TraceRecord {
	if r.SchemaVersion == "" {
		r.SchemaVersion = SchemaVersion
	}
	if r.Timestamp == "" {
		r.Timestamp = now.Format(time.RFC3339)
	}
	if r.Runtime.CutoverIntents == nil {
		r.Runtime.CutoverIntents = []string{}
	}
	if r.Planner.Slots.TargetRefs == nil {
		r.Planner.Slots.TargetRefs = []any{}
	}
	if r.Planner.Slots.Metrics == nil {
		r.Planner.Slots.Metrics = []string{}
	}
	if r.ToolCalls == nil {
		r.ToolCalls = []ToolCallTrace{}
	}
	if r.Renderer.InputToolCallIDs == nil {
		r.Renderer.InputToolCallIDs = []string{}
	}
	if r.Renderer.InputToolArgHashes == nil {
		r.Renderer.InputToolArgHashes = []string{}
	}
	if r.Renderer.InputEnvelopeHashes == nil {
		r.Renderer.InputEnvelopeHashes = []string{}
	}
	if r.Retrieval.QueryExpansions == nil {
		r.Retrieval.QueryExpansions = []string{}
	}
	if r.Retrieval.HitItems == nil {
		r.Retrieval.HitItems = []RetrievalHit{}
	}
	if r.EntityRegistry.SyncEvent == "" {
		r.EntityRegistry.SyncEvent = "unavailable"
	}
	return r
}
