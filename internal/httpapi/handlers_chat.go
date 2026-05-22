package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/bitly/go-simplejson"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/httpapi/sse"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// metaEvent is the first SSE frame emitted after SSE headers are sent.
type metaEvent struct {
	RequestID string `json:"RequestId"`
	SessionID string `json:"SessionId"`
	MessageID string `json:"MessageId"`
}

// tokenEvent carries a single text delta from the LLM.
type tokenEvent struct {
	Text string `json:"Text"`
}

// doneEvent is the final SSE frame on a successful completion.
type doneEvent struct {
	Usage     usageEvent `json:"Usage"`
	LatencyMs int        `json:"LatencyMs"`
	TtftMs    int        `json:"TtftMs"`
}

// usageEvent carries token counts inside doneEvent.
type usageEvent struct {
	InputTokens  int `json:"InputTokens"`
	OutputTokens int `json:"OutputTokens"`
}

// streamErrorEvent is the SSE error frame emitted when the LLM call fails
// after SSE has already started (so writeError can no longer be used).
type streamErrorEvent struct {
	Code    string `json:"Code"`
	Message string `json:"Message"`
}

// handleChat is the Chat SSE handler. It:
//  1. Validates inputs and session ownership (pre-SSE, errors go via writeError).
//  2. Persists user + assistant-placeholder rows.
//  3. Opens the SSE stream and emits event:meta.
//  4. Acquires an engine from the pool (serialized via Lease) and calls ChatWithOptions.
//  5. On completion, updates the assistant row and emits event:done or event:error.
func (h *Handlers) handleChat(c *gin.Context, base BaseRequest, raw *simplejson.Json) {
	// -----------------------------------------------------------------------
	// 1. Input validation
	// -----------------------------------------------------------------------
	sessionID := raw.Get("SessionId").MustString()
	if sessionID == "" {
		h.writeError(c, base.RequestUUID, ErrInvalidParam.WithMessage("missing SessionId"))
		return
	}

	message := strings.TrimSpace(raw.Get("Message").MustString())
	if message == "" {
		h.writeError(c, base.RequestUUID, ErrInvalidParam.WithMessage("missing Message"))
		return
	}
	if len([]rune(message)) > h.cfg.Agent.HTTP.MaxInputLength {
		h.writeError(c, base.RequestUUID, ErrInvalidParam.WithMessage("Message exceeds MaxInputLength"))
		return
	}

	sess, err := h.sessions.GetByID(c.Request.Context(), base.Owner, sessionID)
	if err != nil {
		h.writeError(c, base.RequestUUID, err)
		return
	}

	// Enforce per-session turn cap. Each completed Chat call persists exactly
	// two rows (user + assistant), so message_count == max_session_turns * 2
	// means the cap has been reached. Aborted / errored turns still count —
	// resource-wise they consumed a slot.
	maxTurns := h.cfg.Agent.HTTP.MaxSessionTurns
	if maxTurns <= 0 {
		maxTurns = config.DefaultMaxSessionTurns
	}
	if sess.MessageCount >= maxTurns*2 {
		h.writeError(c, base.RequestUUID, ErrSessionTurnLimit)
		return
	}

	// -----------------------------------------------------------------------
	// 2. Pre-stream persistence
	// -----------------------------------------------------------------------
	userMsgID := uuid.NewString()
	assistantMsgID := uuid.NewString()
	model := h.cfg.Agent.LLM.Model
	reqUUID := base.RequestUUID

	// TODO(phase2): wrap user/assistant Append + BumpUpdatedAtAndIncCount in a transaction.
	if err := h.messages.Append(c.Request.Context(), store.Message{
		ID:          userMsgID,
		SessionID:   sessionID,
		RequestUUID: &reqUUID,
		Role:        "user",
		Content:     message,
		Status:      "ok",
	}); err != nil {
		h.writeError(c, base.RequestUUID, err)
		return
	}

	if err := h.messages.Append(c.Request.Context(), store.Message{
		ID:          assistantMsgID,
		SessionID:   sessionID,
		RequestUUID: &reqUUID,
		Role:        "assistant",
		Content:     "",
		Status:      "pending",
		Model:       &model,
	}); err != nil {
		h.writeError(c, base.RequestUUID, err)
		return
	}

	if err := h.sessions.BumpUpdatedAtAndIncCount(c.Request.Context(), base.Owner, sessionID, 2); err != nil {
		h.writeError(c, base.RequestUUID, err)
		return
	}

	// -----------------------------------------------------------------------
	// 3. Open SSE response
	// -----------------------------------------------------------------------
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	sw := sse.New(c.Writer)
	_ = sw.WriteEvent("meta", metaEvent{
		RequestID: base.RequestUUID,
		SessionID: sessionID,
		MessageID: assistantMsgID,
	})

	// -----------------------------------------------------------------------
	// 4. Acquire engine (serialized per session via Lease)
	// -----------------------------------------------------------------------
	if h.pool == nil {
		writeStreamError(sw, h.messages, base.Owner, assistantMsgID,
			ErrInternal.WithMessage("%s", "engine pool not configured"))
		return
	}

	// Build and inject UserContext so downstream tools can perform STS calls
	// with the correct tenant identity.
	userCtx, ucErr := h.buildUserContext(base)
	if ucErr != nil {
		writeStreamError(sw, h.messages, base.Owner, assistantMsgID, AsAPIError(ucErr))
		return
	}
	ctx := tools.WithUser(c.Request.Context(), userCtx)

	agent, release, err := h.pool.Lease(ctx, base.Owner, sessionID)
	if err != nil {
		writeStreamError(sw, h.messages, base.Owner, assistantMsgID, AsAPIError(err))
		return
	}
	defer release()

	// -----------------------------------------------------------------------
	// 5. Keepalive goroutine
	// -----------------------------------------------------------------------
	start := time.Now()
	var firstToken time.Time
	var usage llm.TokenUsage

	done := make(chan struct{})
	ticker := time.NewTicker(h.cfg.Agent.HTTP.SSEKeepaliveInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = sw.WriteKeepalive()
			case <-c.Request.Context().Done():
				return
			case <-done:
				return
			}
		}
	}()

	// -----------------------------------------------------------------------
	// 5. LLM streaming call
	// -----------------------------------------------------------------------
	reply, chatErr := agent.ChatWithOptions(ctx, message, func(engine.StepEvent) {}, engine.ChatOptions{
		OnTextDelta: func(s string) {
			if firstToken.IsZero() {
				firstToken = time.Now()
			}
			_ = sw.WriteEvent("token", tokenEvent{Text: s})
		},
		OnUsage: func(u llm.TokenUsage) { usage = u },
	})

	// Signal keepalive goroutine to exit.
	close(done)

	// -----------------------------------------------------------------------
	// 6. Post-stream branching
	// -----------------------------------------------------------------------
	latencyMs := int(time.Since(start).Milliseconds())
	ttftMs := latencyMs
	if !firstToken.IsZero() {
		ttftMs = int(firstToken.Sub(start).Milliseconds())
	}

	// Client disconnected.
	if errors.Is(chatErr, context.Canceled) || errors.Is(c.Request.Context().Err(), context.Canceled) {
		_ = h.messages.UpdateAssistant(context.Background(), base.Owner, assistantMsgID,
			store.AssistantPatch{Status: "aborted"})
		return
	}

	// LLM error.
	if chatErr != nil {
		apiErr := classifyChatError(chatErr)
		code := apiErr.Code
		_ = h.messages.UpdateAssistant(context.Background(), base.Owner, assistantMsgID,
			store.AssistantPatch{
				Status:    "error",
				ErrorCode: &code,
				LatencyMs: &latencyMs,
				TTFTMs:    &ttftMs,
			})
		_ = sw.WriteEvent("error", streamErrorEvent{Code: apiErr.Code, Message: apiErr.Message})
		return
	}

	// Success.
	inputTokens := usage.PromptTokens
	outputTokens := usage.CompletionTokens
	_ = h.messages.UpdateAssistant(context.Background(), base.Owner, assistantMsgID,
		store.AssistantPatch{
			Content:      reply,
			Status:       "ok",
			InputTokens:  &inputTokens,
			OutputTokens: &outputTokens,
			TTFTMs:       &ttftMs,
			LatencyMs:    &latencyMs,
		})
	_ = sw.WriteEvent("done", doneEvent{
		Usage:     usageEvent{InputTokens: inputTokens, OutputTokens: outputTokens},
		LatencyMs: latencyMs,
		TtftMs:    ttftMs,
	})
}

// writeStreamError updates the assistant message status to "error" and emits
// an event:error frame after SSE has already started.
func writeStreamError(sw *sse.Writer, messages store.MessageStore, owner store.Owner, msgID string, apiErr *APIError) {
	code := apiErr.Code
	_ = messages.UpdateAssistant(context.Background(), owner, msgID,
		store.AssistantPatch{Status: "error", ErrorCode: &code})
	_ = sw.WriteEvent("error", streamErrorEvent{Code: apiErr.Code, Message: apiErr.Message})
}

// classifyChatError maps LLM errors to API error codes.
func classifyChatError(err error) *APIError {
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrModelTimeout
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	classified := AsAPIError(err)
	if classified.Code == ErrInternal.Code {
		return ErrModelError.WithMessage("%s", err.Error())
	}
	return classified
}
