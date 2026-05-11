package main

import (
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
