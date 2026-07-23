package deployment

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// platformRowWithTags builds one upstream platform-catalog row carrying the given
// raw Tags — the exact shape DescribeCompShareImages returns.
func platformRowWithTags(id string, tags ...any) map[string]any {
	return map[string]any{
		"ImageSet": []any{map[string]any{
			"CompShareImageId": id,
			"Name":             "PyTorch 2.4",
			"ImageType":        "App",
			"Status":           "Available",
			"Tags":             tags,
		}},
	}
}

// TestCompoundUpstreamTagsBecomeSeparateTags covers the tag values upstream really
// stores. Two spellings of one tag arrive joined by a full-width comma INSIDE a
// single string, so the raw value is neither a usable label nor a matchable member.
//
// Live sample 2026-07-22 (platform catalog, 72 rows): "pytorch，Pytorch" on 17 rows,
// "comfyUI，ComfyUI" on 8, "miniconda，Miniconda3" on 5, "黑神话，黑神话悟空" on 1.
func TestCompoundUpstreamTagsBecomeSeparateTags(t *testing.T) {
	entries := ParsePlatformImageEntries(platformRowWithTags("img-1", "pytorch，Pytorch"), "platform")
	require.Len(t, entries, 1)
	assert.Equal(t, []string{"pytorch"}, entries[0].Tags,
		"the compound string is two spellings of one tag; it must not survive as a literal tag")

	// A genuinely multi-tag row keeps every distinct concept.
	entries = ParsePlatformImageEntries(
		platformRowWithTags("img-2", "comfyUI，ComfyUI", "深度学习"), "platform")
	require.Len(t, entries, 1)
	assert.Equal(t, []string{"comfyUI", "深度学习"}, entries[0].Tags,
		"splitting must not lose the row's other tags")
}

// TestCleanTagsAreLeftExactlyAsUpstreamSentThem is the other half: this is a split,
// not a normalizer that gets to rewrite tag text. Community rows are already clean
// (219/219 tagged, live 2026-07-22) and their tags are the platform taxonomy's own
// values — rewriting any of them would break exact membership against that taxonomy.
func TestCleanTagsAreLeftExactlyAsUpstreamSentThem(t *testing.T) {
	entries := ParsePlatformImageEntries(
		platformRowWithTags("img-3", "ComfyUI", "Qwen-Image", "语音合成"), "platform")
	require.Len(t, entries, 1)
	assert.Equal(t, []string{"ComfyUI", "Qwen-Image", "语音合成"}, entries[0].Tags,
		"clean tags must pass through byte-identical, including their casing")
}

// TestAbsentTagsStayNil guards the documented Tags contract: nil means "we don't
// know this image's tags", never "matches no tag". A consumer that excluded
// untagged images would drop all 9 platform System images, which carry no tags at
// all (live 2026-07-22).
func TestAbsentTagsStayNil(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags []any
	}{
		{"no Tags key", nil},
		{"empty list", []any{}},
		{"blank strings", []any{"", "   "}},
		{"separators only", []any{"，", ","}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := ParsePlatformImageEntries(platformRowWithTags("img-4", tc.tags...), "platform")
			require.Len(t, entries, 1)
			assert.Nil(t, entries[0].Tags,
				"absence must stay absent, not become an empty set that excludes the image")
		})
	}
}
