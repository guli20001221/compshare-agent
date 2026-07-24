//go:build live

// Splits the 22 `partial` retrieval cases from the 6.26-7.9 production
// evaluation into the two buckets that imply completely different work.
//
// The manual audit of the 12 outright misses (eval/rag_v2_real_chat_gap_audit)
// already returned 9 source-document gaps, 0 confirmed retrieval-pipeline gaps
// and 3 judge false negatives. The 22 partials are the larger and unaudited
// half, so this split is what decides whether a retrieval loop — corrective
// re-query, query rewriting, deeper ranking, any of the agentic-RAG designs — is
// worth building at all. If the missing content is not in the index, none of
// them can reach it.
//
// # Buckets
//
// The buckets are named for what is actually observed, not for what one would
// like to conclude:
//
//	retrievable_missed  — a chunk in the wide net supplies the missing piece and
//	                      the top-3 did not. A ranking/second-hop fix can pay off
//	                      here, and only here.
//	not_in_wide_net     — nothing in the net supplies it. Read this as "no
//	                      realistic retrieval change reaches it", NOT as "proven
//	                      absent from the corpus": the net is the top 40 of two
//	                      queries, not an exhaustive scan of all 1744 chunks. A
//	                      chunk ranked below 40 by both queries is unreachable by
//	                      any ranking fix anyway, which is the decision this
//	                      probe has to inform.
//	evidence_sufficient — the three chunks the judge saw do answer the question.
//	                      Same shape as the 3 judge false negatives in the miss
//	                      audit, so this bucket is expected to be non-empty.
//	undetermined        — the classifier failed (LLM error, unparseable output).
//
// # Method
//
//  1. Gap statement. The eval records only `partial_evidence` as the reason, so
//     what is missing has to be re-derived from the question plus the three
//     chunks the judge actually saw. One LLM call does that, and it may answer
//     "nothing is missing" (-> evidence_sufficient).
//  2. Candidate net. The question is "is the content reachable", not "does
//     production rank it first", so the net is deliberately wider than
//     production: qwen3_rrf top-40 (BM25 top-50 fused with dense-full-corpus
//     top-50) over the SAME merged platform+external index production serves,
//     run twice — once on the original question, once on the gap statement. The
//     second run is exactly what a corrective second hop would do, so its yield
//     is also a direct measurement of that feature's ceiling.
//  3. Verdict. The candidates are read in batches of 10 with an early exit, so
//     each judgement happens on a short context instead of one 40-chunk dump.
//
// # Two guards against this probe confirming its author's prior
//
//   - The 12 audited misses run through the identical pipeline as a labelled
//     control. If the classifier calls the known source gaps "retrievable", it
//     over-claims and the partial numbers are unusable. The gate is
//     pre-registered below: >=3 of the 9 fails the test.
//   - A failure is never folded into not_in_wide_net. It gets its own bucket,
//     because the cheap failure mode here is silently defaulting to the answer
//     the author already believes.
//
// Real customer questions are never committed. This reads them from
// COMPSHARE_REAL_QUERY_CORPUS and writes the per-case detail (which quotes them)
// only to COMPSHARE_PROBE_OUT, expected to point outside the repo.
//
//	go test ./internal/engine -tags live -run TestLivePartialGapClassification -v -timeout 40m
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/embedding"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

const (
	bucketRetrievable = "retrievable_missed"
	bucketNotInNet    = "not_in_wide_net"
	bucketSufficient  = "evidence_sufficient"
	bucketUndetermin  = "undetermined"

	// wideNetTopK is how deep each of the two queries reaches. Anything ranked
	// below this by BOTH queries is out of reach of a ranking fix, so widening
	// further would not change the decision this probe informs.
	wideNetTopK = 40
	// verdictBatch keeps each judgement on a short context.
	verdictBatch = 10
	// chunkExcerptRunes bounds one chunk's contribution to the verdict prompt.
	chunkExcerptRunes = 700
	// classifyRounds is how many times each case is classified before a
	// majority is taken. Five, because two single passes disagreed on 18% of
	// cases; an odd count avoids ties on a two-way split.
	classifyRounds = 5
)

type gradedCase struct {
	CaseID   string   `json:"case_id"`
	Date     string   `json:"date"`
	Category string   `json:"category"`
	Query    string   `json:"query"`
	Top3     []string `json:"top3_chunk_ids"`
	Judge    struct {
		Grade  string `json:"retrieval_grade"`
		Reason string `json:"reason_category"`
	} `json:"judge"`
}

type classification struct {
	CaseID   string `json:"case_id"`
	Category string `json:"category"`
	Grade    string `json:"eval_grade"`
	Bucket   string `json:"bucket"`
	// Missing is a model-written description of the unanswered part. It
	// paraphrases a real customer question, so it goes to the local probe
	// output only.
	Missing string `json:"missing"`
	// GapQuery is the retrieval query derived from Missing — same privacy note.
	GapQuery string `json:"gap_query"`
	// FoundChunk names the chunk that supplies the missing piece, empty unless
	// bucket == retrievable_missed.
	FoundChunk string `json:"found_chunk"`
	// FoundVia records which query surfaced it: "original" means a pure ranking
	// problem (production already retrieved it, just not in the top 3);
	// "gap_query" means only a corrective second hop reaches it.
	FoundVia string `json:"found_via"`
	// FoundRank is its rank within the query that surfaced it.
	FoundRank int `json:"found_rank"`
	// NetIDs are the candidate chunk ids the verdict actually read. Recorded so
	// a not_in_wide_net verdict can be checked by hand against the corpus:
	// without it there is no way to tell "the net missed it" from "the verdict
	// was too strict", and those two need opposite fixes.
	NetIDs []string `json:"net_ids"`
	// VoteSplit records how the repeated runs landed, e.g.
	// "not_in_wide_net=3,retrievable_missed=2". A case whose split is close is
	// not a classified case, it is a coin flip, and reporting it as either
	// bucket would overstate what was measured.
	VoteSplit string `json:"vote_split"`
	Note      string `json:"note"`
}

// majorityVerdict picks the bucket the repeated runs agreed on most often and
// returns that run's detail, annotated with the split. Ties break toward the
// first-seen bucket; the split is recorded either way so a 3-2 never reads like
// a 5-0.
func majorityVerdict(votes []classification) classification {
	if len(votes) == 0 {
		return classification{Bucket: bucketUndetermin, Note: "no votes"}
	}
	counts := map[string]int{}
	for _, v := range votes {
		counts[v.Bucket]++
	}
	best, bestN := votes[0].Bucket, 0
	for _, v := range votes {
		if counts[v.Bucket] > bestN {
			best, bestN = v.Bucket, counts[v.Bucket]
		}
	}
	buckets := make([]string, 0, len(counts))
	for b := range counts {
		buckets = append(buckets, b)
	}
	sort.Strings(buckets)
	parts := make([]string, 0, len(buckets))
	for _, b := range buckets {
		parts = append(parts, fmt.Sprintf("%s=%d", b, counts[b]))
	}
	for _, v := range votes {
		if v.Bucket == best {
			v.VoteSplit = strings.Join(parts, ",")
			return v
		}
	}
	return votes[0]
}

func TestLivePartialGapClassification(t *testing.T) {
	cfg := loadLiveConfig(t)
	cases := loadGradedCases(t)
	corpus, sidecar := mergedProductionIndex(t)
	retriever := wideNetRetriever(t, cfg, corpus, sidecar)
	byID := chunkIndex(corpus)

	var partials, misses []gradedCase
	for _, c := range cases {
		switch c.Judge.Grade {
		case "partial":
			partials = append(partials, c)
		case "miss":
			misses = append(misses, c)
		}
	}
	if len(partials) == 0 {
		t.Fatalf("corpus has no partial-graded case; wrong file?")
	}
	t.Logf("partial=%d  miss(control)=%d  merged index=%d chunks", len(partials), len(misses), len(corpus.Chunks))

	all := append(append([]gradedCase{}, partials...), misses...)

	// Majority vote over repeated classifications, because a single pass is not
	// a measurement here. Two full single-pass runs flipped 6 of 33 verdicts
	// (18%) against each other — the classifier's own run-to-run noise is the
	// same magnitude as the effect it is being used to size, so one pass can
	// only support a bound, not a rate. The vote split is reported per case so
	// the residual instability stays visible instead of being averaged away.
	votes := make([][]classification, len(all))
	type job struct{ caseIdx, round int }
	jobs := make(chan job)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				got := classifyOne(cfg, retriever, byID, all[j.caseIdx])
				mu.Lock()
				votes[j.caseIdx] = append(votes[j.caseIdx], got)
				mu.Unlock()
			}
		}()
	}
	for round := 0; round < classifyRounds; round++ {
		for i := range all {
			jobs <- job{caseIdx: i, round: round}
		}
	}
	close(jobs)
	wg.Wait()

	results := make([]classification, len(all))
	unanimous := 0
	for i := range all {
		results[i] = majorityVerdict(votes[i])
		if results[i].VoteSplit == fmt.Sprintf("%s=%d", results[i].Bucket, classifyRounds) {
			unanimous++
		}
	}
	t.Logf("每条跑 %d 次取多数；%d/%d 条 %d 次结论完全一致", classifyRounds, unanimous, len(all), classifyRounds)

	partialResults := results[:len(partials)]
	missResults := results[len(partials):]

	reportBuckets(t, "22 条 partial", partialResults)
	reportBuckets(t, "12 条 miss (对照组，已有人工标注)", missResults)
	reportControlGate(t, misses, missResults)
	writeProbeDetail(t, results)
}

// classifyOne runs the two-stage classification for a single case.
func classifyOne(cfg *config.Config, retriever *knowledge.Retriever, byID map[string]knowledge.KBChunk, c gradedCase) classification {
	out := classification{CaseID: c.CaseID, Category: c.Category, Grade: c.Judge.Grade}

	seen := make([]string, 0, len(c.Top3))
	for _, id := range c.Top3 {
		if chunk, ok := byID[id]; ok {
			seen = append(seen, renderChunk(chunk))
		}
	}
	if len(seen) == 0 {
		out.Bucket = bucketUndetermin
		out.Note = "none of the recorded top-3 chunk ids exist in the merged index"
		return out
	}

	missing, gapQuery, err := stateGap(cfg, c.Query, seen)
	if err != nil {
		out.Bucket = bucketUndetermin
		out.Note = "gap statement failed: " + err.Error()
		return out
	}
	out.Missing, out.GapQuery = missing, gapQuery
	if missing == "" || strings.EqualFold(missing, "NONE") {
		out.Bucket = bucketSufficient
		return out
	}

	candidates := wideNet(retriever, c.Query, gapQuery, c.Top3)
	for _, cand := range candidates {
		out.NetIDs = append(out.NetIDs, cand.chunk.ChunkID)
	}
	if len(candidates) == 0 {
		out.Bucket = bucketNotInNet
		out.Note = "wide net returned nothing beyond the judged top-3"
		return out
	}

	hit, err := findSupportingChunk(cfg, c.Query, missing, candidates)
	if err != nil {
		out.Bucket = bucketUndetermin
		out.Note = "verdict failed: " + err.Error()
		return out
	}
	if hit == nil {
		out.Bucket = bucketNotInNet
		return out
	}
	out.Bucket = bucketRetrievable
	out.FoundChunk = hit.chunk.ChunkID
	out.FoundVia = hit.via
	out.FoundRank = hit.rank
	return out
}

// stateGap asks what the judged evidence does not answer. It is allowed to
// answer "nothing", which is the judge-false-negative shape the miss audit
// already found three of — folding that into a gap would inflate every
// downstream bucket.
func stateGap(cfg *config.Config, question string, seen []string) (missing, gapQuery string, err error) {
	prompt := fmt.Sprintf(`下面是用户的一个真实问题，以及检索系统当时返回、并被判为"部分覆盖"的资料。

用户问题：%s

检索到的资料：
%s

判断：用户问题中，有哪一部分是这些资料没有回答的？

只输出 JSON，不要任何其他文字：
{"missing":"一句话说明缺失的是什么信息","search":"用来找这条缺失信息的中文检索查询，不超过20字"}

如果这些资料其实已经把问题回答完整了，missing 填 "NONE"，search 填空字符串。`,
		question, strings.Join(seen, "\n\n"))

	raw, err := askJSON(cfg, prompt)
	if err != nil {
		return "", "", err
	}
	var parsed struct {
		Missing string `json:"missing"`
		Search  string `json:"search"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", "", fmt.Errorf("unparseable gap statement: %w", err)
	}
	missing = strings.TrimSpace(parsed.Missing)
	gapQuery = strings.TrimSpace(parsed.Search)
	if gapQuery == "" {
		gapQuery = missing
	}
	return missing, gapQuery, nil
}

type netCandidate struct {
	chunk knowledge.KBChunk
	via   string
	rank  int
}

// wideNet builds the candidate pool: top-40 for the original question and
// top-40 for the gap statement, interleaved by rank so neither query dominates
// the early batches, deduped, with the three already-judged chunks removed.
func wideNet(retriever *knowledge.Retriever, question, gapQuery string, judged []string) []netCandidate {
	fromOriginal := retriever.Retrieve(question, "").HitItems
	fromGap := retriever.Retrieve(gapQuery, "").HitItems

	skip := map[string]struct{}{}
	for _, id := range judged {
		skip[id] = struct{}{}
	}
	var out []netCandidate
	take := func(hit knowledge.RetrievalHit, via string, rank int) {
		if _, ok := skip[hit.Chunk.ChunkID]; ok {
			return
		}
		skip[hit.Chunk.ChunkID] = struct{}{}
		out = append(out, netCandidate{chunk: hit.Chunk, via: via, rank: rank})
	}
	for i := 0; i < wideNetTopK; i++ {
		if i < len(fromOriginal) {
			take(fromOriginal[i], "original", i+1)
		}
		if i < len(fromGap) {
			take(fromGap[i], "gap_query", i+1)
		}
	}
	if len(out) > wideNetTopK {
		out = out[:wideNetTopK]
	}
	return out
}

// findSupportingChunk reads the candidates in batches and returns the first one
// that supplies the missing piece, or nil.
func findSupportingChunk(cfg *config.Config, question, missing string, candidates []netCandidate) (*netCandidate, error) {
	for start := 0; start < len(candidates); start += verdictBatch {
		end := start + verdictBatch
		if end > len(candidates) {
			end = len(candidates)
		}
		batch := candidates[start:end]

		var b strings.Builder
		for i, c := range batch {
			fmt.Fprintf(&b, "[%d] %s\n\n", i+1, renderChunk(c.chunk))
		}
		prompt := fmt.Sprintf(`用户问题：%s

这个问题里没有被回答的部分是：%s

下面是知识库里的另外几段资料：

%s
判断：其中是否有任何一段，明确写出了上面缺失的那条信息？

只输出 JSON，不要任何其他文字：
{"found":true,"index":2}
或
{"found":false,"index":0}

判定从严：只有资料里确实写出了缺失的那条信息才算 found=true。主题相关、沾边、只给了上位概念或只提了一句但没展开，都算 false。`,
			question, missing, b.String())

		raw, err := askJSON(cfg, prompt)
		if err != nil {
			return nil, err
		}
		var parsed struct {
			Found bool `json:"found"`
			Index int  `json:"index"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, fmt.Errorf("unparseable verdict: %w", err)
		}
		if parsed.Found && parsed.Index >= 1 && parsed.Index <= len(batch) {
			hit := batch[parsed.Index-1]
			return &hit, nil
		}
	}
	return nil, nil
}

// askJSON makes one pinned-temperature call and returns the first JSON object in
// the reply. Temperature is pinned because both prompts are closer to an
// extraction than to a judgement; see llm.ChatRequest.Temperature for why that
// buys less determinism than it appears to.
// One retry, because a reply with no JSON object at all is a formatting miss
// rather than a verdict, and letting it land in `undetermined` costs a case.
func askJSON(cfg *config.Config, prompt string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		var zero float32
		resp, err := llm.NewClient(cfg.Agent.LLM).Chat(context.Background(), llm.ChatRequest{
			Messages:    []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: prompt}},
			Temperature: &zero,
		})
		if err != nil {
			lastErr = err
			continue
		}
		if resp == nil {
			lastErr = fmt.Errorf("nil response")
			continue
		}
		if obj := firstJSONObject(resp.Content); obj != nil {
			return obj, nil
		}
		lastErr = fmt.Errorf("reply carried no JSON object")
	}
	return nil, lastErr
}

// firstJSONObject extracts the first balanced {...} run, so a model that wraps
// its JSON in prose or a code fence still parses.
func firstJSONObject(s string) []byte {
	depth, start := 0, -1
	inString, escaped := false, false
	for i, r := range s {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && inString:
			escaped = true
		case r == '"':
			inString = !inString
		case inString:
			// nothing
		case r == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case r == '}':
			depth--
			if depth == 0 && start >= 0 {
				return []byte(s[start : i+1])
			}
		}
	}
	return nil
}

func renderChunk(c knowledge.KBChunk) string {
	body := []rune(c.Content)
	if len(body) > chunkExcerptRunes {
		body = body[:chunkExcerptRunes]
	}
	return fmt.Sprintf("chunk_id=%s title=%s\n%s", c.ChunkID, c.Title, string(body))
}

func chunkIndex(corpus knowledge.Corpus) map[string]knowledge.KBChunk {
	out := make(map[string]knowledge.KBChunk, len(corpus.Chunks))
	for _, c := range corpus.Chunks {
		out[c.ChunkID] = c
	}
	return out
}

// mergedProductionIndex loads exactly what production serves: the platform
// corpus merged with the external tool/ops corpus, each pinned to its own
// digest. Loading platform-only would make every externally-documented answer
// look like a source gap, which is the specific mistake this probe exists to
// avoid.
func mergedProductionIndex(t *testing.T) (knowledge.Corpus, knowledge.EmbeddingSidecar) {
	t.Helper()
	kb := func(name string) string { return filepath.Join("..", "..", "deploy", "kb", name) }
	corpus, sidecar, err := knowledge.LoadPinnedCorporaWithEmbeddings([]knowledge.PinnedCorpusSource{
		{
			CorpusPath:              kb("stage2b_w0.jsonl"),
			EmbeddingsPath:          kb(fmt.Sprintf("embeddings_%s_qwen3-embedding-8b.jsonl", knowledge.CorpusDigestExpected)),
			ExpectedCorpusDigest:    knowledge.CorpusDigestExpected,
			ExpectedEmbeddingDigest: knowledge.EmbeddingDigestExpectedQwen3,
		},
		{
			CorpusPath:              kb("external_w0.jsonl"),
			EmbeddingsPath:          kb(fmt.Sprintf("embeddings_%s_qwen3-embedding-8b.jsonl", knowledge.ExternalCorpusDigestExpected)),
			ExpectedCorpusDigest:    knowledge.ExternalCorpusDigestExpected,
			ExpectedEmbeddingDigest: knowledge.ExternalEmbeddingDigestExpectedQwen3,
		},
	})
	if err != nil {
		t.Fatalf("load merged production index: %v", err)
	}
	return corpus, sidecar
}

func wideNetRetriever(t *testing.T, cfg *config.Config, corpus knowledge.Corpus, sidecar knowledge.EmbeddingSidecar) *knowledge.Retriever {
	t.Helper()
	embedModel := "qwen3-embedding-8b"
	embedClient, err := embedding.NewClient(embedding.ClientOptions{
		BaseURL: cfg.Agent.LLM.BaseURL,
		APIKey:  cfg.Agent.LLM.APIKey,
		Model:   embedModel,
	})
	if err != nil {
		t.Fatalf("embedding client: %v", err)
	}
	// No reranker: it reorders a pool, it does not widen one, and this probe
	// asks a reachability question. Retrieval mode otherwise matches production.
	return knowledge.NewRetriever(corpus, knowledge.RetrieverOptions{
		TopK:             wideNetTopK,
		Mode:             knowledge.RetrievalModeQwen3RRF,
		EmbeddingSidecar: &sidecar,
		Embedder:         embedClient,
		EmbeddingModel:   embedModel,
		Now:              realCorpusRecallNow,
	})
}

// loadGradedCases takes the grade and the judged top-3 from the COMMITTED
// evaluation artifact, and joins only the question text from the uncommitted
// corpus.
//
// This split is deliberate. A second replay of the same 50 cases exists locally
// and disagrees with the committed run on 5 grades (all ±1 grade, top-3
// byte-identical on every one of them) — the judge is not deterministic. The
// committed run is the artifact of record and the one the 12-miss audit was
// performed against, so partitioning by anything else would silently move cases
// in and out of the labelled control set.
func loadGradedCases(t *testing.T) []gradedCase {
	t.Helper()
	path := os.Getenv("COMPSHARE_REAL_QUERY_CORPUS")
	if path == "" {
		t.Skip("COMPSHARE_REAL_QUERY_CORPUS not set; real questions are never committed")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	questions := map[string]string{}
	for _, line := range splitJSONLines(raw) {
		var one gradedCase
		if err := json.Unmarshal(line, &one); err != nil {
			t.Fatalf("corpus line: %v", err)
		}
		questions[one.CaseID] = one.Query
	}

	evalPath := filepath.Join("..", "..", "eval", "rag_v2_real_chat_retrieval_2026-07-15.json")
	evalRaw, err := os.ReadFile(evalPath)
	if err != nil {
		t.Fatalf("read %s: %v", evalPath, err)
	}
	var file struct {
		Cases []gradedCase `json:"cases"`
	}
	if err := json.Unmarshal(evalRaw, &file); err != nil {
		t.Fatalf("parse %s: %v", evalPath, err)
	}
	var out []gradedCase
	for _, c := range file.Cases {
		q, ok := questions[c.CaseID]
		if !ok || strings.TrimSpace(q) == "" {
			t.Fatalf("case %s has no question text in %s; the two files are not the same 50 cases", c.CaseID, path)
		}
		c.Query = q
		out = append(out, c)
	}
	if len(out) == 0 {
		t.Fatalf("%s produced no cases", evalPath)
	}
	return out
}

func reportBuckets(t *testing.T, label string, results []classification) {
	t.Helper()
	counts := map[string]int{}
	viaCounts := map[string]int{}
	contested := 0
	for _, r := range results {
		counts[r.Bucket]++
		if r.Bucket == bucketRetrievable {
			viaCounts[r.FoundVia]++
		}
		// A bucket that won without taking every vote is a case the classifier
		// could not settle; counting them is what keeps a 3-2 from being read
		// as a classification.
		if r.VoteSplit != fmt.Sprintf("%s=%d", r.Bucket, classifyRounds) {
			contested++
		}
	}
	n := len(results)
	t.Logf("== %s ==", label)
	t.Logf("  (其中 %d/%d 条 %d 次投票不一致，下面的分子含这些摇摆条目)", contested, n, classifyRounds)
	for _, b := range []string{bucketRetrievable, bucketNotInNet, bucketSufficient, bucketUndetermin} {
		t.Logf("  %-20s : %2d/%d (%.0f%%)", b, counts[b], n, 100*float64(counts[b])/float64(n))
	}
	if counts[bucketRetrievable] > 0 {
		t.Logf("  其中 original 查询就能召回(纯排序问题) : %d", viaCounts["original"])
		t.Logf("  其中只有 gap 查询能召回(纠错第二跳)   : %d", viaCounts["gap_query"])
	}
}

// reportControlGate scores the classifier against the 12 hand-audited misses.
// The gate is pre-registered: the audit found 0 retrieval-pipeline gaps among
// them, so a classifier that calls 3 or more of the 9 source-document gaps
// "retrievable" has a false-positive rate high enough (>=33%) that the partial
// numbers it produced cannot be read.
func reportControlGate(t *testing.T, misses []gradedCase, results []classification) {
	t.Helper()
	labels := loadAuditLabels(t)
	var knownSourceGap, misclassified int
	var detail []string
	for i, c := range misses {
		label := labels[c.CaseID]
		if label != "source_document_gap" {
			continue
		}
		knownSourceGap++
		if results[i].Bucket == bucketRetrievable {
			misclassified++
			detail = append(detail, fmt.Sprintf("    %s -> %s (via %s, %s)",
				c.CaseID, results[i].Bucket, results[i].FoundVia, results[i].FoundChunk))
		}
	}
	t.Logf("== 对照门 ==")
	t.Logf("  人工标注为 source gap 的 miss : %d", knownSourceGap)
	t.Logf("  被本分类器判成 retrievable    : %d", misclassified)
	for _, d := range detail {
		t.Logf("%s", d)
	}
	if knownSourceGap == 0 {
		t.Fatalf("control set is empty; the audit file and the corpus disagree on case ids")
	}
	if misclassified >= 3 {
		t.Errorf("classifier called %d/%d known source gaps retrievable: false-positive rate too high, the partial split above is not readable",
			misclassified, knownSourceGap)
	}
}

// TestLiveClassifierFalseNegativeRate is the control the miss set cannot
// provide.
//
// The audited misses only catch the classifier OVER-claiming: they are all
// source gaps, so a classifier hard-wired to answer "not_in_wide_net" scores a
// perfect 0 false positives on them while being completely useless. Every
// not_in_wide_net verdict in the main test rests on the opposite property —
// that the classifier says "found" when the content IS there — and nothing
// above measures it. A hand-check already turned up one miss of exactly that
// shape (a question about starting a VS Code remote session classified
// not_in_wide_net while ext-vscode-remote-server-001, titled for that exact
// procedure, sits in the index), so this is a known-nonzero rate, not a
// hypothetical.
//
// Construction: take cases the judge graded `full`. Their recorded top-3
// provably answers the question — that is what `full` means — and the original
// question retrieves those same chunks, so ground truth is guaranteed to be
// inside the net. Then hand the gap stage three chunks from an unrelated case,
// so it reports the whole question as unanswered. A classifier with no false
// negatives finds the answer in every one of these.
//
// Because ground truth is inside the net by construction, a "not found" here
// isolates the verdict stage: it is the verdict LLM being too strict, not the
// retrieval net being too narrow.
//
//	go test ./internal/engine -tags live -run TestLiveClassifierFalseNegativeRate -v -timeout 20m
func TestLiveClassifierFalseNegativeRate(t *testing.T) {
	cfg := loadLiveConfig(t)
	cases := loadGradedCases(t)
	corpus, sidecar := mergedProductionIndex(t)
	retriever := wideNetRetriever(t, cfg, corpus, sidecar)
	byID := chunkIndex(corpus)

	var full []gradedCase
	for _, c := range cases {
		if c.Judge.Grade == "full" {
			full = append(full, c)
		}
	}
	if len(full) < 4 {
		t.Fatalf("need several full-graded cases as ground truth, got %d", len(full))
	}

	type fnResult struct {
		caseID string
		found  bool
		note   string
	}
	results := make([]fnResult, len(full))
	var wg sync.WaitGroup
	ch := make(chan int)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				c := full[i]
				// Decoy evidence: the top-3 of a different case, so the gap
				// stage cannot see the real answer and reports the whole
				// question as unanswered.
				decoy := full[(i+1)%len(full)]
				var seen []string
				for _, id := range decoy.Top3 {
					if chunk, ok := byID[id]; ok {
						seen = append(seen, renderChunk(chunk))
					}
				}
				missing, gapQuery, err := stateGap(cfg, c.Query, seen)
				if err != nil {
					results[i] = fnResult{caseID: c.CaseID, note: "gap statement failed: " + err.Error()}
					continue
				}
				if missing == "" || strings.EqualFold(missing, "NONE") {
					// The decoy accidentally covered the question. Not a
					// classifier failure, but not a usable trial either.
					results[i] = fnResult{caseID: c.CaseID, note: "decoy covered the question; trial discarded"}
					continue
				}
				// Nothing is excluded from the net here: the real top-3 must be
				// reachable, since it is the ground truth being looked for.
				candidates := wideNet(retriever, c.Query, gapQuery, nil)
				hit, err := findSupportingChunk(cfg, c.Query, missing, candidates)
				if err != nil {
					results[i] = fnResult{caseID: c.CaseID, note: "verdict failed: " + err.Error()}
					continue
				}
				results[i] = fnResult{caseID: c.CaseID, found: hit != nil}
			}
		}()
	}
	for i := range full {
		ch <- i
	}
	close(ch)
	wg.Wait()

	var trials, found int
	for _, r := range results {
		if r.note != "" {
			t.Logf("  %s discarded: %s", r.caseID, r.note)
			continue
		}
		trials++
		if r.found {
			found++
		} else {
			t.Logf("  %s: FALSE NEGATIVE (answer is in the net, verdict said no)", r.caseID)
		}
	}
	if trials == 0 {
		t.Fatalf("every trial was discarded; no false-negative rate was measured")
	}
	missRate := 100 * float64(trials-found) / float64(trials)
	t.Logf("== 分类器漏报率 (ground truth 保证在网内) ==")
	t.Logf("  有效试验 : %d", trials)
	t.Logf("  判对     : %d", found)
	t.Logf("  漏报     : %d (%.0f%%)", trials-found, missRate)
	t.Logf("  => not_in_wide_net 的真实比例应按此漏报率向下修正；retrievable 的比例是下界")
}

func loadAuditLabels(t *testing.T) map[string]string {
	t.Helper()
	path := filepath.Join("..", "..", "eval", "rag_v2_real_chat_gap_audit_2026-07-15.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var file struct {
		Cases []struct {
			CaseID         string `json:"case_id"`
			Classification string `json:"classification"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse audit: %v", err)
	}
	out := map[string]string{}
	for _, c := range file.Cases {
		out[c.CaseID] = c.Classification
	}
	return out
}

// writeProbeDetail dumps the per-case detail for local review. It carries
// model-written descriptions of real customer questions, so the destination is
// expected to be outside the repo and nothing here is committed.
func writeProbeDetail(t *testing.T, results []classification) {
	t.Helper()
	out := os.Getenv("COMPSHARE_PROBE_OUT")
	if out == "" {
		t.Logf("COMPSHARE_PROBE_OUT unset; per-case detail not written")
		return
	}
	sorted := append([]classification{}, results...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CaseID < sorted[j].CaseID })
	blob, err := json.MarshalIndent(sorted, "", "  ")
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	if err := os.WriteFile(out, blob, 0o600); err != nil {
		t.Fatalf("write detail: %v", err)
	}
	t.Logf("per-case detail -> %s", out)
}
