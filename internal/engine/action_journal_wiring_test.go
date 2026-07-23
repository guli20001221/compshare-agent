package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pathJournal struct{ calls []string }

func (j *pathJournal) Execute(ctx context.Context, action string, args map[string]any, call tools.ActionCall) (map[string]any, error) {
	j.calls = append(j.calls, action)
	return call(ctx, action, args)
}

func (j *pathJournal) Err() error { return nil }

// sagaFakeExecutor is a recording tool executor: it captures every action name
// and returns a benign success payload.
type sagaFakeExecutor struct{ calls []string }

func (f *sagaFakeExecutor) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, action)
	return map[string]any{"RetCode": float64(0)}, nil
}

func newJournaledPathEngine(journal tools.ActionJournal, inner tools.ToolExecutor) *Engine {
	return NewSession(&SharedDeps{ExternalExecutor: inner}, SessionOptions{
		ConfirmFn: func(string, map[string]any) bool { return true }, MutatingToolsEnabled: true,
		ActionJournal: journal, RequireActionJournal: true,
	})
}

func TestProductionMutationPathsShareSafeExecutorJournalBoundary(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Engine) error
	}{
		{
			name: "main react tool dispatch",
			run: func(eng *Engine) error {
				result := eng.executeTool(context.Background(), openai.ToolCall{
					Function: openai.FunctionCall{Name: "StopCompShareInstance", Arguments: `{"UHostId":"uhost-1"}`},
				}, func(StepEvent) {})
				require.NotContains(t, result, tools.ErrActionJournalRequired.Error())
				return nil
			},
		},
		{
			name: "planner routed handler",
			run: func(eng *Engine) error {
				_, err := (capabilityHandlerExecutor{engine: eng}).Execute(context.Background(), "StopCompShareInstance", map[string]any{"UHostId": "uhost-1"})
				return err
			},
		},
		{
			name: "workflow internal executor",
			run: func(eng *Engine) error {
				_, err := eng.toolExecutorFor(tools.OriginWorkflowInternal).Execute(context.Background(), "StopCompShareInstance", map[string]any{"UHostId": "uhost-1"})
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			journal := &pathJournal{}
			inner := &sagaFakeExecutor{}
			eng := newJournaledPathEngine(journal, inner)
			require.NoError(t, tc.run(eng))
			assert.Equal(t, []string{"StopCompShareInstance"}, journal.calls)
			assert.Equal(t, []string{"StopCompShareInstance"}, inner.calls)
		})
	}
}
