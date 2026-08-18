package feishu

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/stretchr/testify/require"
)

func TestTopicParticipantMatchFailsClosedWhenOpenIDsDiffer(t *testing.T) {
	owner := topicParticipant{openID: "ou_owner", userID: "tenant_user"}
	other := topicParticipant{openID: "ou_other", userID: "tenant_user"}
	require.False(t, owner.matches(other))
}

func TestTopicFollowupWithoutRootSenderIdentityFailsClosed(t *testing.T) {
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
	require.Empty(t, service.queue, "without the root sender identity, a follow-up must not be attributed to the topic creator")
}

func TestOnlyTopicCreatorDirectFollowupAutoReplies(t *testing.T) {
	service := &Service{
		cfg:     config.FeishuConfig{AutoReplyNewTopics: true},
		allowed: map[string]struct{}{"oc_topic": {}},
		queue:   make(chan job, 4),
		seen:    make(map[string]time.Time),
	}
	chatType := "group"
	chatID := "oc_topic"
	senderType := "user"
	messageType := "text"
	threadID := "omt_topic"
	rootMessageID := "om_root"
	ownerOpenID := "ou_topic_owner"
	otherOpenID := "ou_other_member"
	ownerID := &larkim.UserId{OpenId: &ownerOpenID}
	otherID := &larkim.UserId{OpenId: &otherOpenID}

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
	require.Len(t, service.queue, 1)
	<-service.queue

	// Another member can write directly in the topic, but it must not create
	// an automatic answer because that member did not create the topic.
	otherDirectMessageID := "om_other_direct"
	otherDirectContent := `{"text":"我也想知道"}`
	otherDirect := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: otherID, SenderType: &senderType},
			Message: &larkim.EventMessage{
				MessageId: &otherDirectMessageID, RootId: &rootMessageID, ParentId: &rootMessageID,
				ChatId: &chatID, ChatType: &chatType, MessageType: &messageType, Content: &otherDirectContent,
				ThreadId: &threadID,
			},
		},
	}
	require.NoError(t, service.onMessage(context.Background(), otherDirect))
	require.Empty(t, service.queue)

	otherCommentID := "om_other_comment"
	creatorCommentID := "om_creator_comment"
	creatorCommentContent := `{"text":"回复其他成员"}`
	creatorComment := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: ownerID, SenderType: &senderType},
			Message: &larkim.EventMessage{
				MessageId: &creatorCommentID, RootId: &rootMessageID, ParentId: &otherCommentID,
				ChatId: &chatID, ChatType: &chatType, MessageType: &messageType, Content: &creatorCommentContent,
				ThreadId: &threadID,
			},
		},
	}
	require.NoError(t, service.onMessage(context.Background(), creatorComment))
	require.Empty(t, service.queue, "a nested reply from the creator must not automatically trigger")

	creatorFollowupMessageID := "om_creator_followup"
	creatorFollowupContent := `{"text":"我的后续问题"}`
	creatorFollowup := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: ownerID, SenderType: &senderType},
			Message: &larkim.EventMessage{
				MessageId: &creatorFollowupMessageID, RootId: &rootMessageID, ParentId: &rootMessageID,
				ChatId: &chatID, ChatType: &chatType, MessageType: &messageType, Content: &creatorFollowupContent,
				ThreadId: &threadID,
			},
		},
	}
	require.NoError(t, service.onMessage(context.Background(), creatorFollowup))
	require.Len(t, service.queue, 1)
	queued := <-service.queue
	require.Equal(t, "我的后续问题", queued.question)
	require.Equal(t, "oc_topic:omt_topic", queued.topicKey)
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
