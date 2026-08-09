package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Both URLs below are literal values from the corpus, checked against the live
// docs site on 2026-08-09: membership answered 200 with real content, and every
// one of the eight /docs/gpus/action/ pages answered 404. They are written out
// rather than invented so the test fails if the policy stops matching reality.
const (
	liveDocsURL        = "https://www.compshare.cn/docs/overview/member/membership"
	unreachableDocsURL = "https://www.compshare.cn/docs/gpus/action/StartCompShareInstance"
)

func strptr(s string) *string { return &s }

// citationEngine wires an agent-loop turn that already ran SearchKnowledge and
// kept the given chunks, with the visible-citation flag forced on for the test.
func citationEngine(t *testing.T, chunks ...knowledge.KBChunk) *Engine {
	t.Helper()
	previous := answerCitationsEnabled
	answerCitationsEnabled = true
	t.Cleanup(func() { answerCitationsEnabled = previous })

	hits := make([]knowledge.RetrievalHit, 0, len(chunks))
	for _, chunk := range chunks {
		hits = append(hits, knowledge.RetrievalHit{Kept: true, Score: 90, Chunk: chunk})
	}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.searchKnowledgeRanThisTurn = true
	eng.searchKnowledgeHitsThisTurn = hits
	eng.searchKnowledgeLedgerThisTurn = knowledge.BuildSubstantiveEvidenceLedger("会员等级", hits, 3, 0)
	require.Len(t, eng.searchKnowledgeLedgerThisTurn.Items, len(chunks), "precondition: every kept hit produced a ledger item")
	return eng
}

// finish returns the two texts that must differ: what the user reads, and what
// becomes the assistant history message the model reads next turn. The engine
// composes them at exactly one place (the final-text branch); this mirrors it.
func finish(t *testing.T, eng *Engine, question, answer string) (display, history string) {
	t.Helper()
	history = eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), question, answer)
	return withAnswerCitations(history, eng.answerCitationsBlockThisTurn), history
}

func memberChunk() knowledge.KBChunk {
	return knowledge.KBChunk{
		ChunkID: "kb-member-001", KBVersion: "test.fixture", Title: "会员规则",
		Content: "会员等级由实名认证或累计消费决定。", SurfaceURL: strptr(liveDocsURL),
	}
}

// The whole point of the block: an answer that cites retrieved evidence tells the
// user which page it came from, with a link they can open.
func TestAnswerCitations_GroundedAnswerCarriesTheCitedSource(t *testing.T) {
	eng := citationEngine(t, memberChunk())
	display, _ := finish(t, eng, "会员等级", "会员等级由实名认证或累计消费决定[[kb-member-001]]。")

	assert.Equal(t, "会员等级由实名认证或累计消费决定。\n\n参考来源：\n- [会员规则]("+liveDocsURL+")", display)
	require.Equal(t, groundingSupported, eng.groundingOutcomeThisTurn)
}

// THE containment property. A rendered URL must never enter the assistant history
// message: the model would meet a documentation link in its own prior turn and
// could learn to produce one from memory, and a URL in prose bypasses the surface
// policy and the liveness filter that are the only reason a link can be trusted.
func TestAnswerCitations_HistoryTextNeverCarriesALink(t *testing.T) {
	eng := citationEngine(t, memberChunk())
	display, history := finish(t, eng, "会员等级", "会员等级由实名认证或累计消费决定[[kb-member-001]]。")

	assert.Equal(t, "会员等级由实名认证或累计消费决定。", history, "history is the answer alone")
	assert.NotContains(t, history, "https://")
	assert.NotContains(t, history, answerCitationsHeading)
	assert.Contains(t, display, liveDocsURL, "premise: the user did get a link, so this is a real separation")
}

// Default off must leave both texts exactly as they were before the block existed.
func TestAnswerCitations_FlagOffAppendsNothing(t *testing.T) {
	eng := citationEngine(t, memberChunk())
	answerCitationsEnabled = false

	display, history := finish(t, eng, "会员等级", "会员等级由实名认证或累计消费决定[[kb-member-001]]。")
	assert.Equal(t, "会员等级由实名认证或累计消费决定。", display)
	assert.Equal(t, display, history)
	assert.Empty(t, eng.answerCitationsBlockThisTurn)
}

// The fail-open arm strips markers it could NOT resolve. It does not know which
// evidence the prose came from, so it must not guess a source.
func TestAnswerCitations_FailOpenArmCarriesNoSources(t *testing.T) {
	eng := citationEngine(t, memberChunk())
	uncited, _ := finish(t, eng, "会员等级", "会员等级由实名认证决定。")
	assert.Equal(t, "会员等级由实名认证决定。", uncited, "an uncited answer ships unchanged, with no source list")
	require.Equal(t, groundingUnavailable, eng.groundingOutcomeThisTurn)

	eng2 := citationEngine(t, memberChunk())
	misCited, _ := finish(t, eng2, "会员等级", "会员等级由实名认证决定[[kb-does-not-exist]]。")
	assert.Equal(t, "会员等级由实名认证决定。", misCited, "a fabricated marker resolves to nothing, so it contributes no link")
	assert.Empty(t, eng2.answerCitationsBlockThisTurn)
}

// A link that 404s is worse than no link: it reads as a fabricated source.
func TestAnswerCitations_SkipsUnreachableDocsPath(t *testing.T) {
	chunk := knowledge.KBChunk{
		ChunkID: "kb-action-001", KBVersion: "test.fixture", Title: "启动实例",
		Content: "StartCompShareInstance 启动一台已停止的实例。", SurfaceURL: strptr(unreachableDocsURL),
	}
	eng := citationEngine(t, chunk)

	display, _ := finish(t, eng, "如何启动实例", "调用 StartCompShareInstance 即可[[kb-action-001]]。")
	assert.Equal(t, "调用 StartCompShareInstance 即可。", display, "the answer still ships; only the dead link is withheld")
	assert.NotContains(t, display, unreachableDocsURL)
}

// The exclusion is a path-segment match, not a spelling one: a different section
// that merely starts with the same letters is still a perfectly good source.
func TestCitationURLIsReachable_MatchesWholeSegmentsOnly(t *testing.T) {
	assert.False(t, citationURLIsReachable(unreachableDocsURL))
	assert.False(t, citationURLIsReachable("https://www.compshare.cn/docs/gpus/action/StopCompShareInstance"))
	assert.True(t, citationURLIsReachable("https://www.compshare.cn/docs/gpus/actionlog/overview"), "a longer segment is a different section")
	// The bare section path is excluded too, and that matches the site: /docs/gpus/action
	// answers 404 and /docs/gpus/action/ answers 308 back to it (so does /docs/gpus —
	// the site serves no section indexes). No corpus chunk points there either.
	assert.False(t, citationURLIsReachable("https://www.compshare.cn/docs/gpus/action"))
	assert.True(t, citationURLIsReachable("https://www.compshare.cn/docs/action/gpus/x"), "segment order matters")
	assert.True(t, citationURLIsReachable(liveDocsURL))
}

// Retrieval keeps several chunks; only the ones the answer actually cited may be
// presented as its sources.
func TestAnswerCitations_ListsOnlyTheCitedChunks(t *testing.T) {
	other := knowledge.KBChunk{
		ChunkID: "kb-billing-001", KBVersion: "test.fixture", Title: "计费说明",
		Content: "按量计费按小时结算。", SurfaceURL: strptr("https://www.compshare.cn/docs/gpus/billing/overview"),
	}
	eng := citationEngine(t, memberChunk(), other)

	display, _ := finish(t, eng, "会员等级", "会员等级由实名认证决定[[kb-member-001]]。")
	assert.Contains(t, display, liveDocsURL)
	assert.NotContains(t, display, "计费说明", "an uncited retrieved chunk is not a source of this answer")
	assert.Equal(t, 1, strings.Count(display, "\n- ["))
}

// Two chunks of one document page must not produce the same link twice.
func TestAnswerCitations_DedupesByURL(t *testing.T) {
	first := memberChunk()
	second := memberChunk()
	second.ChunkID = "kb-member-002"
	second.Title = "会员权益"
	eng := citationEngine(t, first, second)

	display, _ := finish(t, eng, "会员等级",
		"会员等级由实名认证决定[[kb-member-001]]，权益随等级提升[[kb-member-002]]。")
	assert.Equal(t, 1, strings.Count(display, liveDocsURL), "one page, one link")
	assert.Contains(t, display, "会员规则", "the first cited chunk's title labels the shared page")
}

// The surface-URL safety policy stays the gate for what may be shown at all.
func TestAnswerCitations_RejectsURLsTheSurfacePolicyDenies(t *testing.T) {
	for _, badURL := range []string{
		"http://www.compshare.cn/docs/overview/member/membership", // not https
		"https://wiki.internal.example.com/docs/member",           // host not allowlisted
		"https://www.compshare.cn/admin/member",                   // internal path
	} {
		chunk := memberChunk()
		chunk.SurfaceURL = strptr(badURL)
		eng := citationEngine(t, chunk)

		display, _ := finish(t, eng, "会员等级", "会员等级由实名认证决定[[kb-member-001]]。")
		assert.NotContains(t, display, answerCitationsHeading, "rejected by surface policy: %s", badURL)
		assert.NotContains(t, display, badURL)
	}
}

// A chunk with no URL at all contributes nothing, and must not leave an empty
// heading behind. 1200 of the corpus's 1744 chunks are in this state.
func TestAnswerCitations_ChunkWithoutURLProducesNoHeading(t *testing.T) {
	chunk := memberChunk()
	chunk.SurfaceURL = nil
	eng := citationEngine(t, chunk)

	display, _ := finish(t, eng, "会员等级", "会员等级由实名认证决定[[kb-member-001]]。")
	assert.Equal(t, "会员等级由实名认证决定。", display)
}

// An answer the Agent deliberately left empty (the pure-billing shape) must not
// be turned into a bare heading with links under it.
func TestWithAnswerCitations_NeverProducesAnOrphanBlock(t *testing.T) {
	block := "参考来源：\n- [会员规则](" + liveDocsURL + ")"
	assert.Empty(t, withAnswerCitations("", block))
	assert.Equal(t, "   ", withAnswerCitations("   ", block))
	assert.Equal(t, "答案。", withAnswerCitations("答案。", ""))
	assert.Equal(t, "答案。\n\n"+block, withAnswerCitations("答案。\n\n\n", block), "trailing newlines collapse to one break")
}

// The deployed corpus emits surface_url and no longer emits source_url; older
// corpora do the reverse. Both must resolve, with the current field winning.
func TestChunkSurfaceURL_PrefersSurfaceOverLegacySource(t *testing.T) {
	legacy := "https://www.compshare.cn/docs/legacy/page"
	assert.Equal(t, liveDocsURL, chunkSurfaceURL(knowledge.KBChunk{SurfaceURL: strptr(liveDocsURL), SourceURL: legacy}))
	assert.Equal(t, legacy, chunkSurfaceURL(knowledge.KBChunk{SourceURL: legacy}), "legacy corpora still cite")
	assert.Equal(t, liveDocsURL, chunkSurfaceURL(knowledge.KBChunk{SurfaceURL: strptr("  " + liveDocsURL + "  ")}))
	assert.Empty(t, chunkSurfaceURL(knowledge.KBChunk{SurfaceURL: strptr("   ")}), "a blank surface_url falls through, not through as whitespace")
	assert.Empty(t, chunkSurfaceURL(knowledge.KBChunk{}))
}
