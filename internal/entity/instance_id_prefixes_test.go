package entity

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The vocabulary is now supplied by callers, so the tokenizer — not the caller —
// has to be the thing that decides what a given set of prefixes recognises.
func TestInstanceIDTokensForPrefixesNormalizesWhatCallersHandOver(t *testing.T) {
	const text = "先看 cpod-1udq2s5hweqm，再看 UHOST-1qy6d8tkfrl4"

	base := InstanceIDTokensForPrefixes(text, []string{"cpod", "uhost"})
	require.Equal(t, []string{"cpod-1udq2s5hweqm", "UHOST-1qy6d8tkfrl4"}, base,
		"tokens come back in the order they appear in the TEXT, verbatim")

	require.Equal(t, base, InstanceIDTokensForPrefixes(text, []string{"uhost", "cpod"}),
		"the order the caller lists prefixes in must not change what is found")
	require.Equal(t, base, InstanceIDTokensForPrefixes(text, []string{"CPOD", " uhost ", "cpod", ""}),
		"nor must case, padding or duplicates")

	require.Empty(t, InstanceIDTokensForPrefixes(text, nil), "an empty vocabulary recognises nothing")
	require.Empty(t, InstanceIDTokensForPrefixes("", []string{"cpod"}))
}

// A prefix may not match inside a LONGER prefix's id. The '-' the match is anchored
// on is what enforces that, which is why the ordering above is only a determinism
// rule — this is the assertion that would catch dropping the anchor.
func TestAShortPrefixDoesNotTruncateALongerInstanceID(t *testing.T) {
	tokens := InstanceIDTokensForPrefixes("uhostx-abc123 现在怎么样", []string{"uhost", "uhostx"})
	require.Equal(t, []string{"uhostx-abc123"}, tokens)
}

// The vocabulary that made this split necessary: an id alone, with no registry.
func TestInstanceIDPrefixOfReadsThePrefixOffAnIDAlone(t *testing.T) {
	prefix, ok := InstanceIDPrefixOf("  cpod-1udplmzj28gl ")
	require.True(t, ok)
	require.Equal(t, "cpod", prefix)

	_, ok = InstanceIDPrefixOf("no-leading-prefix-is-fine-but-this-is-not-an-id")
	require.True(t, ok, "a hyphenated string does yield a prefix; existence is adjudicated elsewhere")

	_, ok = InstanceIDPrefixOf("nohyphen")
	require.False(t, ok)
	_, ok = InstanceIDPrefixOf("")
	require.False(t, ok)
}
