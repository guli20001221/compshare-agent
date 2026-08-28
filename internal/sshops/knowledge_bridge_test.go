package sshops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/opscontext"
)

type bridgeRetriever struct {
	result        knowledge.RetrievalResult
	queries       []string
	hints         []string
	chunks        map[string]knowledge.KBChunk
	readSearchIDs []string
	readErr       error
}

func (r *bridgeRetriever) RetrieveContext(_ context.Context, question, hint string) knowledge.RetrievalResult {
	r.queries = append(r.queries, question)
	r.hints = append(r.hints, hint)
	return r.result
}

func (r *bridgeRetriever) ReadChunks(_ context.Context, searchID string, chunkIDs []string) ([]knowledge.KBChunk, error) {
	r.readSearchIDs = append(r.readSearchIDs, searchID)
	if r.readErr != nil {
		return nil, r.readErr
	}
	result := make([]knowledge.KBChunk, 0, len(chunkIDs))
	for _, id := range chunkIDs {
		if chunk, ok := r.chunks[id]; ok {
			result = append(result, chunk)
		}
	}
	return result, nil
}

func bridgeHits(count int, content string) []knowledge.RetrievalHit {
	hits := make([]knowledge.RetrievalHit, 0, count)
	for i := 0; i < count; i++ {
		id := "chunk-" + string(rune('a'+i))
		hits = append(hits, knowledge.RetrievalHit{Kept: true, Score: 0.9, Chunk: knowledge.KBChunk{
			ChunkID: id, KBVersion: "kb-v1", Title: "标题 " + id,
			SourceType: "official_doc", ProductArea: "monitor", Content: content,
		}})
	}
	return hits
}

func TestKnowledgeBridgeBoundsSearchAndKeepsCapabilityPrivate(t *testing.T) {
	secretCapability := "search-capability-must-stay-in-go"
	secretAuthorization := "Bearer-secret-must-not-reach-kb"
	secretHint := "hint-secret-must-not-reach-kb"
	retriever := &bridgeRetriever{result: knowledge.RetrievalResult{
		Enabled: true, KBVersion: "kb-v1", SearchID: secretCapability,
		HybridMode: knowledge.RetrievalModeHybridCosine,
		HitItems:   bridgeHits(4, strings.Repeat("证据", 300)),
	}}
	bridge := newKnowledgeBridge(context.Background(), retriever)
	reply := bridge.handle(KnowledgeRequest{
		ID: "search-1", Operation: "search",
		Query:       "容器监控 Authorization: Bearer " + secretAuthorization,
		ContextHint: "monitor Authorization: Bearer " + secretHint,
	})
	if !reply.OK {
		t.Fatalf("search failed: %+v", reply)
	}
	result, ok := reply.Result.(knowledgeSearchResult)
	if !ok || len(result.Hits) != maxKnowledgeSearchHits {
		t.Fatalf("search result was not bounded to %d hits: %#v", maxKnowledgeSearchHits, reply.Result)
	}
	for _, hit := range result.Hits {
		if utf8.RuneCountInString(hit.Snippet) > knowledge.DefaultEvidenceSnippetMaxRunes {
			t.Fatalf("snippet exceeded %d runes: %d", knowledge.DefaultEvidenceSnippetMaxRunes, utf8.RuneCountInString(hit.Snippet))
		}
	}
	if len(retriever.queries) != 1 || strings.Contains(retriever.queries[0], secretAuthorization) {
		t.Fatalf("knowledge query was not redacted before leaving the broker: %#v", retriever.queries)
	}
	if len(retriever.hints) != 1 || strings.Contains(retriever.hints[0], secretHint) {
		t.Fatalf("knowledge context hint was not redacted before leaving the broker: %#v", retriever.hints)
	}
	wire, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), secretCapability) || strings.Contains(string(wire), "search_id") {
		t.Fatalf("private search capability crossed the harness wire: %s", wire)
	}
	if got := bridge.handle(KnowledgeRequest{ID: "read-unknown", Operation: "read", ChunkIDs: []string{"chunk-d"}}); got.OK || got.ErrorClass != "not_authorized" {
		t.Fatalf("a hit omitted by the search result became readable: %+v", got)
	}
}

func TestKnowledgeBridgeUsesTheSharedWeakEvidenceFloorAndKeepsCandidatesReviewable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     string
		score    float64
		wantWeak bool
	}{
		{name: "weak semantic", mode: knowledge.RetrievalModeHybridCosine, score: 0.2, wantWeak: true},
		{name: "weak bm25", mode: knowledge.RetrievalModeBM25Only, score: 10, wantWeak: true},
		{name: "unknown scale stays unjudged", mode: "future_scale", score: 0.01},
		{name: "raw rrf stays unjudged", mode: knowledge.RetrievalModeQwen3RRF, score: 0.01},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retriever := &bridgeRetriever{
				result: knowledge.RetrievalResult{
					Enabled: true, SearchID: "private-search", HybridMode: tc.mode,
					HitItems: []knowledge.RetrievalHit{{Kept: true, Score: tc.score,
						Chunk: knowledge.KBChunk{
							ChunkID: "chunk-a", Title: "候选", SourceType: "official_doc",
							ProductArea: "network", Content: "搜索正文不得作为候选元数据泄露",
						}}},
				},
				chunks: map[string]knowledge.KBChunk{
					"chunk-a": {ChunkID: "chunk-a", Title: "候选", Content: "读取后的完整正文"},
				},
			}
			bridge := newKnowledgeBridge(context.Background(), retriever)
			reply := bridge.handle(KnowledgeRequest{ID: "s", Operation: "search", Query: "平台契约"})
			if !reply.OK {
				t.Fatalf("search failed: %+v", reply)
			}
			result := reply.Result.(knowledgeSearchResult)
			if !tc.wantWeak {
				if len(result.Hits) != 1 {
					t.Fatalf("unjudged score scale lost its hit: %#v", result.Hits)
				}
				if result.FloorDroppedAll || len(result.BelowFloorCandidates) != 0 {
					t.Fatalf("unjudged score scale was mislabeled weak: %#v", result)
				}
				return
			}
			if len(result.Hits) != 0 || !result.Empty || !result.FloorDroppedAll {
				t.Fatalf("weak evidence became a normal hit: %#v", result)
			}
			if len(result.BelowFloorCandidates) != 1 {
				t.Fatalf("weak search lead was hidden: %#v", result.BelowFloorCandidates)
			}
			candidate := result.BelowFloorCandidates[0]
			if candidate.ChunkID != "chunk-a" || candidate.Title != "候选" || candidate.Strength != "below_floor" {
				t.Fatalf("weak candidate metadata = %#v", candidate)
			}
			wire, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{
				"搜索正文", "official_doc", "network", "snippet", "source_type", "product_area",
			} {
				if strings.Contains(string(wire), forbidden) {
					t.Fatalf("weak search candidate exposed %q: %s", forbidden, wire)
				}
			}
			read := bridge.handle(KnowledgeRequest{ID: "r", Operation: "read", ChunkIDs: []string{"chunk-a"}})
			if !read.OK {
				t.Fatalf("current-run weak candidate was not reviewable: %+v", read)
			}
			items := read.Result.(knowledgeReadResult).Chunks
			if len(items) != 1 || items[0].Content != "读取后的完整正文" || items[0].Strength != "below_floor" {
				t.Fatalf("weak full-text read lost its strength: %#v", items)
			}
			fresh := newKnowledgeBridge(context.Background(), retriever)
			if got := fresh.handle(KnowledgeRequest{ID: "fresh-read", Operation: "read", ChunkIDs: []string{"chunk-a"}}); got.OK || got.ErrorClass != "not_authorized" {
				t.Fatalf("weak read capability escaped its search run: %+v", got)
			}
		})
	}
}

func TestKnowledgeBridgeBoundsBelowFloorCandidatesAndReadCapabilities(t *testing.T) {
	overlongID := strings.Repeat("x", maxKnowledgeChunkIDRunes+1)
	hits := []knowledge.RetrievalHit{{
		Kept: true, Score: 0.2,
		Chunk: knowledge.KBChunk{ChunkID: overlongID, Title: "不可暴露", Content: "不可暴露正文"},
	}}
	chunks := map[string]knowledge.KBChunk{}
	for i := 0; i < maxKnowledgeSearchHits+1; i++ {
		id := "weak-" + string(rune('a'+i))
		hits = append(hits, knowledge.RetrievalHit{Kept: true, Score: 0.2 - float64(i)*0.01,
			Chunk: knowledge.KBChunk{ChunkID: id, Title: "候选 " + id, Content: "不随搜索返回"}})
		chunks[id] = knowledge.KBChunk{ChunkID: id, Content: "read " + id}
	}
	retriever := &bridgeRetriever{result: knowledge.RetrievalResult{
		Enabled: true, SearchID: "private-search", HybridMode: knowledge.RetrievalModeHybridCosine,
		HitItems: hits,
	}, chunks: chunks}
	bridge := newKnowledgeBridge(context.Background(), retriever)
	reply := bridge.handle(KnowledgeRequest{ID: "s", Operation: "search", Query: "平台契约"})
	if !reply.OK {
		t.Fatalf("search failed: %+v", reply)
	}
	result := reply.Result.(knowledgeSearchResult)
	if len(result.BelowFloorCandidates) != maxKnowledgeSearchHits {
		t.Fatalf("candidate count = %d, want %d: %#v",
			len(result.BelowFloorCandidates), maxKnowledgeSearchHits, result.BelowFloorCandidates)
	}
	if result.BelowFloorCandidates[0].ChunkID != "weak-a" {
		t.Fatalf("invalid candidate consumed the bounded window: %#v", result.BelowFloorCandidates)
	}
	if got := bridge.handle(KnowledgeRequest{ID: "r-overlong", Operation: "read", ChunkIDs: []string{overlongID}}); got.OK || got.ErrorClass != "invalid_request" {
		t.Fatalf("overlong candidate id reached capability lookup: %+v", got)
	}
	if got := bridge.handle(KnowledgeRequest{ID: "r-omitted", Operation: "read", ChunkIDs: []string{"weak-d"}}); got.OK || got.ErrorClass != "not_authorized" {
		t.Fatalf("unexposed bounded candidate became readable: %+v", got)
	}
}

func TestKnowledgeBridgeRejectsOverlongSearchInputsWithoutCallingRetriever(t *testing.T) {
	for name, req := range map[string]KnowledgeRequest{
		"query": {
			ID: "q", Operation: "search", Query: strings.Repeat("问", maxKnowledgeQueryRunes+1),
		},
		"context hint": {
			ID: "h", Operation: "search", Query: "监控",
			ContextHint: strings.Repeat("类", maxKnowledgeContextHintRunes+1),
		},
	} {
		t.Run(name, func(t *testing.T) {
			retriever := &bridgeRetriever{result: knowledge.RetrievalResult{Enabled: true, Empty: true}}
			got := newKnowledgeBridge(context.Background(), retriever).handle(req)
			if got.OK || got.ErrorClass != "invalid_request" {
				t.Fatalf("overlong input was accepted: %+v", got)
			}
			if len(retriever.queries) != 0 || len(retriever.hints) != 0 {
				t.Fatalf("invalid input reached the knowledge service: queries=%v hints=%v", retriever.queries, retriever.hints)
			}
		})
	}
}

func TestKnowledgeBridgeSearchDoesNotExposeUnreadableChunkIDsOrUnboundedProductArea(t *testing.T) {
	overlongID := strings.Repeat("c", maxKnowledgeChunkIDRunes+1)
	longArea := strings.Repeat("领域", maxKnowledgeProductAreaRunes)
	retriever := &bridgeRetriever{result: knowledge.RetrievalResult{
		Enabled: true, KBVersion: "kb-v1", SearchID: "private-search-id",
		HybridMode: knowledge.RetrievalModeHybridCosine,
		HitItems: []knowledge.RetrievalHit{
			{Kept: true, Score: 0.9, Chunk: knowledge.KBChunk{
				ChunkID: overlongID, Title: "unreadable", Content: "不应暴露",
			}},
			{Kept: true, Score: 0.8, Chunk: knowledge.KBChunk{
				ChunkID: "chunk-readable", Title: "可读取", ProductArea: longArea, Content: "证据",
			}},
		},
	}}
	bridge := newKnowledgeBridge(context.Background(), retriever)
	reply := bridge.handle(KnowledgeRequest{ID: "s", Operation: "search", Query: "监控"})
	if !reply.OK {
		t.Fatalf("search failed: %+v", reply)
	}
	result := reply.Result.(knowledgeSearchResult)
	if len(result.Hits) != 1 || result.Hits[0].ChunkID != "chunk-readable" {
		t.Fatalf("search exposed an unreadable chunk id: %#v", result.Hits)
	}
	if got := utf8.RuneCountInString(result.Hits[0].ProductArea); got != maxKnowledgeProductAreaRunes {
		t.Fatalf("product area length = %d, want %d: %q", got, maxKnowledgeProductAreaRunes, result.Hits[0].ProductArea)
	}
	if _, ok := bridge.capabilities[overlongID]; ok {
		t.Fatal("an unexposable chunk id was retained as a read capability")
	}
}

func TestKnowledgeBridgeReadIsSearchBoundAndPerCallRuneBounded(t *testing.T) {
	searchID := "private-search-id"
	content := strings.Repeat("界", 3000)
	retriever := &bridgeRetriever{
		result: knowledge.RetrievalResult{Enabled: true, KBVersion: "kb-v1", SearchID: searchID,
			HybridMode: knowledge.RetrievalModeHybridCosine,
			HitItems:   bridgeHits(3, "摘要")},
		chunks: map[string]knowledge.KBChunk{
			"chunk-a": {ChunkID: "chunk-a", Content: content},
			"chunk-b": {ChunkID: "chunk-b", Content: content},
			"chunk-c": {ChunkID: "chunk-c", Content: content},
		},
	}
	bridge := newKnowledgeBridge(context.Background(), retriever)
	if got := bridge.handle(KnowledgeRequest{ID: "s", Operation: "search", Query: "监控"}); !got.OK {
		t.Fatalf("search failed: %+v", got)
	}
	if got := bridge.handle(KnowledgeRequest{ID: "wide", Operation: "read", ChunkIDs: []string{"chunk-a", "chunk-b", "chunk-c", "chunk-d"}}); got.OK || got.ErrorClass != "limit_exceeded" {
		t.Fatalf("over-wide read was accepted: %+v", got)
	}
	read := bridge.handle(KnowledgeRequest{ID: "r1", Operation: "read", ChunkIDs: []string{"chunk-a", "chunk-b", "chunk-c"}})
	if !read.OK {
		t.Fatalf("read failed: %+v", read)
	}
	result := read.Result.(knowledgeReadResult)
	total := 0
	for _, item := range result.Chunks {
		total += utf8.RuneCountInString(item.Content)
		if item.Strength != "" {
			t.Fatalf("normal evidence read was mislabeled weak: %#v", item)
		}
	}
	if total != maxKnowledgeReadRunes {
		t.Fatalf("read body total = %d runes, want %d", total, maxKnowledgeReadRunes)
	}
	if len(retriever.readSearchIDs) != 1 || retriever.readSearchIDs[0] != searchID {
		t.Fatalf("broker did not use its private search capability: %#v", retriever.readSearchIDs)
	}
	second := bridge.handle(KnowledgeRequest{ID: "r2", Operation: "read", ChunkIDs: []string{"chunk-a", "chunk-b", "chunk-c"}})
	if !second.OK {
		t.Fatalf("second permitted read failed: %+v", second)
	}
	secondTotal := 0
	for _, item := range second.Result.(knowledgeReadResult).Chunks {
		secondTotal += utf8.RuneCountInString(item.Content)
	}
	if secondTotal != maxKnowledgeReadRunes {
		t.Fatalf("the per-call rune budget leaked across calls: second=%d want=%d", secondTotal, maxKnowledgeReadRunes)
	}
	if got := bridge.handle(KnowledgeRequest{ID: "r3", Operation: "read", ChunkIDs: []string{"chunk-a"}}); got.OK || got.ErrorClass != "limit_exceeded" {
		t.Fatalf("third read did not hit the per-run limit: %+v", got)
	}
}

func TestKnowledgeBridgeSearchBudgetAndRunIsolation(t *testing.T) {
	retriever := &bridgeRetriever{result: knowledge.RetrievalResult{Enabled: true, Empty: true}}
	bridge := newKnowledgeBridge(context.Background(), retriever)
	for i := 0; i < maxKnowledgeSearchCallsPerRun; i++ {
		if got := bridge.handle(KnowledgeRequest{ID: "s" + string(rune('0'+i)), Operation: "search", Query: "q"}); !got.OK {
			t.Fatalf("search %d failed early: %+v", i, got)
		}
	}
	if got := bridge.handle(KnowledgeRequest{ID: "over", Operation: "search", Query: "q"}); got.OK || got.ErrorClass != "limit_exceeded" {
		t.Fatalf("fifth search did not hit the limit: %+v", got)
	}
	fresh := newKnowledgeBridge(context.Background(), retriever)
	if got := fresh.handle(KnowledgeRequest{ID: "fresh", Operation: "search", Query: "q"}); !got.OK {
		t.Fatalf("a new run inherited the previous run's budget: %+v", got)
	}
}

func TestKnowledgeBridgeUnavailableDoesNotBecomeAProcessError(t *testing.T) {
	for name, retriever := range map[string]KnowledgeRetriever{
		"not configured": nil,
		"upstream unavailable": &bridgeRetriever{result: knowledge.RetrievalResult{
			Enabled: true, Empty: true, Unavailable: true, FailureReason: "must-not-cross-wire",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			reply := newKnowledgeBridge(context.Background(), retriever).handle(KnowledgeRequest{
				ID: "s", Operation: "search", Query: "监控",
			})
			if reply.OK || reply.ErrorClass != "unavailable" {
				t.Fatalf("unavailable retrieval was not a bounded tool failure: %+v", reply)
			}
			wire, _ := json.Marshal(reply)
			if strings.Contains(string(wire), "must-not-cross-wire") {
				t.Fatalf("raw upstream failure crossed the harness wire: %s", wire)
			}
		})
	}
}

func TestSupervisorKnowledgeSidebandRoundTripIsNotAGuestCommand(t *testing.T) {
	const searchID = "private-capability"
	retriever := &bridgeRetriever{
		result: knowledge.RetrievalResult{
			Enabled: true, KBVersion: "kb-v1", SearchID: searchID,
			HybridMode: knowledge.RetrievalModeHybridCosine,
			HitItems:   bridgeHits(1, "平台负责该运行时的监控采集。"),
		},
		chunks: map[string]knowledge.KBChunk{
			"chunk-a": {ChunkID: "chunk-a", Title: "容器监控", Content: "完整平台监控契约。"},
		},
	}
	fake := `
import json, sys
handshake = json.loads(sys.stdin.readline())
print('@@KNOWLEDGE ' + json.dumps({'id':'k1','operation':'search','query':'容器监控'}), flush=True)
searched = json.loads(sys.stdin.readline())
chunk_id = searched['result']['hits'][0]['chunk_id']
print('@@KNOWLEDGE ' + json.dumps({'id':'k2','operation':'read','chunk_ids':[chunk_id]}), flush=True)
read = json.loads(sys.stdin.readline())
print('<<<VERDICT>>>')
print('BRIDGE=%s SEARCH=%s READ=%s' % (handshake.get('knowledge_bridge_available'), searched['ok'], read['ok']))
print(json.dumps({'search': searched, 'read': read}, ensure_ascii=False, separators=(',', ':')))
print('<<<END>>>')
`
	sup := Supervisor{
		Python: requirePython(t), HarnessPath: writeFakeHarness(t, fake),
		SessionRoot: t.TempDir(), BaseURL: testAnthropicBaseURL,
		APIKey: testAnthropicAPIKey, Model: "gpt-5.6-terra", Timeout: 30 * time.Second,
		KnowledgeRetriever: retriever,
	}
	stepsSeen := 0
	res, err := sup.RunWithContext(context.Background(),
		cred("uhost-abc", "1.2.3.4", "root", 22, "pw"), "diagnose", opscontext.Context{},
		func(Step) { stepsSeen++ }, nil)
	if err != nil {
		t.Fatalf("run: %v (output=%q)", err, res.Output)
	}
	if !strings.Contains(res.Output, "BRIDGE=True SEARCH=True READ=True") {
		t.Fatalf("knowledge request did not make the complete process round trip: %q", res.Output)
	}
	if strings.Contains(res.Output, searchID) || strings.Contains(res.Output, "search_id") {
		t.Fatalf("private search capability crossed the process boundary: %q", res.Output)
	}
	if len(res.Steps) != 0 || stepsSeen != 0 {
		t.Fatalf("knowledge reads were counted as guest commands: steps=%+v streamed=%d", res.Steps, stepsSeen)
	}
	if len(retriever.readSearchIDs) != 1 || retriever.readSearchIDs[0] != searchID {
		t.Fatalf("private capability was not used inside Go: %#v", retriever.readSearchIDs)
	}
}

func TestSupervisorMalformedKnowledgeSidebandGetsOneBoundedFailureReply(t *testing.T) {
	fake := `
import json, sys
json.loads(sys.stdin.readline())
print('@@KNOWLEDGE {not-json', flush=True)
reply = json.loads(sys.stdin.readline())
print('<<<VERDICT>>>')
print('ID=%r OK=%s ERROR=%s' % (reply.get('id'), reply.get('ok'), reply.get('error_class')))
print('<<<END>>>')
`
	sup := Supervisor{
		Python: requirePython(t), HarnessPath: writeFakeHarness(t, fake),
		SessionRoot: t.TempDir(), BaseURL: testAnthropicBaseURL,
		APIKey: testAnthropicAPIKey, Model: "gpt-5.6-terra", Timeout: 30 * time.Second,
		KnowledgeRetriever: &bridgeRetriever{result: knowledge.RetrievalResult{Enabled: true, Empty: true}},
	}
	res, err := sup.RunWithContext(context.Background(),
		cred("uhost-abc", "1.2.3.4", "root", 22, "pw"), "diagnose", opscontext.Context{}, nil, nil)
	if err != nil {
		t.Fatalf("run: %v (output=%q)", err, res.Output)
	}
	if !strings.Contains(res.Output, "ID='' OK=False ERROR=invalid_request") {
		t.Fatalf("malformed request was not answered once with invalid_request: %q", res.Output)
	}
	if len(res.Steps) != 0 {
		t.Fatalf("malformed knowledge traffic became a guest command: %+v", res.Steps)
	}
}

type cancelBlockingKnowledgeRetriever struct {
	started chan struct{}
}

func (r *cancelBlockingKnowledgeRetriever) RetrieveContext(ctx context.Context, _, _ string) knowledge.RetrievalResult {
	close(r.started)
	<-ctx.Done()
	return knowledge.RetrievalResult{Enabled: true, Unavailable: true}
}

func TestSupervisorCancellationUnblocksKnowledgeRetrieverAndProcess(t *testing.T) {
	fake := `
import json, sys
json.loads(sys.stdin.readline())
print('@@KNOWLEDGE ' + json.dumps({'id':'k1','operation':'search','query':'容器监控'}), flush=True)
json.loads(sys.stdin.readline())
print('<<<VERDICT>>>')
print('unexpected completion')
print('<<<END>>>')
`
	retriever := &cancelBlockingKnowledgeRetriever{started: make(chan struct{})}
	sup := Supervisor{
		Python: requirePython(t), HarnessPath: writeFakeHarness(t, fake),
		SessionRoot: t.TempDir(), BaseURL: testAnthropicBaseURL,
		APIKey: testAnthropicAPIKey, Model: "gpt-5.6-terra", Timeout: 30 * time.Second,
		KnowledgeRetriever: retriever,
	}
	ctx, cancel := context.WithCancel(context.Background())
	type runResult struct {
		result Result
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		result, err := sup.RunWithContext(ctx,
			cred("uhost-abc", "1.2.3.4", "root", 22, "pw"), "diagnose", opscontext.Context{}, nil, nil)
		done <- runResult{result: result, err: err}
	}()
	select {
	case <-retriever.started:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("fake harness never reached the blocking knowledge retriever")
	}
	select {
	case got := <-done:
		if got.err == nil {
			t.Fatalf("canceled supervisor unexpectedly succeeded: %+v", got.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor and knowledge retriever deadlocked after context cancellation")
	}
}
