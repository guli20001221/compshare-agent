package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/compshare-agent/internal/embedding"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/reranker"
)

type getenvFunc func(string) string

func traceWriterFromEnv(getenv getenvFunc) (observability.Writer, bool, error) {
	if getenv("COMPSHARE_TRACE_ENABLED") != "1" {
		return nil, false, nil
	}
	sink := strings.ToLower(strings.TrimSpace(getenv("COMPSHARE_TRACE_SINK")))
	if sink == "" {
		sink = "file"
	}
	dir := getenv("COMPSHARE_TRACE_DIR")
	dsn := getenv("MYSQL_DSN")

	switch sink {
	case "file":
		writer, err := observability.NewWriter(observability.WriterOptions{Dir: dir})
		if err != nil {
			return nil, false, err
		}
		return writer, true, nil
	case "mysql":
		writer, err := observability.NewMySQLWriter(dsn, observability.MySQLWriterOptions{})
		if err != nil {
			return nil, false, err
		}
		return writer, true, nil
	case "both":
		fileW, err := observability.NewWriter(observability.WriterOptions{Dir: dir})
		if err != nil {
			return nil, false, err
		}
		mysqlW, err := observability.NewMySQLWriter(dsn, observability.MySQLWriterOptions{})
		if err != nil {
			_ = fileW.Close(context.Background())
			return nil, false, err
		}
		return multiTraceWriter{fileW, mysqlW}, true, nil
	default:
		return nil, false, fmt.Errorf("unknown COMPSHARE_TRACE_SINK value %q (want file|mysql|both)", sink)
	}
}

func traceMySQLSinkEnabled(getenv getenvFunc) bool {
	if getenv("COMPSHARE_TRACE_ENABLED") != "1" {
		return false
	}
	sink := strings.ToLower(strings.TrimSpace(getenv("COMPSHARE_TRACE_SINK")))
	return sink == "mysql" || sink == "both"
}

// multiTraceWriter fans out a TraceRecord to multiple sinks. Used when
// COMPSHARE_TRACE_SINK=both during cutover (run file + mysql side-by-side
// to compare). Failures from any individual sink are logged-then-ignored
// so one sink's downtime does not stall the other.
type multiTraceWriter []observability.Writer

func (m multiTraceWriter) Append(rec observability.TraceRecord) error {
	for _, w := range m {
		if err := w.Append(rec); err != nil {
			log.Printf("trace sink append failed (sink dir=%q): %v", w.Dir(), err)
		}
	}
	return nil
}

func (m multiTraceWriter) EmitStep(step observability.StepTrace) error {
	var firstErr error
	for _, w := range m {
		if err := w.EmitStep(step); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m multiTraceWriter) Enqueue(tenant observability.TenantContext, rec observability.TraceRecord) error {
	for _, w := range m {
		if enqueuer, ok := w.(interface {
			Enqueue(observability.TenantContext, observability.TraceRecord) error
		}); ok {
			if err := enqueuer.Enqueue(tenant, rec); err != nil {
				log.Printf("trace sink enqueue failed (sink dir=%q): %v", w.Dir(), err)
			}
			continue
		}
		if err := w.Append(rec); err != nil {
			log.Printf("trace sink append failed (sink dir=%q): %v", w.Dir(), err)
		}
	}
	return nil
}

func (m multiTraceWriter) Dir() string {
	for _, w := range m {
		if d := w.Dir(); d != "" {
			return d
		}
	}
	return ""
}

func (m multiTraceWriter) Close(ctx context.Context) error {
	var firstErr error
	for _, w := range m {
		if err := w.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func cleanupTraceWriter(writer observability.Writer, now time.Time) error {
	if writer == nil {
		return nil
	}
	// MySQLWriter returns "" — Cleanup is a no-op on empty dir, which is
	// correct (nothing to delete on disk for the db-backed sink).
	dir := writer.Dir()
	if dir == "" {
		return nil
	}
	return observability.Cleanup(dir, observability.DefaultTraceRetentionDays, now)
}

func intentPlannerShadowEnabled(getenv getenvFunc) bool {
	return getenv("COMPSHARE_INTENT_ROUTER_MODE") == "shadow"
}

func plannerRuntimeModeLine(shadowEnabled, plannerDispatchEnabled bool, routeIntents []intent.Intent) string {
	mode := "off"
	if plannerDispatchEnabled {
		mode = "dispatch"
	} else if shadowEnabled {
		mode = "shadow"
	}
	return fmt.Sprintf("router_mode=%s route_intents=%s", mode, formatRouteIntents(routeIntents))
}

func groundedRendererRuntimeLine(mode string) string {
	if mode == "" {
		mode = "off"
	}
	return fmt.Sprintf("grounded_renderer=%s", mode)
}

func mutatingToolsEnabledFromEnv(getenv getenvFunc) (bool, string) {
	value := strings.TrimSpace(getenv("COMPSHARE_ENABLE_MUTATING_TOOLS"))
	switch value {
	case "":
		return false, ""
	case "1":
		return true, ""
	default:
		return false, value
	}
}

func mutatingToolsRuntimeLine(enabled bool) string {
	if enabled {
		return "mutating=enabled"
	}
	return "mutating=disabled (read-only mode)"
}

func sessionFactContextEnabledFromEnv(getenv getenvFunc) (bool, string) {
	value := strings.TrimSpace(getenv("USE_SESSION_FACT_CONTEXT"))
	switch value {
	case "", "0":
		return false, ""
	case "1":
		return true, ""
	default:
		return false, value
	}
}

func reactResultProjectionEnabledFromEnv(getenv getenvFunc) (bool, string) {
	value := strings.TrimSpace(getenv("USE_REACT_RESULT_PROJECTION"))
	switch value {
	case "", "0":
		return false, ""
	case "1":
		return true, ""
	default:
		return false, value
	}
}

func reactHistoryCompactionEnabledFromEnv(getenv getenvFunc) (bool, string) {
	value := strings.TrimSpace(getenv("USE_REACT_HISTORY_COMPACTION"))
	switch value {
	case "", "0":
		return false, ""
	case "1":
		return true, ""
	default:
		return false, value
	}
}

func intentScopedReActPromptEnabledFromEnv(getenv getenvFunc) (bool, string) {
	value := strings.TrimSpace(getenv("USE_INTENT_SCOPED_REACT_PROMPT"))
	switch value {
	case "", "0":
		return false, ""
	case "1":
		return true, ""
	default:
		return false, value
	}
}

type plannerStructuredOutputMode string

const (
	plannerStructuredOutputOff        plannerStructuredOutputMode = ""
	plannerStructuredOutputJSONObject plannerStructuredOutputMode = "json_object"
	plannerStructuredOutputJSONSchema plannerStructuredOutputMode = "json_schema"
)

func plannerStructuredOutputModeFromEnv(getenv getenvFunc) (plannerStructuredOutputMode, string) {
	value := strings.ToLower(strings.TrimSpace(getenv("COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT")))
	switch value {
	case "", "0", "off":
		return plannerStructuredOutputOff, ""
	case string(plannerStructuredOutputJSONObject):
		return plannerStructuredOutputJSONObject, ""
	case string(plannerStructuredOutputJSONSchema):
		return plannerStructuredOutputJSONSchema, ""
	default:
		return plannerStructuredOutputOff, value
	}
}

// useSkillExecutorFromEnv reads USE_SKILL_EXECUTOR (P2a gray-rollout). "1"
// enables the body-driven skill executor gate. Diagnosis still requires the
// USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS allowlist. "" is off; any other value is
// unknown and treated as off (caller warns). Default off; boot-only, flips need a
// restart.
func useSkillExecutorFromEnv(getenv getenvFunc) (bool, string) {
	value := strings.TrimSpace(getenv("USE_SKILL_EXECUTOR"))
	switch value {
	case "":
		return false, ""
	case "1":
		return true, ""
	default:
		return false, value
	}
}

// agenticSearchKnowledgeEnabledFromEnv gates the agentic-RAG SearchKnowledge
// registry tool (P3/P4a). DEFAULT ON. When on, a symptom/tool-ops diagnosis turn
// can call SearchKnowledge for prior tool/ops evidence before any Diagnose* tool,
// and the empty-target which-instance dead-end is relaxed so the loop reaches that
// retrieval. ""/1/true/yes/on => on; 0/off/false/no => off; unknown => off +
// non-empty warn string (CLAUDE.md: never silently coerce). Boot-only: resolved
// once in cmd (CLI + HTTP) and frozen via tools.SetAgenticSearchKnowledgeEnabled;
// the Go-package default (tools.agenticSearchKnowledgeOn) stays false so the
// engine/tools unit tests are unaffected. Rollback = COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE=0.
//
// WHY DEFAULT-ON (2026-06-07): enabled TOGETHER with COMPSHARE_EXTERNAL_KNOWLEDGE
// (the agentic value comes from the external tool/ops KB — agentic-alone at
// external-off had no positive value and a false-grounding risk, see the P5 report).
// The joint enablement was eval-gated on merged main: the 34-probe joint eval
// (170 runs) showed no platform-hosted-API vs self-hosted-service confusion and a
// clean regression set, and the platform-FAQ faithfulness eval (15 probes x both
// conditions) showed ZERO external-corpus contamination of platform answers. See
// eval/trace_gate/{joint_onmain_anchor_observations.jsonl, agentic_rag_default_on_report.md}.
func agenticSearchKnowledgeEnabledFromEnv(getenv getenvFunc) (bool, string) {
	raw := strings.TrimSpace(getenv("COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE"))
	switch strings.ToLower(raw) {
	case "", "1", "true", "yes", "on":
		return true, ""
	case "0", "off", "no", "false", "disabled", "none":
		return false, ""
	default:
		return false, raw
	}
}

// groundedAnswerValidatorEnabledFromEnv gates the route-independent grounded-answer
// (cite + leak) validator on the agentic SearchKnowledge synthesis (#126). DEFAULT
// OFF — deliberately separate from COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE (default-on)
// because the cite contract must stay off until a flag-on eval proves the agent
// attributes its answer at the hard-gate bar (100% cite-or-refuse / 0 raw-leak).
// When on, the SearchKnowledge tool result carries a [[chunk_id]] cite_protocol and
// a synthesis that does not cite a retrieved chunk (or cites an unknown one) is
// replaced with the canned no-evidence reply. ""/0/off/false/no => off;
// 1/true/yes/on => on; unknown => off + non-empty warn string (CLAUDE.md: never
// silently coerce). Boot-only; the Go-package default (engine.groundedAnswerValidatorOn)
// stays false so engine/tools unit tests are unaffected.
func groundedAnswerValidatorEnabledFromEnv(getenv getenvFunc) (bool, string) {
	raw := strings.TrimSpace(getenv("COMPSHARE_RAG_GROUNDED_VALIDATOR"))
	switch strings.ToLower(raw) {
	case "", "0", "off", "no", "false", "disabled", "none":
		return false, ""
	case "1", "true", "yes", "on":
		return true, ""
	default:
		return false, raw
	}
}

// domainMatchGuardEnabledFromEnv gates the #5 wrong-domain REFUSE arm
// (COMPSHARE_RAG_DOMAIN_MATCH_GUARD). DEFAULT OFF — the domain verdict is always
// recorded in the trace (all_cited_off_domain / domain_inference_empty), but the
// synthesis is replaced with a refusal only when this is on. Kept off until a
// flag-on eval proves 0 over-refusal (an over-eager domain refusal would suppress
// legitimate answers whenever inferKnowledgeProductArea and the chunk product_area
// tags disagree on a true match). ""/0/off/... => off; 1/true/yes/on => on;
// unknown => off + non-empty warn string (CLAUDE.md: never silently coerce).
// Boot-only; the Go-package default (engine.domainMatchGuardOn) stays false so
// engine/knowledge unit tests are unaffected.
func domainMatchGuardEnabledFromEnv(getenv getenvFunc) (bool, string) {
	raw := strings.TrimSpace(getenv("COMPSHARE_RAG_DOMAIN_MATCH_GUARD"))
	switch strings.ToLower(raw) {
	case "", "0", "off", "no", "false", "disabled", "none":
		return false, ""
	case "1", "true", "yes", "on":
		return true, ""
	default:
		return false, raw
	}
}

// flashKnowledgeRouteGuardEnabledFromEnv gates the default-off flash route
// fallback for a small set of product-fact questions that can otherwise be sent
// to live tools. This is not part of the primary routing strategy.
func flashKnowledgeRouteGuardEnabledFromEnv(getenv getenvFunc) (bool, string) {
	raw := strings.TrimSpace(getenv("COMPSHARE_FLASH_KNOWLEDGE_ROUTE_GUARD"))
	switch strings.ToLower(raw) {
	case "", "0", "off", "no", "false", "disabled", "none":
		return false, ""
	case "1", "true", "yes", "on":
		return true, ""
	default:
		return false, raw
	}
}

// createPreferenceExtractorEnabledFromEnv gates the optional create/deploy
// preference extractor. DEFAULT ON: it adds one LLM pass before create/deploy
// image matching, and the extracted fields only affect preference matching,
// never routing or final workflow validation.
func createPreferenceExtractorEnabledFromEnv(getenv getenvFunc) (bool, string) {
	raw := strings.TrimSpace(getenv("COMPSHARE_CREATE_PREF_EXTRACTOR"))
	switch strings.ToLower(raw) {
	case "", "1", "true", "yes", "on":
		return true, ""
	case "0", "off", "no", "false", "disabled", "none":
		return false, ""
	default:
		return false, raw
	}
}

// unifiedCreateEnabledFromEnv gates the R2b first-class create_instance route.
// DEFAULT ON: the router prompt/schema expose create_instance by default. Set
// COMPSHARE_UNIFIED_CREATE=0/off/false to roll back during soak.
func unifiedCreateEnabledFromEnv(getenv getenvFunc) (bool, string) {
	raw := strings.TrimSpace(getenv("COMPSHARE_UNIFIED_CREATE"))
	switch strings.ToLower(raw) {
	case "", "1", "true", "yes", "on":
		return true, ""
	case "0", "off", "no", "false", "disabled", "none":
		return false, ""
	default:
		return false, raw
	}
}

// contextContinuationEnabledFromEnv gates the global LLM-backed context
// continuation layer. DEFAULT ON: short follow-ups may resume create/deploy
// frames and mutating workflow tasks, but final execution still goes through
// workflow validation and confirmation cards. Set =0/off/false to roll back.
func contextContinuationEnabledFromEnv(getenv getenvFunc) (bool, string) {
	raw := strings.TrimSpace(getenv("COMPSHARE_CONTEXT_CONTINUATION"))
	switch strings.ToLower(raw) {
	case "", "1", "true", "yes", "on":
		return true, ""
	case "0", "off", "no", "false", "disabled", "none":
		return false, ""
	default:
		return false, raw
	}
}

// knowledgeQAAgentLoopEnabledFromEnv gates the terminal-knowledge_qa → agent-loop
// route (COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP). DEFAULT ON (2026-06-09) — a knowledge_qa
// turn routes through the agent loop: a forced SearchKnowledge first hop retrieves
// evidence, then the disciplined-synthesis primitive writes the final cited answer
// (see disciplinedKnowledgeQASynthesisEnabledFromEnv, also default-on). This collapses the
// separate deterministic terminal-RAG route into the single agent loop (the lead's
// "rag as a tool the agent calls in a loop" north star). The flip was gated on the
// #150 A/B: on the decisive code-heavy probe (PyTorch DDP, N=20) the
// agent-loop+disciplined answer matched terminal RAG — refusal 0.00 == terminal, 0
// fabrication / 0 contamination (opus-4-7 judge); the other 7 real-tone probes were
// already 0-refusal. The terminal route (tryStage2BRetrieval) is retained as the =0
// rollback, not deleted. Inert unless COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE is also on
// (the tool must be visible) and a retriever is wired — the engine route gate enforces
// both, so the forced first hop can never name an absent tool (the 400 trap).
// ""/1/true/yes/on => on; 0/off/false/no => off; unknown => off + non-empty warn string
// (CLAUDE.md: never silently coerce). Boot-only; the Go-package default
// (engine.knowledgeQAAgentLoopOn) stays false so engine/tools unit tests are
// unaffected. Rollback = COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP=0.
func knowledgeQAAgentLoopEnabledFromEnv(getenv getenvFunc) (bool, string) {
	raw := strings.TrimSpace(getenv("COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP"))
	switch strings.ToLower(raw) {
	case "", "1", "true", "yes", "on":
		return true, ""
	case "0", "off", "no", "false", "disabled", "none":
		return false, ""
	default:
		return false, raw
	}
}

// disciplinedKnowledgeQASynthesisEnabledFromEnv gates the disciplined-synthesis primitive on
// an agent-loop knowledge_qa turn (COMPSHARE_KNOWLEDGE_QA_DISCIPLINED_SYNTHESIS). DEFAULT ON
// (2026-06-09) and effective only when COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP is also on:
// the FINAL answer for the turn is written by terminal RAG's tight cited-synthesis
// prompt (answerWithRetrievedEvidence, with its own cite-harder retry) on the evidence
// the agent gathered via SearchKnowledge — rather than the free ReAct write, which
// under flash intermittently omits the cite or dumps raw text. This is what made the
// agent loop match terminal on faithfulness/refusal in the #150 A/B (DDP N=20: refusal
// 0.00, 0 fab; see knowledgeQAAgentLoopEnabledFromEnv). On synthesis failure it falls
// through to the existing cite-retry/refusal, so it is never worse than free-write.
// ""/1/true/yes/on => on; 0/off/false/no => off; unknown => off + non-empty warn
// (CLAUDE.md: never silently coerce). Boot-only; the Go-package default
// (engine.disciplinedKnowledgeQASynthesisOn) stays false so unit tests are unaffected.
func disciplinedKnowledgeQASynthesisEnabledFromEnv(getenv getenvFunc) (bool, string) {
	raw := strings.TrimSpace(getenv("COMPSHARE_KNOWLEDGE_QA_DISCIPLINED_SYNTHESIS"))
	switch strings.ToLower(raw) {
	case "", "1", "true", "yes", "on":
		return true, ""
	case "0", "off", "no", "false", "disabled", "none":
		return false, ""
	default:
		return false, raw
	}
}

func skillExecutorDiagnosisPilotsFromEnv(getenv getenvFunc) ([]string, []string) {
	raw := strings.TrimSpace(getenv("USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS"))
	if raw == "" {
		return nil, nil
	}
	known := map[string]struct{}{}
	for _, name := range engine.KnownDiagnosisSkillExecutorPilots() {
		known[name] = struct{}{}
	}
	seenKnown := map[string]struct{}{}
	seenUnknown := map[string]struct{}{}
	var pilots []string
	var unknown []string
	for _, part := range strings.Split(raw, ",") {
		rawName := strings.TrimSpace(part)
		if rawName == "" {
			continue
		}
		name := engine.CanonicalDiagnosisSkillName(rawName)
		if _, ok := known[name]; ok {
			if _, seen := seenKnown[name]; !seen {
				pilots = append(pilots, name)
				seenKnown[name] = struct{}{}
			}
			continue
		}
		if _, seen := seenUnknown[rawName]; !seen {
			unknown = append(unknown, rawName)
			seenUnknown[rawName] = struct{}{}
		}
	}
	return pilots, unknown
}

func plannerRuntimeTrace(shadowEnabled, plannerDispatchEnabled bool, routeIntents []intent.Intent) observability.RuntimeTrace {
	mode := "off"
	if plannerDispatchEnabled {
		mode = "dispatch"
	} else if shadowEnabled {
		mode = "shadow"
	}
	return observability.RuntimeTrace{
		RouterMode:   mode,
		RouteIntents: routeIntentLabels(routeIntents),
	}
}

func formatRouteIntents(routeIntents []intent.Intent) string {
	labels := routeIntentLabels(routeIntents)
	if len(labels) == 0 {
		return "[]"
	}
	return "[" + strings.Join(labels, ",") + "]"
}

func routeIntentLabels(routeIntents []intent.Intent) []string {
	if len(routeIntents) == 0 {
		return nil
	}
	labels := make([]string, 0, len(routeIntents))
	for _, enabled := range routeIntents {
		switch enabled {
		case intent.IntentResourceInfo:
			labels = append(labels, "resource")
		case intent.IntentMonitorQuery:
			labels = append(labels, "monitor")
		case intent.IntentGPUSpecsQuery:
			labels = append(labels, "gpu_specs")
		case intent.IntentStockAvailability:
			labels = append(labels, "stock")
		case intent.IntentImageTagCatalog:
			labels = append(labels, "image_tags")
		case intent.IntentModelRepositoryBrowse:
			labels = append(labels, "model_repo")
		case intent.IntentImageList:
			labels = append(labels, "image_list")
		case intent.IntentDiagnosis:
			labels = append(labels, "diagnosis")
		case intent.IntentVagueFailure:
			labels = append(labels, "vague_failure")
		default:
			labels = append(labels, string(enabled))
		}
	}
	return labels
}

const defaultKnowledgeCorpusPath = "deploy/kb/stage2b_w0.jsonl"
const defaultExternalKnowledgeCorpusPath = "deploy/kb/external_w0.jsonl"

func knowledgeRetrievalModeFromEnv(getenv getenvFunc) (bool, string) {
	raw := strings.ToLower(strings.TrimSpace(getenv("USE_KNOWLEDGE_RETRIEVAL")))
	switch raw {
	case "", "curated":
		return true, ""
	case "off", "none", "disabled", "false", "0":
		return false, ""
	default:
		return false, raw
	}
}

func knowledgeCorpusPathFromEnv(getenv getenvFunc) string {
	path := strings.TrimSpace(getenv("COMPSHARE_KNOWLEDGE_CORPUS"))
	if path == "" {
		return defaultKnowledgeCorpusPath
	}
	return path
}

func knowledgeRetrieverFromEnv(getenv getenvFunc) (*knowledge.Retriever, bool, error) {
	enabled, unknown := knowledgeRetrievalModeFromEnv(getenv)
	if unknown != "" || !enabled {
		return nil, false, nil
	}
	corpusPath := knowledgeCorpusPathFromEnv(getenv)
	mode := ragRetrievalModeFromEnv(getenv)
	if mode == knowledge.RetrievalModeBM25Only {
		corpus, err := knowledge.LoadPinnedCorpus(corpusPath)
		if err != nil {
			return nil, false, err
		}
		return knowledge.NewRetriever(corpus, knowledge.RetrieverOptions{
			Mode: knowledge.RetrievalModeBM25Only,
		}), true, nil
	}
	// Hybrid-or-better path: corpus + embedding sidecar must both load and
	// pass their pinned-digest checks. Failure is fatal — the runtime must
	// never serve a hybrid result against a stale or mismatched index (see
	// memory feedback_constraints_anchor_to_validated_artifact).
	embedModel := embedModelForMode(mode, getenv)
	expectedDigest := embeddingDigestForMode(mode)
	embeddingsPath := hybridEmbeddingsPathFromEnv(getenv, corpusPath, embedModel)
	corpus, sidecar, err := loadKnowledgeCorpora(getenv, mode, corpusPath, embeddingsPath, expectedDigest)
	if err != nil {
		return nil, false, fmt.Errorf("rag hybrid load (mode=%s): %w", mode, err)
	}
	embedClient, err := embeddingClientFromEnvWithModel(getenv, embedModel)
	if err != nil {
		return nil, false, fmt.Errorf("rag hybrid embedding client: %w", err)
	}
	opts := knowledge.RetrieverOptions{
		EmbeddingSidecar:     &sidecar,
		Embedder:             embedClient,
		EmbeddingModel:       embedModel,
		HybridContextTimeout: hybridTimeoutFromEnv(getenv),
		Mode:                 mode,
	}
	if mode == knowledge.RetrievalModeHybridRerank ||
		mode == knowledge.RetrievalModeQwen3Full ||
		mode == knowledge.RetrievalModeQwen3RRF {
		rerankerModel := strings.TrimSpace(getenv("MODELVERSE_RERANKER_MODEL"))
		if rerankerModel == "" {
			rerankerModel = "qwen3-reranker-8b"
		}
		rerankerClient, err := rerankerClientFromEnv(getenv, rerankerModel)
		if err != nil {
			return nil, false, fmt.Errorf("rag reranker client: %w", err)
		}
		opts.Reranker = rerankerClient
		opts.RerankerModel = rerankerModel
		opts.RerankerContextTimeout = rerankerTimeoutFromEnv(getenv)
	}
	return knowledge.NewRetriever(corpus, opts), true, nil
}

// ragRetrievalModeFromEnv resolves the effective retrieval mode with this
// precedence: explicit RAG_RETRIEVAL_MODE > legacy RAG_HYBRID_ENABLED.
// Unset and unrecognized values yield qwen3_rrf, the current default answer
// retrieval path. Legacy RAG_HYBRID_ENABLED=1 still maps to hybrid_cosine for
// old smoke scripts that have not moved to RAG_RETRIEVAL_MODE.
func ragRetrievalModeFromEnv(getenv getenvFunc) string {
	mode := strings.ToLower(strings.TrimSpace(getenv("RAG_RETRIEVAL_MODE")))
	switch mode {
	case knowledge.RetrievalModeBM25Only,
		knowledge.RetrievalModeHybridCosine,
		knowledge.RetrievalModeHybridRerank,
		knowledge.RetrievalModeQwen3Full,
		knowledge.RetrievalModeQwen3RRF:
		return mode
	case "":
		if hybridEnabledFromEnv(getenv) {
			return knowledge.RetrievalModeHybridCosine
		}
		return knowledge.RetrievalModeQwen3RRF
	default:
		log.Printf("rag: unrecognized RAG_RETRIEVAL_MODE=%q, falling back to legacy RAG_HYBRID_ENABLED check", mode)
		if hybridEnabledFromEnv(getenv) {
			return knowledge.RetrievalModeHybridCosine
		}
		return knowledge.RetrievalModeQwen3RRF
	}
}

// embedModelForMode returns the embedding model that goes with the chosen
// retrieval mode. qwen3_full and qwen3_rrf both use qwen3-embedding-8b
// (and the same pinned sidecar); other hybrid modes use text-embedding-3-large.
func embedModelForMode(mode string, getenv getenvFunc) string {
	if mode == knowledge.RetrievalModeQwen3Full || mode == knowledge.RetrievalModeQwen3RRF {
		if explicit := strings.TrimSpace(getenv("MODELVERSE_EMBED_MODEL")); explicit != "" {
			return explicit
		}
		return "qwen3-embedding-8b"
	}
	if explicit := strings.TrimSpace(getenv("MODELVERSE_EMBED_MODEL")); explicit != "" {
		return explicit
	}
	return "text-embedding-3-large"
}

// embeddingDigestForMode returns the pinned sidecar digest that goes with
// the chosen retrieval mode. qwen3_full and qwen3_rrf both pin the
// qwen3-embedding-8b sidecar; other hybrid modes pin the
// text-embedding-3-large sidecar.
func embeddingDigestForMode(mode string) string {
	if mode == knowledge.RetrievalModeQwen3Full || mode == knowledge.RetrievalModeQwen3RRF {
		return knowledge.EmbeddingDigestExpectedQwen3
	}
	return knowledge.EmbeddingDigestExpected
}

func hybridEnabledFromEnv(getenv getenvFunc) bool {
	switch strings.ToLower(strings.TrimSpace(getenv("RAG_HYBRID_ENABLED"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// hybridEmbeddingsPathFromEnv picks the sidecar file. When the embed model
// is the default text-embedding-3-large the path matches the legacy
// embeddings_<digest>.jsonl (no model suffix) so existing deployments are
// untouched; non-default models get the _<model> suffix per B.2.
//
// COMPSHARE_KNOWLEDGE_EMBEDDINGS overrides the computed path so tests +
// staged sidecar files can be wired without renaming.
func hybridEmbeddingsPathFromEnv(getenv getenvFunc, corpusPath, embedModel string) string {
	if override := strings.TrimSpace(getenv("COMPSHARE_KNOWLEDGE_EMBEDDINGS")); override != "" {
		return override
	}
	dir := filepath.Dir(corpusPath)
	if embedModel == "" || embedModel == "text-embedding-3-large" {
		return filepath.Join(dir, "embeddings_"+knowledge.CorpusDigestExpected+".jsonl")
	}
	return filepath.Join(dir, "embeddings_"+knowledge.CorpusDigestExpected+"_"+embedModel+".jsonl")
}

// externalKnowledgeEnabled gates the additive external tool/ops corpus
// (deploy/kb/external_w0.jsonl). DEFAULT ON: ""/1/true/yes/on => the retriever
// merges the external corpus into the index; 0/off/false/no => platform-only
// (byte-identical to pre-Phase-2); unknown => off + warn (CLAUDE.md: never silently
// coerce). The merge stays ADDITIVE and safe: loadKnowledgeCorpora falls back to
// platform-only if the external corpus is missing/bad/digest-drifted, so a broken
// external file never takes down platform RAG. Platform retrieval parity is
// preserved (the merged 687+55 index keeps platform Top-3 unchanged, re-verified
// per the #237 256-Q parity gate after the Linux-ops + PyTorch-basics vertical).
// Rollback =
// COMPSHARE_EXTERNAL_KNOWLEDGE=0.
func externalKnowledgeEnabled(getenv getenvFunc) bool {
	raw := strings.TrimSpace(getenv("COMPSHARE_EXTERNAL_KNOWLEDGE"))
	switch strings.ToLower(raw) {
	case "", "1", "true", "yes", "on":
		return true
	case "0", "off", "no", "false", "disabled", "none":
		return false
	default:
		log.Printf("warning: ignoring unknown COMPSHARE_EXTERNAL_KNOWLEDGE value %q; treating as off", raw)
		return false
	}
}

// externalEmbeddingsPathFromEnv derives the external qwen3 sidecar path, mirroring
// hybridEmbeddingsPathFromEnv. COMPSHARE_EXTERNAL_KNOWLEDGE_EMBEDDINGS overrides it.
func externalEmbeddingsPathFromEnv(getenv getenvFunc, corpusPath string) string {
	if override := strings.TrimSpace(getenv("COMPSHARE_EXTERNAL_KNOWLEDGE_EMBEDDINGS")); override != "" {
		return override
	}
	dir := filepath.Dir(corpusPath)
	return filepath.Join(dir, "embeddings_"+knowledge.ExternalCorpusDigestExpected+"_qwen3-embedding-8b.jsonl")
}

// externalKnowledgeSource returns the pinned external source to merge, or
// ok=false to serve platform-only. External is opt-in (externalKnowledgeEnabled)
// and ships only a qwen3 sidecar, so it can merge only in the qwen3 retrieval
// modes; any other mode logs and is skipped.
func externalKnowledgeSource(getenv getenvFunc, mode string) (knowledge.PinnedCorpusSource, bool) {
	if !externalKnowledgeEnabled(getenv) {
		return knowledge.PinnedCorpusSource{}, false
	}
	if mode != knowledge.RetrievalModeQwen3Full && mode != knowledge.RetrievalModeQwen3RRF {
		log.Printf("rag: COMPSHARE_EXTERNAL_KNOWLEDGE set but mode=%s is not qwen3-based; serving platform-only", mode)
		return knowledge.PinnedCorpusSource{}, false
	}
	corpusPath := strings.TrimSpace(getenv("COMPSHARE_EXTERNAL_KNOWLEDGE_CORPUS"))
	if corpusPath == "" {
		corpusPath = defaultExternalKnowledgeCorpusPath
	}
	return knowledge.PinnedCorpusSource{
		CorpusPath:              corpusPath,
		EmbeddingsPath:          externalEmbeddingsPathFromEnv(getenv, corpusPath),
		ExpectedCorpusDigest:    knowledge.ExternalCorpusDigestExpected,
		ExpectedEmbeddingDigest: knowledge.ExternalEmbeddingDigestExpectedQwen3,
	}, true
}

// loadKnowledgeCorpora loads the platform corpus + sidecar, optionally merging
// the external corpus when enabled. External is ADDITIVE and must never take
// down platform RAG: if it is enabled but fails to load (bad/missing file, digest
// drift), we log and fall back to the platform-only load — the exact pre-Phase-2
// behavior. When external is off, this is byte-identical to the single-source
// LoadPinnedCorpusWithEmbeddingsDigest.
func loadKnowledgeCorpora(getenv getenvFunc, mode, corpusPath, embeddingsPath, expectedDigest string) (knowledge.Corpus, knowledge.EmbeddingSidecar, error) {
	extSrc, ok := externalKnowledgeSource(getenv, mode)
	if !ok {
		return knowledge.LoadPinnedCorpusWithEmbeddingsDigest(corpusPath, embeddingsPath, expectedDigest)
	}
	platform := knowledge.PinnedCorpusSource{
		CorpusPath:              corpusPath,
		EmbeddingsPath:          embeddingsPath,
		ExpectedCorpusDigest:    knowledge.CorpusDigestExpected,
		ExpectedEmbeddingDigest: expectedDigest,
	}
	merged, sidecar, err := knowledge.LoadPinnedCorporaWithEmbeddings([]knowledge.PinnedCorpusSource{platform, extSrc})
	if err != nil {
		log.Printf("rag: external knowledge corpus %s failed to load (%v); serving platform-only", extSrc.CorpusPath, err)
		return knowledge.LoadPinnedCorpusWithEmbeddingsDigest(corpusPath, embeddingsPath, expectedDigest)
	}
	log.Printf("rag: merged external knowledge corpus %s into the index (%d total chunks)", extSrc.CorpusPath, len(merged.Chunks))
	return merged, sidecar, nil
}

// hybridTimeoutFromEnv reads RAG_HYBRID_TIMEOUT_MS and returns a duration.
// Zero return means "use retriever default" — knowledge.NewRetriever
// substitutes 5s when HybridContextTimeout <= 0, preserving baseline
// behavior when the env var is unset or invalid. Set this env var in
// production to override; the value must be a positive integer in
// milliseconds (e.g. "8000" for 8s).
func hybridTimeoutFromEnv(getenv getenvFunc) time.Duration {
	raw := strings.TrimSpace(getenv("RAG_HYBRID_TIMEOUT_MS"))
	if raw == "" {
		return 0
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms <= 0 {
		log.Printf("rag.hybrid: invalid RAG_HYBRID_TIMEOUT_MS=%q, falling back to retriever default", raw)
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

func embeddingClientFromEnv(getenv getenvFunc) (*embedding.Client, error) {
	model := strings.TrimSpace(getenv("MODELVERSE_EMBED_MODEL"))
	if model == "" {
		model = "text-embedding-3-large"
	}
	return embeddingClientFromEnvWithModel(getenv, model)
}

// embeddingClientFromEnvWithModel builds an embedding client with an
// explicit model override. Used by knowledgeRetrieverFromEnv to honor the
// mode-driven model selection (qwen3-embedding-8b for qwen3_full,
// text-embedding-3-large for hybrid_cosine / hybrid_rerank) without
// requiring callers to also set MODELVERSE_EMBED_MODEL.
func embeddingClientFromEnvWithModel(getenv getenvFunc, model string) (*embedding.Client, error) {
	apiKey := modelverseAPIKeyFromEnv(getenv)
	if apiKey == "" {
		return nil, fmt.Errorf("MODELVERSE_API_KEY or LLM_API_KEY is required for hybrid retrieval")
	}
	baseURL := strings.TrimSpace(getenv("MODELVERSE_BASE_URL"))
	if baseURL == "" {
		baseURL = "https://api.modelverse.cn/v1"
	}
	if strings.TrimSpace(model) == "" {
		model = "text-embedding-3-large"
	}
	return embedding.NewClient(embedding.ClientOptions{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
	})
}

// rerankerClientAdapter wraps reranker.Client (which returns
// []reranker.Result) into knowledge.RerankerClient (which returns
// []knowledge.RerankerResult). The knowledge package stays free of the
// reranker package import — same pattern as VectorEmbedder.
type rerankerClientAdapter struct {
	client reranker.Client
}

func (a rerankerClientAdapter) Rerank(ctx context.Context, query string, docs []string, topN int) ([]knowledge.RerankerResult, error) {
	results, err := a.client.Rerank(ctx, query, docs, topN)
	if err != nil {
		return nil, err
	}
	out := make([]knowledge.RerankerResult, 0, len(results))
	for _, r := range results {
		out = append(out, knowledge.RerankerResult{Index: r.Index, Score: r.Score})
	}
	return out, nil
}

func rerankerClientFromEnv(getenv getenvFunc, model string) (knowledge.RerankerClient, error) {
	apiKey := modelverseAPIKeyFromEnv(getenv)
	if apiKey == "" {
		return nil, fmt.Errorf("MODELVERSE_API_KEY or LLM_API_KEY is required for reranker")
	}
	baseURL := strings.TrimSpace(getenv("MODELVERSE_BASE_URL"))
	if baseURL == "" {
		baseURL = "https://api.modelverse.cn/v1"
	}
	if strings.TrimSpace(model) == "" {
		model = "qwen3-reranker-8b"
	}
	client, err := reranker.NewModelverseClient(reranker.ClientOptions{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
		Timeout: rerankerTimeoutFromEnv(getenv),
	})
	if err != nil {
		return nil, err
	}
	return rerankerClientAdapter{client: client}, nil
}

func modelverseAPIKeyFromEnv(getenv getenvFunc) string {
	if apiKey := strings.TrimSpace(getenv("MODELVERSE_API_KEY")); apiKey != "" {
		return apiKey
	}
	return strings.TrimSpace(getenv("LLM_API_KEY"))
}

// rerankerTimeoutFromEnv parses RAG_RERANKER_TIMEOUT_MS. Zero return means
// "use reranker package default" (5s, matches B.0 probe sizing).
func rerankerTimeoutFromEnv(getenv getenvFunc) time.Duration {
	raw := strings.TrimSpace(getenv("RAG_RERANKER_TIMEOUT_MS"))
	if raw == "" {
		return 0
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms <= 0 {
		log.Printf("rag.reranker: invalid RAG_RERANKER_TIMEOUT_MS=%q, falling back to reranker default", raw)
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

func defaultRouteIntents() []intent.Intent {
	return []intent.Intent{
		intent.IntentResourceInfo,
		intent.IntentMonitorQuery,
		intent.IntentBillingAccountUnsupported,
		intent.IntentGPUSpecsQuery,
		intent.IntentStockAvailability,
		intent.IntentPricingQuery,
		intent.IntentRefundEstimate,
		intent.IntentImageTagCatalog,
		intent.IntentModelRepositoryBrowse,
		intent.IntentImageList,
		intent.IntentNetAcceleratorStatus,
	}
}

func intentPlannerRouteIntentsFromEnv(getenv getenvFunc) ([]intent.Intent, []string) {
	raw := getenv("COMPSHARE_DIRECT_DISPATCH_INTENTS")
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultRouteIntents(), nil
	}
	switch strings.ToLower(trimmed) {
	case "off", "none", "disabled":
		return nil, nil
	}
	seen := map[intent.Intent]struct{}{}
	intents := []intent.Intent{}
	unknown := []string{}
	for _, part := range strings.Split(raw, ",") {
		value := strings.ToLower(strings.TrimSpace(part))
		if value == "" {
			continue
		}
		var enabled intent.Intent
		switch value {
		case "resource":
			enabled = intent.IntentResourceInfo
		case "monitor":
			enabled = intent.IntentMonitorQuery
		case "billing_account_unsupported", "account_billing_unsupported":
			enabled = intent.IntentBillingAccountUnsupported
		case "gpu_specs":
			enabled = intent.IntentGPUSpecsQuery
		case "stock":
			enabled = intent.IntentStockAvailability
		case "image_tags", "image_tag", "image_tag_catalog":
			enabled = intent.IntentImageTagCatalog
		case "model_repo", "model_repository", "model_repository_browse":
			enabled = intent.IntentModelRepositoryBrowse
		case "image", "image_list", "platform_image", "platform_image_list", "custom_image", "custom_image_list", "community_image", "community_image_list", "shared_image", "sharing_image", "shared_image_list":
			enabled = intent.IntentImageList
		case "network_accelerator", "network_accelerator_status", "net_accelerator":
			enabled = intent.IntentNetAcceleratorStatus
		case "pricing", "pricing_query":
			// Accept both the short form ("pricing", convention-consistent
			// with the sibling cases above) and the full intent label
			// ("pricing_query") so existing eval scripts and operator runbooks
			// using either form work. Short form is canonical going forward.
			enabled = intent.IntentPricingQuery
		case "refund", "refund_estimate":
			enabled = intent.IntentRefundEstimate
		case "diagnosis":
			enabled = intent.IntentDiagnosis
		case "vague_failure":
			enabled = intent.IntentVagueFailure
		default:
			unknown = append(unknown, value)
			continue
		}
		if _, ok := seen[enabled]; ok {
			continue
		}
		seen[enabled] = struct{}{}
		intents = append(intents, enabled)
	}
	return intents, unknown
}

func useSeparateShadowRunner(traceEnabled, shadowEnabled, routeEnabled bool) bool {
	return traceEnabled && shadowEnabled && !routeEnabled
}

type cliTraceRecorder struct {
	writer                observability.Writer
	record                observability.TraceRecord
	start                 time.Time
	totalTokens           int
	promptTokens          int
	completionTokens      int
	pendingByID           map[string][]int
	registryTraceSupplier func(time.Time) observability.EntityRegistryTrace
	plannerTraceSupplier  func() observability.RouterTrace
	terminalSignals       observability.FinishSignals
	stateTrace            observability.StateTrace
}

// newCLITraceRecorder constructs a per-turn trace recorder for the CLI path.
// traceID, when non-empty, becomes the trace's TraceID verbatim — used by
// server callers that already have a request_uuid (plan §10 / A7). Empty
// traceID falls back to the legacy auto-generated form so existing CLI
// callsites (cmd/agent.go) keep working unchanged.
func newCLITraceRecorder(writer observability.Writer, traceID string, turnIndex int, userMsg string, start time.Time) *cliTraceRecorder {
	userMsgHash, _ := observability.HashTracePayload(userMsg)
	if traceID == "" {
		traceID = fmt.Sprintf("trace-%d-%d", turnIndex, start.UnixNano())
	}
	return &cliTraceRecorder{
		writer: writer,
		record: observability.TraceRecord{
			TraceID:     traceID,
			TurnID:      fmt.Sprintf("turn-%d", turnIndex),
			TurnIndex:   turnIndex,
			UserMsgHash: userMsgHash,
		},
		start:       start,
		pendingByID: map[string][]int{},
	}
}

func (r *cliTraceRecorder) SetRegistryTraceSupplier(supplier func(time.Time) observability.EntityRegistryTrace) {
	if r == nil {
		return
	}
	r.registryTraceSupplier = supplier
}

func (r *cliTraceRecorder) SetRuntimeTrace(trace observability.RuntimeTrace) {
	if r == nil {
		return
	}
	r.record.Runtime = trace
}

func (r *cliTraceRecorder) SetPlannerTraceSupplier(supplier func() observability.RouterTrace) {
	if r == nil {
		return
	}
	r.plannerTraceSupplier = supplier
}

func (r *cliTraceRecorder) SetPlannerTrace(trace observability.RouterTrace) {
	if r == nil {
		return
	}
	r.record.IntentRouter = trace
	r.addPlannerTokens(trace)
	r.plannerTraceSupplier = nil
}

func (r *cliTraceRecorder) SetRetrievalTrace(trace observability.RetrievalTrace) {
	if r == nil {
		return
	}
	r.record.Retrieval = observability.MergeRetrievalTrace(r.record.Retrieval, trace)
}

func (r *cliTraceRecorder) SetFreshnessTrace(trace observability.FreshnessTrace) {
	if r == nil {
		return
	}
	r.record.Freshness = observability.MergeFreshnessTrace(r.record.Freshness, trace)
}

func (r *cliTraceRecorder) SetDiagnosisTrace(trace observability.DiagnosisTrace) {
	if r == nil {
		return
	}
	r.record.Diagnosis = trace
}

func (r *cliTraceRecorder) SetOutcomeTrace(trace observability.OutcomeTrace) {
	if r == nil {
		return
	}
	r.record.Outcome.AttemptedHallucinatedCount = trace.AttemptedHallucinatedCount
	r.record.Outcome.EscapedHallucinatedCount = trace.EscapedHallucinatedCount
	r.record.Outcome.KBConflictCount = trace.KBConflictCount
}

func groundedRendererModeFromEnv(getenv getenvFunc) (string, string) {
	raw := strings.ToLower(strings.TrimSpace(getenv("USE_GROUNDED_RENDERER")))
	switch raw {
	case "", "llm":
		return "llm", ""
	case "fast_template":
		// B3: fast-tier catalog envelopes render via deterministic template
		// (handler Reply); knowledge/agent tiers still use the LLM renderer.
		return "fast_template", ""
	case "off", "none", "disabled", "false", "0":
		return "", ""
	default:
		return "", raw
	}
}

func (r *cliTraceRecorder) SetRendererTrace(trace observability.RendererTrace) {
	if r == nil {
		return
	}
	r.record.Renderer = trace
}

func (r *cliTraceRecorder) SetEngineHardBlock(trace observability.EngineHardBlockTrace) {
	if r == nil {
		return
	}
	r.record.EngineHardBlock = trace
}

func (r *cliTraceRecorder) SetRateLimitDecision(decision governance.Decision) {
	if r == nil {
		return
	}
	// Decision.SubjectHash is expected to be pre-hashed by governance callers.
	// The recorder copies it verbatim and never accepts raw key material.
	trace := observability.RateLimitTrace{
		Checked:      true,
		Allowed:      decision.Allowed,
		Class:        string(decision.Class),
		Action:       decision.Action,
		Reason:       string(decision.Reason),
		SubjectHash:  decision.SubjectHash,
		RetryAfterMS: decision.RetryAfter.Milliseconds(),
	}
	current := r.record.RateLimit
	if !current.Checked {
		r.record.RateLimit = trace
		return
	}
	// Aggregation rule from T-005 trace semantics:
	// first denial wins; if no denial occurs, record the latest allow.
	if !current.Allowed {
		return
	}
	if !trace.Allowed {
		r.record.RateLimit = trace
		return
	}
	r.record.RateLimit = trace
}

func (r *cliTraceRecorder) HasRateLimitDenial() bool {
	return r != nil && r.record.RateLimit.Checked && !r.record.RateLimit.Allowed
}

func (r *cliTraceRecorder) AddTokenUsage(usage llm.TokenUsage) {
	if r == nil {
		return
	}
	r.totalTokens += llmTokenUsageTotal(usage)
	r.promptTokens += usage.PromptTokens
	r.completionTokens += usage.CompletionTokens
}

func (r *cliTraceRecorder) OnStep(ev engine.StepEvent) {
	if r == nil || r.writer == nil || ev.Action == "" {
		return
	}
	source := ev.Source
	if source == "" {
		source = observability.ToolSourceMainReAct
	}
	key := source + "\x00" + ev.Action
	switch ev.Type {
	case engine.StepToolCall:
		argsHash, _ := observability.HashTracePayload(ev.Args)
		requestedTargets := ev.RequestedTargets
		if requestedTargets == 0 {
			requestedTargets = traceRequestedTargets(ev.Args)
		}
		windowSeconds := ev.WindowSeconds
		if windowSeconds == 0 {
			windowSeconds = traceWindowSeconds(ev.Args)
		}
		r.record.ToolCalls = append(r.record.ToolCalls, observability.ToolCallTrace{
			ID:               fmt.Sprintf("tool-%d", len(r.record.ToolCalls)+1),
			TurnIndex:        r.record.TurnIndex,
			Action:           ev.Action,
			Source:           source,
			ArgsHash:         argsHash,
			RequestedTargets: requestedTargets,
			WindowSeconds:    windowSeconds,
		})
		r.pendingByID[key] = append(r.pendingByID[key], len(r.record.ToolCalls)-1)
	case engine.StepToolResult:
		idx := r.matchPending(key, ev.Action, source)
		resultHash, _ := observability.HashTracePayload(ev.TraceResult)
		r.record.ToolCalls[idx].Status = observability.ToolStatusSuccess
		r.record.ToolCalls[idx].ResultHash = resultHash
		r.record.ToolCalls[idx].Attempts = ev.Attempts
		r.record.ToolCalls[idx].Projected = ev.Projected
		if r.record.ToolCalls[idx].RequestedTargets > 0 && r.record.ToolCalls[idx].ExecutedTargets == 0 {
			r.record.ToolCalls[idx].ExecutedTargets = r.record.ToolCalls[idx].RequestedTargets
		}
		if len(ev.RendererInputToolArgHashes) > 0 {
			r.record.Renderer.InputToolArgHashes = append(r.record.Renderer.InputToolArgHashes, ev.RendererInputToolArgHashes...)
		}
	case engine.StepError:
		idx := r.matchPending(key, ev.Action, source)
		r.record.ToolCalls[idx].Status = observability.ToolStatusError
		r.record.ToolCalls[idx].ErrorClass = ev.Message
	case engine.StepBlocked:
		idx := r.matchPending(key, ev.Action, source)
		r.record.ToolCalls[idx].Status = observability.ToolStatusError
		r.record.ToolCalls[idx].ErrorClass = "blocked"
		r.applyCapFields(idx, ev)
	}
}

// EmitStep accumulates one agent-tier saga StepTrace into THIS turn's record
// (B6.2). The orchestrator saga runner uses the recorder as its StepSink. Steps
// are folded into record.Steps in memory and persisted ONCE at Finish via
// Append → prepareForPersist (which redacts Args/Result) — never a per-step
// INSERT (a per-step INSERT would collide uk_request_uuid: one row per turn).
// This makes *cliTraceRecorder satisfy orchestrator.StepSink.
func (r *cliTraceRecorder) EmitStep(step observability.StepTrace) error {
	if r == nil {
		return nil
	}
	r.record.Steps = append(r.record.Steps, step)
	return nil
}

// SetTerminalSignals records the per-turn terminal facts (empty reply, ReAct
// round count, round-ceiling) the trace record cannot observe on its own. The
// chat error is passed separately to Finish. Call it before Finish; a
// never-called recorder finalizes a clean turn as "done".
func (r *cliTraceRecorder) SetTerminalSignals(signals observability.FinishSignals) {
	if r == nil {
		return
	}
	r.terminalSignals = signals
}

// SetStateTrace records the per-turn instance-binding state (#3) the recorder
// reads from the engine getters. Call it before Finish; an un-set recorder
// leaves State zero (omitted, SHA-stable).
func (r *cliTraceRecorder) SetStateTrace(state observability.StateTrace) {
	if r == nil {
		return
	}
	r.stateTrace = state
}

func (r *cliTraceRecorder) Finish(chatErr error, end time.Time) error {
	if r == nil || r.writer == nil {
		return nil
	}
	if r.registryTraceSupplier != nil {
		r.record.EntityRegistry = r.registryTraceSupplier(end)
	}
	if r.plannerTraceSupplier != nil {
		r.record.IntentRouter = r.plannerTraceSupplier()
		r.addPlannerTokens(r.record.IntentRouter)
	}
	r.record.Outcome.TotalLatencyMS = end.Sub(r.start).Milliseconds()
	r.record.Outcome.TotalTokens = r.totalTokens
	r.record.Outcome.PromptTokens = r.promptTokens
	r.record.Outcome.CompletionTokens = r.completionTokens
	for _, call := range r.record.ToolCalls {
		if call.TurnIndex == r.record.TurnIndex && call.Action == "GetCompShareInstanceMonitor" {
			r.record.Freshness.MonitorCallInCurrentTurn = true
			break
		}
	}
	r.record.ActualExecutionTier = r.record.DeriveActualExecutionTier()
	r.record.ActualExecutionPath = r.record.DeriveActualExecutionPath()
	r.record.Retrieval.RefusalType = r.record.Retrieval.DeriveRefusalType()
	r.record.State = r.stateTrace
	signals := r.terminalSignals
	signals.ChatErr = chatErr
	r.record.FinalizeOutcome(signals)
	return r.writer.Append(r.record)
}

func (r *cliTraceRecorder) addPlannerTokens(trace observability.RouterTrace) {
	r.totalTokens += trace.InputTokens + trace.OutputTokens
	r.promptTokens += trace.InputTokens
	r.completionTokens += trace.OutputTokens
}

func llmTokenUsageTotal(usage llm.TokenUsage) int {
	if usage.TotalTokens > 0 {
		return usage.TotalTokens
	}
	return usage.PromptTokens + usage.CompletionTokens
}

func (r *cliTraceRecorder) applyCapFields(idx int, ev engine.StepEvent) {
	call := &r.record.ToolCalls[idx]
	if call.ArgsHash == "" && ev.Args != nil {
		call.ArgsHash, _ = observability.HashTracePayload(ev.Args)
	}
	if call.RequestedTargets == 0 {
		call.RequestedTargets = traceRequestedTargets(ev.Args)
	}
	if call.WindowSeconds == 0 {
		call.WindowSeconds = traceWindowSeconds(ev.Args)
	}
	call.ExecutedTargets = 0
	if ev.Capped != "" {
		call.Capped = ev.Capped
	}
	if ev.CapReason != "" {
		call.CapReason = ev.CapReason
	}
}

func (r *cliTraceRecorder) matchPending(key, action, source string) int {
	if queue := r.pendingByID[key]; len(queue) > 0 {
		idx := queue[0]
		if len(queue) == 1 {
			delete(r.pendingByID, key)
		} else {
			r.pendingByID[key] = queue[1:]
		}
		return idx
	}
	r.record.ToolCalls = append(r.record.ToolCalls, observability.ToolCallTrace{
		ID:        fmt.Sprintf("tool-%d", len(r.record.ToolCalls)+1),
		TurnIndex: r.record.TurnIndex,
		Action:    action,
		Source:    source,
	})
	return len(r.record.ToolCalls) - 1
}

func traceRequestedTargets(args map[string]any) int {
	if args == nil {
		return 0
	}
	if count := traceTargetValueCount(args["UHostIds"]); count > 0 {
		return count
	}
	if value, ok := args["UHostId"].(string); ok && strings.TrimSpace(value) != "" {
		return 1
	}
	return 0
}

func traceTargetValueCount(value any) int {
	switch typed := value.(type) {
	case []string:
		return len(typed)
	case []any:
		return len(typed)
	default:
		return 0
	}
}

func traceWindowSeconds(args map[string]any) int {
	if args == nil {
		return 0
	}
	start, okStart := traceInt64(args["StartTime"])
	end, okEnd := traceInt64(args["EndTime"])
	if !okStart || !okEnd || start < 0 || end < 0 || end <= start {
		return 0
	}
	return int(end - start)
}

func traceInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed < math.MinInt64 || typed > math.MaxInt64 {
			return 0, false
		}
		if typed != float64(int64(typed)) {
			return 0, false
		}
		return int64(typed), true
	case float32:
		f := float64(typed)
		if math.IsNaN(f) || math.IsInf(f, 0) || f < math.MinInt64 || f > math.MaxInt64 {
			return 0, false
		}
		if f != float64(int64(f)) {
			return 0, false
		}
		return int64(f), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return n, err == nil
	case json.Number:
		n, err := typed.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}
