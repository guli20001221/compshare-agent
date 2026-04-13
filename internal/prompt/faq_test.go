package prompt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFAQContent_NotEmpty(t *testing.T) {
	assert.NotEmpty(t, FAQContent)
}

func TestFAQContent_ContainsKeyTopics(t *testing.T) {
	// Must cover W2 validation scenarios
	assert.Contains(t, FAQContent, "关机后还扣费吗")
	assert.Contains(t, FAQContent, "计费模式")
	assert.Contains(t, FAQContent, "按量")
	assert.Contains(t, FAQContent, "包月")
	assert.Contains(t, FAQContent, "无卡启动模式")
	assert.Contains(t, FAQContent, "SSH")
	assert.Contains(t, FAQContent, "nvidia-smi")
}

func TestFAQContent_ContainsBillingSection(t *testing.T) {
	assert.Contains(t, FAQContent, "计费相关")
	assert.Contains(t, FAQContent, "磁盘")
	assert.Contains(t, FAQContent, "继续收费")
}

func TestFAQContent_ContainsModelSection(t *testing.T) {
	assert.Contains(t, FAQContent, "模型套餐")
	assert.Contains(t, FAQContent, "积分")
}

func TestFAQContent_TokenBudget(t *testing.T) {
	// FAQ should be within token budget (~7K tokens ≈ ~28K chars for Chinese)
	// Rough estimate: 1 token ≈ 1.5 Chinese chars or 4 English chars
	charCount := len([]rune(FAQContent))
	t.Logf("FAQ content: %d runes, ~%.0f tokens (estimate)", charCount, float64(charCount)/1.5)
	assert.Less(t, charCount, 15000, "FAQ content should be under ~10K tokens")
}

func TestBuildSystem_IncludesFAQ(t *testing.T) {
	system := BuildSystem("用户有1个实例")
	assert.Contains(t, system, "平台常见问题")
	assert.Contains(t, system, "关机后还扣费吗")
}

func TestBuildSystem_IncludesGPUToolGuidance(t *testing.T) {
	system := BuildSystem("")
	assert.Contains(t, system, "GetGPUSpecs")
	assert.Contains(t, system, "GetGPURecommendation")
}

func TestBuildSystem_FormatIntegrity(t *testing.T) {
	system := BuildSystem("test context")
	// Should not contain unresolved format directives
	assert.False(t, strings.Contains(system, "%s"), "system prompt should not have unresolved %%s")
	assert.False(t, strings.Contains(system, "%!("), "system prompt should not have format errors")
}
