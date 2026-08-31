package refusal

import (
	"strings"
	"testing"
)

func TestMonitorHistoryUnsupported_Anchors(t *testing.T) {
	want := []string{"历史监控", "20 台实例", "30 天", "时间段"}
	for _, anchor := range want {
		if !strings.Contains(MonitorHistoryUnsupported, anchor) {
			t.Errorf("MonitorHistoryUnsupported lost anchor %q", anchor)
		}
	}
}
