package engine

import (
	"github.com/compshare-agent/internal/entity"
	openai "github.com/sashabaranov/go-openai"
)

func testInstance(id, name, state string) entity.InstanceSnapshot {
	return entity.InstanceSnapshot{UHostId: id, Name: name, State: state}
}

func stringSliceArg(value any) []string {
	switch v := value.(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func toolNameSet(ts []openai.Tool) map[string]struct{} {
	out := make(map[string]struct{}, len(ts))
	for _, tool := range ts {
		if tool.Function != nil {
			out[tool.Function.Name] = struct{}{}
		}
	}
	return out
}
