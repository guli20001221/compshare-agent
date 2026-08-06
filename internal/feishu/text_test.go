package feishu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"unicode/utf8"

	"github.com/compshare-agent/internal/config"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
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

func TestMarkdownPostContentPreservesMarkdown(t *testing.T) {
	markdown := "## 创建实例\n\n**第一步**：选择 GPU。\n\n1. 打开控制台"
	raw, err := markdownPostContent(markdown)
	require.NoError(t, err)

	var content struct {
		ZhCN struct {
			Content [][]struct {
				Tag  string `json:"tag"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"zh_cn"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &content))
	require.Equal(t, "md", content.ZhCN.Content[0][0].Tag)
	require.Equal(t, markdown, content.ZhCN.Content[0][0].Text)
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
	require.False(t, isNewTopicRoot(nil))

	// Feishu sends chat_type=group even when this is a topic-mode group.
	chatType := "group"
	threadID := "omt_topic"
	rootID := "om_root"
	root := &larkim.EventMessage{MessageId: &rootID, ChatType: &chatType, ThreadId: &threadID}
	require.True(t, isNewTopicRoot(root))
	require.True(t, shouldRespond(root, false, true))
	replyID := "om_reply"
	// Topic replies use the same thread_id and point both IDs to the root.
	reply := &larkim.EventMessage{
		MessageId: &replyID, ChatType: &chatType, ThreadId: &threadID,
		RootId: &rootID, ParentId: &rootID,
	}
	require.False(t, isNewTopicRoot(reply))
	require.False(t, shouldRespond(reply, false, true))
	require.True(t, shouldRespond(reply, true, true))

	incomplete := &larkim.EventMessage{ChatType: &chatType}
	require.False(t, isNewTopicRoot(incomplete), "missing thread_id must fail closed")
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

func TestImageReadFailureReplyExplainsExternalGroupLimitation(t *testing.T) {
	reply := imageReadFailureReply(errExternalGroupImageResourceUnsupported)
	require.Contains(t, reply, "外部群")
	require.Contains(t, reply, "内部群")
	require.NotEqual(t, reply, imageReadFailureReply(assertionError{}))
}

func TestImageReadFailureReplyExplainsMissingExternalImageAuthorization(t *testing.T) {
	reply := imageReadFailureReply(errExternalImageUserAuthorizationUnavailable)
	require.Contains(t, reply, "尚未完成授权")
	require.Contains(t, reply, "本企业成员")
}

func TestDownloadImageBytesUsesDelegatedUserAccessToken(t *testing.T) {
	pngBytes, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M8AAQUBAScY42YAAAAASUVORK5CYII=",
	)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, http.MethodGet, request.Method)
		require.Equal(t, "/open-apis/im/v1/messages/om_test/resources/img_test", request.URL.Path)
		require.Equal(t, "image", request.URL.Query().Get("type"))
		require.Equal(t, "Bearer user-token", request.Header.Get("Authorization"))
		writer.Header().Set("Content-Type", "image/png")
		_, _ = writer.Write(pngBytes)
	}))
	defer server.Close()

	service := &Service{
		cfg: config.FeishuConfig{MaxImageBytes: 1024},
		api: lark.NewClient("cli_test", "app-secret", lark.WithOpenBaseUrl(server.URL), lark.WithEnableTokenCache(false)),
	}
	got, err := service.downloadImageBytes(context.Background(), "om_test", "img_test", larkcore.WithUserAccessToken("user-token"))
	require.NoError(t, err)
	require.Equal(t, pngBytes, got)
}

type assertionError struct{}

func (assertionError) Error() string { return "test error" }

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
