package capability

import (
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/platform"
)

// ValidateCurrentTurnGrounding verifies the small subset of read arguments that
// must be literal current-turn evidence. The typed handler still never receives
// chat text: this check runs at the engine boundary before dispatch.
func ValidateCurrentTurnGrounding(request platform.ReadRequest, currentUserText string) error {
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
