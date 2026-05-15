package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/prompt"
	"github.com/stretchr/testify/assert"
)

func TestApplyStartupSuggestionOnlyOnFirstTurn(t *testing.T) {
	suggestions := []prompt.Suggestion{
		{Text: "first suggestion"},
		{Text: "second suggestion"},
	}

	got, ok := applyStartupSuggestion("2", suggestions, 0)
	assert.True(t, ok)
	assert.Equal(t, "second suggestion", got)

	got, ok = applyStartupSuggestion("2", suggestions, 1)
	assert.False(t, ok)
	assert.Equal(t, "2", got)
}

func TestApplyStartupSuggestionIgnoresInvalidInput(t *testing.T) {
	suggestions := []prompt.Suggestion{{Text: "first suggestion"}}

	got, ok := applyStartupSuggestion("select two", suggestions, 0)
	assert.False(t, ok)
	assert.Equal(t, "select two", got)

	got, ok = applyStartupSuggestion("2", suggestions, 0)
	assert.False(t, ok)
	assert.Equal(t, "2", got)
}

func TestKnowledgeRetrieverStartupFatalsWhenRequestedAndCorpusInvalid(t *testing.T) {
	originalFatalf := startupFatalf
	t.Cleanup(func() { startupFatalf = originalFatalf })
	startupFatalf = func(format string, args ...any) {
		panic(fmt.Sprintf(format, args...))
	}

	err := errors.New("corpus digest mismatch: got bad want good")

	assert.PanicsWithValue(t, "RAG enabled but corpus digest mismatch (refusing to start): corpus digest mismatch: got bad want good", func() {
		applyKnowledgeRetrieverStartup(nil, true, nil, false, err)
	})
}

func TestKnowledgeRetrieverStartupSkipsDigestCheckWhenRAGDisabled(t *testing.T) {
	originalFatalf := startupFatalf
	t.Cleanup(func() { startupFatalf = originalFatalf })
	startupFatalf = func(format string, args ...any) {
		t.Fatalf("startupFatalf should not be called: "+format, args...)
	}

	applyKnowledgeRetrieverStartup(nil, false, nil, false, errors.New("corpus digest mismatch"))
}

func TestKnowledgeRetrieverStartupFatalMessageMentionsDigestMismatch(t *testing.T) {
	originalFatalf := startupFatalf
	t.Cleanup(func() { startupFatalf = originalFatalf })
	var got string
	startupFatalf = func(format string, args ...any) {
		got = fmt.Sprintf(format, args...)
		panic(got)
	}

	assert.Panics(t, func() {
		applyKnowledgeRetrieverStartup(nil, true, nil, false, errors.New("open corpus: digest mismatch"))
	})
	assert.True(t, strings.Contains(got, "digest mismatch"), got)
}
