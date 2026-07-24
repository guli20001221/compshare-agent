package knowledge

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTokenizerIsInvariantToWhitespace states the contract directly: how a
// question is spaced must not change what it retrieves.
//
// This is not only eval hygiene. The live query planner rewrites the same
// question with different spacing run to run, and real users type both forms —
// so before this invariant held, two people asking the same thing could be
// handed different evidence.
func TestTokenizerIsInvariantToWhitespace(t *testing.T) {
	equivalent := [][]string{
		{"监听 端口", "监听端口", "监听  端口", " 监听端口 "},
		{"https 地址", "https地址", "HTTPS 地址", "ＨＴＴＰＳ地址"},
		{"优云智算 API base URL HTTPS 地址", "优云智算 API base URL https地址"},
		{"预付费 包月 退款", "预付费包月退款"},
		{"comfyui 导入工作流", "ComfyUI导入工作流"},
	}
	for _, forms := range equivalent {
		want := sortedTokens(forms[0])
		require.NotEmpty(t, want, "form %q tokenized to nothing", forms[0])
		for _, form := range forms[1:] {
			assert.Equal(t, want, sortedTokens(form),
				"%q and %q must tokenize identically", forms[0], form)
		}
	}
}

// TestTokenizerStillSeparatesScripts guards the other direction: making
// whitespace irrelevant must not fuse a latin word into its neighbouring CJK
// run, which would destroy the whole-word token BM25 relies on for API names,
// error codes and product terms.
func TestTokenizerStillSeparatesScripts(t *testing.T) {
	tokens := tokenizeRetrievalText("https地址")
	assert.Contains(t, tokens, "https", "an ASCII run must survive as one whole token")
	assert.Contains(t, tokens, "地址")
	for _, token := range tokens {
		assert.False(t, mixesScripts(token), "token %q fuses scripts", token)
	}

	codes := tokenizeRetrievalText("错误码226604资源不足")
	assert.Contains(t, codes, "226604", "an error code must stay a single token")
}

func sortedTokens(value string) []string {
	tokens := append([]string(nil), tokenizeRetrievalText(value)...)
	sort.Strings(tokens)
	return tokens
}

func mixesScripts(token string) bool {
	var hasASCII, hasCJK bool
	for _, r := range token {
		if isASCIIAlnum(r) {
			hasASCII = true
		}
		if isCJK(r) {
			hasCJK = true
		}
	}
	return hasASCII && hasCJK
}
