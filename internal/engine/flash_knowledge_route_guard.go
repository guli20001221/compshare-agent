package engine

import (
	"strings"
	"unicode"
)

// flashKnowledgeRouteGuardOn gates a deterministic fallback for a small set of
// product-fact questions that deepseek-v4-flash can route to live tools. Default
// off: this is a conscious rollback guard, not the primary route strategy.
var flashKnowledgeRouteGuardOn bool

// SetFlashKnowledgeRouteGuardEnabled toggles the fallback guard at boot.
func SetFlashKnowledgeRouteGuardEnabled(v bool) { flashKnowledgeRouteGuardOn = v }

// FlashKnowledgeRouteGuardEnabled reports whether the fallback guard is enabled.
func FlashKnowledgeRouteGuardEnabled() bool { return flashKnowledgeRouteGuardOn }

type flashKnowledgeGuardCase struct {
	Reason      string
	AllOf       []string
	AnyOf       []string
	ExcludeLive bool
}

var flashKnowledgeGuardCases = []flashKnowledgeGuardCase{
	{
		Reason: "disk_billing_fact",
		AllOf:  []string{"磁盘"},
		AnyOf:  []string{"收费", "计费", "免费", "价格", "多少钱"},
	},
	{
		Reason: "coding_plan_package_fact",
		AnyOf:  []string{"coding plan", "agent plan", "套餐"},
		AllOf:  []string{"删除|取消|退订|退款|退了"},
	},
	{
		Reason:      "resource_capacity_semantics",
		AnyOf:       []string{"暂无资源", "资源不足", "没有资源", "没资源", "卖完", "售罄", "normal", "soldout", "sold out"},
		ExcludeLive: true,
	},
}

func matchFlashKnowledgeRouteGuard(userMsg string) (bool, string) {
	normalized := strings.ToLower(strings.TrimSpace(userMsg))
	if normalized == "" {
		return false, ""
	}
	if containsExplicitGPUModelToken(normalized) && containsAny(normalized, []string{"暂无资源", "资源不足", "没有资源", "没资源", "卖完", "售罄", "库存", "有货"}) {
		return false, ""
	}
	for _, c := range flashKnowledgeGuardCases {
		if c.ExcludeLive && containsExplicitGPUModelToken(normalized) {
			continue
		}
		if !allGroupsMatch(normalized, c.AllOf) {
			continue
		}
		if len(c.AnyOf) > 0 && !containsAny(normalized, c.AnyOf) {
			continue
		}
		return true, c.Reason
	}
	return false, ""
}

func allGroupsMatch(text string, groups []string) bool {
	for _, group := range groups {
		terms := strings.Split(group, "|")
		if !containsAny(text, terms) {
			return false
		}
	}
	return true
}

func containsAny(text string, terms []string) bool {
	for _, term := range terms {
		if strings.Contains(text, strings.ToLower(strings.TrimSpace(term))) {
			return true
		}
	}
	return false
}

func containsExplicitGPUModelToken(text string) bool {
	token := strings.Builder{}
	flush := func() bool {
		if isGPUModelToken(token.String()) {
			return true
		}
		token.Reset()
		return false
	}
	for _, r := range strings.ToLower(text) {
		if (r >= 'a' && r <= 'z') || unicode.IsDigit(r) || r == '_' {
			token.WriteRune(r)
			continue
		}
		if flush() {
			return true
		}
	}
	return flush()
}

func isGPUModelToken(token string) bool {
	token = strings.Trim(token, "_")
	if token == "" {
		return false
	}
	digitCount := 0
	letterCount := 0
	for _, r := range token {
		if unicode.IsDigit(r) {
			digitCount++
		} else if r >= 'a' && r <= 'z' {
			letterCount++
		}
	}
	if digitCount == 0 {
		return false
	}
	if letterCount > 0 && digitCount >= 2 && len(token) <= 12 {
		return true
	}
	if letterCount == 0 && len(token) == 4 {
		switch token[0] {
		case '2', '3', '4', '5':
			return true
		}
	}
	return false
}
