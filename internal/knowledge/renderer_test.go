package knowledge

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRenderKnowledgeAnswerUsesRetrievedChunkContent(t *testing.T) {
	result := RetrievalResult{
		Enabled:   true,
		KBVersion: "kb-test",
		Hits: []KBChunk{
			{
				ChunkID:    "faq-billing-001",
				SourceType: sourceTypeFAQ,
				Title:      "按量实例关机后的计费",
				Content:    "按量实例关机后 GPU、CPU 和内存停止计费，但额外数据盘继续收费。具体费用以控制台财务中心或 API 查询为准。",
				SourceURL:  "https://www.compshare.cn/docs/",
			},
		},
	}

	answer := RenderKnowledgeAnswer(result)

	assert.Contains(t, answer, "根据平台知识库")
	assert.Contains(t, answer, "按量实例关机后的计费")
	assert.Contains(t, answer, "额外数据盘继续收费")
	assert.Contains(t, answer, "控制台财务中心或 API 查询为准")
	assert.Contains(t, answer, "https://www.compshare.cn/docs/")
	assert.NotContains(t, answer, "faq-billing-001")
	assert.NotContains(t, answer, "kb-test")
}

func TestRenderKnowledgeAnswerDoesNotExposeChunkInternals(t *testing.T) {
	result := RetrievalResult{
		Enabled: true,
		Hits: []KBChunk{
			{
				ChunkID:          "internal-chunk-id",
				KBVersion:        "internal-kb-version",
				SourceType:       sourceTypeFAQ,
				ProductArea:      "internal-product-area",
				ACL:              "customer_safe",
				ValidFrom:        "2026-05-09",
				ValidTo:          ptrString("2026-06-01"),
				Confidence:       "high",
				Title:            "安全标题",
				QuestionPatterns: []string{"internal-question-pattern"},
				Content:          "安全内容。",
			},
		},
	}

	answer := RenderKnowledgeAnswer(result)

	assert.NotContains(t, answer, "internal-chunk-id")
	assert.NotContains(t, answer, "internal-kb-version")
	assert.NotContains(t, answer, "internal-product-area")
	assert.NotContains(t, answer, "customer_safe")
	assert.NotContains(t, answer, "2026-05-09")
	assert.NotContains(t, answer, "2026-06-01")
	assert.NotContains(t, answer, "high")
	assert.NotContains(t, answer, "internal-question-pattern")
}

func TestRenderKnowledgeAnswerMultipleChunks(t *testing.T) {
	result := RetrievalResult{
		Enabled: true,
		Hits: []KBChunk{
			{SourceType: sourceTypeFAQ, Title: "登录实例", Content: "可通过 SSH、VS Code Remote-SSH、JupyterLab 或 Windows RDP 登录。"},
			{SourceType: sourceTypeRunbook, Title: "端口服务", Content: "平台已知服务端口映射可通过 DescribeCompShareSoftwarePort 查询。"},
		},
	}

	answer := RenderKnowledgeAnswer(result)

	assert.Contains(t, answer, "1. 登录实例")
	assert.Contains(t, answer, "2. 端口服务")
	assert.Contains(t, answer, "FAQ")
	assert.Contains(t, answer, "Runbook")
}

func TestRenderKnowledgeAnswerDefensiveFallbackLabels(t *testing.T) {
	result := RetrievalResult{
		Enabled: true,
		Hits: []KBChunk{
			{SourceType: "unexpected", Content: "安全内容。"},
		},
	}

	answer := RenderKnowledgeAnswer(result)

	assert.Contains(t, answer, "相关条目")
	assert.Contains(t, answer, "平台知识")
}

func TestRenderKnowledgeAnswerMissIsDeterministic(t *testing.T) {
	assert.Equal(t, KnowledgeMissReply, RenderKnowledgeAnswer(RetrievalResult{Enabled: true, Empty: true}))
	assert.Equal(t, KnowledgeMissReply, RenderKnowledgeAnswer(RetrievalResult{Enabled: true}))
}

func TestRenderKnowledgeAnswerOmitsEmptySourceURL(t *testing.T) {
	result := RetrievalResult{
		Enabled: true,
		Hits: []KBChunk{
			{SourceType: sourceTypeFAQ, Title: "镜像选择", Content: "平台镜像和社区镜像入口不同。"},
		},
	}

	answer := RenderKnowledgeAnswer(result)

	assert.NotContains(t, answer, "http")
	assert.False(t, strings.Contains(answer, "来源链接："))
}
