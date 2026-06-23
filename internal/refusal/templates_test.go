package refusal

import (
	"strings"
	"testing"
)

// Anchor-signal tests guard against silent wording drift. Byte-equal
// assertion is enforced at the engine integration layer; here we just
// verify that the canonical anchors users / dashboards rely on remain
// present.

func TestMonitorHistoryUnsupported_Anchors(t *testing.T) {
	want := []string{"历史监控", "一台实例", "24 小时", "时间段"}
	for _, anchor := range want {
		if !strings.Contains(MonitorHistoryUnsupported, anchor) {
			t.Errorf("MonitorHistoryUnsupported lost anchor %q", anchor)
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

func TestOffTopic_Anchors(t *testing.T) {
	want := []string{"算力平台", "回答范围", "专业人士", "GPU 规格"}
	for _, anchor := range want {
		if !strings.Contains(OffTopic, anchor) {
			t.Errorf("OffTopic lost anchor %q", anchor)
		}
	}
	// Anti-stale-hotline guard — refusal must redirect to professional
	// channels without embedding specific phone numbers. A jurisdiction-
	// mismatched or stale hotline number would be worse than a generic
	// redirect. Per PR #155 review item 1.
	forbidden := []string{"010-", "1-800", "hotline", "12320"}
	for _, bad := range forbidden {
		if strings.Contains(OffTopic, bad) {
			t.Errorf("OffTopic reintroduced forbidden stale-hotline phrase %q", bad)
		}
	}
}

func TestCategoryStrings_NeverChange(t *testing.T) {
	// Downstream MySQL ingest + per-category eval depend on these
	// EXACT strings as a stable contract. Changing them would break
	// historical aggregations silently.
	cases := map[string]string{
		"CategoryMonitorHistory":   "monitor_history_unsupported",
		"CategoryJailbreakAttempt": "jailbreak_attempt",
		"CategoryOffTopic":         "off_topic_refused",
	}
	if CategoryMonitorHistory != cases["CategoryMonitorHistory"] {
		t.Errorf("CategoryMonitorHistory = %q; want %q", CategoryMonitorHistory, cases["CategoryMonitorHistory"])
	}
	if CategoryJailbreakAttempt != cases["CategoryJailbreakAttempt"] {
		t.Errorf("CategoryJailbreakAttempt = %q; want %q", CategoryJailbreakAttempt, cases["CategoryJailbreakAttempt"])
	}
	if CategoryOffTopic != cases["CategoryOffTopic"] {
		t.Errorf("CategoryOffTopic = %q; want %q", CategoryOffTopic, cases["CategoryOffTopic"])
	}
}
