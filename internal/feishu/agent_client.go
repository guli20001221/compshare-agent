package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/compshare-agent/internal/config"
)

const knowledgeOnlyFeature = "knowledge_only_v1"

type AgentError struct {
	Code    string
	Message string
}

func (e *AgentError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

type AgentClient struct {
	wsURL     string
	httpURL   string
	companyID uint32
	orgID     uint32
	projectID string
	userEmail string
	http      *http.Client
}

func NewAgentClient(cfg config.FeishuConfig) (*AgentClient, error) {
	wsURL, err := url.Parse(strings.TrimSpace(cfg.AgentWSURL))
	if err != nil {
		return nil, fmt.Errorf("parse agent_ws_url: %w", err)
	}
	if wsURL.Scheme != "ws" && wsURL.Scheme != "wss" {
		return nil, fmt.Errorf("agent_ws_url must use ws or wss")
	}
	if wsURL.Query().Get("Action") == "" {
		query := wsURL.Query()
		query.Set("Action", "CreateCSAgentWS")
		wsURL.RawQuery = query.Encode()
	}
	httpURL := *wsURL
	if httpURL.Scheme == "wss" {
		httpURL.Scheme = "https"
	} else {
		httpURL.Scheme = "http"
	}
	httpURL.RawQuery = ""
	httpURL.Fragment = ""
	return &AgentClient{
		wsURL: wsURL.String(), httpURL: httpURL.String(),
		companyID: cfg.CompanyID, orgID: cfg.OrganizationID,
		projectID: cfg.ProjectID, userEmail: cfg.UserEmail,
		http: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (c *AgentClient) CreateSession(ctx context.Context, title string) (string, error) {
	payload := map[string]any{
		"Action":              "CreateCSAgentSession",
		"top_organization_id": c.companyID,
		"organization_id":     c.orgID,
		"Title":               title,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.httpURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("create agent session: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var result struct {
		RetCode   int    `json:"RetCode"`
		Message   string `json:"Message"`
		SessionID string `json:"SessionId"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("decode create session response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || result.RetCode != 0 || result.SessionID == "" {
		return "", &AgentError{Code: strconv.Itoa(result.RetCode), Message: firstNonEmpty(result.Message, resp.Status)}
	}
	return result.SessionID, nil
}

func (c *AgentClient) Ask(ctx context.Context, sessionID, clientTurnID, question, imageDataURL string) (string, string, error) {
	headers := http.Header{}
	headers.Set("X-Company-Id", strconv.FormatUint(uint64(c.companyID), 10))
	headers.Set("X-Organization-Id", strconv.FormatUint(uint64(c.orgID), 10))
	headers.Set("X-Request-Id", clientTurnID)
	if c.userEmail != "" {
		headers.Set("X-User-Email", c.userEmail)
	}
	conn, _, err := websocket.Dial(ctx, c.wsURL, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		return "", sessionID, fmt.Errorf("connect agent websocket: %w", err)
	}
	defer conn.CloseNow()

	frame := map[string]any{
		"Action":          "SendCSAgentChat",
		"ProtocolVersion": 2,
		"SessionId":       sessionID,
		"ClientTurnId":    clientTurnID,
		"request_uuid":    clientTurnID,
		"Message":         question,
		"Features":        []string{knowledgeOnlyFeature},
	}
	if c.projectID != "" {
		frame["ProjectId"] = c.projectID
	}
	if imageDataURL != "" {
		frame["Image"] = imageDataURL
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return "", sessionID, err
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		return "", sessionID, fmt.Errorf("send agent question: %w", err)
	}

	for {
		_, raw, err = conn.Read(ctx)
		if err != nil {
			return "", sessionID, fmt.Errorf("read agent reply: %w", err)
		}
		var event struct {
			Event     string `json:"event"`
			SessionID string `json:"SessionId"`
			Content   string `json:"Content"`
			Code      string `json:"Code"`
			Message   string `json:"Message"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			return "", sessionID, fmt.Errorf("decode agent event: %w", err)
		}
		if event.SessionID != "" {
			sessionID = event.SessionID
		}
		switch event.Event {
		case "done":
			if strings.TrimSpace(event.Content) == "" {
				return "", sessionID, &AgentError{Code: "EmptyReply", Message: "Agent returned an empty answer"}
			}
			return event.Content, sessionID, nil
		case "error", "aborted":
			return "", sessionID, &AgentError{Code: event.Code, Message: firstNonEmpty(event.Message, "Agent request failed")}
		case "confirmation", "selection":
			return "", sessionID, &AgentError{Code: "KnowledgeBoundaryViolation", Message: "public Q&A requested an interactive operation"}
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
