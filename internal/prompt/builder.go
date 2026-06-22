package prompt

import (
	"encoding/json"
	"fmt"
	"strings"
)

type BuildOptions struct {
	MutatingToolsEnabled    bool
	IntentScopedReActPrompt bool
}

// BuildSystemWithOptions creates the system prompt for the active runtime mode.
// Shared sections (segmentIdentity, segmentScopeBoundary, segmentKnowledgeBoundary)
// are defined once in segments.go; mode-specific sections live in
// segment_operation.go (mutating) and segment_readonly.go (read-only).
func BuildSystemWithOptions(userContext string, opts BuildOptions) string {
	if userContext == "" {
		userContext = "暂无用户信息（首次对话，正在获取...）"
	}

	var b strings.Builder

	b.WriteString(segmentIdentity)
	b.WriteString("\n\n")

	// KV-cache prefix stability (P0 阶段1A): the static body is written first and
	// the volatile per-turn "## 用户当前状态" block is appended LAST (see below), so
	// everything up to that block is byte-identical across turns and the provider's
	// automatic prefix cache hits it. The block used to sit mid-prompt (after
	// capabilities in mutating mode, after the readonly boundary in read-only mode)
	// — that position was frozen only for snapshot stability, not for any semantic
	// reason — which left the entire static tail (boundary/rules/knowledge/reply
	// style) downstream of a value that changes every round, defeating the cache.
	if opts.MutatingToolsEnabled {
		b.WriteString(segmentMutatingCapabilities)
		b.WriteString("\n\n")
		b.WriteString(segmentScopeBoundary)
		b.WriteString("\n\n")
		if opts.IntentScopedReActPrompt {
			b.WriteString(segmentIntentScopedMutatingRules)
		} else {
			b.WriteString(segmentMutatingRules)
		}
		b.WriteString("\n\n")
		b.WriteString(segmentKnowledgeBoundary)
		b.WriteString("\n\n")
		if opts.IntentScopedReActPrompt {
			b.WriteString(segmentIntentScopedMutatingReplyStyle)
		} else {
			b.WriteString(segmentMutatingReplyStyle)
		}
		b.WriteString("\n")
	} else {
		b.WriteString(segmentReadOnlyCapabilities)
		b.WriteString("\n\n")
		b.WriteString(segmentReadOnlyBoundary)
		b.WriteString("\n\n")
		b.WriteString(segmentScopeBoundary)
		b.WriteString("\n\n")
		if opts.IntentScopedReActPrompt {
			b.WriteString(segmentIntentScopedReadOnlyBehavior)
		} else {
			b.WriteString(segmentReadOnlyBehavior)
		}
		b.WriteString("\n\n")
		b.WriteString(segmentKnowledgeBoundary)
		b.WriteString("\n\n")
		b.WriteString(segmentReadOnlyReplyStyle)
		b.WriteString("\n")
	}

	// Volatile tail — the ONLY part that changes turn-to-turn. Keep it last so the
	// static prefix above stays cacheable. Guard: TestReActPromptStaticPrefixStable.
	b.WriteString("\n## 用户当前状态\n")
	b.WriteString(userContext)
	b.WriteString("\n")

	return b.String()
}

// FormatInstanceContext formats instance list into a context string.
func FormatInstanceContext(apiResult map[string]any) string {
	hosts, ok := apiResult["UHostSet"].([]any)
	if !ok || len(hosts) == 0 {
		return "用户当前没有实例。"
	}

	var lines []string
	running, stopped := 0, 0
	for _, h := range hosts {
		host, ok := h.(map[string]any)
		if !ok {
			continue
		}
		id, _ := host["UHostId"].(string)
		name, _ := host["Name"].(string)
		state, _ := host["State"].(string)
		gpuType, _ := host["GpuType"].(string)
		gpu, _ := host["GPU"].(float64)
		chargeType, _ := host["ChargeType"].(string)

		line := fmt.Sprintf("- %s (%s): GPU=%s×%.0f, 状态=%s, 计费=%s",
			name, id, gpuType, gpu, translateState(state), chargeType)
		lines = append(lines, line)

		if state == "Running" {
			running++
		} else {
			stopped++
		}
	}

	summary := fmt.Sprintf("您有 %d 个实例（%d 个运行中、%d 个其他状态）\n",
		len(hosts), running, stopped)
	return summary + strings.Join(lines, "\n")
}

var stateTranslation = map[string]string{
	"Running":      "运行中",
	"Stopped":      "关机",
	"Starting":     "启动中",
	"Stopping":     "关机中",
	"Install":      "初始化中",
	"Rebooting":    "重启中",
	"Install Fail": "初始化失败",
}

func translateState(state string) string {
	if v, ok := stateTranslation[state]; ok {
		return v
	}
	return state
}

// FormatToolResult returns a compact JSON string (<= maxRunes runes) for
// feeding back to the LLM. If the result exceeds the cap it first truncates
// individual array/list fields to stay valid JSON; only when that cannot bring
// it under the cap (a giant scalar field, or an array nested too deep to reach)
// does it fall back to a hard rune-cut — invalid JSON, but a bounded result.
func FormatToolResult(result map[string]any) string {
	b, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf(`{"error": %q}`, err.Error())
	}
	const maxRunes = 4000
	runes := []rune(string(b))
	if len(runes) <= maxRunes {
		return string(b)
	}

	// Over budget: shrink array fields and re-marshal so the result stays
	// valid JSON. truncateMapArrays only reaches top-level []any fields, so
	// this does not help a giant scalar field or an array nested deeper.
	best := runes
	if b2, err := json.Marshal(truncateMapArrays(result, 5)); err == nil {
		best = []rune(string(b2))
		if len(best) <= maxRunes {
			return string(best)
		}
	}
	// Array-shrink could not bring it under the cap. Hard-cut as a last
	// resort: this yields invalid JSON, but a bounded result beats an
	// unbounded one that would masquerade as a token-budget refusal.
	return string(best[:maxRunes])
}

// truncateMapArrays limits []any fields in the map to maxItems entries,
// appending a truncation notice. Works one level deep.
func truncateMapArrays(m map[string]any, maxItems int) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch arr := v.(type) {
		case []any:
			if len(arr) > maxItems {
				trimmed := make([]any, maxItems+1)
				copy(trimmed, arr[:maxItems])
				trimmed[maxItems] = fmt.Sprintf("...(共 %d 条，已截取前 %d 条)", len(arr), maxItems)
				out[k] = trimmed
			} else {
				out[k] = v
			}
		default:
			out[k] = v
		}
	}
	return out
}
