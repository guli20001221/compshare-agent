package capability

import (
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/platform"
)

// ValidateCurrentTurnGrounding verifies the small subset of read arguments that
// must be grounded in user-authored text. Mutating-adjacent facts such as zones
// and time windows stay current-turn-only. A read-only image catalog query may
// also reuse a literal phrase from recent user turns, which preserves normal
// follow-ups without trusting assistant prose or tool output. The typed handler
// still never receives chat text: this runs at the engine boundary.
func ValidateCurrentTurnGrounding(request platform.ReadRequest, currentUserText string, priorUserTexts ...string) error {
	if err := validateUserLiteralInstanceIDs(targetRefsForGrounding(request), currentUserText, priorUserTexts...); err != nil {
		return err
	}
	switch req := request.(type) {
	case ImageListRequest:
		query := strings.TrimSpace(req.Query)
		if query != "" {
			grounded := platform.ContainsLiteralSpan(currentUserText, query)
			for _, prior := range priorUserTexts {
				grounded = grounded || platform.ContainsLiteralSpan(prior, query)
			}
			if !grounded {
				return fmt.Errorf("query: %q 不是本轮或近期用户原文的字面子串", query)
			}
		}
		if len(req.SemanticQueries) > 3 {
			return fmt.Errorf("semantic_queries: 最多提供 3 个语义扩展查询词")
		}
		if len(req.SemanticQueries) > 0 {
			// Community and platform both expand; custom and shared do not.
			// The two that do are the two catalogs a recommendation draws from,
			// and platform is the one that NEEDS it: its images are named after
			// the runtime (vLLM / SGLang / Ollama), so a 用途 word matches none of
			// them and the Agent could previously only answer from community.
			// Custom and shared stay out because their contents are the tenant's
			// own artifacts — expanding a user's words into guessed technology
			// terms there surfaces images by a name the user never used.
			//
			// An empty source IS platform: imageListHandle routes it there via its
			// default branch, so rejecting "" would refuse expansion on exactly the
			// request shape a model that omits the optional field produces.
			if req.Source != platform.ImageSourceCommunity &&
				req.Source != platform.ImageSourcePlatform && req.Source != "" {
				return fmt.Errorf("semantic_queries: 仅社区或平台镜像目录支持语义扩展")
			}
			if query == "" {
				return fmt.Errorf("semantic_queries: 必须同时保留来自用户原话的 query，语义扩展不能替代原话查询")
			}
			if req.Mode == platform.ListModeAll {
				return fmt.Errorf("semantic_queries: mode=all 时不要提供筛选查询")
			}
			for _, expansion := range req.SemanticQueries {
				if strings.TrimSpace(expansion) == "" {
					return fmt.Errorf("semantic_queries: 查询词不能为空")
				}
			}
		}
	case StockAvailabilityRequest:
		for _, mention := range req.ZoneMentions {
			if err := requireLiteralSpan(currentUserText, mention, "zone_mentions"); err != nil {
				return err
			}
		}
	case MonitorHistoryRequest:
		if req.TimeWindow == nil {
			return nil
		}
		span := strings.TrimSpace(req.TimeWindow.SourceSpan)
		if err := requireLiteralSpan(currentUserText, span, "time_window.source_span"); err != nil {
			return err
		}
		if req.TimeWindow.Type == platform.TimeWindowAbsolute && !absoluteWindowIsLiteral(req.TimeWindow, span) {
			return fmt.Errorf("time_window: absolute 起止时间必须逐项出现在 source_span 中；相对表达请使用 preset/relative")
		}
	}
	return nil
}

func targetRefsForGrounding(request platform.ReadRequest) []platform.TargetRef {
	switch req := request.(type) {
	case ResourceInfoRequest:
		return req.Targets
	case InstanceAccessRequest:
		return req.Targets
	case MonitorCurrentRequest:
		return req.Targets
	case MonitorHistoryRequest:
		return req.Targets
	case RefundEstimateRequest:
		return req.Targets
	default:
		return nil
	}
}

func validateUserLiteralInstanceIDs(refs []platform.TargetRef, currentUserText string, priorUserTexts ...string) error {
	// Current and recent user messages are both literal evidence. Source is
	// model-provided metadata, so the check intentionally ignores it.
	grounded := foldedInstanceIDs(currentUserText)
	for _, text := range priorUserTexts {
		for id := range foldedInstanceIDs(text) {
			grounded[id] = struct{}{}
		}
	}
	if len(grounded) == 0 {
		return nil
	}
	for _, ref := range refs {
		if ref.Type != platform.TargetRefUHostIDUserInput {
			continue
		}
		value := strings.ToLower(strings.TrimSpace(ref.Value))
		if _, ok := grounded[value]; !ok {
			return fmt.Errorf("targets: 实例 ID %q 不是当前或近期用户原文中的完整 ID", strings.TrimSpace(ref.Value))
		}
	}
	return nil
}

func foldedInstanceIDs(text string) map[string]struct{} {
	ids := (entity.RegistrySnapshot{}).InstanceIDTokensInText(text)
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[strings.ToLower(id)] = struct{}{}
	}
	return out
}

func requireLiteralSpan(userText, span, field string) error {
	span = strings.TrimSpace(span)
	if span == "" {
		return fmt.Errorf("%s: 必须提供用户原文字面子串", field)
	}
	if !platform.ContainsLiteralSpan(userText, span) {
		return fmt.Errorf("%s: %q 不是本轮用户原文的字面子串", field, span)
	}
	return nil
}

func absoluteWindowIsLiteral(window *platform.TimeWindow, span string) bool {
	startDate, startClock, ok := dateAndClock(window.Start)
	if !ok {
		return false
	}
	endDate, endClock, ok := dateAndClock(window.End)
	if !ok {
		return false
	}
	compact := platform.FoldLiteralSpan(span)
	if !platform.ContainsLiteralSpan(compact, startDate) || !platform.ContainsLiteralSpan(compact, startClock) || !platform.ContainsLiteralSpan(compact, endClock) {
		return false
	}
	return endDate == startDate || platform.ContainsLiteralSpan(compact, endDate)
}

func dateAndClock(value string) (string, string, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 16 || value[4] != '-' || value[7] != '-' {
		return "", "", false
	}
	clockStart := 11
	if value[10] == 'T' || value[10] == ' ' {
		return value[:10], value[clockStart : clockStart+5], true
	}
	return "", "", false
}
