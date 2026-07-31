package feishu

import (
	"encoding/base64"
	"testing"
	"unicode/utf8"

	"github.com/compshare-agent/internal/config"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/stretchr/testify/require"
)

func TestInputFromTextMessageStripsMentions(t *testing.T) {
	messageType := "text"
	content := `{"text":"@_user_1  外部数据库还能连接吗？  "}`
	key := "@_user_1"
	message := &larkim.EventMessage{
		MessageType: &messageType,
		Content:     &content,
		Mentions:    []*larkim.MentionEvent{{Key: &key}},
	}
	input, ok := inputFromMessage(message)
	require.True(t, ok)
	require.Equal(t, "外部数据库还能连接吗？", input.Question)
}

func TestInputFromPostCollectsTopicTextAndImages(t *testing.T) {
	messageType := "post"
	content := `{
		"title":"训练报错",
		"content":[
			[{"tag":"text","text":"CUDA 初始化失败"}],
			[{"tag":"img","image_key":"img_123"}],
			[{"tag":"code_block","text":"RuntimeError"}]
		]
	}`
	input, ok := inputFromMessage(&larkim.EventMessage{MessageType: &messageType, Content: &content})
	require.True(t, ok)
	require.Equal(t, "训练报错\nCUDA 初始化失败\nRuntimeError", input.Question)
	require.Equal(t, []string{"img_123"}, input.ImageKeys)
}

func TestInputFromImageUsesOCRQuestion(t *testing.T) {
	messageType := "image"
	content := `{"image_key":"img_123"}`
	input, ok := inputFromMessage(&larkim.EventMessage{MessageType: &messageType, Content: &content})
	require.True(t, ok)
	require.Equal(t, imageOnlyQuestion, input.Question)
	require.Equal(t, []string{"img_123"}, input.ImageKeys)
}

func TestNewTopicRootIsDistinctFromTopicReply(t *testing.T) {
	chatType := "topic_group"
	root := &larkim.EventMessage{ChatType: &chatType}
	require.True(t, isNewTopicRoot(root))
	require.True(t, shouldRespond(root, false, true))
	rootID := "om_root"
	reply := &larkim.EventMessage{ChatType: &chatType, RootId: &rootID}
	require.False(t, isNewTopicRoot(reply))
	require.False(t, shouldRespond(reply, false, true))
	require.True(t, shouldRespond(reply, true, true))
}

func TestEncodeImageDataURLForExistingOCRProtocol(t *testing.T) {
	pngBytes, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M8AAQUBAScY42YAAAAASUVORK5CYII=",
	)
	require.NoError(t, err)
	dataURL, err := encodeImageDataURL(pngBytes, 1024)
	require.NoError(t, err)
	require.Contains(t, dataURL, "data:image/png;base64,")
	_, err = encodeImageDataURL(pngBytes, 4)
	require.ErrorContains(t, err, "exceeds")
}

func TestSplitReplyKeepsUnicodeWithinConfiguredSize(t *testing.T) {
	parts := splitReply("第一段内容\n\n第二段内容很长", 8)
	require.Greater(t, len(parts), 1)
	for _, part := range parts {
		require.LessOrEqual(t, utf8.RuneCountInString(part), 15)
	}
	require.Contains(t, parts[0], "[1/")
}

func TestValidateConfigIsFailClosedForChatAllowlist(t *testing.T) {
	cfg := config.FeishuConfig{
		AppID: "cli_x", AppSecret: "secret", AgentWSURL: "ws://127.0.0.1:7429/",
		CompanyID: 1, OrganizationID: 2,
	}
	require.ErrorContains(t, ValidateConfig(cfg), "allowed_chat_ids")
	cfg.AllowedChatIDs = []string{"oc_test"}
	require.NoError(t, ValidateConfig(cfg))
}

func TestMentionedBotMatchesOpenID(t *testing.T) {
	openID := "ou_bot"
	require.True(t, mentionedBot([]*larkim.MentionEvent{{Id: &larkim.UserId{OpenId: &openID}}}, openID, ""))
	require.False(t, mentionedBot(nil, openID, ""))
}
