package feishu

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/stretchr/testify/require"
)

func TestNewTopicIsQueuedWithoutMentionButReplyIsNot(t *testing.T) {
	service := &Service{
		cfg:     config.FeishuConfig{AutoReplyNewTopics: true},
		allowed: map[string]struct{}{"oc_topic": {}},
		queue:   make(chan job, 2),
		seen:    make(map[string]time.Time),
	}
	chatType := "topic_group"
	chatID := "oc_topic"
	senderType := "user"
	messageType := "post"
	content := `{"title":"新问题","content":[[{"tag":"text","text":"怎么解决？"}]]}`
	threadID := "omt_topic"
	rootMessageID := "om_root"
	rootEvent := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderType: &senderType},
			Message: &larkim.EventMessage{
				MessageId: &rootMessageID, ChatId: &chatID, ChatType: &chatType,
				ThreadId: &threadID, MessageType: &messageType, Content: &content,
			},
		},
	}
	require.NoError(t, service.onMessage(context.Background(), rootEvent))
	require.Len(t, service.queue, 1)
	require.Equal(t, "新问题\n怎么解决？", (<-service.queue).question)

	replyMessageID := "om_reply"
	replyEvent := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderType: &senderType},
			Message: &larkim.EventMessage{
				MessageId: &replyMessageID, RootId: &rootMessageID, ParentId: &rootMessageID,
				ChatId: &chatID, ChatType: &chatType, MessageType: &messageType, Content: &content,
				ThreadId: &threadID,
			},
		},
	}
	require.NoError(t, service.onMessage(context.Background(), replyEvent))
	require.Empty(t, service.queue, "topic replies without @bot must not trigger automatic chatter")
}
