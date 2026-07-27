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

const SchemaVersion = "trace.v0.7"

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
	// EmitStep is a reserved no-op hook on the sink. Workflow step
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
	// ActualExecutionTier* consts) the planner PREDICTS for this turn — an input,
	// the predicted twin of ActualExecutionTier. RESERVED SCHEMA SLOT, NOT YET
	// WIRED: no production code emits it (planner-predicted task_tier is
	// B4b, still pending), so outside tests it is always empty. Added early
	// as a reserved field so consumers (analytics SQL, dashboards) can treat
	// its eventual presence as the signal to switch from legacy per-turn
	// rows to tier-aware aggregation. Memory: attribution-observable-only —
	// empty means "tier not known for this turn", never default-to-agent.
	TaskTier string `json:"task_tier,omitempty"`
	// ActualExecutionTier is the work-tier axis: the task-complexity tier the turn
	// ACTUALLY ran on (fast / knowledge / agent), DERIVED from observed dispatch signals.
	// It is distinct from TaskTier, which is the planner's PREDICTED tier
	// (an input): predicted and realized diverge whenever the planner picks
	// a tier but the turn falls through to a different path, so the two are
	// kept in separate fields to preserve both the prediction and the
	// outcome. Set by the recorders via DeriveActualExecutionTier at Finish, after
	// every signal is final. Empty when the tier is not observable for the
	// turn (no-tool ReAct answer, hard-block / canned reply) — empty means
	// "tier not known", never default-to-agent (attribution-observable-only).
	ActualExecutionTier string `json:"actual_execution_tier,omitempty"`
	// ActualExecutionPath is the LEGACY runtime-form axis (routing / terminal_rag /
	// agent) — see the ExecutionPath* const note. It is retained for trace/dashboard
	// continuity across the P6 cutover, NOT the current architecture (which has one
	// execution form, the central Agent loop). Derived from observed signals, not
	// from any planner (the planner was deleted). Empty means not observable.
	ActualExecutionPath string               `json:"actual_execution_path,omitempty"`
	Runtime             RuntimeTrace         `json:"runtime"`
	IntentRouter        RouterTrace          `json:"intent_router"`
	EngineHardBlock     EngineHardBlockTrace `json:"engine_hard_block"`
	EntityRegistry      EntityRegistryTrace  `json:"entity_registry"`
	ToolCalls           []ToolCallTrace      `json:"tool_calls"`
	Renderer            RendererTrace        `json:"renderer"`
	Freshness           FreshnessTrace       `json:"freshness"`
	RateLimit           RateLimitTrace       `json:"rate_limit"`
	Retrieval           RetrievalTrace       `json:"retrieval"`
	Diagnosis           DiagnosisTrace       `json:"diagnosis"`
	State               StateTrace           `json:"state"`
	Continuity          ContinuityTrace      `json:"continuity"`
	Completion          TurnCompletionTrace  `json:"completion"`
	Outcome             OutcomeTrace         `json:"outcome"`
	// Steps holds workflow step traces. Empty values are omitted for turns that
	// did not execute a workflow.
	Steps []StepTrace `json:"steps,omitempty"`
	// Authorizations holds the per-target dual-proof audit record for each write a
	// mutating action authorized this turn. Empty / omitempty for every non-mutating
	// turn, so trace output stays byte-identical until a write target is proven
	// (same reserved-slot precedent as Steps).
	Authorizations []AuthorizationTrace `json:"authorizations,omitempty"`
}

type traceRecordJSON struct {
	SchemaVersion       string                `json:"schema_version"`
	TraceID             string                `json:"trace_id"`
	TurnID              string                `json:"turn_id"`
	TurnIndex           int                   `json:"turn_index"`
	Timestamp           string                `json:"timestamp"`
	UserMsgHash         string                `json:"user_msg_hash"`
	TaskTier            string                `json:"task_tier,omitempty"`
	ActualExecutionTier string                `json:"actual_execution_tier,omitempty"`
	ActualExecutionPath string                `json:"actual_execution_path,omitempty"`
	Runtime             *RuntimeTrace         `json:"runtime,omitempty"`
	IntentRouter        *RouterTrace          `json:"intent_router,omitempty"`
	EngineHardBlock     *EngineHardBlockTrace `json:"engine_hard_block,omitempty"`
	EntityRegistry      *EntityRegistryTrace  `json:"entity_registry,omitempty"`
	ToolCalls           []ToolCallTrace       `json:"tool_calls,omitempty"`
	Renderer            *RendererTrace        `json:"renderer,omitempty"`
	Freshness           *FreshnessTrace       `json:"freshness,omitempty"`
	RateLimit           *RateLimitTrace       `json:"rate_limit,omitempty"`
	Retrieval           *RetrievalTrace       `json:"retrieval,omitempty"`
	Diagnosis           *DiagnosisTrace       `json:"diagnosis,omitempty"`
	State               *StateTrace           `json:"state,omitempty"`
	Continuity          *ContinuityTrace      `json:"continuity,omitempty"`
	Completion          *TurnCompletionTrace  `json:"completion,omitempty"`
	Outcome             *OutcomeTrace         `json:"outcome,omitempty"`
	Steps               []StepTrace           `json:"steps,omitempty"`
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
		TaskTier:            r.TaskTier,
		ActualExecutionTier: r.ActualExecutionTier,
		ActualExecutionPath: r.ActualExecutionPath,
	}
	if traceRuntimeObserved(r.Runtime) {
		out.Runtime = &r.Runtime
	}
	if tracePlannerObserved(r.IntentRouter) {
		out.IntentRouter = &r.IntentRouter
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
	if traceContinuityObserved(r.Continuity) {
		out.Continuity = &r.Continuity
	}
	if traceCompletionObserved(r.Completion) {
		out.Completion = &r.Completion
	}
	if traceOutcomeObserved(r.Outcome) {
		out.Outcome = &r.Outcome
	}
	if len(r.Steps) > 0 {
		out.Steps = r.Steps
	}
	if len(r.Authorizations) > 0 {
		out.Authorizations = r.Authorizations
	}
	return json.Marshal(out)
}

// ContinuityTrace records the durable turn protocol around one execution
// attempt. It deliberately contains only identity checks, counters, versions
// and closed-set outcomes: no user text, model text, tool payload or persisted
// session JSON is allowed across this observability boundary.
type ContinuityTrace struct {
	SessionIdentityMatch   bool   `json:"session_identity_match"`
	TurnSequence           int64  `json:"turn_sequence"`
	LeaseEpoch             int64  `json:"lease_epoch"`
	SnapshotContextVersion int    `json:"snapshot_context_version"`
	EnvelopeParseOutcome   string `json:"envelope_parse_outcome"`
	ContextParseOutcome    string `json:"context_parse_outcome"`
	RetryCount             int    `json:"retry_count"`
	RecoveryAttempt        bool   `json:"recovery_attempt"`
	CommitOutcome          string `json:"commit_outcome"`
	CommitReason           string `json:"commit_reason,omitempty"`
}

func traceContinuityObserved(trace ContinuityTrace) bool {
	return trace.TurnSequence != 0 || trace.LeaseEpoch != 0 ||
		trace.SnapshotContextVersion != 0 || trace.EnvelopeParseOutcome != "" ||
		trace.ContextParseOutcome != "" || trace.RetryCount != 0 ||
		trace.RecoveryAttempt || trace.CommitOutcome != "" || trace.CommitReason != ""
}

// LEGACY TRACE COMPAT: these two axes describe records produced before the
// central-Agent cutover. They do not select a model or execution path today.
// Keep them readable until the stored trace schema is retired.
// TestActualExecutionTierAndExecutionPathAreSeparateAxes pins the divergence.
//
//   - Work-tier axis (TaskTier predicted / ActualExecutionTier realized): WHAT KIND of
//     work the turn did, on the complexity scale fast < knowledge < agent.
//   - Runtime-form axis (PlannedExecutionPath / ActualExecutionPath): the LEGACY
//     runtime-form label — routing / terminal_rag / agent (see the ExecutionPath*
//     const note). Retained for trace continuity; the current runtime has one form
//     (the central Agent loop), so this axis is legacy trace-compat, not current.
//
// The axes correspond only loosely (fast↔routing, knowledge↔terminal_rag,
// agent↔agent): a turn can do knowledge-tier WORK on the agent FORM — a
// knowledge_qa turn forced through the ReAct loop, or diagnosis that retrieves
// mid-flow, both realize knowledge while their runtime form is agent (see
// DeriveActualExecutionTier / DeriveActualExecutionPath).

// ActualExecutionTier* are the work-tier-axis values for TraceRecord.ActualExecutionTier (and
// the predicted TaskTier). Mirror the task-complexity tiers.
const (
	ActualExecutionTierFast      = "fast"
	ActualExecutionTierKnowledge = "knowledge"
	ActualExecutionTierAgent     = "agent"
)

// LEGACY TRACE COMPAT (post-P6). The routing / terminal_rag / agent runtime-form
// taxonomy predates the central-Agent cutover. The CURRENT runtime has a single
// execution form — the central Agent loop — so these values are retained ONLY so
// trace storage, dashboards, and the eval harness keep reading historical and
// cutover-era records. Do NOT treat them as the current architecture: P7 (and any
// new acceptance) must judge a turn by its real tools / steps / observations /
// final answer, not by a derived form label. Removing them is a wire-visible
// trace-schema migration, not a comment/test cleanup.
//
// ExecutionPath* are the runtime-form-axis values for TraceRecord.ActualExecutionPath
// (and PlannedExecutionPath). NOTE: "terminal_rag" is the odd value out vs the
// rest of the routing/agent vocabulary; renaming it to "rag" is a wire-visible
// trace-schema change and must be done as its own compatibility-aware migration,
// not folded into a comment/test cleanup.
const (
	ExecutionPathRouting     = "routing"
	ExecutionPathTerminalRAG = "terminal_rag"
	ExecutionPathAgent       = "agent"
)

// DeriveActualExecutionTier computes the tier the turn ACTUALLY ran on from observed
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
func (r TraceRecord) DeriveActualExecutionTier() string {
	// route_status literals mirror internal/intent/handler.go:42-58
	// (RouteStatus*); pinned by TestDeriveActualExecutionTier.
	switch r.IntentRouter.RouteStatus {
	case "dispatched_retrieval", "dispatched_knowledge_agent_loop":
		// dispatched_knowledge_agent_loop is a knowledge_qa turn forced through the
		// knowledge agent loop: the realized work is still
		// knowledge retrieval, so the realized-tier attribution stays comparable
		// across the terminal→agent-loop migration even though the runtime FORM
		// becomes agent (see DeriveActualExecutionPath).
		return ActualExecutionTierKnowledge
	case "dispatched_agent":
		return ActualExecutionTierAgent
	case "dispatched", "selection_required":
		return ActualExecutionTierFast
	}
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

// DeriveActualExecutionPath computes the LEGACY runtime-form label for a turn
// (see the ExecutionPath* const note). It maps a trace's router status to the
// retired routing / terminal_rag / agent taxonomy for trace/dashboard continuity;
// it is NOT the current architecture (one central Agent form). Kept coarse:
// terminal RAG is only a final-answer retrieval workflow; retrieval used inside
// diagnosis or another agent path remains agent.
func (r TraceRecord) DeriveActualExecutionPath() string {
	switch r.IntentRouter.RouteStatus {
	case "dispatched_agent", "dispatched_knowledge_agent_loop":
		// dispatched_knowledge_agent_loop: a knowledge_qa turn forced through the
		// shared ReAct loop. It runs the agent
		// loop (a SearchKnowledge tool call fires), so the runtime FORM is agent —
		// the migration's whole point (terminal_rag → agent). The engine projects
		// PlannedExecutionPath=agent for the same turn so planned==actual.
		return ExecutionPathAgent
	case "dispatched_retrieval":
		return ExecutionPathTerminalRAG
	case "dispatched", "selection_required":
		return ExecutionPathRouting
	}
	if len(r.Steps) > 0 {
		return ExecutionPathAgent
	}
	for _, call := range r.ToolCalls {
		switch call.Source {
		case ToolSourceMainReAct, ToolSourceWorkflowInternal, ToolSourceDiagnosisInternal, ToolSourceKnowledgeLocal:
			return ExecutionPathAgent
		}
	}
	if r.Retrieval.Enabled && r.Retrieval.Hits > 0 {
		return ExecutionPathTerminalRAG
	}
	for _, call := range r.ToolCalls {
		if call.Source == ToolSourcePlannerHandler {
			return ExecutionPathRouting
		}
	}
	return ""
}

func (r TraceRecord) ExecutionPathMismatch() (bool, bool) {
	planned := strings.TrimSpace(r.IntentRouter.PlannedExecutionPath)
	actual := strings.TrimSpace(r.ActualExecutionPath)
	if actual == "" {
		actual = strings.TrimSpace(r.DeriveActualExecutionPath())
	}
	if planned == "" || actual == "" {
		return false, false
	}
	return planned != actual, true
}

type RuntimeTrace struct {
	RouterMode   string   `json:"router_mode"`
	RouteIntents []string `json:"route_intents"`
}

type RouterTrace struct {
	Enabled              bool                `json:"enabled"`
	Model                string              `json:"model"`
	LatencyMS            int64               `json:"latency_ms"`
	InputTokens          int                 `json:"input_tokens"`
	OutputTokens         int                 `json:"output_tokens"`
	SchemaValid          bool                `json:"schema_valid"`
	Intent               string              `json:"intent"`
	PlannedExecutionPath string              `json:"planned_execution_path,omitempty"`
	Skills               []PlannerSkillTrace `json:"skills,omitempty"`
	Slots                PlannerSlots        `json:"slots"`
	Confidence           float64             `json:"confidence"`
	HardBlockHint        bool                `json:"hard_block_hint"`
	RouteStatus          string              `json:"route_status"`

	// Why the router fell back — the question route_status alone cannot answer.
	//
	// When validation fails on every attempt, ProjectPlannerTrace overwrites Intent
	// with "unknown" and the engine reports route_status=fallback_invalid. Both the
	// intent the model actually chose and the reason it was rejected were then
	// discarded, so `fallback_invalid` has been an opaque bucket. On real 6.26-7.9
	// traffic it is 6.0% of follow-up turns vs 1.0% of opening turns — a 6x
	// multi-turn degradation that nothing in the system could explain.
	//
	// ValidationCode is an ErrorCode enum and ValidationField a schema path
	// (e.g. "slots.target_refs[0].value"). Neither carries user text, so this is
	// safe to leave on in production — the same counts-and-enums-only rule
	// PlannerSlots follows.
	ValidationCode  string `json:"validation_code,omitempty"`
	ValidationField string `json:"validation_field,omitempty"`
	// RejectedIntent is the intent the model chose on the final failing attempt —
	// preserved BEFORE the unknown-overwrite above, so a rejected-but-correct route
	// is distinguishable from a genuinely off-platform one.
	RejectedIntent string `json:"rejected_intent,omitempty"`
	Attempts       int    `json:"attempts,omitempty"`
}

type PlannerSkillTrace struct {
	Name       string `json:"name,omitempty"`
	Resolution string `json:"resolution"`
}

// PlannerSlots projects intent.Slots for the trace. Nothing here may carry raw
// model- or user-supplied text, so each slot is recorded under one of two
// classes, following the TargetRef/TimeWindow precedent already established in
// this file:
//
//   - verbatim, when the value is provably confined to a closed set. Those slots
//     ARE the router's decision variable, and a trace that hides them cannot
//     explain why a turn was refined the way it was.
//   - hashed, when the value is free-form. A hash still shows whether the slot
//     was populated and whether it was stable across runs, without carrying the
//     raw text.
//
// "Provably confined" means confined by the time it reaches here — not merely
// enum-typed in Go. ImageSource/ListMode/PriceKind/CFSKind/ChargeType/DetailLevel
// and SizeGB used to be checked by intent.ValidateRoute, which was deleted with
// the route stack (it had zero production callers by then, so the check it is
// credited with here had already stopped running before it was removed). Their
// confinement now rests on whatever produces the plan, not on a validator — if a
// producer is ever added that does not confine them, this projection would record
// unconfined text verbatim, so re-establish the check before trusting it. Action is
// NOT: the router schema deliberately omits slots.action and is non-strict, so a
// model that volunteers an arbitrary string still produces a SchemaValid plan
// (see internal/intent/router_schema.go). It is therefore closed at projection
// time against the known LifecycleAction constants — a known verb is recorded
// verbatim, anything else is hashed, exactly as a non-canonical TimeWindow is.
type PlannerSlots struct {
	TargetRefs []any    `json:"target_refs"`
	Metrics    []string `json:"metrics"`
	TimeWindow any      `json:"time_window"`

	// Validator-constrained / bounded — verbatim.
	ImageSource string `json:"image_source,omitempty"`
	ListMode    string `json:"list_mode,omitempty"`
	PriceKind   string `json:"price_kind,omitempty"`
	CFSKind     string `json:"cfs_kind,omitempty"`
	ChargeType  string `json:"charge_type,omitempty"`
	DetailLevel string `json:"detail_level,omitempty"`
	SizeGB      int    `json:"size_gb,omitempty"`

	// Closed at projection time: verbatim iff a known LifecycleAction.
	Action     string `json:"action,omitempty"`
	ActionHash string `json:"action_hash,omitempty"`

	// Free-form user-derived slots — hashed, never raw.
	SearchQueryHash string `json:"search_query_hash,omitempty"`
	ZoneHash        string `json:"zone_hash,omitempty"`
}

// EngineHardBlock TriggeredBy enum values. Single-source attribution
// (no "both") — see EngineHardBlockTrace.TriggeredBy doc.
const (
	HardBlockTriggerKeyword       = "keyword"
	HardBlockTriggerPlannerIntent = "planner_intent"
	HardBlockTriggerPostLLM       = "post_llm"
	HardBlockTriggerTokenBudget   = "token_budget"
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
	// hard-block — single-source (no "both"), since short-circuited stages
	// are unobservable. Allowed values:
	//   "keyword"        — Chat() head inputguard.PreBlock keyword match
	//   "planner_intent" — planner-classified intent hard-block (e.g. the
	//                      account-billing-unsupported refusal)
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

// AuthorizationTrace is the per-write-target dual-proof audit record: for each
// target a mutating action acted on this turn, it captures WHICH read interface
// (oracle) established the target's existence, WHEN, for WHICH account, with what
// VERDICT (the ExistenceProof), plus whether the user's confirmation authorized
// execution (the SelectionProof outcome). It exists so a post-hoc audit can answer
// "由哪个接口、在什么时间、为哪个账户证明了这个目标存在" — which the target's
// existence evidence (engine.targetEvidence) established but which was previously
// consumed only as a resolver gate and then discarded.
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
// can retrieve more than once (e.g. the knowledge agent loop's forced
// SearchKnowledge first hop, then a voluntary re-query later in the same ReAct loop).
// The recorders historically kept only the LAST retrieval, so a trailing no-hit
// re-query would clobber the forced hop's substantive retrieval and make it look like
// nothing was retrieved. Preserve the substantive one: keep the existing hits-bearing
// retrieval when the incoming one has no hits; otherwise the latest (substantive or
// first-ever) wins. Terminal RAG retrieves once, so its trace is unchanged.
func MergeRetrievalTrace(current, next RetrievalTrace) RetrievalTrace {
	if next.TurnAggregate || len(next.CitedChunkIDs) > 0 || len(next.CitedRefs) > 0 {
		if !traceRetrievalObserved(current) {
			return next
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
		return merged
	}
	if current.Enabled && current.Hits > 0 && (!next.Enabled || next.Hits == 0) {
		return current
	}
	return next
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
	// RefusalType classifies a RAG refusal into the #5 four-state taxonomy
	// (corpus_gap / all_below_floor / synthesis_refused / wrong_domain). Derived
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
	FloorValue float64 `json:"floor_value,omitempty"`
	// DomainInferenceEmpty is true when the question's product area could not be
	// inferred, so the #5 wrong-domain guard could not judge this turn. Recorded
	// so a low wrong_domain rate is not misread as "no problem" (the
	// question-side routing coverage gap).
	DomainInferenceEmpty bool `json:"domain_inference_empty,omitempty"`
	// AllCitedOffDomain is true when every judgeable retrieved chunk was off the
	// question's product area (the #5 case: a 库存 question grounded on billing
	// chunks). Trace-only by default; the COMPSHARE_RAG_DOMAIN_MATCH_GUARD refuse
	// arm turns it into refusal_type=wrong_domain.
	AllCitedOffDomain     bool `json:"all_cited_off_domain,omitempty"`
	WeakEvidence          bool `json:"weak_evidence,omitempty"`
	RankingErrorCandidate bool `json:"ranking_error_candidate,omitempty"`
	// AnswerEchoedChunkID names the chunk whose body the final answer reproduced
	// verbatim (>=32 contiguous runes), empty when it paraphrased. Trace-only: a
	// customer-safe corpus makes an echo a synthesis-quality signal, not a leak,
	// and it must never gate the answer — it used to, and replaced whole correct
	// runbook answers with a canned line. Query it to measure copy-instead-of-write.
	AnswerEchoedChunkID string `json:"answer_echoed_chunk_id,omitempty"`
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
	// Continuity contract metadata is bounded and content-free. It proves which
	// inputs and answer path were used without persisting prompts or user text.
	ContextSources     []string `json:"context_sources,omitempty"`
	ResponseContract   string   `json:"response_contract,omitempty"`
	PromptSectionIDs   []string `json:"prompt_section_ids,omitempty"`
	MemoryUpdateSource string   `json:"memory_update_source,omitempty"`
	GroundingOutcome   string   `json:"grounding_outcome,omitempty"`
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
	ActionProposalDisposition  string `json:"action_proposal_disposition,omitempty"`
	TotalLatencyMS             int64  `json:"total_latency_ms,omitempty"`
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
	return trace.RouterMode != "" || len(trace.RouteIntents) > 0
}

func tracePlannerObserved(trace RouterTrace) bool {
	return trace.Enabled ||
		trace.Model != "" ||
		trace.LatencyMS != 0 ||
		trace.InputTokens != 0 ||
		trace.OutputTokens != 0 ||
		trace.SchemaValid ||
		trace.Intent != "" ||
		trace.PlannedExecutionPath != "" ||
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
		len(trace.Activities) > 0 ||
		len(trace.References) > 0 ||
		len(trace.CitedRefs) > 0 ||
		trace.RefusedReason != "" ||
		trace.RefusalType != "" ||
		trace.FloorDroppedAll ||
		trace.FloorValue != 0 ||
		trace.DomainInferenceEmpty ||
		trace.AllCitedOffDomain ||
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
		trace.MemoryUpdateSource != "" ||
		trace.GroundingOutcome != "" ||
		trace.ActionProposalDisposition != ""
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
	if r.IntentRouter.Slots.TargetRefs == nil {
		r.IntentRouter.Slots.TargetRefs = []any{}
	}
	if r.IntentRouter.Slots.Metrics == nil {
		r.IntentRouter.Slots.Metrics = []string{}
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
