package textutil

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// destroy is a stand-in for what the citation strippers actually do: delete
// bracketed numbers and collapse runs of spaces. If MapOutsideCode ever lets it
// reach code, these tests show exactly the damage that shipped to users.
func destroy(s string) string {
	out := strings.NewReplacer("[1]", "", "[2]", "", "[0]", "").Replace(s)
	for strings.Contains(out, "  ") {
		out = strings.ReplaceAll(out, "  ", " ")
	}
	return out
}

func TestMapOutsideCode_FencedBlockIsByteIdentical(t *testing.T) {
	in := "看资料 [1]：\n" +
		"```python\n" +
		"def main():\n" +
		"    rank = int(os.environ[\"RANK\"])\n" +
		"    x = outputs[1]\n" +
		"    if rank == 0:\n" +
		"        print(sys.argv[1])\n" +
		"```\n" +
		"完毕 [2]。"

	got := MapOutsideCode(in, destroy)

	// The code, byte for byte.
	code := "```python\ndef main():\n    rank = int(os.environ[\"RANK\"])\n    x = outputs[1]\n    if rank == 0:\n        print(sys.argv[1])\n```"
	assert.Contains(t, got, code,
		"the fenced block must survive untouched — four-space indents intact, outputs[1] and sys.argv[1] intact. "+
			"Anything else is code that raises IndentationError when the user pastes it")

	// The prose around it, still rewritten.
	assert.True(t, strings.HasPrefix(got, "看资料 ："), "prose before the fence must still be processed, got %q", got[:12])
	assert.True(t, strings.HasSuffix(got, "完毕 。"), "prose after the fence must still be processed")
}

func TestMapOutsideCode_InlineSpanIsByteIdentical(t *testing.T) {
	in := "用 `sys.argv[1]` 取参数 [1]，见 `gpus[0]`。"

	got := MapOutsideCode(in, destroy)

	assert.Contains(t, got, "`sys.argv[1]`", "an inline code span must keep its subscript")
	assert.Contains(t, got, "`gpus[0]`", "an inline code span must keep its subscript")
	assert.NotContains(t, got, "参数 [1]", "the citation in prose must still be stripped")
}

func TestMapOutsideCode_TildeFence(t *testing.T) {
	in := "a [1]\n~~~\nkeep  me[1]\n~~~\nb [2]"
	got := MapOutsideCode(in, destroy)
	assert.Contains(t, got, "keep  me[1]", "~~~ fences must be honoured too")
	assert.NotContains(t, got, "a [1]")
}

// A fence that is never closed swallows the rest of the answer. That is the
// deliberate direction: an un-stripped [1] in prose is cosmetic, whereas a
// stripped indent in code is broken code.
func TestMapOutsideCode_UnterminatedFenceFailsSafe(t *testing.T) {
	in := "before [1]\n```\ncode  here[1]\nstill code [2]"
	got := MapOutsideCode(in, destroy)
	assert.Contains(t, got, "code  here[1]")
	assert.Contains(t, got, "still code [2]")
	assert.NotContains(t, got, "before [1]", "prose before the fence is still processed")
}

// The identity property: with f = identity, MapOutsideCode must reproduce the
// input byte for byte, whatever the mix of fences, spans and newlines. This is
// what proves the newline bookkeeping in splitKeepingNewlines is not silently
// eating or adding a line.
func TestMapOutsideCode_IdentityIsLossless(t *testing.T) {
	identity := func(s string) string { return s }
	for _, in := range []string{
		"",
		"plain",
		"plain\n",
		"\n\n\n",
		"a\n```\nb\n```\nc",
		"a\n```\nb\n```",
		"```\nunterminated",
		"`inline` and `more`",
		"trailing space \n and `x` \n",
		"~~~go\nfunc f() {}\n~~~\n",
		"mixed `a` ```\nfence\n``` tail",
	} {
		require.Equal(t, in, MapOutsideCode(in, identity), "identity must be lossless for %q", in)
	}
}

func TestMapOutsideCode_NoCodeBehavesExactlyLikeTheRawFunction(t *testing.T) {
	in := "全是散文 [1]，没有代码 [2]。  两个空格。"
	assert.Equal(t, destroy(in), MapOutsideCode(in, destroy),
		"with no code present, MapOutsideCode must be a pure passthrough to f — "+
			"otherwise this refactor silently changed prose behaviour")
}
