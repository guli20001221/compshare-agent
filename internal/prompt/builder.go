package prompt

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

type BuildOptions struct {
	MutatingToolsEnabled bool
}

type PromptSection struct {
	ID   string
	Text string
}

func renderPromptSections(sections []PromptSection) string {
	text, _ := renderPromptSectionsWithIDs(sections)
	return text
}

func renderPromptSectionsWithIDs(sections []PromptSection) (string, []string) {
	seen := make(map[string]struct{}, len(sections))
	ids := make([]string, 0, len(sections))
	var b strings.Builder
	for _, section := range sections {
		id := strings.TrimSpace(section.ID)
		text := strings.TrimSpace(section.Text)
		if id == "" || text == "" {
			panic("prompt section requires non-empty id and text")
		}
		if _, exists := seen[id]; exists {
			panic("duplicate prompt section id: " + id)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(text)
	}
	return b.String(), ids
}

// BuildSystemWithOptions creates the system prompt for the central Agent.
func BuildSystemWithOptions(userContext string, opts BuildOptions) string {
	text, _ := BuildSystemWithOptionsAndTrace(userContext, opts)
	return text
}

// BuildSystemWithOptionsAndTrace returns the rendered prompt and the exact,
// ordered section IDs that produced it. Prompt text and user context never
// enter traces.
func BuildSystemWithOptionsAndTrace(userContext string, opts BuildOptions) (string, []string) {
	if userContext == "" {
		userContext = "暂无用户信息（首次对话，正在获取...）"
	}

	sections := []PromptSection{{ID: "identity", Text: segmentIdentity}}
	if !opts.MutatingToolsEnabled {
		sections = append(sections, PromptSection{ID: "readonly_boundary", Text: segmentReadOnlyBoundary})
	}
	sections = append(sections, PromptSection{ID: "scope_boundary", Text: segmentScopeBoundary},
		PromptSection{ID: "behavior", Text: segmentCentralAgentBehavior},
		PromptSection{ID: "knowledge_turn_policy", Text: segmentKnowledgeTurnPolicy},
		PromptSection{ID: "reply_style", Text: segmentCentralAgentReplyStyle},
	)

	// Volatile tail is a named section and stays last so the static prefix remains
	// cacheable. Section IDs make duplicate policy injection a construction error.
	sections = append(sections, PromptSection{ID: "user_state", Text: "## 用户当前状态\n" + userContext})
	text, ids := renderPromptSectionsWithIDs(sections)
	return text + "\n", ids
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

const maxToolResultRunes = 4000

// toolResultOversizeKey names the field an all-but-empty result carries. It
// exists so that "we dropped everything" is a fact the model reads, not a
// silence it has to infer.
const toolResultOversizeKey = "_ResultTooLarge"

// toolResultShrinkLevels is the ladder FormatToolResult walks when a result is
// over the cap, from gentlest to harshest. arrayItems is how many entries of
// each list survive; scalarRunes caps individual string values (0 = leave
// strings alone). Every rung re-marshals a real Go value, so every rung is
// valid JSON — the ladder can only lose content, never well-formedness.
var toolResultShrinkLevels = []struct {
	arrayItems  int
	scalarRunes int
}{
	{arrayItems: 5},
	{arrayItems: 3},
	{arrayItems: 2},
	{arrayItems: 1},
	{arrayItems: 0},
	{arrayItems: 0, scalarRunes: 2000},
	{arrayItems: 0, scalarRunes: 500},
	{arrayItems: 0, scalarRunes: 120},
}

// FormatToolResult returns a compact JSON string (<= maxToolResultRunes runes)
// for feeding back to the LLM.
//
// The contract is absolute: what comes out is ALWAYS parseable JSON, and if
// anything was dropped to fit, the JSON itself says so. It is never a fragment.
//
// The previous implementation shrank lists to 5 items and, if that still did
// not fit, hard-cut the marshalled bytes at the cap — "invalid JSON, but a
// bounded result", as its comment conceded. On the shape that matters most,
// DescribeCompShareInstance with the real ~1.2-1.5 KB rows the upstream API
// returns, the shrink never fit, so the byte-cut was not a last resort at all:
// it was the normal path from three instances up. What reached the model was
// two complete rows, a third cut mid-key, and — because Go marshals map keys in
// sorted order and the cut takes the tail — "TotalCount":30 sitting intact at
// the front. The model was told there were thirty instances, shown one or two,
// and given no marker at all, because the "已截取前 5 条" notice is the LAST
// element of the list and was the first thing the cut ate. That is not a
// degraded context, it is a context that lies, and inviting a model to account
// for twenty-eight rows it cannot see is how instances get fabricated.
//
// So: drop whole elements, never bytes.
func FormatToolResult(result map[string]any) string {
	b, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf(`{"error": %q}`, err.Error())
	}
	if utf8.RuneCount(b) <= maxToolResultRunes {
		return string(b)
	}

	for _, level := range toolResultShrinkLevels {
		shrunk := truncateArrays(result, level.arrayItems)
		if level.scalarRunes > 0 {
			shrunk = truncateStrings(shrunk, level.scalarRunes)
		}
		if b2, err := json.Marshal(shrunk); err == nil && utf8.RuneCount(b2) <= maxToolResultRunes {
			return string(b2)
		}
	}

	// Every rung failed: the payload is over the cap even with all lists
	// emptied and all strings clipped to 120 runes. No shape we have seen does
	// this, but the contract holds anyway — an explicit notice, not a fragment.
	return oversizeNotice(result)
}

// truncateArrays limits every []any it can reach — top level, nested in maps,
// nested in other arrays — to maxItems entries, appending a notice element that
// names how many were dropped. The notice is a sibling of the surviving
// elements rather than a trailing string, so it cannot be separated from the
// data it describes.
func truncateArrays(v any, maxItems int) any {
	switch typed := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, val := range typed {
			out[k] = truncateArrays(val, maxItems)
		}
		return out
	case []any:
		total := len(typed)
		keep := maxItems
		if keep > total {
			keep = total
		}
		out := make([]any, 0, keep+1)
		for i := 0; i < keep; i++ {
			out = append(out, truncateArrays(typed[i], maxItems))
		}
		if total > keep {
			out = append(out, arrayTruncationNotice(total, keep))
		}
		return out
	default:
		return v
	}
}

func arrayTruncationNotice(total, kept int) string {
	if kept == 0 {
		return fmt.Sprintf("…(共 %d 条，因结果过大已全部省略；请用更具体的条件重新查询，例如指定实例 ID)", total)
	}
	return fmt.Sprintf("…(共 %d 条，此处只保留前 %d 条；其余未展示，如需完整列表请用更具体的条件重新查询)", total, kept)
}

// truncateStrings clips oversized string values, marking each clip inline. The
// value stays a JSON string, so the document stays parseable.
func truncateStrings(v any, maxRunes int) any {
	switch typed := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, val := range typed {
			out[k] = truncateStrings(val, maxRunes)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, val := range typed {
			out[i] = truncateStrings(val, maxRunes)
		}
		return out
	case string:
		if utf8.RuneCountInString(typed) <= maxRunes {
			return typed
		}
		runes := []rune(typed)
		return string(runes[:maxRunes]) + fmt.Sprintf("…(原文共 %d 字，此处已截断)", len(runes))
	default:
		return v
	}
}

// oversizeNotice is the floor of the ladder: keep the small identifying scalars
// that let the model know which call this was and that it failed to fit, and
// drop the rest.
func oversizeNotice(result map[string]any) string {
	notice := map[string]any{
		toolResultOversizeKey: "工具结果过大，内容已全部省略。请用更具体的条件重新查询（例如指定实例 ID 或缩小时间范围）。",
	}
	for _, key := range []string{"RetCode", "Action", "Message", "TotalCount"} {
		value, ok := result[key]
		if !ok {
			continue
		}
		if encoded, err := json.Marshal(value); err == nil && len(encoded) <= 64 {
			notice[key] = value
		}
	}
	b, err := json.Marshal(notice)
	if err != nil || utf8.RuneCount(b) > maxToolResultRunes {
		return `{"` + toolResultOversizeKey + `":"工具结果过大，内容已全部省略。请用更具体的条件重新查询。"}`
	}
	return string(b)
}
