package prompt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFAQContent_NotEmpty(t *testing.T) {
	assert.NotEmpty(t, FAQContent)
}

func TestFAQContent_Contains11Topics(t *testing.T) {
	topics := []string{
		"### 1. 镜像选择",
		"### 2. 登录实例",
		"### 3. 防火墙/端口",
		"### 4. 云硬盘",
		"### 5. 公共模型库",
		"### 6. 网络加速",
		"### 7. 无卡模式",
		"### 8. 计费/回收规则",
		"### 9. 模型套餐",
		"### 10. 实践部署",
		"### 11. 账号管理",
	}
	for _, topic := range topics {
		assert.Contains(t, FAQContent, topic, "missing topic: %s", topic)
	}
}

func TestFAQContent_ContainsKeyBillingInfo(t *testing.T) {
	assert.Contains(t, FAQContent, "按量")
	assert.Contains(t, FAQContent, "包月")
	assert.Contains(t, FAQContent, "关机后")
	assert.Contains(t, FAQContent, "磁盘")
	assert.Contains(t, FAQContent, "继续收费")
	assert.Contains(t, FAQContent, "8357")
	assert.Contains(t, FAQContent, "8095")
	assert.Contains(t, FAQContent, "8429")
	// High-frequency billing topics from original FAQ
	assert.Contains(t, FAQContent, "初始化是否收费")
	assert.Contains(t, FAQContent, "按量转包月")
	assert.Contains(t, FAQContent, "欠费")
}

func TestFAQContent_ContainsKeyInstanceInfo(t *testing.T) {
	assert.Contains(t, FAQContent, "SSH")
	assert.Contains(t, FAQContent, "JupyterLab")
	assert.Contains(t, FAQContent, "/start.d/")
	assert.Contains(t, FAQContent, "无卡模式")
	assert.Contains(t, FAQContent, "以控制台为准")
}

func TestFAQContent_ContainsImageGuidance(t *testing.T) {
	assert.NotContains(t, FAQContent, "平台有三类镜像")
	assert.Contains(t, FAQContent, "平台镜像和社区镜像")
	assert.Contains(t, FAQContent, "共享镜像")
	assert.Contains(t, FAQContent, "私有镜像")
	assert.Contains(t, FAQContent, "基础镜像")
	assert.Contains(t, FAQContent, "系统镜像")
	assert.Contains(t, FAQContent, "第三方镜像")
	assert.Contains(t, FAQContent, "付费")
	assert.Contains(t, FAQContent, "免费")
	assert.Contains(t, FAQContent, "DescribeCompShareImages")
	assert.Contains(t, FAQContent, "DescribeCommunityImages")
	assert.Contains(t, FAQContent, "DescribeCompShareCustomImages")
	assert.Contains(t, FAQContent, "不支持查询 Custom")
}

func TestFAQContent_ContainsModelSuiteInfo(t *testing.T) {
	assert.Contains(t, FAQContent, "模型套餐")
	assert.Contains(t, FAQContent, "积分")
	assert.Contains(t, FAQContent, "Claude Code")
	assert.Contains(t, FAQContent, "Anthropic")
}

func TestFAQContent_ContainsPracticalDeployment(t *testing.T) {
	assert.Contains(t, FAQContent, "Docker")
	assert.Contains(t, FAQContent, "Ollama")
	assert.Contains(t, FAQContent, "nvidia-smi")
}

func TestFAQContent_ContainsAccountManagement(t *testing.T) {
	assert.Contains(t, FAQContent, "发票")
	assert.Contains(t, FAQContent, "团队管理")
}

func TestFAQContent_NoHardcodedPrices(t *testing.T) {
	assert.NotContains(t, FAQContent, "元/卡/时")
	assert.NotContains(t, FAQContent, "元/时")
	assert.NotContains(t, FAQContent, "200GB")
	assert.Contains(t, FAQContent, "GetCompShareInstanceUserPrice")
	assert.Contains(t, FAQContent, "以控制台", "dynamic values should point to console")
}

func TestFAQContent_PointsToAPIs(t *testing.T) {
	assert.Contains(t, FAQContent, "GetCompShareInstanceUserPrice")
	assert.Contains(t, FAQContent, "GetCompShareInstancePrice")
	assert.Contains(t, FAQContent, "DescribeCompShareSoftwarePort")
	assert.Contains(t, FAQContent, "DescribeCompShareJupyterToken")
}

func TestFAQContent_TokenBudget(t *testing.T) {
	charCount := len([]rune(FAQContent))
	t.Logf("FAQ content: %d runes, ~%.0f tokens (estimate)", charCount, float64(charCount)/1.5)
	assert.Greater(t, charCount, 1000, "FAQ content should not be too thin")
	assert.Less(t, charCount, 4000, "FAQ content should stay under ~2600 tokens")
}

func TestBuildSystem_IncludesFAQ(t *testing.T) {
	system := BuildSystem("用户有1个实例")
	assert.Contains(t, system, "平台常见问题")
	assert.Contains(t, system, "镜像选择")
	assert.Contains(t, system, "计费/回收规则")
}

func TestBuildSystem_IncludesGPUToolGuidance(t *testing.T) {
	system := BuildSystem("")
	assert.Contains(t, system, "GetGPUSpecs")
	assert.Contains(t, system, "GetGPURecommendation")
}

func TestBuildSystem_FormatIntegrity(t *testing.T) {
	system := BuildSystem("test context")
	assert.False(t, strings.Contains(system, "%s"), "system prompt should not have unresolved %%s")
	assert.False(t, strings.Contains(system, "%!("), "system prompt should not have format errors")
}
