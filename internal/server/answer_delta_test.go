package server

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

// TestChunkReplyForDelta_RoundTrip is the invariant that anchors the
// whole answer_delta contract: concat of all chunks == input,
// byte-for-byte. Clients can choose between reading deltas
// incrementally or reading the answer_final canonical text; either
// path must produce the same string.
func TestChunkReplyForDelta_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"empty", ""},
		{"single_line", "hello world"},
		{"multi_line", "line1\nline2\nline3"},
		{"chinese", "中文回复行 1\n中文回复行 2"},
		{"trailing_newline", "line1\nline2\n"},
		{"empty_lines", "\n\nline\n\n"},
		{"long_line_no_newline", strings.Repeat("x", 250)},
		{"long_chinese_line", strings.Repeat("中", 250)},
		{"mixed_long_short", strings.Repeat("a", 100) + "\nshort\n" + strings.Repeat("b", 50)},
		{"realistic_reply", "您可以通过控制台 -> 实例列表 查看所有实例。\n\n相关入口:\n1. 控制台首页\n2. 实例管理\n\n如有疑问请联系平台支持。"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := chunkReplyForDelta(tc.text)
			joined := strings.Join(chunks, "")
			assert.Equal(t, tc.text, joined,
				"chunk concat must equal input byte-for-byte (chunks=%d)", len(chunks))
		})
	}
}

// TestChunkReplyForDelta_EmptyInputProducesNil ensures we don't emit a
// pointless empty-text answer_delta frame for an empty reply.
func TestChunkReplyForDelta_EmptyInputProducesNil(t *testing.T) {
	chunks := chunkReplyForDelta("")
	assert.Nil(t, chunks, "empty input must produce nil chunks (no answer_delta frame emitted)")
}

// TestChunkReplyForDelta_NoChunkExceedsCap verifies the size cap.
// A chunk larger than answerDeltaMaxRunes would defeat the
// "render incrementally" UX goal.
func TestChunkReplyForDelta_NoChunkExceedsCap(t *testing.T) {
	long := strings.Repeat("a", answerDeltaMaxRunes*5+17)
	chunks := chunkReplyForDelta(long)
	for i, c := range chunks {
		runes := utf8.RuneCountInString(c)
		assert.LessOrEqual(t, runes, answerDeltaMaxRunes,
			"chunk %d has %d runes, exceeds cap %d", i, runes, answerDeltaMaxRunes)
	}
}

// TestChunkReplyForDelta_NeverSplitsRune verifies that no chunk ends
// mid-UTF-8-sequence. Chinese / emoji / any multi-byte rune must
// survive the split intact — otherwise the client renders garbage.
func TestChunkReplyForDelta_NeverSplitsRune(t *testing.T) {
	cases := []string{
		strings.Repeat("中", 200),
		"中文混合 ASCII " + strings.Repeat("a中b中c", 50),
		"🚀 emoji line 1\n" + strings.Repeat("✨", 100),
	}
	for _, text := range cases {
		t.Run(text[:min(len(text), 30)], func(t *testing.T) {
			for _, c := range chunkReplyForDelta(text) {
				assert.True(t, utf8.ValidString(c),
					"chunk %q is not valid UTF-8 — split occurred mid-rune", c)
			}
		})
	}
}

// TestChunkReplyForDelta_PrefersNewlineBoundaries verifies that
// natural-language structure isn't trashed by mid-line splits when
// the cap isn't hit. Each line under the cap should be a single chunk
// (or part of a multi-line chunk that ends on a newline).
func TestChunkReplyForDelta_PrefersNewlineBoundaries(t *testing.T) {
	text := "短行 1\n短行 2\n短行 3"
	chunks := chunkReplyForDelta(text)
	// 3 short lines well under the cap should fold into one chunk.
	assert.Len(t, chunks, 1, "short lines should fold into a single chunk; got %v", chunks)
	assert.Equal(t, text, chunks[0])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
