package feishu

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const imageOnlyQuestion = "请识别图片内容，并结合知识库回答图片中的问题。"

type messageInput struct {
	Question  string
	ImageKeys []string
}

func inputFromMessage(message *larkim.EventMessage) (messageInput, bool) {
	if message == nil {
		return messageInput{}, false
	}
	var input messageInput
	switch stringValue(message.MessageType) {
	case "text":
		var content struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(stringValue(message.Content)), &content); err != nil {
			return messageInput{}, false
		}
		input.Question = content.Text

	case "post":
		var content struct {
			Title   string `json:"title"`
			Content [][]struct {
				Tag      string `json:"tag"`
				Text     string `json:"text"`
				Href     string `json:"href"`
				ImageKey string `json:"image_key"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(stringValue(message.Content)), &content); err != nil {
			return messageInput{}, false
		}
		var paragraphs []string
		if strings.TrimSpace(content.Title) != "" {
			paragraphs = append(paragraphs, strings.TrimSpace(content.Title))
		}
		for _, paragraph := range content.Content {
			var line strings.Builder
			for _, element := range paragraph {
				switch element.Tag {
				case "text", "a", "code_block", "md":
					line.WriteString(element.Text)
				case "img":
					if element.ImageKey != "" {
						input.ImageKeys = append(input.ImageKeys, element.ImageKey)
					}
				}
			}
			if text := strings.TrimSpace(line.String()); text != "" {
				paragraphs = append(paragraphs, text)
			}
		}
		input.Question = strings.Join(paragraphs, "\n")

	case "image":
		var content struct {
			ImageKey string `json:"image_key"`
		}
		if err := json.Unmarshal([]byte(stringValue(message.Content)), &content); err != nil || content.ImageKey == "" {
			return messageInput{}, false
		}
		input.ImageKeys = []string{content.ImageKey}

	default:
		return messageInput{}, false
	}

	for _, mention := range message.Mentions {
		if mention != nil {
			input.Question = strings.ReplaceAll(input.Question, stringValue(mention.Key), "")
		}
	}
	input.Question = strings.TrimSpace(input.Question)
	if input.Question == "" && len(input.ImageKeys) != 0 {
		input.Question = imageOnlyQuestion
	}
	return input, input.Question != ""
}

func isNewTopicRoot(message *larkim.EventMessage) bool {
	return message != nil &&
		stringValue(message.ChatType) == "topic_group" &&
		stringValue(message.RootId) == "" &&
		stringValue(message.ParentId) == ""
}

func splitReply(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if limit <= 0 {
		limit = 3500
	}
	if utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}
	var chunks []string
	remaining := []rune(text)
	for len(remaining) > 0 {
		end := min(limit, len(remaining))
		chunk := strings.TrimSpace(string(remaining[:end]))
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		remaining = remaining[end:]
	}
	if len(chunks) > 1 {
		for i := range chunks {
			chunks[i] = "[" + itoa(i+1) + "/" + itoa(len(chunks)) + "]\n" + chunks[i]
		}
	}
	return chunks
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func itoa(value int) string {
	const digits = "0123456789"
	if value < 10 {
		return string(digits[value])
	}
	return itoa(value/10) + string(digits[value%10])
}
