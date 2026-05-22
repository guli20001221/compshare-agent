package refusal

import (
	"strings"
	"testing"
)

// Anchor-signal tests guard against silent wording drift. Byte-equal
// assertion is enforced at the engine integration layer; here we just
// verify that the canonical anchors users / dashboards rely on remain
// present.

func TestAccountBillingUnsupported_Anchors(t *testing.T) {
	want := []string{"财务中心", "余额", "账单", "消费记录", "发票"}
	for _, anchor := range want {
		if !strings.Contains(AccountBillingUnsupported, anchor) {
			t.Errorf("AccountBillingUnsupported lost anchor %q", anchor)
		}
	}
}

func TestMonitorHistoryUnsupported_Anchors(t *testing.T) {
	want := []string{"历史时间段", "实时监控", "控制台监控页"}
	for _, anchor := range want {
		if !strings.Contains(MonitorHistoryUnsupported, anchor) {
			t.Errorf("MonitorHistoryUnsupported lost anchor %q", anchor)
		}
	}
}

func TestResourceShortage226604_Anchors(t *testing.T) {
	want := []string{"226604", "资源池", "重试", "包日", "包月"}
	for _, anchor := range want {
		if !strings.Contains(ResourceShortage226604, anchor) {
			t.Errorf("ResourceShortage226604 lost anchor %q", anchor)
		}
	}
	// Over-promise guard — PR #139 review #4
	forbidden := []string{"独占机器", "不会因为资源紧张"}
	for _, bad := range forbidden {
		if strings.Contains(ResourceShortage226604, bad) {
			t.Errorf("ResourceShortage226604 reintroduced forbidden phrase %q", bad)
		}
	}
}

func TestJailbreakAttempt_Anchors(t *testing.T) {
	want := []string{"安全限制", "核心规则", "算力平台", "我无法忽略"}
	for _, anchor := range want {
		if !strings.Contains(JailbreakAttempt, anchor) {
			t.Errorf("JailbreakAttempt lost anchor %q", anchor)
		}
	}
	// Anti-leak guard — wording must not confirm the system prompt
	// exists or name what the override target was; both would give a
	// determined attacker structural confirmation. Tracked per PR #152
	// review item 5.
	forbidden := []string{"系统提示词", "system prompt", "你的指令是"}
	for _, bad := range forbidden {
		if strings.Contains(JailbreakAttempt, bad) {
			t.Errorf("JailbreakAttempt reintroduced forbidden anti-leak phrase %q", bad)
		}
	}
}

func TestCategoryStrings_NeverChange(t *testing.T) {
	// Downstream MySQL ingest + per-category eval depend on these
	// EXACT strings as a stable contract. Changing them would break
	// historical aggregations silently.
	cases := map[string]string{
		"CategoryAccountBilling":   "account_billing_unsupported",
		"CategoryMonitorHistory":   "monitor_history_unsupported",
		"CategoryResourceShortage": "resource_shortage_226604",
		"CategoryJailbreakAttempt": "jailbreak_attempt",
	}
	if CategoryAccountBilling != cases["CategoryAccountBilling"] {
		t.Errorf("CategoryAccountBilling = %q; want %q", CategoryAccountBilling, cases["CategoryAccountBilling"])
	}
	if CategoryMonitorHistory != cases["CategoryMonitorHistory"] {
		t.Errorf("CategoryMonitorHistory = %q; want %q", CategoryMonitorHistory, cases["CategoryMonitorHistory"])
	}
	if CategoryResourceShortage != cases["CategoryResourceShortage"] {
		t.Errorf("CategoryResourceShortage = %q; want %q", CategoryResourceShortage, cases["CategoryResourceShortage"])
	}
	if CategoryJailbreakAttempt != cases["CategoryJailbreakAttempt"] {
		t.Errorf("CategoryJailbreakAttempt = %q; want %q", CategoryJailbreakAttempt, cases["CategoryJailbreakAttempt"])
	}
}
