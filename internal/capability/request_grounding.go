package capability

import (
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/platform"
)

// InstanceIDGroundingMismatch reports that a model-authored target does not
// equal any complete instance ID the user wrote in the current or recent
// conversation. UserLiteralIDs preserve the user's exact tokens in recency
// order; callers may use them to explain a rejected call, but they remain
// evidence rather than an instruction to select a target.
type InstanceIDGroundingMismatch struct {
	Provided       string
	UserLiteralIDs []string
}

func (e *InstanceIDGroundingMismatch) Error() string {
	if e == nil {
		return "targets: 实例 ID 与用户原文不一致"
	}
	return fmt.Sprintf("targets: 实例 ID %q 不是当前或近期用户原文中的完整 ID", strings.TrimSpace(e.Provided))
}

// ValidateCurrentTurnGrounding verifies the small subset of read arguments that
// must be grounded in user-authored text. Mutating-adjacent facts such as zones
// and time windows stay current-turn-only. The typed handler never receives
// chat text: this runs at the engine boundary.
func ValidateCurrentTurnGrounding(request platform.ReadRequest, currentUserText string, priorUserTexts ...string) error {
	if err := validateUserLiteralInstanceIDs(targetRefsForGrounding(request), currentUserText, priorUserTexts...); err != nil {
		return err
	}
	switch req := request.(type) {
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
	groundedIDs := userLiteralInstanceIDs(currentUserText, priorUserTexts...)
	grounded := make(map[string]struct{}, len(groundedIDs))
	for _, id := range groundedIDs {
		grounded[strings.ToLower(id)] = struct{}{}
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
			return &InstanceIDGroundingMismatch{
				Provided:       strings.TrimSpace(ref.Value),
				UserLiteralIDs: append([]string(nil), groundedIDs...),
			}
		}
	}
	return nil
}

// ValidateUserLiteralInstanceID applies the same literal-integrity check to a
// single target outside the typed read adapter (currently the SSH diagnosis
// lane). With no literal ID in recent user text it deliberately returns nil, so
// ordinary references such as "继续排查刚才那台" remain governed by that
// lane's existing deterministic binder.
func ValidateUserLiteralInstanceID(value, currentUserText string, priorUserTexts ...string) error {
	return validateUserLiteralInstanceIDs([]platform.TargetRef{{
		Type:  platform.TargetRefUHostIDUserInput,
		Value: value,
	}}, currentUserText, priorUserTexts...)
}

func userLiteralInstanceIDs(currentUserText string, priorUserTexts ...string) []string {
	texts := append([]string{currentUserText}, priorUserTexts...)
	seen := map[string]struct{}{}
	var out []string
	for _, text := range texts {
		for _, id := range (entity.RegistrySnapshot{}).InstanceIDTokensInText(text) {
			folded := strings.ToLower(id)
			if _, ok := seen[folded]; ok {
				continue
			}
			seen[folded] = struct{}{}
			out = append(out, id)
		}
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
