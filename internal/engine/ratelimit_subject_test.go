package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type subjectMockLLM struct{}

func (m subjectMockLLM) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "ok"}, nil
}

func TestRateLimitSubjectDerivedFromContext(t *testing.T) {
	eng := NewWithDeps(subjectMockLLM{}, &mockExecutor{}, func(string, map[string]any) bool { return false })
	eng.InitWithContext("test")

	assert.Equal(t, governance.AnonymousSubjectKey, eng.RateLimitSubjectKey())

	ctx := tools.WithUser(context.Background(), tools.UserContext{TopOrganizationID: 100, OrganizationID: 200})
	_, err := eng.ChatWithOptions(ctx, "hi", nil, ChatOptions{})
	require.NoError(t, err)

	expected, ok := governance.SubjectKeyFromOrganization(100, 200)
	require.True(t, ok)
	assert.Equal(t, expected, eng.RateLimitSubjectKey())
}

func TestRateLimitSubjectDefaultsToAnonymous(t *testing.T) {
	eng := NewWithDeps(subjectMockLLM{}, &mockExecutor{}, func(string, map[string]any) bool { return false })
	eng.InitWithContext("test")

	_, err := eng.ChatWithOptions(context.Background(), "hi", nil, ChatOptions{})
	require.NoError(t, err)

	assert.Equal(t, governance.AnonymousSubjectKey, eng.RateLimitSubjectKey())
}
