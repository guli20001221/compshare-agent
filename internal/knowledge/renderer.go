package knowledge

import (
	"fmt"
	"strings"
)

const KnowledgeMissReply = "\u6682\u672a\u627e\u5230\u5339\u914d\u7684\u5e73\u53f0FAQ\u6216\u64cd\u4f5c\u6307\u5357\uff0c\u8bf7\u6362\u4e2a\u95ee\u6cd5\uff0c\u6216\u4f7f\u7528\u63a7\u5236\u53f0\u5e2e\u52a9\u6587\u6863\u67e5\u8be2\u3002"

func RenderKnowledgeAnswer(result RetrievalResult) string {
	if result.Empty || len(result.Hits) == 0 {
		return KnowledgeMissReply
	}
	var b strings.Builder
	b.WriteString("根据平台知识库：\n")
	for i, chunk := range result.Hits {
		if i > 0 {
			b.WriteString("\n")
		}
		title := strings.TrimSpace(chunk.Title)
		if title == "" {
			title = "相关条目"
		}
		b.WriteString(fmt.Sprintf("%d. %s（%s）\n", i+1, title, sourceTypeLabel(chunk.SourceType)))
		b.WriteString(strings.TrimSpace(chunk.Content))
		if strings.TrimSpace(chunk.SourceURL) != "" {
			b.WriteString("\n来源链接：")
			b.WriteString(strings.TrimSpace(chunk.SourceURL))
		}
	}
	return b.String()
}

func sourceTypeLabel(sourceType string) string {
	switch sourceType {
	case sourceTypeFAQ:
		return "FAQ"
	case sourceTypeRunbook:
		return "Runbook"
	default:
		return "平台知识"
	}
}
