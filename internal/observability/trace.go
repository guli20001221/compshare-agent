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

const SchemaVersion = "trace.v0.15"

const (
	ToolSourceMainReAct          = "main_react"
	ToolSourceWorkflowInternal   = "workflow_internal"
	ToolSourceDiagnosisInternal  = "diagnosis_internal"
	ToolSourceKnowledgeLocal     = "knowledge_local"
	ToolSourceKnowledgeMCP       = "knowledge_mcp"
	ToolSourceInitContext        = "init_context"
	ToolSourceCapabilityInternal = "capability_internal"
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

// Writer appends one completed turn; sinks do not receive independent events.
//
// Dir returns the file root or "" for database sinks. Close drains any queue.
type Writer interface {
	Append(record TraceRecord) error
	Dir() string
	Close(ctx context.Context) error
}

// FileWriter writes one JSONL file per day.
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
	// ActualExecutionTier is derived from observed retrieval and tool activity.
	// Empty means the tier was not observable; it never defaults to Agent.
	ActualExecutionTier string               `json:"actual_execution_tier,omitempty"`
	EngineHardBlock     EngineHardBlockTrace `json:"engine_hard_block"`
	EntityRegistry      EntityRegistryTrace  `json:"entity_registry"`
	ToolCalls           []ToolCallTrace      `json:"tool_calls"`
	Renderer            RendererTrace        `json:"renderer"`
	Freshness           FreshnessTrace       `json:"freshness"`
	RateLimit           RateLimitTrace       `json:"rate_limit"`
	Retrieval           RetrievalTrace       `json:"retrieval"`
	Diagnosis           DiagnosisTrace       `json:"diagnosis"`
	State               StateTrace           `json:"state"`
	Completion          TurnCompletionTrace  `json:"completion"`
	Outcome             OutcomeTrace         `json:"outcome"`
	// Confirmations records every human confirmation gate that reached a terminal
	// outcome during this turn. Guided cards add only their bounded step metadata;
	// an approved final create card also carries a redacted fixed-field projection
	// of the contract shown to the user. Confirmation ids, full forms and card
	// prose stay out of the trace.
	Confirmations []ConfirmationTrace `json:"confirmations,omitempty"`
	// Authorizations holds the per-target dual-proof audit record for each write a
	// mutating action authorized this turn. Empty / omitempty for every non-mutating
	// turn, so trace output stays byte-identical until a write target is proven.
	Authorizations []AuthorizationTrace `json:"authorizations,omitempty"`
}

type traceRecordJSON struct {
	SchemaVersion       string                `json:"schema_version"`
	TraceID             string                `json:"trace_id"`
	TurnID              string                `json:"turn_id"`
	TurnIndex           int                   `json:"turn_index"`
	Timestamp           string                `json:"timestamp"`
	UserMsgHash         string                `json:"user_msg_hash"`
	ActualExecutionTier string                `json:"actual_execution_tier,omitempty"`
	EngineHardBlock     *EngineHardBlockTrace `json:"engine_hard_block,omitempty"`
	EntityRegistry      *EntityRegistryTrace  `json:"entity_registry,omitempty"`
	ToolCalls           []ToolCallTrace       `json:"tool_calls,omitempty"`
	Renderer            *RendererTrace        `json:"renderer,omitempty"`
	Freshness           *FreshnessTrace       `json:"freshness,omitempty"`
	RateLimit           *RateLimitTrace       `json:"rate_limit,omitempty"`
	Retrieval           *RetrievalTrace       `json:"retrieval,omitempty"`
	Diagnosis           *DiagnosisTrace       `json:"diagnosis,omitempty"`
	State               *StateTrace           `json:"state,omitempty"`
	Completion          *TurnCompletionTrace  `json:"completion,omitempty"`
	Outcome             *OutcomeTrace         `json:"outcome,omitempty"`
	Confirmations       []ConfirmationTrace   `json:"confirmations,omitempty"`
	Authorizations      []AuthorizationTrace  `json:"authorizations,omitempty"`
}

func (r TraceRecord) MarshalJSON() ([]byte, error) {
	out := traceRecordJSON{
		SchemaVersion:       r.SchemaVersion,
		TraceID:             r.TraceID,
		TurnID:              r.TurnID,
		TurnIndex:           r.TurnIndex,
		Timestamp:           r.Timestamp,
		UserMsgHash:         r.UserMsgHash,
		ActualExecutionTier: r.ActualExecutionTier,
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
	if traceStateObserved(r.State) {
		out.State = &r.State
	}
	if traceCompletionObserved(r.Completion) {
		out.Completion = &r.Completion
	}
	if traceOutcomeObserved(r.Outcome) {
		out.Outcome = &r.Outcome
	}
	if len(r.Confirmations) > 0 {
		out.Confirmations = r.Confirmations
	}
	if len(r.Authorizations) > 0 {
		out.Authorizations = r.Authorizations
	}
	return json.Marshal(out)
}

// ActualExecutionTier values describe observed work, not a planner prediction.
const (
	ActualExecutionTierKnowledge = "knowledge"
	ActualExecutionTierAgent     = "agent"
)

// DeriveActualExecutionTier returns the strongest work tier observable from a
// completed trace. Retrieval with evidence is knowledge work; otherwise a main
// ReAct tool call is agent work. Tool-free answers remain unknown.
func (r TraceRecord) DeriveActualExecutionTier() string {
	if r.Retrieval.Enabled && r.Retrieval.Hits > 0 {
		return ActualExecutionTierKnowledge
	}
	for _, call := range r.ToolCalls {
		if call.Source == ToolSourceMainReAct {
			return ActualExecutionTierAgent
		}
	}
	return ""
}

// EngineHardBlock TriggeredBy enum values. Single-source attribution
// (no "both") — see EngineHardBlockTrace.TriggeredBy doc.
const (
	HardBlockTriggerTokenBudget = "token_budget"
)

// EngineHardBlock Category values the outcome derivation special-cases. They are
// NOT genuine "blocked" terminuses (see TraceRecord.isGenuineBlock):
//   - HardBlockCategoryTokenBudget: per-turn token budget exhaustion → budget terminus.
//   - HardBlockCategoryChatError:   the synthetic marker the HTTP recorder stamps
//     when chatErr != nil → error terminus.
//
// Defined here so the producers (engine.emitTokenBudgetExceededHardBlock,
// chatTraceRecorder.Finish) and the consumer (outcome.go) share one literal and
// cannot drift.
const (
	HardBlockCategoryTokenBudget = "token_budget_exceeded"
	HardBlockCategoryChatError   = "chat_error"
)

type EngineHardBlockTrace struct {
	Hit      bool   `json:"hit"`
	Category string `json:"category"`
	// TriggeredBy records the actually-executed stage that produced the
	// hard-block. Empty when Hit=false.
	TriggeredBy string `json:"triggered_by,omitempty"`
}

type EntityRegistryTrace struct {
	SnapshotID string `json:"snapshot_id"`
	AgeSeconds int64  `json:"age_seconds"`
	SyncEvent  string `json:"sync_event"`
}

type ToolCallTrace struct {
	ID        string `json:"id"`
	TurnIndex int    `json:"turn_index"`
	Action    string `json:"action"`
	Source    string `json:"source"`
	ArgsHash  string `json:"args_hash"`
	// LatencyMS is nil when this recorder did not observe both endpoints of a
	// tool call. A pointer to 0 is a real measured duration below one
	// millisecond. Keeping those states distinct prevents unmatched events from
	// biasing latency analysis toward zero.
	LatencyMS  *int64 `json:"latency_ms,omitempty"`
	Attempts   int    `json:"attempts"`
	Status     string `json:"status"`
	ErrorClass string `json:"error_class"`
	// ErrorCode is the bounded AgentToolResult code. Unknown or malformed
	// producer values are stored as _OTHER; model/user-facing error prose is
	// never parsed into this field.
	ErrorCode        string `json:"error_code,omitempty"`
	ResultHash       string `json:"result_hash"`
	Capped           string `json:"capped"`
	CapReason        string `json:"cap_reason"`
	RequestedTargets int    `json:"requested_targets"`
	ExecutedTargets  int    `json:"executed_targets"`
	WindowSeconds    int    `json:"window_seconds"`
	Projected        bool   `json:"projected,omitempty"`
	// ToolResult* measures only the generic FormatToolResult layer, after any
	// tool-specific projection. Pointers preserve absent vs a measured zero.
	ToolResultRawRunes     *int  `json:"tool_result_raw_runes,omitempty"`
	ToolResultVisibleRunes *int  `json:"tool_result_visible_runes,omitempty"`
	ToolResultTruncated    *bool `json:"tool_result_truncated,omitempty"`
}

const (
	ConfirmationStateConfirmed    = "confirmed"
	ConfirmationStateNotConfirmed = "not_confirmed"

	ConfirmationReasonUserConfirmed    = "user_confirmed"
	ConfirmationReasonUserDeclined     = "user_declined"
	ConfirmationReasonTimeout          = "timeout"
	ConfirmationReasonClientDisconnect = "client_disconnect"
	ConfirmationReasonDeliveryFailed   = "delivery_failed"
	ConfirmationReasonBrokerCancelled  = "broker_cancelled"
)

// ConfirmationTrace is the terminal observation for one confirmation card.
// State and TerminalReason are closed-set values above. ElapsedMS measures the
// wait from presenting the card to its terminal outcome; it may be zero for an
// immediately resolved confirmation. Step metadata is present only for guided
// forms; plain y/n cards retain the legacy empty-metadata shape.
type ConfirmationTrace struct {
	Action            string                   `json:"action"`
	State             string                   `json:"state"`
	TerminalReason    string                   `json:"terminal_reason"`
	ElapsedMS         int64                    `json:"elapsed_ms"`
	StepIndex         *int                     `json:"step_index,omitempty"`
	StepTitle         string                   `json:"step_title,omitempty"`
	Final             *bool                    `json:"final,omitempty"`
	ConfirmedContract *ConfirmedCreateContract `json:"confirmed_contract,omitempty"`
}

// ConfirmedCreateContract is the bounded, typed projection of the final create
// card the user approved. Values are copied from that card's Summary rather than
// recalculated, then passed through the existing text redactors.
type ConfirmedCreateContract struct {
	GPUType        string `json:"gpu_type,omitempty"`
	GPU            int    `json:"gpu,omitempty"`
	CPU            int    `json:"cpu,omitempty"`
	MemoryMB       int    `json:"memory_mb,omitempty"`
	Zone           string `json:"zone,omitempty"`
	ZoneLabel      string `json:"zone_label,omitempty"`
	Image          string `json:"image,omitempty"`
	SystemDisk     string `json:"system_disk,omitempty"`
	DataDisk       string `json:"data_disk,omitempty"`
	ChargeType     string `json:"charge_type,omitempty"`
	EstimatedPrice string `json:"estimated_price,omitempty"`
}

// AuthorizationTrace is the per-write-target dual-proof audit record: for each
// target a mutating action acted on this turn, it captures WHICH read interface
// (oracle) established the target's existence, WHEN, for WHICH account, with what
// VERDICT (the ExistenceProof), plus whether the user's confirmation authorized
// execution (the SelectionProof outcome). It lets an audit answer which interface
// proved a target existed, when, and for which account.
//
// Content-free by the same rule as the rest of the trace: the target id and the
// account are HASHED (never raw uhost-/cfs-/disk- ids or org ids), the kind /
// oracle / verdict are closed-set enums, and the user email is deliberately not
// carried. So it is safe to persist to the cross-tenant analytics trace.
type AuthorizationTrace struct {
	// Operation is the mutating workflow (e.g. StopInstanceWorkflow) — a closed set.
	Operation string `json:"operation"`
	// TargetKind is the resource kind proven: instance | cfs | disk.
	TargetKind string `json:"target_kind"`
	// TargetIDHash is a hash of the exact target id — correlatable, never raw.
	TargetIDHash string `json:"target_id_hash,omitempty"`
	// ExistenceVerdict is the closed-set verdict: verified | not_found | unavailable.
	ExistenceVerdict string `json:"existence_verdict"`
	// ExistenceOracle names the read interface that established existence:
	// DescribeCompShareInstance | DescribeCFS | DescribeCompShareInstance#DiskSet.
	ExistenceOracle string `json:"existence_oracle,omitempty"`
	// ObservedUnix is when the existence check ran (server clock, unix seconds).
	ObservedUnix int64 `json:"observed_unix,omitempty"`
	// AccountHash is a hash of the (top_organization_id/organization_id) the proof
	// was scoped to — correlatable to the turn's tenant, never the raw ids or email.
	AccountHash string `json:"account_hash,omitempty"`
	// ExecutionAuthorized is the SelectionProof outcome: true when the user confirmed
	// the target on the card (authorizing execution — even if the upstream write then
	// failed), false when the card was declined / never resolved. The confirmation
	// card IS the SelectionProof, so this records whether that human gate was passed.
	ExecutionAuthorized bool `json:"execution_authorized"`
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

func MergeFreshnessTrace(current, next FreshnessTrace) FreshnessTrace {
	if next.MonitorCallInCurrentTurn {
		current.MonitorCallInCurrentTurn = true
	}
	return current
}

// MergeRetrievalTrace folds repeated searches into one turn record without letting
// a trailing no-hit search erase earlier substantive evidence.
func MergeRetrievalTrace(current, next RetrievalTrace) RetrievalTrace {
	if next.TurnAggregate || len(next.CitedChunkIDs) > 0 || len(next.CitedRefs) > 0 {
		if !traceRetrievalObserved(current) {
			return mergeRetrievalAvailability(next, current, next)
		}
		merged := current
		if next.TurnAggregate {
			merged.TurnAggregate = true
			merged.Hits = next.Hits
			if next.QueryRaw != "" {
				merged.QueryRaw = next.QueryRaw
			}
			if next.QueryNormalized != "" {
				merged.QueryNormalized = next.QueryNormalized
			}
			if next.AnswerEchoedChunkID != "" {
				merged.AnswerEchoedChunkID = next.AnswerEchoedChunkID
			}
		}
		if len(next.References) > 0 {
			merged.References = next.References
		}
		if len(next.CitedRefs) > 0 {
			merged.CitedRefs = next.CitedRefs
		}
		if len(next.CitedChunkIDs) > 0 {
			merged.CitedChunkIDs = next.CitedChunkIDs
		}
		// The citation trace (emitSearchKnowledgeCitationTrace) recomputes
		// References/CitedChunkIDs/Activities from the FULL accumulated turn, but
		// `current` only holds the last SearchKnowledge call's HitItems/Hits (the
		// "latest substantive wins" rule below). Without carrying next's HitItems/Hits
		// too, a multi-hop turn persists cited_chunk_ids/references spanning the whole
		// turn while hit_items reflects only the last call — a cited chunk can then be
		// absent from hit_items in the ingested record. Overlay them from next.
		if len(next.HitItems) > 0 {
			merged.HitItems = next.HitItems
		}
		if !next.TurnAggregate && next.Hits > merged.Hits {
			merged.Hits = next.Hits
		}
		if len(next.Activities) > 0 {
			if next.TurnAggregate {
				merged.Activities = append([]RetrievalActivity(nil), next.Activities...)
			} else {
				merged.Activities = mergeRetrievalActivities(merged.Activities, next.Activities)
			}
		}
		return mergeRetrievalAvailability(merged, current, next)
	}
	if current.Enabled && current.Hits > 0 && (!next.Enabled || next.Hits == 0) {
		return mergeRetrievalAvailability(current, current, next)
	}
	return mergeRetrievalAvailability(next, current, next)
}

// mergeRetrievalAvailability preserves an outage from any retrieval hop even
// when another hop supplies the evidence-bearing trace.
func mergeRetrievalAvailability(merged, current, next RetrievalTrace) RetrievalTrace {
	if !current.Unavailable && !next.Unavailable {
		return merged
	}
	merged.Unavailable = true
	if merged.FailureReason != "" {
		return merged
	}
	if next.Unavailable && next.FailureReason != "" {
		merged.FailureReason = next.FailureReason
		return merged
	}
	if current.Unavailable {
		merged.FailureReason = current.FailureReason
	}
	return merged
}

func mergeRetrievalActivities(current, next []RetrievalActivity) []RetrievalActivity {
	if len(current) == 0 {
		return append([]RetrievalActivity(nil), next...)
	}
	out := append([]RetrievalActivity(nil), current...)
	seen := make(map[string]int, len(out))
	for i, activity := range out {
		if activity.ID != "" {
			seen[activity.ID] = i
		}
	}
	for _, activity := range next {
		if idx, ok := seen[activity.ID]; activity.ID != "" && ok {
			out[idx] = activity
			continue
		}
		if activity.ID != "" {
			seen[activity.ID] = len(out)
		}
		out = append(out, activity)
	}
	return out
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
	Enabled bool `json:"enabled"`
	// Unavailable distinguishes a knowledge-service outage (or no active remote
	// release) from a completed empty search. It is not a corpus-gap refusal.
	Unavailable   bool   `json:"unavailable,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
	// TurnAggregate marks the final de-duplicated evidence snapshot for the
	// entire turn. It is emitted for both grounded answers and grounding
	// failures so a multi-search failure does not retain only its last call.
	TurnAggregate bool   `json:"turn_aggregate,omitempty"`
	KBVersion     string `json:"kb_version"`
	// AnswerQuestion is the standalone conversational question the answer must
	// resolve. QueryRaw/Activities are retrieval strings and may be narrower
	// subqueries; keeping both prevents a search phrase from silently redefining
	// the user's question.
	AnswerQuestion  string               `json:"answer_question,omitempty"`
	QueryRaw        string               `json:"query_raw,omitempty"`
	QueryNormalized string               `json:"query_normalized,omitempty"`
	QueryExpansions []string             `json:"query_expansions,omitempty"`
	Hits            int                  `json:"hits"`
	HitItems        []RetrievalHit       `json:"hit_items,omitempty"`
	Activities      []RetrievalActivity  `json:"activities,omitempty"`
	References      []RetrievalReference `json:"references,omitempty"`
	CitedRefs       []RetrievalCitedRef  `json:"cited_refs,omitempty"`
	RefusedReason   string               `json:"refused_reason,omitempty"`
	// RefusalType classifies a RAG refusal into the three supported categories
	// (corpus_gap / all_below_floor / synthesis_refused). Derived
	// at Finish from RefusedReason + FloorDroppedAll (DeriveRefusalType); empty
	// when the turn did not emit a knowledge-coverage refusal.
	RefusalType string `json:"refusal_type,omitempty"`
	// FloorDroppedAll is true when the relevance floor removed EVERY retrieved hit
	// this turn (the agent-loop drop point) — the signal that distinguishes
	// all_below_floor from a genuinely empty corpus (corpus_gap). A retrieval fact
	// independent of whether the turn then refused or answered with general
	// guidance, so it is queryable on its own even when refusal_type is empty.
	FloorDroppedAll bool `json:"floor_dropped_all,omitempty"`
	// FloorValue is the weak-evidence relevance floor in effect for this turn's
	// HybridMode (0.5 semantic / 55 BM25). With HitItems[0].Score it shows how
	// far the top hit fell from the floor.
	FloorValue            float64 `json:"floor_value,omitempty"`
	WeakEvidence          bool    `json:"weak_evidence,omitempty"`
	RankingErrorCandidate bool    `json:"ranking_error_candidate,omitempty"`
	// AnswerEchoedChunkID records a >=32-rune verbatim match with a customer-safe
	// chunk. It is a synthesis-quality signal and never gates the answer.
	AnswerEchoedChunkID string `json:"answer_echoed_chunk_id,omitempty"`
	// HybridMode mirrors internal/knowledge/retriever.RetrievalResult.HybridMode.
	// One of "bm25_only" | "hybrid_cosine" | "hybrid_rerank" | "qwen3_full"
	// | "qwen3_rrf" | "bm25_fallback" | "unknown_remote". Empty when retrieval is
	// disabled.
	//
	// "unknown_remote" is an operational signal, not a pipeline: the remote
	// knowledge service reported a scoring path this build has no calibrated
	// floor for, so the relevance floor and the ranking-ambiguity metric were
	// both SKIPPED for that query and FloorValue is absent. Query it to find a
	// remote whose retrieval metadata this build no longer understands;
	// HybridFallbackReason carries the raw mode it sent.
	HybridMode string `json:"hybrid_mode,omitempty"`
	// HybridFallbackReason is non-empty when HybridMode == "bm25_fallback"
	// ("embedding_timeout" | "embedding_error" | "embedding_empty") or when
	// HybridMode == "unknown_remote" ("remote_mode_missing" |
	// "remote_mode_unrecognized:<raw>"). A remote that reported its own
	// degradation reason keeps it, joined with "; ".
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
	// Empty when the reranker stage was not engaged (hybrid_cosine, bm25_only,
	// or reranker fallback to cosine). Non-empty example:
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

type RetrievalActivity struct {
	ID              string `json:"id"`
	Query           string `json:"query"`
	Hits            int    `json:"hits"`
	LatencyMS       int64  `json:"latency_ms,omitempty"`
	Fallback        string `json:"fallback,omitempty"`
	Error           string `json:"error,omitempty"`
	FloorDroppedAll bool   `json:"floor_dropped_all,omitempty"`
}

type RetrievalReference struct {
	RefID       string   `json:"ref_id"`
	ChunkID     string   `json:"chunk_id"`
	Title       string   `json:"title,omitempty"`
	SourceArea  string   `json:"source_area,omitempty"`
	Score       float64  `json:"score,omitempty"`
	Rank        int      `json:"rank,omitempty"`
	ActivityIDs []string `json:"activity_ids,omitempty"`
}

type RetrievalCitedRef struct {
	RefID   string `json:"ref_id"`
	ChunkID string `json:"chunk_id"`
}

type RetrievalHit struct {
	ChunkID string `json:"chunk_id"`
	// SourceArea is the cited chunk's declared product_area (KBChunk.ProductArea).
	// Stages the per-chunk domain visibility the #5 wrong-domain guard reads;
	// empty when the chunk declares no product_area.
	SourceArea string  `json:"source_area,omitempty"`
	Score      float64 `json:"score"`
	Kept       bool    `json:"kept"`
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

// prepareForPersist is the sink-independent defaulting and redaction boundary.
// It is idempotent so fan-out writers may call it more than once.
func prepareForPersist(record TraceRecord, now time.Time) TraceRecord {
	record = record.withDefaults(now)
	RedactQueryDerivedFields(&record.Retrieval)
	RedactDiagnosisDerivedFields(&record.Diagnosis)
	return record
}

type OutcomeTrace struct {
	// Continuity contract metadata is bounded and content-free. It proves which
	// inputs and answer path were used without persisting prompts or user text.
	ContextSources       []string `json:"context_sources,omitempty"`
	ResponseContract     string   `json:"response_contract,omitempty"`
	PromptSectionIDs     []string `json:"prompt_section_ids,omitempty"`
	EvidenceUpdateSource string   `json:"memory_update_source,omitempty"`
	GroundingOutcome     string   `json:"grounding_outcome,omitempty"`
	// PromptMessages* are content-free context-assembly telemetry. They show
	// whether the final request had to shed prior messages, without storing the
	// prompt or transcript itself. Prompt token usage remains in PromptTokens.
	PromptMessagesRawPeak       int  `json:"prompt_messages_raw_peak,omitempty"`
	PromptMessagesAssembledPeak int  `json:"prompt_messages_assembled_peak,omitempty"`
	PromptMessagesCapApplied    bool `json:"prompt_messages_cap_applied,omitempty"`
	// TerminatedBy / AbortCause / ErrorClass / Resolution are the four
	// outcome-attribution axes derived at Finish (see outcome.go). They close the
	// "no attribution on ~25% of turns" dark hole. TerminatedBy is always set for a
	// finalized turn (at minimum "done"); the other three are empty unless their
	// condition fires. All omitempty → a record that never ran FinalizeOutcome
	// (raw fixtures) marshals byte-identically to before.
	TerminatedBy string `json:"terminated_by,omitempty"`
	AbortCause   string `json:"abort_cause,omitempty"`
	ErrorClass   string `json:"error_class,omitempty"`
	Resolution   string `json:"resolution,omitempty"`
	// ReactRounds is the number of ReAct loop rounds entered this turn; BudgetHit
	// is true when the turn hit the token budget or the round ceiling. Both feed
	// the D7 (per-turn budget exhaustion) analysis and the budget terminus.
	ReactRounds int  `json:"react_rounds,omitempty"`
	BudgetHit   bool `json:"budget_hit,omitempty"`
	// ActionProposalDisposition records what the resolver did with a write proposal
	// this turn — "confirmation"/"intake_form" when it carded, else the reason it did
	// not ("rejected:<slot>=<kind>", "missing:<fields>", "dependency_failure", ...).
	// "" when no proposal ran. Attributes why a create did or did not card without
	// re-running the model.
	ActionProposalDisposition string `json:"action_proposal_disposition,omitempty"`
	TotalLatencyMS            int64  `json:"total_latency_ms,omitempty"`
	// FirstVisibleEventMS is measured at the transport write boundary for the
	// first successfully written token, step, confirmation, or terminal error.
	// It is not the assistant-row TTFT and remains nil when none was delivered.
	FirstVisibleEventMS        *int64 `json:"first_visible_event_ms,omitempty"`
	TotalTokens                int    `json:"total_tokens,omitempty"`
	PromptTokens               int    `json:"prompt_tokens,omitempty"`
	CompletionTokens           int    `json:"completion_tokens,omitempty"`
	AttemptedHallucinatedCount int    `json:"attempted_hallucinated_count,omitempty"`
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
	return trace.MonitorCallInCurrentTurn
}

func traceRetrievalObserved(trace RetrievalTrace) bool {
	return trace.Enabled ||
		trace.Unavailable ||
		trace.FailureReason != "" ||
		trace.KBVersion != "" ||
		trace.QueryRaw != "" ||
		trace.QueryNormalized != "" ||
		len(trace.QueryExpansions) > 0 ||
		trace.Hits != 0 ||
		len(trace.HitItems) > 0 ||
		len(trace.Activities) > 0 ||
		len(trace.References) > 0 ||
		len(trace.CitedRefs) > 0 ||
		trace.RefusedReason != "" ||
		trace.RefusalType != "" ||
		trace.FloorDroppedAll ||
		trace.FloorValue != 0 ||
		trace.WeakEvidence ||
		trace.RankingErrorCandidate ||
		trace.AnswerEchoedChunkID != "" ||
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
		trace.FirstVisibleEventMS != nil ||
		trace.TotalTokens != 0 ||
		trace.AttemptedHallucinatedCount != 0 ||
		trace.EscapedHallucinatedCount != 0 ||
		trace.KBConflictCount != 0 ||
		trace.TerminatedBy != "" ||
		trace.AbortCause != "" ||
		trace.ErrorClass != "" ||
		trace.Resolution != "" ||
		trace.ReactRounds != 0 ||
		trace.BudgetHit ||
		len(trace.ContextSources) > 0 ||
		trace.ResponseContract != "" ||
		len(trace.PromptSectionIDs) > 0 ||
		trace.EvidenceUpdateSource != "" ||
		trace.GroundingOutcome != "" ||
		trace.PromptMessagesRawPeak != 0 ||
		trace.PromptMessagesAssembledPeak != 0 ||
		trace.PromptMessagesCapApplied ||
		trace.ActionProposalDisposition != ""
}

func (r TraceRecord) withDefaults(now time.Time) TraceRecord {
	if r.SchemaVersion == "" {
		r.SchemaVersion = SchemaVersion
	}
	if r.Timestamp == "" {
		r.Timestamp = now.Format(time.RFC3339)
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
	if r.Retrieval.Activities == nil {
		r.Retrieval.Activities = []RetrievalActivity{}
	}
	if r.Retrieval.References == nil {
		r.Retrieval.References = []RetrievalReference{}
	}
	if r.Retrieval.CitedRefs == nil {
		r.Retrieval.CitedRefs = []RetrievalCitedRef{}
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
