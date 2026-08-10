package feishu

import (
	"context"
	"encoding/base64"
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
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
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

var errExternalGroupImageResourceUnsupported = errors.New("Feishu does not support downloading message images in external groups")

type Service struct {
	cfg       config.FeishuConfig
	agent     *AgentClient
	api       *lark.Client
	botOpenID string
	botUserID string
	allowed   map[string]struct{}
	queue     chan job
	// externalImageUserToken is optional. The default tenant-token path remains
	// the only path for internal groups; this is used only after Feishu rejects
	// an external-group image resource request.
	externalImageUserToken userAccessTokenProvider

	sessionsMu sync.Mutex
	sessions   map[string]string
	topicLocks sync.Map

	seenMu sync.Mutex
	seen   map[string]time.Time
}

type ServiceOption func(*Service)

func WithExternalImageUserToken(provider userAccessTokenProvider) ServiceOption {
	return func(service *Service) {
		service.externalImageUserToken = provider
	}
}

func NewService(ctx context.Context, cfg config.FeishuConfig, options ...ServiceOption) (*Service, error) {
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
	service := &Service{
		cfg: cfg, agent: agent, api: api, botOpenID: bot.OpenID, botUserID: bot.UserID, allowed: allowed,
		queue:    make(chan job, maxConcurrent*16),
		sessions: make(map[string]string), seen: make(map[string]time.Time),
	}
	for _, option := range options {
		if option != nil {
			option(service)
		}
	}
	return service, nil
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
	if cfg.EnableConsoleHandoff {
		if _, err := validateConsoleAssistantURL(cfg.ConsoleAssistantURL); err != nil {
			return err
		}
		if _, err := validateClientDownloadURL(cfg.ClientDownloadURL); err != nil {
			return err
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
	log.Printf("Feishu topic bot connected: allowlist=%d, workers=%d, knowledge_only=true, auto_reply_new_topics=%t, console_handoff=%t, client_handoff=%t", len(s.allowed), workers, s.cfg.AutoReplyNewTopics, consoleHandoffEnabled(s.cfg), strings.TrimSpace(s.cfg.ClientDownloadURL) != "")
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
		if err != nil {
			log.Printf("warning: Feishu image read failed message=%s: %v", item.messageID, err)
			_ = s.reply(ctx, item.messageID, imageReadFailureReply(err))
			return
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
			answer, needsConsoleHandoff := consumeConsoleHandoffMarker(answer)
			if needsConsoleHandoff {
				log.Printf("Feishu console handoff requested by knowledge-only Agent message=%s", item.messageID)
				if consoleHandoffEnabled(s.cfg) {
					answer = appendConsoleHandoff(answer, s.cfg.ConsoleAssistantURL, s.cfg.ClientDownloadURL)
				} else if answer == "" {
					answer = "抱歉，这次没有成功拿到答案，请稍后再试。"
				}
			}
			if replyErr := s.reply(ctx, item.messageID, answer); replyErr != nil {
				log.Printf("warning: Feishu reply failed message=%s: %v", item.messageID, replyErr)
			}
			return
		}
		if isKnowledgeBoundaryViolation(err) && consoleHandoffEnabled(s.cfg) {
			log.Printf("Feishu console handoff requested at knowledge boundary message=%s", item.messageID)
			if replyErr := s.reply(ctx, item.messageID, appendConsoleHandoff("", s.cfg.ConsoleAssistantURL, s.cfg.ClientDownloadURL)); replyErr != nil {
				log.Printf("warning: Feishu console handoff reply failed message=%s: %v", item.messageID, replyErr)
			}
			return
		}
	}
	log.Printf("warning: Feishu Agent request failed message=%s: %v", item.messageID, err)
	_ = s.reply(ctx, item.messageID, "抱歉，这次没有成功拿到答案，请稍后再试。")
}

func (s *Service) downloadImageDataURL(ctx context.Context, messageID, imageKey string) (string, error) {
	data, err := s.downloadImageBytes(ctx, messageID, imageKey)
	if errors.Is(err, errExternalGroupImageResourceUnsupported) && s.externalImageUserToken != nil {
		userAccessToken, tokenErr := s.externalImageUserToken.AccessToken(ctx)
		if tokenErr != nil {
			return "", fmt.Errorf("%w: %v", errExternalImageUserAuthorizationUnavailable, tokenErr)
		}
		data, err = s.downloadImageBytes(ctx, messageID, imageKey, larkcore.WithUserAccessToken(userAccessToken))
	}
	if err != nil {
		return "", err
	}
	maxBytes := s.cfg.MaxImageBytes
	if maxBytes <= 0 {
		maxBytes = 5 << 20
	}
	return encodeImageDataURL(data, maxBytes)
}

func (s *Service) downloadImageBytes(ctx context.Context, messageID, imageKey string, options ...larkcore.RequestOptionFunc) ([]byte, error) {
	req := larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).
		FileKey(imageKey).
		Type("image").
		Build()
	resp, err := s.api.Im.V1.MessageResource.Get(ctx, req, options...)
	if err != nil {
		return nil, fmt.Errorf("download Feishu image: %w", err)
	}
	if resp == nil {
		return nil, fmt.Errorf("download Feishu image: empty response")
	}
	if !resp.Success() {
		if resp.Code == 234009 {
			return nil, fmt.Errorf("download Feishu image: %w", errExternalGroupImageResourceUnsupported)
		}
		return nil, fmt.Errorf("download Feishu image: code=%d message=%s request_id=%s", resp.Code, resp.Msg, resp.RequestId())
	}
	if resp.File == nil {
		return nil, fmt.Errorf("download Feishu image: empty file")
	}
	maxBytes := s.cfg.MaxImageBytes
	if maxBytes <= 0 {
		maxBytes = 5 << 20
	}
	data, err := io.ReadAll(io.LimitReader(resp.File, int64(maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("read Feishu image: %w", err)
	}
	return data, nil
}

func imageReadFailureReply(err error) string {
	if errors.Is(err, errExternalImageUserAuthorizationUnavailable) {
		return "外部群截图读取尚未完成授权。请让目标群内的一名本企业成员在本机执行机器人截图授权；完成后可直接在话题中上传截图提问。"
	}
	if errors.Is(err, errExternalGroupImageResourceUnsupported) {
		return "飞书目前不允许机器人读取外部群消息中的原始图片，因此暂时无法识别这张截图。请将关键报错文字粘贴到话题中；如需截图问答，请在内部群中使用机器人。"
	}
	return "抱歉，这张截图没有读取成功。请重试，或将关键报错文字复制到消息中。"
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
		content, err := markdownPostContent(part)
		if err != nil {
			return err
		}
		idempotencyID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(messageID+":"+itoa(i))).String()
		req := larkim.NewReplyMessageReqBuilder().
			MessageId(messageID).
			Body(larkim.NewReplyMessageReqBodyBuilder().
				Content(content).
				MsgType("post").
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
