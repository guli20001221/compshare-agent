package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureRevisionLLM records the last request and returns a canned revision,
// so we can assert what the self-revision pass sends and how it handles the reply.
type captureRevisionLLM struct {
	resp    string
	lastReq llm.ChatRequest
	calls   int
}

func (m *captureRevisionLLM) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	m.lastReq = req
	m.calls++
	return &llm.ChatResponse{Content: m.resp}, nil
}

func TestKQASelfRevisionFlagDefaultOffAndToggle(t *testing.T) {
	assert.False(t, KQASelfRevisionEnabled(), "Go-package default must stay off so unit tests are unaffected")
	SetKQASelfRevisionEnabled(true)
	assert.True(t, KQASelfRevisionEnabled())
	SetKQASelfRevisionEnabled(false) // restore package default for other tests
	assert.False(t, KQASelfRevisionEnabled())
}

func TestReviseOverConservativeAnswerKeepsDraftOnEmpty(t *testing.T) {
	eng := NewWithDeps(&captureRevisionLLM{resp: "x"}, &mockExecutor{}, func(string, map[string]any) bool { return false })
	got, changed := eng.reviseOverConservativeAnswer(context.Background(), "q", "   ", nil)
	assert.False(t, changed, "empty draft must not be revised")
	assert.Equal(t, "", got)
}

// The revision prompt must carry the question, the evidence content (so the model
// can only commit facts that are present), and the draft; and a non-empty reply is
// returned as the revised answer. Fab-safety (no new facts) is enforced downstream
// by the caller's grounding re-validation + the eval fab gate, not asserted here.
func TestReviseOverConservativeAnswerShowsEvidenceAndReturnsRevision(t *testing.T) {
	mock := &captureRevisionLLM{resp: "按量实例关机后停止计费，可在控制台实例列表点击关机。[1]"}
	eng := NewWithDeps(mock, &mockExecutor{}, func(string, map[string]any) bool { return false })
	evidences, err := evidencesFromRetrievalHits([]knowledge.RetrievalHit{
		{Chunk: knowledge.KBChunk{ChunkID: "c1", Title: "关机计费", Content: "按量实例关机后停止计费", KBVersion: "w0"}, Score: 0.9},
	}, "关机")
	require.NoError(t, err)

	draft := "按量计费实例关机后停止计费。关于具体的关机操作步骤（如控制台入口），资料中未写明。[1]"
	got, changed := eng.reviseOverConservativeAnswer(context.Background(), "按量计费怎么关机", draft, evidences)

	require.True(t, changed)
	assert.Equal(t, mock.resp, got)
	require.Equal(t, 1, mock.calls)
	require.Len(t, mock.lastReq.Messages, 2)
	assert.Contains(t, mock.lastReq.Messages[0].Content, "过度保守", "system prompt states the anti-over-conservatism task")
	userMsg := mock.lastReq.Messages[1].Content
	assert.Contains(t, userMsg, "按量计费怎么关机", "prompt includes the question")
	assert.Contains(t, userMsg, "按量实例关机后停止计费", "prompt includes the evidence content")
	assert.Contains(t, userMsg, draft, "prompt includes the draft to revise")
}
