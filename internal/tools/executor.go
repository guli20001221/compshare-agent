package tools

import "context"

// ToolExecutor abstracts how API calls are made.
type ToolExecutor interface {
	Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error)
}
