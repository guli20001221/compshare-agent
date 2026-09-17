package feishu

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/config"
)

// fakeLarkAPI answers the three Open API calls the support delivery makes and
// records every reply post in the order it arrived.
type fakeLarkAPI struct {
	server  *httptest.Server
	replies []string
}

func newFakeLarkAPI(t *testing.T) *fakeLarkAPI {
	t.Helper()
	api := &fakeLarkAPI{}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","tenant_access_token":"t-test","expire":7200}`)
		case r.URL.Path == "/open-apis/im/v1/images":
			_, _ = io.WriteString(w, `{"code":0,"msg":"success","data":{"image_key":"img_support_qr"}}`)
		case strings.HasPrefix(r.URL.Path, "/open-apis/im/v1/messages/") && strings.HasSuffix(r.URL.Path, "/reply"):
			body, _ := io.ReadAll(r.Body)
			var frame struct {
				Content string `json:"content"`
			}
			require.NoError(t, json.Unmarshal(body, &frame))
			api.replies = append(api.replies, frame.Content)
			_, _ = io.WriteString(w, `{"code":0,"msg":"success","data":{"message_id":"om_reply"}}`)
		default:
			t.Errorf("unexpected Feishu API call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (api *fakeLarkAPI) service() *Service {
	return &Service{
		cfg:               config.FeishuConfig{},
		api:               lark.NewClient("cli_test", "secret", lark.WithOpenBaseUrl(api.server.URL)),
		customerSupportQR: []byte("\x89PNG\r\n\x1a\nfake"),
	}
}

// A support handoff after the Agent found something delivers the Agent's answer
// first, then the support sentence with the QR, as two posts in the thread.
func TestCustomerSupportDeliveryKeepsTheAgentsAnswerAheadOfTheEntry(t *testing.T) {
	api := newFakeLarkAPI(t)
	const answer = "文档没有这项规则，需要平台人员核实。"

	require.NoError(t, api.service().replyCustomerSupport(context.Background(), "om_question", answer))

	require.Len(t, api.replies, 2)
	require.Contains(t, api.replies[0], answer)
	require.NotContains(t, api.replies[0], "img_support_qr")
	require.Contains(t, api.replies[1], "img_support_qr")
	require.Contains(t, api.replies[1], "需要优云智算人工客服协助处理")
	require.NotContains(t, api.replies[1], answer)
}

// A handoff that was the turn's whole answer still sends only the entry.
func TestCustomerSupportDeliveryWithoutAnAnswerSendsTheEntryAlone(t *testing.T) {
	api := newFakeLarkAPI(t)

	require.NoError(t, api.service().replyCustomerSupport(context.Background(), "om_question", "  \n"))

	require.Len(t, api.replies, 1)
	require.Contains(t, api.replies[0], "img_support_qr")
	require.Contains(t, api.replies[0], "需要优云智算人工客服协助处理")
}
