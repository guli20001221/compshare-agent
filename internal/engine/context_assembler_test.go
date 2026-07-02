package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAssembleFactContext_RendersFreshFactsGrouped(t *testing.T) {
	now := time.Unix(1_800, 0)
	facts := []ToolFact{
		{
			Kind:           FactKindMonitorSample,
			SubjectID:      "uhost-A",
			ProducedAtUnix: now.Add(-5 * time.Second).Unix(),
			TTLSeconds:     30,
			Payload: map[string]any{
				"cpu_usage":    "82.5",
				"memory_usage": "44.0",
				"gpu_usage":    "91.0",
				"vram_usage":   "70.0",
			},
		},
		{
			Kind:           FactKindInstanceState,
			SubjectID:      "uhost-A",
			ProducedAtUnix: now.Add(-4 * time.Second).Unix(),
			TTLSeconds:     30,
			Payload: map[string]any{
				"name":     "train-a",
				"state":    "Running",
				"gpu":      float64(2),
				"gpu_type": "RTX4090",
				"cpu":      float64(16),
				"memory":   float64(65536),
				"zone":     "cn-bj-01",
			},
		},
	}

	got := assembleFactContext(facts, now)

	assert.Contains(t, got, recentObservationPrefix)
	assert.Contains(t, got, "uhost-A")
	assert.Contains(t, got, "train-a")
	assert.Contains(t, got, "Running")
	assert.Contains(t, got, "RTX4090 x2")
	assert.Contains(t, got, "CPU 16")
	assert.Contains(t, got, "内存 65536")
	assert.Contains(t, got, "CPU 82.5%")
	assert.Contains(t, got, "GPU 91.0%")
	assert.Contains(t, got, "显存 70.0%")
}

func TestAssembleFactContext_DropsExpiredAndZeroTTL(t *testing.T) {
	now := time.Unix(2_000, 0)
	facts := []ToolFact{
		{
			Kind:           FactKindInstanceState,
			SubjectID:      "uhost-expired",
			ProducedAtUnix: now.Add(-31 * time.Second).Unix(),
			TTLSeconds:     30,
			Payload:        map[string]any{"name": "expired"},
		},
		{
			Kind:           FactKindInstanceState,
			SubjectID:      "uhost-zero-ttl",
			ProducedAtUnix: now.Unix(),
			TTLSeconds:     0,
			Payload:        map[string]any{"name": "zero"},
		},
	}

	assert.Empty(t, assembleFactContext(facts, now))
}

func TestAssembleFactContext_DoesNotInventMissingMetrics(t *testing.T) {
	now := time.Unix(2_100, 0)
	facts := []ToolFact{{
		Kind:           FactKindInstanceState,
		SubjectID:      "uhost-no-gpu",
		ProducedAtUnix: now.Unix(),
		TTLSeconds:     30,
		Payload: map[string]any{
			"name":  "cpu-box",
			"state": "Running",
		},
	}}

	got := assembleFactContext(facts, now)

	assert.Contains(t, got, "cpu-box")
	assert.NotContains(t, got, "GPU")
	assert.NotContains(t, got, "CPU ")
	assert.NotContains(t, got, "显存")
}

func TestAssembleFactContext_RendersStockPriceBillingFacts(t *testing.T) {
	now := time.Unix(2_150, 0)
	facts := []ToolFact{
		{
			Kind:           FactKindStockSnapshot,
			SubjectID:      "stock:4090:cn-wlcb-01",
			ProducedAtUnix: now.Add(-3 * time.Second).Unix(),
			TTLSeconds:     30,
			Payload: map[string]any{
				"model":  "4090",
				"status": "Normal",
				"zone":   "cn-wlcb-01",
				"count":  float64(10),
				"enough": true,
			},
		},
		{
			Kind:           FactKindPriceQuote,
			SubjectID:      "price:GetCompShareInstanceUserPrice:4090",
			ProducedAtUnix: now.Add(-2 * time.Second).Unix(),
			TTLSeconds:     30,
			Payload: map[string]any{
				"gpu_type":    "4090",
				"zone":        "cn-wlcb-01",
				"charge_type": "Dynamic",
				"price":       float64(1.58),
			},
		},
		{
			Kind:           FactKindBillingQuote,
			SubjectID:      "billing:GetCompShareRefundPrice:uhost-1",
			ProducedAtUnix: now.Add(-1 * time.Second).Unix(),
			TTLSeconds:     30,
			Payload: map[string]any{
				"resource_id": "uhost-1",
				"amount":      float64(42.5),
				"note":        "退费估算",
			},
		},
	}

	got := assembleFactContext(facts, now)

	assert.Contains(t, got, "库存 机型 4090")
	assert.Contains(t, got, "可用区 cn-wlcb-01")
	assert.Contains(t, got, "数量 10")
	assert.Contains(t, got, "价格 GPU 4090")
	assert.Contains(t, got, "计费 Dynamic")
	assert.Contains(t, got, "1.58")
	assert.Contains(t, got, "费用 资源 uhost-1")
	assert.Contains(t, got, "42.5")
}

func TestAssembleFactContext_TruncatesLongOutputButKeepsPrefix(t *testing.T) {
	now := time.Unix(2_200, 0)
	var facts []ToolFact
	for i := 0; i < 30; i++ {
		facts = append(facts, ToolFact{
			Kind:           FactKindInstanceState,
			SubjectID:      "uhost-long-" + strings.Repeat("x", i+1),
			ProducedAtUnix: now.Add(time.Duration(-i) * time.Second).Unix(),
			TTLSeconds:     30,
			Payload: map[string]any{
				"name":     "very-long-instance-name-" + strings.Repeat("n", 30),
				"state":    "Running",
				"gpu_type": "RTX4090",
				"gpu":      float64(8),
				"cpu":      float64(128),
				"memory":   float64(1048576),
			},
		})
	}

	got := assembleFactContext(facts, now)

	assert.Contains(t, got, recentObservationPrefix)
	assert.LessOrEqual(t, len([]rune(got)), maxFactContextRunes)
}

func TestAssembleFactContext_EmptyFactsReturnEmpty(t *testing.T) {
	assert.Empty(t, assembleFactContext(nil, time.Unix(1, 0)))
}

// TestOldestFreshFactAgeSeconds verifies the #3 StateTrace stale-cache observable:
// the age returned is the OLDEST still-fresh subject-bearing fact (the highest
// staleness risk), stale and subject-less facts are skipped, and an all-stale /
// empty cache returns -1 (so the recorder omits the bucket).
func TestOldestFreshFactAgeSeconds(t *testing.T) {
	now := time.Unix(10_000, 0)
	t.Run("oldest fresh wins over newer", func(t *testing.T) {
		facts := []ToolFact{
			{Kind: FactKindMonitorSample, SubjectID: "uhost-A", ProducedAtUnix: now.Add(-10 * time.Second).Unix(), TTLSeconds: 300},
			{Kind: FactKindInstanceState, SubjectID: "uhost-B", ProducedAtUnix: now.Add(-200 * time.Second).Unix(), TTLSeconds: 300},
		}
		assert.Equal(t, 200, oldestFreshFactAgeSeconds(facts, now))
	})
	t.Run("stale fact past TTL is skipped", func(t *testing.T) {
		facts := []ToolFact{
			{Kind: FactKindInstanceState, SubjectID: "uhost-A", ProducedAtUnix: now.Add(-400 * time.Second).Unix(), TTLSeconds: 300},
			{Kind: FactKindMonitorSample, SubjectID: "uhost-A", ProducedAtUnix: now.Add(-30 * time.Second).Unix(), TTLSeconds: 300},
		}
		assert.Equal(t, 30, oldestFreshFactAgeSeconds(facts, now))
	})
	t.Run("subject-less fact is skipped", func(t *testing.T) {
		facts := []ToolFact{
			{Kind: FactKindInstanceState, SubjectID: "", ProducedAtUnix: now.Add(-250 * time.Second).Unix(), TTLSeconds: 300},
			{Kind: FactKindMonitorSample, SubjectID: "uhost-A", ProducedAtUnix: now.Add(-5 * time.Second).Unix(), TTLSeconds: 300},
		}
		assert.Equal(t, 5, oldestFreshFactAgeSeconds(facts, now))
	})
	t.Run("all-stale and empty return -1", func(t *testing.T) {
		assert.Equal(t, -1, oldestFreshFactAgeSeconds(nil, now))
		stale := []ToolFact{{Kind: FactKindInstanceState, SubjectID: "uhost-A", ProducedAtUnix: now.Add(-9_000 * time.Second).Unix(), TTLSeconds: 300}}
		assert.Equal(t, -1, oldestFreshFactAgeSeconds(stale, now))
	})
}
