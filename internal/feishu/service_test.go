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
	// Feishu represents topic-mode group messages as chat_type=group.
	chatType := "group"
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

func TestTopicCreatorCommentDoesNotAutoReply(t *testing.T) {
	service := &Service{
		cfg:     config.FeishuConfig{AutoReplyNewTopics: true},
		allowed: map[string]struct{}{"oc_topic": {}},
		queue:   make(chan job, 2),
		seen:    make(map[string]time.Time),
	}
	chatType := "group"
	chatID := "oc_topic"
	senderType := "user"
	messageType := "text"
	content := `{"text":"补充一下"}`
	threadID := "omt_topic"
	rootMessageID := "om_root"
	otherCommentID := "om_other_comment"
	creatorCommentID := "om_creator_comment"

	// This event is sent by the topic creator, but it is a comment on another
	// user's comment. Auto-reply mode must use the topic-root shape rather than
	// merely accepting every user message from the original author.
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderType: &senderType},
			Message: &larkim.EventMessage{
				MessageId: &creatorCommentID, RootId: &rootMessageID, ParentId: &otherCommentID,
				ChatId: &chatID, ChatType: &chatType, MessageType: &messageType, Content: &content,
				ThreadId: &threadID,
			},
		},
	}
	require.NoError(t, service.onMessage(context.Background(), event))
	require.Empty(t, service.queue, "comments must not automatically trigger, even from the topic creator")
}

func TestTopicReplyIsQueuedWithoutMentionWhenAllMessagesEnabled(t *testing.T) {
	service := &Service{
		cfg:     config.FeishuConfig{AutoReplyAllMessages: true},
		allowed: map[string]struct{}{"oc_topic": {}},
		queue:   make(chan job, 1),
		seen:    make(map[string]time.Time),
	}
	chatType := "group"
	chatID := "oc_topic"
	senderType := "user"
	messageType := "text"
	content := `{"text":"继续看这个截图"}`
	threadID := "omt_topic"
	rootMessageID := "om_root"
	replyMessageID := "om_reply"
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderType: &senderType},
			Message: &larkim.EventMessage{
				MessageId: &replyMessageID, RootId: &rootMessageID, ParentId: &rootMessageID,
				ChatId: &chatID, ChatType: &chatType, MessageType: &messageType, Content: &content,
				ThreadId: &threadID,
			},
		},
	}
	require.NoError(t, service.onMessage(context.Background(), event))
	require.Len(t, service.queue, 1)
	queued := <-service.queue
	require.Equal(t, "继续看这个截图", queued.question)
	require.Equal(t, "oc_topic:omt_topic", queued.topicKey)
}

func TestAllMessagesStillIgnoresBotSender(t *testing.T) {
	service := &Service{
		cfg:     config.FeishuConfig{AutoReplyAllMessages: true},
		allowed: map[string]struct{}{"oc_topic": {}},
		queue:   make(chan job, 1),
		seen:    make(map[string]time.Time),
	}
	chatType := "group"
	chatID := "oc_topic"
	senderType := "bot"
	messageType := "text"
	content := `{"text":"机器人自己的回复"}`
	messageID := "om_bot"
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderType: &senderType},
			Message: &larkim.EventMessage{
				MessageId: &messageID, ChatId: &chatID, ChatType: &chatType,
				MessageType: &messageType, Content: &content,
			},
		},
	}
	require.NoError(t, service.onMessage(context.Background(), event))
	require.Empty(t, service.queue)
}
