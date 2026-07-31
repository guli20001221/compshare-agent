package feishu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkchannel "github.com/larksuite/oapi-sdk-go/v3/channel"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/compshare-agent/internal/config"
	"github.com/google/uuid"
)

type job struct {
	messageID string
	chatID    string
	topicKey  string
	question  string
	imageKeys []string
}

type Service struct {
	cfg       config.FeishuConfig
	agent     *AgentClient
	api       *lark.Client
	botOpenID string
	botUserID string
	allowed   map[string]struct{}
	queue     chan job

	sessionsMu sync.Mutex
	sessions   map[string]string
	topicLocks sync.Map

	seenMu sync.Mutex
	seen   map[string]time.Time
}

func NewService(ctx context.Context, cfg config.FeishuConfig) (*Service, error) {
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	agent, err := NewAgentClient(cfg)
	if err != nil {
		return nil, err
	}
	api := lark.NewClient(cfg.AppID, cfg.AppSecret)
	bot := larkchannel.NewChannel(api, nil).GetBotIdentity(ctx)
	if bot == nil || strings.TrimSpace(bot.OpenID) == "" {
		return nil, fmt.Errorf("cannot resolve Feishu bot identity; check app_id, app_secret and bot capability")
	}
	allowed := make(map[string]struct{}, len(cfg.AllowedChatIDs))
	for _, chatID := range cfg.AllowedChatIDs {
		allowed[strings.TrimSpace(chatID)] = struct{}{}
	}
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}
	return &Service{
		cfg: cfg, agent: agent, api: api, botOpenID: bot.OpenID, botUserID: bot.UserID, allowed: allowed,
		queue:    make(chan job, maxConcurrent*16),
		sessions: make(map[string]string), seen: make(map[string]time.Time),
	}, nil
}

func ValidateConfig(cfg config.FeishuConfig) error {
	switch {
	case strings.TrimSpace(cfg.AppID) == "":
		return errors.New("agent.feishu.app_id is required")
	case strings.TrimSpace(cfg.AppSecret) == "":
		return errors.New("agent.feishu.app_secret is required")
	case strings.TrimSpace(cfg.AgentWSURL) == "":
		return errors.New("agent.feishu.agent_ws_url is required")
	case cfg.CompanyID == 0:
		return errors.New("agent.feishu.company_id is required")
	case cfg.OrganizationID == 0:
		return errors.New("agent.feishu.organization_id is required")
	case len(cfg.AllowedChatIDs) == 0:
		return errors.New("agent.feishu.allowed_chat_ids is required; use [\"*\"] only for initial testing")
	}
	for _, chatID := range cfg.AllowedChatIDs {
		if strings.TrimSpace(chatID) == "" {
			return errors.New("agent.feishu.allowed_chat_ids cannot contain an empty value")
		}
	}
	return nil
}

func (s *Service) Run(ctx context.Context) error {
	workers := s.cfg.MaxConcurrent
	if workers <= 0 {
		workers = 4
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.worker(ctx)
		}()
	}
	defer wg.Wait()

	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(s.onMessage)
	client := larkws.NewClient(
		s.cfg.AppID,
		s.cfg.AppSecret,
		larkws.WithEventHandler(eventHandler),
	)
	log.Printf("Feishu topic bot connected: allowlist=%d, workers=%d, knowledge_only=true", len(s.allowed), workers)
	started := make(chan error, 1)
	go func() {
		started <- client.Start(ctx)
	}()
	select {
	case err := <-started:
		return err
	case <-ctx.Done():
		return nil
	}
}

func (s *Service) onMessage(_ context.Context, event *larkim.P2MessageReceiveV1) error {
	if event == nil || event.Event == nil || event.Event.Message == nil || event.Event.Sender == nil {
		return nil
	}
	message := event.Event.Message
	sender := event.Event.Sender
	chatType := stringValue(message.ChatType)
	if stringValue(sender.SenderType) != "user" || (chatType != "group" && chatType != "topic_group") {
		return nil
	}
	chatID := stringValue(message.ChatId)
	if !s.chatAllowed(chatID) {
		return nil
	}
	mentioned := mentionedBot(message.Mentions, s.botOpenID, s.botUserID)
	if !shouldRespond(message, mentioned, s.cfg.AutoReplyNewTopics) {
		return nil
	}
	input, ok := inputFromMessage(message)
	if !ok {
		return nil
	}
	messageID := stringValue(message.MessageId)
	if messageID == "" || s.seenBefore(messageID) {
		return nil
	}
	topicID := firstNonEmpty(
		stringValue(message.ThreadId),
		stringValue(message.RootId),
		stringValue(message.ParentId),
		messageID,
	)
	log.Printf("Feishu message accepted: message=%s chat=%s topic=%s", messageID, chatID, topicID)
	select {
	case s.queue <- job{
		messageID: messageID,
		chatID:    chatID,
		topicKey:  chatID + ":" + topicID,
		question:  input.Question,
		imageKeys: input.ImageKeys,
	}:
	default:
		log.Printf("warning: Feishu queue full; dropped message=%s chat=%s", messageID, chatID)
	}
	return nil
}

func shouldRespond(message *larkim.EventMessage, mentioned, autoReplyNewTopics bool) bool {
	return mentioned || (autoReplyNewTopics && isNewTopicRoot(message))
}

func (s *Service) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-s.queue:
			s.handleJob(ctx, item)
		}
	}
}

func (s *Service) handleJob(ctx context.Context, item job) {
	lockValue, _ := s.topicLocks.LoadOrStore(item.topicKey, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	var imageDataURL string
	var err error
	if len(item.imageKeys) != 0 {
		imageDataURL, err = s.downloadImageDataURL(ctx, item.messageID, item.imageKeys[0])
		if len(item.imageKeys) > 1 {
			log.Printf("Feishu message=%s contains %d images; OCR uses the first image", item.messageID, len(item.imageKeys))
		}
	}
	var sessionID string
	if err == nil {
		sessionID, err = s.sessionForTopic(ctx, item.topicKey, item.question)
	}
	if err == nil {
		var authoritative string
		var answer string
		answer, authoritative, err = s.agent.Ask(ctx, sessionID, item.messageID, item.question, imageDataURL)
		if authoritative != "" && authoritative != sessionID {
			s.setSession(item.topicKey, authoritative)
		}
		if isSessionLimit(err) {
			s.deleteSession(item.topicKey)
			if sessionID, createErr := s.sessionForTopic(ctx, item.topicKey, item.question); createErr == nil {
				answer, authoritative, err = s.agent.Ask(ctx, sessionID, item.messageID+"-retry", item.question, imageDataURL)
				if authoritative != "" && authoritative != sessionID {
					s.setSession(item.topicKey, authoritative)
				}
			} else {
				err = createErr
			}
		}
		if err == nil {
			if replyErr := s.reply(ctx, item.messageID, answer); replyErr != nil {
				log.Printf("warning: Feishu reply failed message=%s: %v", item.messageID, replyErr)
			}
			return
		}
	}
	log.Printf("warning: Feishu Agent request failed message=%s: %v", item.messageID, err)
	_ = s.reply(ctx, item.messageID, "抱歉，这次没有成功拿到答案，请稍后再试。")
}

func (s *Service) downloadImageDataURL(ctx context.Context, messageID, imageKey string) (string, error) {
	req := larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).
		FileKey(imageKey).
		Type("image").
		Build()
	resp, err := s.api.Im.V1.MessageResource.Get(ctx, req)
	if err != nil {
		return "", fmt.Errorf("download Feishu image: %w", err)
	}
	if resp == nil || resp.File == nil {
		return "", fmt.Errorf("download Feishu image: empty response")
	}
	maxBytes := s.cfg.MaxImageBytes
	if maxBytes <= 0 {
		maxBytes = 5 << 20
	}
	data, err := io.ReadAll(io.LimitReader(resp.File, int64(maxBytes)+1))
	if err != nil {
		return "", fmt.Errorf("read Feishu image: %w", err)
	}
	return encodeImageDataURL(data, maxBytes)
}

func encodeImageDataURL(data []byte, maxBytes int) (string, error) {
	if len(data) > maxBytes {
		return "", fmt.Errorf("Feishu image exceeds %d bytes", maxBytes)
	}
	mimeType := http.DetectContentType(data)
	switch mimeType {
	case "image/jpeg", "image/png", "image/webp":
	default:
		return "", fmt.Errorf("unsupported Feishu image format %s", mimeType)
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func (s *Service) sessionForTopic(ctx context.Context, topicKey, question string) (string, error) {
	s.sessionsMu.Lock()
	sessionID := s.sessions[topicKey]
	s.sessionsMu.Unlock()
	if sessionID != "" {
		return sessionID, nil
	}
	title := []rune(strings.TrimSpace(question))
	if len(title) > 60 {
		title = title[:60]
	}
	sessionID, err := s.agent.CreateSession(ctx, string(title))
	if err != nil {
		return "", err
	}
	s.setSession(topicKey, sessionID)
	return sessionID, nil
}

func (s *Service) setSession(topicKey, sessionID string) {
	s.sessionsMu.Lock()
	s.sessions[topicKey] = sessionID
	s.sessionsMu.Unlock()
}

func (s *Service) deleteSession(topicKey string) {
	s.sessionsMu.Lock()
	delete(s.sessions, topicKey)
	s.sessionsMu.Unlock()
}

func (s *Service) reply(ctx context.Context, messageID, answer string) error {
	parts := splitReply(answer, s.cfg.MaxReplyRunes)
	for i, part := range parts {
		content, err := json.Marshal(map[string]string{"text": part})
		if err != nil {
			return err
		}
		idempotencyID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(messageID+":"+itoa(i))).String()
		req := larkim.NewReplyMessageReqBuilder().
			MessageId(messageID).
			Body(larkim.NewReplyMessageReqBodyBuilder().
				Content(string(content)).
				MsgType("text").
				ReplyInThread(true).
				Uuid(idempotencyID).
				Build()).
			Build()
		resp, err := s.api.Im.V1.Message.Reply(ctx, req)
		if err != nil {
			return err
		}
		if !resp.Success() {
			return fmt.Errorf("Feishu API code=%d message=%s request_id=%s",
				resp.Code, resp.Msg, resp.RequestId())
		}
	}
	return nil
}

func (s *Service) chatAllowed(chatID string) bool {
	if _, ok := s.allowed["*"]; ok {
		return true
	}
	_, ok := s.allowed[chatID]
	return ok
}

func (s *Service) seenBefore(messageID string) bool {
	now := time.Now()
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	if seenAt, ok := s.seen[messageID]; ok && now.Sub(seenAt) < time.Hour {
		return true
	}
	s.seen[messageID] = now
	if len(s.seen) > 10000 {
		for key, seenAt := range s.seen {
			if now.Sub(seenAt) >= time.Hour {
				delete(s.seen, key)
			}
		}
	}
	return false
}

func mentionedBot(mentions []*larkim.MentionEvent, botOpenID, botUserID string) bool {
	for _, mention := range mentions {
		if mention == nil || mention.Id == nil {
			continue
		}
		if stringValue(mention.Id.OpenId) == botOpenID ||
			stringValue(mention.Id.UserId) == botOpenID ||
			(botUserID != "" && stringValue(mention.Id.UserId) == botUserID) {
			return true
		}
	}
	return false
}

func isSessionLimit(err error) bool {
	var agentErr *AgentError
	return errors.As(err, &agentErr) && agentErr.Code == "SessionTurnLimitExceeded"
}
