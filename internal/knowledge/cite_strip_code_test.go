package knowledge

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// StripCiteMarkers is the agent-loop half of the citation-stripping pair (the
// terminal-RAG half lives in engine/cited_guard.go). It ran its [[chunk_id]]
// removal, its positional-[n] removal AND its space-collapsing tidy over the
// entire answer — code blocks included — so a correct PyTorch snippet reached
// the user with its indentation flattened and its subscripts deleted.
//
// The knowledge package keeps its own gate so this cannot regress here without
// a test in this package failing; the cross-route parity assertion (the two
// strippers must hand the user identical code) lives in internal/engine, which
// is the only place both are reachable.
func TestStripCiteMarkers_CodeSurvives(t *testing.T) {
	answer := "先装依赖 [[kb-001]]，再跑 [1]：\n" +
		"\n" +
		"```python\n" +
		"def load(argv):\n" +
		"    path = argv[1]\n" +
		"    dev = devices[0]\n" +
		"    if dev is None:\n" +
		"        raise SystemExit(2)\n" +
		"```\n" +
		"\n" +
		"参数 `argv[1]` 是模型路径 [[kb-001]]。"

	got := StripCiteMarkers(answer)

	assert.Contains(t, got, "    path = argv[1]",
		"four-space indent and the [1] subscript must both survive — this is code the user pastes")
	assert.Contains(t, got, "    dev = devices[0]",
		"positionalCiteRE matches [0] as well, so a [0] subscript was being deleted here too")
	assert.Contains(t, got, "        raise SystemExit(2)",
		"the nested eight-space indent must survive")
	assert.Contains(t, got, "`argv[1]`", "inline code spans must survive")

	// Prose is still stripped — the function must still do its job.
	assert.NotContains(t, got, "[[kb-001]]", "chunk-id markers must be removed from prose")
	assert.NotContains(t, got, "再跑 [1]", "positional citations must be removed from prose")
}

// A shell `[[ ... ]]` conditional written WITHOUT a fence or backticks is prose as
// far as MapOutsideCode is concerned, and it used to match the citation marker: the
// whole test collapsed away and the user was handed `if; then echo ok; fi`. A
// chunk_id never contains whitespace, so the id class excludes it and the shell
// form no longer matches. Fenced/inline code was never affected (covered above).
func TestStripCiteMarkers_UnfencedShellConditionalSurvives(t *testing.T) {
	assert.Equal(t, "先确认 if [[ -f /root/model.bin ]]; then echo ok; fi 再继续。",
		StripCiteMarkers("先确认 if [[ -f /root/model.bin ]]; then echo ok; fi 再继续。"))
	assert.Equal(t, "用 [[ -d /data ]] 判断目录",
		StripCiteMarkers("用 [[ -d /data ]] 判断目录"))

	// The tolerated spaced-bracket citation form must still be recognized and
	// stripped — spaces around the id are fine, spaces inside it are not.
	assert.Equal(t, "实例已欠费，请支付订单。",
		StripCiteMarkers("实例已欠费，请支付订单 [ [w0-init_failure-001] ]。"))
}

// A model that emits an UNBALANCED marker — two opening brackets, one closing —
// used to slip past the strip (it demanded >=2 closing brackets) and leak the raw
// internal chunk_id into the user's reply. Seen live: an agent answer ended with
// `…关自动续费。[[v2-resource_purchase-1aa5f4052663d9bd]`. The doubled `[[` is
// signal enough; strip through the single `]`.
func TestStripCiteMarkers_UnbalancedTrailingMarkerStripped(t *testing.T) {
	assert.Equal(t, "在财务中心关闭自动续费。",
		StripCiteMarkers("在财务中心关闭自动续费。[[v2-resource_purchase-1aa5f4052663d9bd]"))
	// Mid-sentence, and the three-open/one-close variant, both leave no residue.
	assert.Equal(t, "先付款，再重试。",
		StripCiteMarkers("先付款[[kb-001]，再重试。"))
	assert.Equal(t, "见文档。",
		StripCiteMarkers("见文档[[[kb-002]。"))
	// A validated unbalanced marker must still register as a real citation so a
	// grounded answer is not misjudged as uncited.
	ledger := EvidenceLedger{Items: []EvidenceItem{{ChunkID: "kb-001"}}}
	report := ValidateGroundedCitations("先付款[[kb-001]，再重试。", ledger)
	assert.True(t, report.Grounded(), "an unbalanced but resolvable marker is a real citation")
	assert.Equal(t, []string{"kb-001"}, report.CitedChunkIDs)
}
