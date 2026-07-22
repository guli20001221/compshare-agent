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

type cliTraceRecorder struct {
	writer                observability.Writer
	record                observability.TraceRecord
	start                 time.Time
	totalTokens           int
	promptTokens          int
	completionTokens      int
	pendingByID           map[string][]int
	registryTraceSupplier func(time.Time) observability.EntityRegistryTrace
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

// AddAuthorizationTrace appends one write target's dual-proof audit record; the
// engine calls it once per verified target of a mutating action.
func (r *cliTraceRecorder) AddAuthorizationTrace(trace observability.AuthorizationTrace) {
	if r == nil {
		return
	}
	r.record.Authorizations = append(r.record.Authorizations, trace)
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

// EmitStep accumulates one workflow step trace into this turn's record. Steps are
// folded into record.Steps in memory and persisted once at Finish via
// Append → prepareForPersist (which redacts Args/Result) — never a per-step
// INSERT (a per-step INSERT would collide uk_request_uuid: one row per turn).
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
