package prompt

import (
	"embed"
	"fmt"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

type RAGReference struct {
	Number  int
	Title   string
	Content string
}

//go:embed rag_system_segments/*.txt
var ragSystemSegmentFS embed.FS

func ragSystemSegments() map[string]string {
	names := ragSystemSegmentNames()
	names = append(names, "weak_branch", "retry_branch")
	segments := make(map[string]string, len(names))
	for _, name := range names {
		data, err := ragSystemSegmentFS.ReadFile("rag_system_segments/" + name + ".txt")
		if err != nil {
			panic(fmt.Sprintf("prompt: read RAG system segment %q: %v", name, err))
		}
		segments[name] = strings.TrimSpace(string(data))
	}
	return segments
}

func ragSystemSegmentNames() []string {
	data, err := ragSystemSegmentFS.ReadFile("rag_system_segments/order.txt")
	if err != nil {
		panic(fmt.Sprintf("prompt: read RAG system segment order: %v", err))
	}
	names := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		names = append(names, line)
	}
	if len(names) == 0 {
		panic("prompt: RAG system segment order is empty")
	}
	return names
}

func buildRAGSystemPrompt(weak bool, retry bool) string {
	segments := ragSystemSegments()
	order := ragSystemSegmentNames()
	parts := make([]string, 0, len(order)+2)
	for _, name := range order {
		parts = append(parts, segments[name])
	}
	if weak {
		parts = append(parts, segments["weak_branch"])
	}
	if retry {
		parts = append(parts, segments["retry_branch"])
	}
	return strings.Join(parts, "\n\n")
}

func BuildRAGMessages(question string, refs []RAGReference, weak bool, retry bool) []openai.ChatCompletionMessage {
	system := buildRAGSystemPrompt(weak, retry)

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
