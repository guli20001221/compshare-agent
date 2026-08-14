package engine

import (
	"strings"
	"unicode/utf8"

	"github.com/compshare-agent/internal/opscontext"
	"github.com/compshare-agent/internal/security"
)

// These limits bound only the independent contextual reference block. The
// planner Task remains byte-for-byte untouched: it is the audited/replay-hashed
// request, not a mutable prompt assembly buffer.
const (
	instanceOpsCurrentReportLimit = 4096
	instanceOpsPriorReportLimit   = 2048
	instanceOpsPriorReportCount   = 2
)

const instanceOpsContextTruncation = "\n[context truncated]\n"

// instanceOpsModelContext projects direct user reports for the inner agent.
// It intentionally excludes assistant messages and OCR text: both can carry
// unsupported outer-agent inferences or untrusted image text. User reports are
// redacted before leaving the engine and remain clearly labelled as reports,
// not as executable instructions or verified platform facts.
func (e *Engine) instanceOpsModelContext() opscontext.Context {
	ctx := opscontext.Context{SchemaVersion: opscontext.SchemaVersion}
	if e == nil {
		return ctx
	}
	if report := instanceOpsRedactedReport(e.lastUserMsg, instanceOpsCurrentReportLimit); report != "" {
		ctx.CurrentUserReport = &opscontext.UserReport{
			Text:       report,
			Source:     "chat.current_user",
			ObservedAt: opscontext.StatusUnknown,
			Status:     opscontext.StatusReported,
		}
	}

	pairs := e.recentCompleteConversationPairs()
	start := len(pairs) - instanceOpsPriorReportCount
	if start < 0 {
		start = 0
	}
	for _, pair := range pairs[start:] {
		report := instanceOpsRedactedReport(pair.User, instanceOpsPriorReportLimit)
		if report == "" {
			continue
		}
		ctx.PriorUserReports = append(ctx.PriorUserReports, opscontext.UserReport{
			Text:       report,
			Source:     "chat.prior_user",
			ObservedAt: opscontext.StatusUnknown,
			Status:     opscontext.StatusReported,
		})
	}
	return ctx
}

func instanceOpsRedactedReport(text string, limit int) string {
	text = userAuthoredText(text)
	text = strings.TrimSpace(security.RedactUserConversationText(text))
	return truncateInstanceOpsContextText(text, limit)
}

func truncateInstanceOpsContextText(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	marker := instanceOpsContextTruncation
	if len(marker) >= limit {
		return marker[:limit]
	}
	end := limit - len(marker)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + marker
}
