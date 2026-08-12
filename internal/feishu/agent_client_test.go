package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/compshare-agent/internal/agentprotocol"
	"github.com/compshare-agent/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAgentClientSelectsFailClosedFeishuToolScope(t *testing.T) {
	tests := []struct {
		name          string
		platformReads bool
		expected      string
		unexpected    string
	}{
		{
			name:       "legacy config remains knowledge only",
			expected:   knowledgeOnlyFeature,
			unexpected: agentprotocol.FeatureFeishuPublicPlatformReadOnly,
		},
		{
			name:          "public platform reads are explicit opt in",
			platformReads: true,
			expected:      agentprotocol.FeatureFeishuPublicPlatformReadOnly,
			unexpected:    knowledgeOnlyFeature,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var observedFeature bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					var body map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					require.Equal(t, "CreateCSAgentSession", body["Action"])
					_, _ = w.Write([]byte(`{"RetCode":0,"SessionId":"session-1"}`))
					return
				}
				require.Equal(t, "1", r.Header.Get("X-Company-Id"))
				require.Equal(t, "2", r.Header.Get("X-Organization-Id"))
				conn, err := websocket.Accept(w, r, nil)
				require.NoError(t, err)
				defer conn.CloseNow()
				_, raw, err := conn.Read(r.Context())
				require.NoError(t, err)
				var frame struct {
					Features []string `json:"Features"`
					Session  string   `json:"SessionId"`
					Image    string   `json:"Image"`
				}
				require.NoError(t, json.Unmarshal(raw, &frame))
				require.Equal(t, "session-1", frame.Session)
				require.Equal(t, "data:image/png;base64,aW1hZ2U=", frame.Image)
				require.Contains(t, frame.Features, tt.expected)
				require.NotContains(t, frame.Features, tt.unexpected)
				require.Contains(t, frame.Features, agentprotocol.FeatureFeishuConsoleHandoff)
				observedFeature = true
				require.NoError(t, conn.Write(r.Context(), websocket.MessageText, []byte(`{"event":"meta","SessionId":"session-1"}`)))
				require.NoError(t, conn.Write(r.Context(), websocket.MessageText, []byte(`{"event":"done","Content":"知识库答案"}`)))
			}))
			defer server.Close()

			cfg := config.FeishuConfig{
				AgentWSURL: strings.Replace(server.URL, "http://", "ws://", 1),
				CompanyID:  1, OrganizationID: 2,
				EnablePlatformReadOnlyQueries: tt.platformReads,
				EnableConsoleHandoff:          true,
			}
			client, err := NewAgentClient(cfg)
			require.NoError(t, err)
			sessionID, err := client.CreateSession(context.Background(), "测试话题")
			require.NoError(t, err)
			answer, authoritative, err := client.Ask(
				context.Background(), sessionID, "message-1", "问题", "data:image/png;base64,aW1hZ2U=",
			)
			require.NoError(t, err)
			require.Equal(t, "知识库答案", answer)
			require.Equal(t, sessionID, authoritative)
			require.True(t, observedFeature)
		})
	}
}
