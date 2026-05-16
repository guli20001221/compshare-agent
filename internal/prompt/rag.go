package prompt

import (
	"fmt"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

type RAGReference struct {
	Number  int
	Title   string
	Content string
}

func BuildRAGMessages(question string, refs []RAGReference, weak bool, retry bool) []openai.ChatCompletionMessage {
	system := `你是 CompShare 平台知识库问答助手。
只能根据给定资料回答，不要补充资料中没有的事实。
如果资料没有覆盖用户问题，请回复：当前知识库未覆盖该问题,我无法回答。
如果资料只覆盖一部分，请说明：当前知识库只收录了以下信息。
如果标题和正文存在冲突，以正文中的明确陈述为准，并说明资料表述不一致。
涉及时间、金额、窗口、条件判断时，必须保留资料里的原始条件，不要把示例改写成通用规则。
如果多个资料给出不同价格或规则，只使用与用户问题直接相关的资料，不要混合无关资料推断。
回答中的每个事实必须带引用编号，例如 [1]。`
	if weak {
		system += "\n资料相关性较低；只有在资料确实能回答时才回答，否则拒答。"
	}
	if retry {
		system += "\n上一次回答缺少引用。必须带 [1]、[2] 这样的引用编号；如果无法引用，直接拒答。"
	}

	var user strings.Builder
	user.WriteString("用户问题：\n")
	user.WriteString(strings.TrimSpace(question))
	user.WriteString("\n\n资料：\n")
	for _, ref := range refs {
		number := ref.Number
		if number <= 0 {
			number = 1
		}
		user.WriteString(fmt.Sprintf("[%d] %s\n", number, strings.TrimSpace(ref.Title)))
		user.WriteString(strings.TrimSpace(ref.Content))
		user.WriteString("\n\n")
	}
	user.WriteString("请基于以上资料回答。")

	return []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: system},
		{Role: openai.ChatMessageRoleUser, Content: user.String()},
	}
}
