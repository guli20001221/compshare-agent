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
