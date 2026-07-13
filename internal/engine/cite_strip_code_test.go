package engine

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// The citation strippers were destroying the answers they were meant to clean.
//
// Both of them ran their [n]-removal AND their cosmetic space-collapsing over
// the whole reply, code blocks included. For an assistant whose knowledge corpus
// is vLLM / SGLang / PyTorch, that is not an edge case — a code block is what a
// good answer looks like. What the user actually received:
//
//	def main():
//	 rank = int(os.environ["RANK"])     <- four-space indent collapsed to one
//	 x = outputs                        <- outputs[1] lost its subscript
//	 if rank == 0:
//	 print(sys.argv)                    <- and so did sys.argv[1]
//
// That does not run. And the destruction was silent: no log, no trace, no guard
// fired. The retrieval was right, the model was right, the answer was right, and
// then a regex ate it on the way out the door.
//
// Nothing in the storage / session-context line of work touches this. It is not
// context that was lost; it is the answer.
// ---------------------------------------------------------------------------

// ddpAnswer is a realistic knowledge_qa reply: cited prose wrapped around a
// fenced Python block that uses both a [0] and a [1] subscript, plus an inline
// code span.
const ddpAnswer = "多卡训练用 torchrun 启动 [1]，每个进程绑定一张卡 [2]。\n" +
	"\n" +
	"```python\n" +
	"import os, sys\n" +
	"\n" +
	"def main():\n" +
	"    rank = int(os.environ[\"RANK\"])\n" +
	"    gpu = visible_gpus[0]\n" +
	"    ckpt = sys.argv[1]\n" +
	"    if rank == 0:\n" +
	"        print(f\"loading {ckpt} on {gpu}\")\n" +
	"```\n" +
	"\n" +
	"其中 `sys.argv[1]` 是检查点路径 [1]。"

// codeOf returns the fenced block, so a test can assert on it without the prose
// (which SHOULD change — the citations there are supposed to be stripped).
func codeOf(t *testing.T, s string) string {
	t.Helper()
	open := strings.Index(s, "```python")
	require.GreaterOrEqual(t, open, 0, "no fenced block in %q", s)
	rest := s[open+len("```python"):]
	end := strings.Index(rest, "```")
	require.GreaterOrEqual(t, end, 0, "unterminated fence")
	return rest[:end]
}

// Gate: the terminal-RAG stripper must not touch code.
//
// Mutation: replace stripCitationMarkers' body with the old whole-string rewrite
// (call stripCitationMarkersInProse directly) and this fails on the indent.
func TestStripCitationMarkers_LeavesCodeAlone(t *testing.T) {
	got := stripCitationMarkers(ddpAnswer)
	code := codeOf(t, got)

	assert.Contains(t, code, "    rank = int(os.environ[\"RANK\"])",
		"four-space indentation must survive — collapsing it produces code that raises IndentationError")
	assert.Contains(t, code, "        print(", "the nested eight-space indent must survive")
	assert.Contains(t, code, "visible_gpus[0]", "a [0] subscript must survive")
	assert.Contains(t, code, "sys.argv[1]", "a [1] subscript must survive")
	assert.Contains(t, got, "`sys.argv[1]`", "an inline code span must survive")

	// And the prose is still cleaned, which is the whole point of the function.
	assert.NotContains(t, got, "torchrun 启动 [1]", "citations in PROSE must still be stripped")
	assert.Contains(t, got, "多卡训练用 torchrun 启动，")
}

// Gate: the agent-loop stripper must not touch code either.
func TestStripCiteMarkers_LeavesCodeAlone(t *testing.T) {
	got := knowledge.StripCiteMarkers(ddpAnswer)
	code := codeOf(t, got)

	assert.Contains(t, code, "    rank = int(os.environ[\"RANK\"])")
	assert.Contains(t, code, "visible_gpus[0]")
	assert.Contains(t, code, "sys.argv[1]")
	assert.NotContains(t, got, "torchrun 启动 [1]")
}

// Gate: the two routes must not disagree about the user's code.
//
// This one is not hypothetical. The regexes were never the same —
// engine/cited_guard.go used `\[[1-9][0-9]*\]` (does NOT match [0]) while
// knowledge/grounded_validator.go used `\[(\d{1,2})\]` (DOES match [0]). So the
// same answer came back with a different amount of the user's code deleted
// depending on whether the turn went through the agent loop or the terminal RAG
// route — a difference no user could see coming and no test was watching.
//
// Confining both to prose removes the damage and the divergence together. This
// test pins the invariant that matters: whatever the routes do to prose, they
// must hand the user the SAME code.
func TestBothCiteStrippers_AgreeOnTheUsersCode(t *testing.T) {
	terminal := codeOf(t, stripCitationMarkers(ddpAnswer))
	agentLoop := codeOf(t, knowledge.StripCiteMarkers(ddpAnswer))

	assert.Equal(t, terminal, agentLoop,
		"the terminal-RAG route and the agent-loop route must deliver identical code — "+
			"the user cannot know which one answered them")

	original := codeOf(t, ddpAnswer)
	assert.Equal(t, original, terminal,
		"and both must deliver the code the model actually wrote")
}
