package observability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/compshare-agent/internal/policy"
	"github.com/compshare-agent/internal/security"
)

const SchemaVersion = "trace.v0.4"

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

var (
	diagnosisTraceUHostIDRE = regexp.MustCompile(`\buhost-[A-Za-z0-9]+\b`)
	diagnosisTraceIPv4RE    = regexp.MustCompile(`\b(\d{1,3})\.(\d{1,3})\.\d{1,3}\.\d{1,3}\b`)
)

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
	// EmitStep is a reserved no-op hook on the SINK. B6.2 wires agent-tier
	// step accumulation in the per-turn RECORDER (cmd/trace.go
	// cliTraceRecorder.EmitStep, internal/httpapi/trace_recorder.go
	// chatTraceRecorder.EmitStep), NOT here: both sinks write the turn row
	// ONCE at Append/Enqueue, so steps are folded into TraceRecord.Steps[] in
	// memory and persisted with that single write (never a per-step INSERT —
	// that would collide uk_request_uuid, one agent_traces row per turn). This
	// method stays for a future streaming sink that wants per-step delivery.
	EmitStep(step StepTrace) error
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
	// TaskTier is the work-tier axis (fast / knowledge / agent; see the
	// RealizedTier* consts) the planner PREDICTS for this turn — an input,
	// the predicted twin of RealizedTier. RESERVED SCHEMA SLOT, NOT YET
	// WIRED: no production code emits it (planner-predicted task_tier is
	// B4b, still pending), so outside tests it is always empty. Added early
	// as a reserved field so consumers (analytics SQL, dashboards) can treat
	// its eventual presence as the signal to switch from legacy per-turn
	// rows to tier-aware aggregation. Memory: attribution-observable-only —
	// empty means "tier not known for this turn", never default-to-agent.
	TaskTier string `json:"task_tier,omitempty"`
	// RealizedTier is the work-tier axis: the task-complexity tier the turn
	// ACTUALLY ran on (fast / knowledge / agent), DERIVED from observed dispatch signals.
	// It is distinct from TaskTier, which is the planner's PREDICTED tier
	// (an input): predicted and realized diverge whenever the planner picks
	// a tier but the turn falls through to a different path, so the two are
	// kept in separate fields to preserve both the prediction and the
	// outcome. Set by the recorders via DeriveRealizedTier at Finish, after
	// every signal is final. Empty when the tier is not observable for the
	// turn (no-tool ReAct answer, hard-block / canned reply) — empty means
	// "tier not known", never default-to-agent (attribution-observable-only).
	RealizedTier string `json:"realized_tier,omitempty"`
	// ActualRuntimeForm is the runtime-form axis: the coarse runtime architecture
	// form that actually handled this turn. It is derived from observed execution
	// signals, not from planner output: routing / terminal_rag / agent. This is a
	// DIFFERENT axis from RealizedTier and may diverge from it by design (see the
	// RuntimeForm* consts). Empty means not observable.
	ActualRuntimeForm string               `json:"actual_runtime_form,omitempty"`
	Runtime           RuntimeTrace         `json:"runtime"`
	Planner           RouterTrace          `json:"planner"`
	EngineHardBlock   EngineHardBlockTrace `json:"engine_hard_block"`
	EntityRegistry    EntityRegistryTrace  `json:"entity_registry"`
	ToolCalls         []ToolCallTrace      `json:"tool_calls"`
	Renderer          RendererTrace        `json:"renderer"`
	Freshness         FreshnessTrace       `json:"freshness"`
	RateLimit         RateLimitTrace       `json:"rate_limit"`
	Retrieval         RetrievalTrace       `json:"retrieval"`
	Diagnosis         DiagnosisTrace       `json:"diagnosis"`
	Outcome           OutcomeTrace         `json:"outcome"`
	// Steps holds agent-tier saga step traces, populated by B6.2. Empty /
	// omitempty for all non-agent turns, so trace output stays byte-identical
	// until a producer exists (same reserved-slot precedent as TaskTier in B1).
	Steps []StepTrace `json:"steps,omitempty"`
}

type traceRecordJSON struct {
	SchemaVersion     string                `json:"schema_version"`
	TraceID           string                `json:"trace_id"`
	TurnID            string                `json:"turn_id"`
	TurnIndex         int                   `json:"turn_index"`
	Timestamp         string                `json:"timestamp"`
	UserMsgHash       string                `json:"user_msg_hash"`
	TaskTier          string                `json:"task_tier,omitempty"`
	RealizedTier      string                `json:"realized_tier,omitempty"`
	ActualRuntimeForm string                `json:"actual_runtime_form,omitempty"`
	Runtime           *RuntimeTrace         `json:"runtime,omitempty"`
	Planner           *RouterTrace          `json:"planner,omitempty"`
	EngineHardBlock   *EngineHardBlockTrace `json:"engine_hard_block,omitempty"`
	EntityRegistry    *EntityRegistryTrace  `json:"entity_registry,omitempty"`
	ToolCalls         []ToolCallTrace       `json:"tool_calls,omitempty"`
	Renderer          *RendererTrace        `json:"renderer,omitempty"`
	Freshness         *FreshnessTrace       `json:"freshness,omitempty"`
	RateLimit         *RateLimitTrace       `json:"rate_limit,omitempty"`
	Retrieval         *RetrievalTrace       `json:"retrieval,omitempty"`
	Diagnosis         *DiagnosisTrace       `json:"diagnosis,omitempty"`
	Outcome           *OutcomeTrace         `json:"outcome,omitempty"`
	Steps             []StepTrace           `json:"steps,omitempty"`
}

func (r TraceRecord) MarshalJSON() ([]byte, error) {
	out := traceRecordJSON{
		SchemaVersion:     r.SchemaVersion,
		TraceID:           r.TraceID,
		TurnID:            r.TurnID,
		TurnIndex:         r.TurnIndex,
		Timestamp:         r.Timestamp,
		UserMsgHash:       r.UserMsgHash,
		TaskTier:          r.TaskTier,
		RealizedTier:      r.RealizedTier,
		ActualRuntimeForm: r.ActualRuntimeForm,
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
	if traceFreshnessObserved(r.Freshness) {
		out.Freshness = &r.Freshness
	}
	if traceRateLimitObserved(r.RateLimit) {
		out.RateLimit = &r.RateLimit
	}
	if traceRetrievalObserved(r.Retrieval) {
		out.Retrieval = &r.Retrieval
	}
	if traceDiagnosisObserved(r.Diagnosis) {
		out.Diagnosis = &r.Diagnosis
	}
	if traceOutcomeObserved(r.Outcome) {
		out.Outcome = &r.Outcome
	}
	if len(r.Steps) > 0 {
		out.Steps = r.Steps
	}
	return json.Marshal(out)
}

// TWO SEPARATE AXES describe how a turn ran. They are NOT interchangeable and
// deliberately diverge for some turns — keep them distinct (do not collapse the
// two fields into one, or force one shared vocabulary onto both).
// TestRealizedTierAndRuntimeFormAreSeparateAxes pins the divergence.
//
//   - Work-tier axis (TaskTier predicted / RealizedTier realized): WHAT KIND of
//     work the turn did, on the ADR-001 complexity scale fast < knowledge < agent.
//   - Runtime-form axis (PlannedRuntimeForm / ActualRuntimeForm): WHICH runtime
//     architecture executed — routing (deterministic handler) / terminal_rag
//     (final-answer retrieval workflow) / agent (ReAct loop).
//
// The axes correspond only loosely (fast↔routing, knowledge↔terminal_rag,
// agent↔agent): a turn can do knowledge-tier WORK on the agent FORM — a
// knowledge_qa turn forced through the ReAct loop, or diagnosis that retrieves
// mid-flow, both realize knowledge while their runtime form is agent (see
// DeriveRealizedTier / DeriveActualRuntimeForm).

// RealizedTier* are the work-tier-axis values for TraceRecord.RealizedTier (and
// the predicted TaskTier). Mirror the ADR-001 task-complexity tiers.
const (
	RealizedTierFast      = "fast"
	RealizedTierKnowledge = "knowledge"
	RealizedTierAgent     = "agent"
)

// RuntimeForm* are the runtime-form-axis values for TraceRecord.ActualRuntimeForm
// (and PlannedRuntimeForm). NOTE: "terminal_rag" is the odd value out vs the
// rest of the routing/agent vocabulary; renaming it to "rag" is a wire-visible
// trace-schema change and must be done as its own compatibility-aware migration,
// not folded into a comment/test cleanup.
const (
	RuntimeFormRouting     = "routing"
	RuntimeFormTerminalRAG = "terminal_rag"
	RuntimeFormAgent       = "agent"
)

// DeriveRealizedTier computes the tier the turn ACTUALLY ran on from observed
// trace signals, returning "" when the tier is not observable. It is pure
// (depends only on the record) and is invoked by the recorders at Finish —
// after every signal is final — so the result reflects the turn's terminal
// state. The HTTP/MySQL sink bypasses FileWriter.Append/withDefaults, so this
// must be called in the recorders (not in Append) to populate both sinks.
//
// Derivation is priority-ordered, NOT a flat prefix sweep of cutover_status
// (which has 13 values and is set only on the Phase-1 route path — see
// internal/intent/handler.go RouteStatus*). It maps the unambiguous route
// dispositions, then falls through to what actually executed:
//
//  1. dispatched_retrieval            -> knowledge (RAG handler ran)
//  2. dispatched / selection_required -> fast      (deterministic handler ran;
//     selection returned a clarify prompt,
//     still no RAG / ReAct)
//  3. else retrieval produced hits    -> knowledge (RAG path that never went
//     through routing)
//  4. else a main_react tool fired    -> agent     (ReAct loop ran)
//  5. else                            -> ""        (not observable: no-tool
//     ReAct answer, or hard-block / canned
//     reply)
//
// Route fallbacks (fallback_*) and failure_after_tool are deliberately NOT
// mapped by status name: they mean the route attempt declined and the turn
// continued, so the real tier is whatever steps 3-4 observe (typically agent).
// Returning "" rather than defaulting to agent keeps the realized mix honest —
// it under-counts no-tool agent turns instead of mis-labelling refusals as
// agent (memory: attribution-observable-only). A future "ReAct loop ran"
// signal (e.g. a round counter) could promote step 5 from "" to agent.
func (r TraceRecord) DeriveRealizedTier() string {
	// cutover_status literals mirror internal/intent/handler.go:42-58
	// (RouteStatus*); pinned by TestDeriveRealizedTier.
	switch r.Planner.RouteStatus {
	case "dispatched_retrieval", "dispatched_knowledge_agent_loop":
		// dispatched_knowledge_agent_loop is a knowledge_qa turn forced through the
		// agent loop (COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP): the realized work is still
		// knowledge retrieval, so the realized-tier attribution stays comparable
		// across the terminal→agent-loop migration even though the runtime FORM
		// becomes agent (see DeriveActualRuntimeForm).
		return RealizedTierKnowledge
	case "dispatched_agent":
		return RealizedTierAgent
	case "dispatched", "selection_required":
		return RealizedTierFast
	}
	if r.Retrieval.Enabled && r.Retrieval.Hits > 0 {
		return RealizedTierKnowledge
	}
	for _, call := range r.ToolCalls {
		if call.Source == ToolSourceMainReAct {
			return RealizedTierAgent
		}
	}
	return ""
}

// DeriveActualRuntimeForm computes the production architecture form that
// actually handled the turn. It is intentionally coarser than RealizedTier:
// terminal RAG is only a final-answer retrieval workflow; retrieval used inside
// diagnosis or another agent path remains agent.
func (r TraceRecord) DeriveActualRuntimeForm() string {
	switch r.Planner.RouteStatus {
	case "dispatched_agent", "dispatched_knowledge_agent_loop":
		// dispatched_knowledge_agent_loop: a knowledge_qa turn forced through the
		// shared ReAct loop (COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP). It runs the agent
		// loop (a SearchKnowledge tool call fires), so the runtime FORM is agent —
		// the migration's whole point (terminal_rag → agent). The engine projects
		// PlannedRuntimeForm=agent for the same turn so planned==actual.
		return RuntimeFormAgent
	case "dispatched_retrieval":
		return RuntimeFormTerminalRAG
	case "dispatched", "selection_required":
		return RuntimeFormRouting
	}
	if len(r.Steps) > 0 {
		return RuntimeFormAgent
	}
	for _, call := range r.ToolCalls {
		switch call.Source {
		case ToolSourceMainReAct, ToolSourceWorkflowInternal, ToolSourceDiagnosisInternal, ToolSourceKnowledgeLocal:
			return RuntimeFormAgent
		}
	}
	if r.Retrieval.Enabled && r.Retrieval.Hits > 0 {
		return RuntimeFormTerminalRAG
	}
	for _, call := range r.ToolCalls {
		if call.Source == ToolSourcePlannerHandler {
			return RuntimeFormRouting
		}
	}
	return ""
}

func (r TraceRecord) RuntimeFormMismatch() (bool, bool) {
	planned := strings.TrimSpace(r.Planner.PlannedRuntimeForm)
	actual := strings.TrimSpace(r.ActualRuntimeForm)
	if actual == "" {
		actual = strings.TrimSpace(r.DeriveActualRuntimeForm())
	}
	if planned == "" || actual == "" {
		return false, false
	}
	return planned != actual, true
}

type RuntimeTrace struct {
	PlannerMode  string   `json:"planner_mode"`
	RouteIntents []string `json:"cutover_intents"`
}

type RouterTrace struct {
	Enabled            bool                `json:"enabled"`
	Model              string              `json:"model"`
	LatencyMS          int64               `json:"latency_ms"`
	InputTokens        int                 `json:"input_tokens"`
	OutputTokens       int                 `json:"output_tokens"`
	SchemaValid        bool                `json:"schema_valid"`
	Intent             string              `json:"intent"`
	PlannedRuntimeForm string              `json:"planned_runtime_form,omitempty"`
	Skills             []PlannerSkillTrace `json:"skills,omitempty"`
	Slots              PlannerSlots        `json:"slots"`
	Confidence         float64             `json:"confidence"`
	HardBlockHint      bool                `json:"hard_block_hint"`
	RouteStatus        string              `json:"cutover_status"`
}

type PlannerSkillTrace struct {
	Name       string `json:"name,omitempty"`
	Resolution string `json:"resolution"`
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
	Projected        bool   `json:"projected,omitempty"`
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
	MonitorCallInCurrentTurn    bool   `json:"monitor_call_in_current_turn"`
	MonitorRecallForced         bool   `json:"monitor_recall_forced,omitempty"`
	MonitorRecallMode           string `json:"monitor_recall_mode,omitempty"`
	MonitorRecallFallbackReason string `json:"monitor_recall_fallback_reason,omitempty"`
	SupportsObjectToolChoice    *bool  `json:"supports_object_tool_choice,omitempty"`
	SupportsRequiredToolChoice  *bool  `json:"supports_required_tool_choice,omitempty"`
}

func MergeFreshnessTrace(current, next FreshnessTrace) FreshnessTrace {
	if next.MonitorCallInCurrentTurn {
		current.MonitorCallInCurrentTurn = true
	}
	if next.MonitorRecallForced {
		current.MonitorRecallForced = true
	}
	if next.MonitorRecallMode != "" {
		current.MonitorRecallMode = next.MonitorRecallMode
	}
	if next.MonitorRecallFallbackReason != "" {
		current.MonitorRecallFallbackReason = next.MonitorRecallFallbackReason
	}
	if next.SupportsObjectToolChoice != nil {
		v := *next.SupportsObjectToolChoice
		current.SupportsObjectToolChoice = &v
	}
	if next.SupportsRequiredToolChoice != nil {
		v := *next.SupportsRequiredToolChoice
		current.SupportsRequiredToolChoice = &v
	}
	return current
}

// MergeRetrievalTrace folds a new per-turn retrieval into the recorded one. A turn
// can retrieve more than once (e.g. the COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP forced
// SearchKnowledge first hop, then a voluntary re-query later in the same ReAct loop).
// The recorders historically kept only the LAST retrieval, so a trailing no-hit
// re-query would clobber the forced hop's substantive retrieval and make it look like
// nothing was retrieved. Preserve the substantive one: keep the existing hits-bearing
// retrieval when the incoming one has no hits; otherwise the latest (substantive or
// first-ever) wins. Terminal RAG retrieves once, so its trace is unchanged.
func MergeRetrievalTrace(current, next RetrievalTrace) RetrievalTrace {
	if current.Enabled && current.Hits > 0 && (!next.Enabled || next.Hits == 0) {
		return current
	}
	return next
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

type DiagnosisTrace struct {
	Claims []DiagnosisClaimTrace `json:"claims,omitempty"`
}

type DiagnosisClaimTrace struct {
	Claim    string   `json:"claim"`
	Status   string   `json:"status"`
	ChunkIDs []string `json:"chunk_ids,omitempty"`
	Reason   string   `json:"reason,omitempty"`
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

func RedactDiagnosisDerivedFields(trace *DiagnosisTrace) {
	if trace == nil {
		return
	}
	for i := range trace.Claims {
		trace.Claims[i].Claim = redactDiagnosisTraceText(trace.Claims[i].Claim)
		trace.Claims[i].Reason = redactDiagnosisTraceText(trace.Claims[i].Reason)
	}
}

func redactDiagnosisTraceText(text string) string {
	text = policy.RedactQueryDerivedValue(text)
	text = security.RedactOperationalTokensInText(text)
	text = diagnosisTraceUHostIDRE.ReplaceAllString(text, "<UHOST_ID>")
	text = diagnosisTraceIPv4RE.ReplaceAllString(text, "$1.$2.x.x")
	return text
}

// prepareForPersist is the single, sink-agnostic choke point every persistence
// sink MUST run a record through before serializing it. It fills defaults
// (schema_version, timestamp, empty slices via withDefaults) and redacts the
// entire query-derived tree (QueryRaw + QueryNormalized + QueryExpansions[] via
// RedactQueryDerivedFields). Both operations are idempotent, so a record fanned
// out to multiple sinks (cmd.multiTraceWriter) is safe to prepare more than once.
//
// History: redaction + withDefaults previously lived ONLY inside
// FileWriter.Append. The MySQL sink (MySQLWriter.Enqueue → worker → rowFromTrace)
// bypassed both, so server traces persisted raw user queries — real PII such as
// staff names — into trace_json with an empty schema_version. Centralizing here
// (called by FileWriter.Append AND MySQLWriter.Enqueue) means a leak is
// impossible unless a future sink bypasses this function entirely
// (memory: sanitization-covers-all-derived-fields — cover the whole derivation
// tree from one choke point, never patch a single field).
func prepareForPersist(record TraceRecord, now time.Time) TraceRecord {
	record = record.withDefaults(now)
	RedactQueryDerivedFields(&record.Retrieval)
	RedactDiagnosisDerivedFields(&record.Diagnosis)
	RedactStepDerivedFields(record.Steps)
	return record
}

type OutcomeTrace struct {
	TotalLatencyMS             int64 `json:"total_latency_ms,omitempty"`
	TotalTokens                int   `json:"total_tokens,omitempty"`
	PromptTokens               int   `json:"prompt_tokens,omitempty"`
	CompletionTokens           int   `json:"completion_tokens,omitempty"`
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

// EmitStep is a no-op on the file sink. Agent-tier saga steps are accumulated
// in the per-turn recorder (cliTraceRecorder.EmitStep) and persisted with the
// single Append at turn Finish — see the Writer.EmitStep interface doc.
func (w *FileWriter) EmitStep(StepTrace) error { return nil }

func (w *FileWriter) Append(record TraceRecord) error {
	now := w.now()
	record = prepareForPersist(record, now)
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
	return trace.PlannerMode != "" || len(trace.RouteIntents) > 0
}

func tracePlannerObserved(trace RouterTrace) bool {
	return trace.Enabled ||
		trace.Model != "" ||
		trace.LatencyMS != 0 ||
		trace.InputTokens != 0 ||
		trace.OutputTokens != 0 ||
		trace.SchemaValid ||
		trace.Intent != "" ||
		trace.PlannedRuntimeForm != "" ||
		len(trace.Skills) > 0 ||
		len(trace.Slots.TargetRefs) > 0 ||
		len(trace.Slots.Metrics) > 0 ||
		trace.Slots.TimeWindow != nil ||
		trace.Confidence != 0 ||
		trace.HardBlockHint ||
		trace.RouteStatus != ""
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

func traceFreshnessObserved(trace FreshnessTrace) bool {
	return trace.MonitorCallInCurrentTurn ||
		trace.MonitorRecallForced ||
		trace.MonitorRecallMode != "" ||
		trace.MonitorRecallFallbackReason != "" ||
		trace.SupportsObjectToolChoice != nil ||
		trace.SupportsRequiredToolChoice != nil
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
		trace.RerankerFallbackReason != "" ||
		len(trace.CitedChunkIDs) > 0
}

func traceDiagnosisObserved(trace DiagnosisTrace) bool {
	return len(trace.Claims) > 0
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
	if r.Runtime.RouteIntents == nil {
		r.Runtime.RouteIntents = []string{}
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
	if r.Diagnosis.Claims == nil {
		r.Diagnosis.Claims = []DiagnosisClaimTrace{}
	}
	for i := range r.Diagnosis.Claims {
		if r.Diagnosis.Claims[i].ChunkIDs == nil {
			r.Diagnosis.Claims[i].ChunkIDs = []string{}
		}
	}
	if r.EntityRegistry.SyncEvent == "" {
		r.EntityRegistry.SyncEvent = "unavailable"
	}
	return r
}
