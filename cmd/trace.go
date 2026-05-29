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
	return getenv("USE_INTENT_PLANNER") == "shadow"
}

func plannerRuntimeModeLine(shadowEnabled, plannerDispatchEnabled bool, cutoverIntents []intent.Intent) string {
	mode := "off"
	if plannerDispatchEnabled {
		mode = "dispatch"
	} else if shadowEnabled {
		mode = "shadow"
	}
	return fmt.Sprintf("planner_mode=%s cutover_intents=%s", mode, formatCutoverIntents(cutoverIntents))
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

func plannerRuntimeTrace(shadowEnabled, plannerDispatchEnabled bool, cutoverIntents []intent.Intent) observability.RuntimeTrace {
	mode := "off"
	if plannerDispatchEnabled {
		mode = "dispatch"
	} else if shadowEnabled {
		mode = "shadow"
	}
	return observability.RuntimeTrace{
		PlannerMode:    mode,
		CutoverIntents: cutoverIntentLabels(cutoverIntents),
	}
}

func formatCutoverIntents(cutoverIntents []intent.Intent) string {
	labels := cutoverIntentLabels(cutoverIntents)
	if len(labels) == 0 {
		return "[]"
	}
	return "[" + strings.Join(labels, ",") + "]"
}

func cutoverIntentLabels(cutoverIntents []intent.Intent) []string {
	if len(cutoverIntents) == 0 {
		return nil
	}
	labels := make([]string, 0, len(cutoverIntents))
	for _, enabled := range cutoverIntents {
		switch enabled {
		case intent.IntentResourceInfo:
			labels = append(labels, "resource")
		case intent.IntentMonitorQuery:
			labels = append(labels, "monitor")
		case intent.IntentGPUSpecsQuery:
			labels = append(labels, "gpu_specs")
		case intent.IntentStockAvailability:
			labels = append(labels, "stock")
		case intent.IntentPlatformImageList:
			labels = append(labels, "platform_image")
		case intent.IntentCustomImageList:
			labels = append(labels, "custom_image")
		case intent.IntentCommunityImageList:
			labels = append(labels, "community_image")
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
	corpus, sidecar, err := knowledge.LoadPinnedCorpusWithEmbeddingsDigest(corpusPath, embeddingsPath, expectedDigest)
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

func defaultCutoverIntents() []intent.Intent {
	return []intent.Intent{
		intent.IntentResourceInfo,
		intent.IntentMonitorQuery,
		intent.IntentGPUSpecsQuery,
		intent.IntentStockAvailability,
		intent.IntentPricingQuery,
		intent.IntentPlatformImageList,
		intent.IntentCustomImageList,
		intent.IntentCommunityImageList,
	}
}

func intentPlannerCutoverIntentsFromEnv(getenv getenvFunc) ([]intent.Intent, []string) {
	raw := getenv("USE_INTENT_PLANNER_FOR")
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultCutoverIntents(), nil
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
		case "gpu_specs":
			enabled = intent.IntentGPUSpecsQuery
		case "stock":
			enabled = intent.IntentStockAvailability
		case "platform_image":
			enabled = intent.IntentPlatformImageList
		case "custom_image":
			enabled = intent.IntentCustomImageList
		case "community_image":
			enabled = intent.IntentCommunityImageList
		case "pricing", "pricing_query":
			// Accept both the short form ("pricing", convention-consistent
			// with the sibling cases above) and the full intent label
			// ("pricing_query") so existing eval scripts and operator runbooks
			// using either form work. Short form is canonical going forward.
			enabled = intent.IntentPricingQuery
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

func useSeparateShadowRunner(traceEnabled, shadowEnabled, cutoverEnabled bool) bool {
	return traceEnabled && shadowEnabled && !cutoverEnabled
}

type cliTraceRecorder struct {
	writer                observability.Writer
	record                observability.TraceRecord
	start                 time.Time
	totalTokens           int
	pendingByID           map[string][]int
	registryTraceSupplier func(time.Time) observability.EntityRegistryTrace
	plannerTraceSupplier  func() observability.PlannerTrace
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

func (r *cliTraceRecorder) SetPlannerTraceSupplier(supplier func() observability.PlannerTrace) {
	if r == nil {
		return
	}
	r.plannerTraceSupplier = supplier
}

func (r *cliTraceRecorder) SetPlannerTrace(trace observability.PlannerTrace) {
	if r == nil {
		return
	}
	r.record.Planner = trace
	r.addPlannerTokens(trace)
	r.plannerTraceSupplier = nil
}

func (r *cliTraceRecorder) SetRetrievalTrace(trace observability.RetrievalTrace) {
	if r == nil {
		return
	}
	r.record.Retrieval = trace
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

func (r *cliTraceRecorder) Finish(chatErr error, end time.Time) error {
	if r == nil || r.writer == nil {
		return nil
	}
	// TODO(T-006+): use chatErr when trace schema grows outcome.error_class.
	_ = chatErr
	if r.registryTraceSupplier != nil {
		r.record.EntityRegistry = r.registryTraceSupplier(end)
	}
	if r.plannerTraceSupplier != nil {
		r.record.Planner = r.plannerTraceSupplier()
		r.addPlannerTokens(r.record.Planner)
	}
	r.record.Outcome.TotalLatencyMS = end.Sub(r.start).Milliseconds()
	r.record.Outcome.TotalTokens = r.totalTokens
	for _, call := range r.record.ToolCalls {
		if call.TurnIndex == r.record.TurnIndex && call.Action == "GetCompShareInstanceMonitor" {
			r.record.Freshness.MonitorCallInCurrentTurn = true
			break
		}
	}
	r.record.RealizedTier = r.record.DeriveRealizedTier()
	return r.writer.Append(r.record)
}

func (r *cliTraceRecorder) addPlannerTokens(trace observability.PlannerTrace) {
	r.totalTokens += trace.InputTokens + trace.OutputTokens
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
