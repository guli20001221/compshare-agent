package refusal

import (
	"strings"
	"testing"
)

func TestMonitorHistoryUnsupported_Anchors(t *testing.T) {
	want := []string{"历史监控", "一台实例", "24 小时", "时间段"}
	for _, anchor := range want {
		if !strings.Contains(MonitorHistoryUnsupported, anchor) {
			t.Errorf("MonitorHistoryUnsupported lost anchor %q", anchor)
		}
	}
}
