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

func TestTopicCreatorFollowupNeedsExplicitBotMention(t *testing.T) {
	botOpenID := "ou_bot"
	service := &Service{
		cfg:       config.FeishuConfig{AutoReplyNewTopics: true},
		botOpenID: botOpenID,
		allowed:   map[string]struct{}{"oc_topic": {}},
		queue:     make(chan job, 4),
		seen:      make(map[string]time.Time),
	}
	chatType := "group"
	chatID := "oc_topic"
	senderType := "user"
	messageType := "text"
	threadID := "omt_topic"
	rootMessageID := "om_root"
	ownerOpenID := "ou_topic_owner"
	ownerID := &larkim.UserId{OpenId: &ownerOpenID}
	botMentionKey := "@_user_1"
	botMention := []*larkim.MentionEvent{{Key: &botMentionKey, Id: &larkim.UserId{OpenId: &botOpenID}}}

	rootContent := `{"text":"第一个问题"}`
	rootEvent := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: ownerID, SenderType: &senderType},
			Message: &larkim.EventMessage{
				MessageId: &rootMessageID, ChatId: &chatID, ChatType: &chatType,
				ThreadId: &threadID, MessageType: &messageType, Content: &rootContent,
			},
		},
	}
	require.NoError(t, service.onMessage(context.Background(), rootEvent))
	require.Len(t, service.queue, 1, "the topic's first question answers itself")
	<-service.queue

	// The same person continuing directly under their own root post is the
	// shape a "one question, one answer" chat would produce. It stays silent.
	ownerFollowupID := "om_owner_followup"
	ownerFollowupContent := `{"text":"我的后续问题"}`
	ownerFollowup := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: ownerID, SenderType: &senderType},
			Message: &larkim.EventMessage{
				MessageId: &ownerFollowupID, RootId: &rootMessageID, ParentId: &rootMessageID,
				ChatId: &chatID, ChatType: &chatType, MessageType: &messageType, Content: &ownerFollowupContent,
				ThreadId: &threadID,
			},
		},
	}
	require.NoError(t, service.onMessage(context.Background(), ownerFollowup))
	require.Empty(t, service.queue, "the creator's own follow-up must wait for an explicit @bot")

	otherCommentID := "om_other_comment"
	ownerNestedID := "om_owner_nested"
	ownerNestedContent := `{"text":"回复其他成员"}`
	ownerNested := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: ownerID, SenderType: &senderType},
			Message: &larkim.EventMessage{
				MessageId: &ownerNestedID, RootId: &rootMessageID, ParentId: &otherCommentID,
				ChatId: &chatID, ChatType: &chatType, MessageType: &messageType, Content: &ownerNestedContent,
				ThreadId: &threadID,
			},
		},
	}
	require.NoError(t, service.onMessage(context.Background(), ownerNested))
	require.Empty(t, service.queue, "a nested reply from the creator must not automatically trigger")

	mentionedFollowupID := "om_owner_mentioned"
	mentionedFollowupContent := `{"text":"@_user_1 那这个呢"}`
	mentionedFollowup := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: ownerID, SenderType: &senderType},
			Message: &larkim.EventMessage{
				MessageId: &mentionedFollowupID, RootId: &rootMessageID, ParentId: &rootMessageID,
				ChatId: &chatID, ChatType: &chatType, MessageType: &messageType, Content: &mentionedFollowupContent,
				ThreadId: &threadID, Mentions: botMention,
			},
		},
	}
	require.NoError(t, service.onMessage(context.Background(), mentionedFollowup))
	require.Len(t, service.queue, 1, "@bot is the only way to continue a topic")
	queued := <-service.queue
	require.Equal(t, "那这个呢", queued.question)
	require.Equal(t, "oc_topic:omt_topic", queued.topicKey, "a mentioned follow-up stays in the topic's session")
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

func TestUserMentionSuppressesAutomaticTopicRootReply(t *testing.T) {
	service := &Service{
		cfg:     config.FeishuConfig{AutoReplyNewTopics: true},
		allowed: map[string]struct{}{"oc_topic": {}},
		queue:   make(chan job, 1),
		seen:    make(map[string]time.Time),
	}
	chatType := "group"
	chatID := "oc_topic"
	senderType := "user"
	messageType := "text"
	messageID := "om_root"
	threadID := "omt_topic"
	content := `{"text":"@_user_1 68962389"}`
	mentionKey := "@_user_1"
	mentionedOpenID := "ou_other_member"
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderType: &senderType},
			Message: &larkim.EventMessage{
				MessageId: &messageID, ChatId: &chatID, ChatType: &chatType, ThreadId: &threadID,
				MessageType: &messageType, Content: &content,
				Mentions: []*larkim.MentionEvent{{Key: &mentionKey, Id: &larkim.UserId{OpenId: &mentionedOpenID}}},
			},
		},
	}
	require.NoError(t, service.onMessage(context.Background(), event))
	require.Empty(t, service.queue, "@ another user must not automatically invoke the bot")
}
