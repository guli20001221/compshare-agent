package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/bitly/go-simplejson"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/guardrails"
	"github.com/compshare-agent/internal/httpapi/sse"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/ocr"
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
// Content carries the final post-processed reply text (e.g. citation markers
// stripped). The frontend should prefer Content over accumulated token deltas
// when present, as the two may differ due to post-processing.
type doneEvent struct {
	Content   string     `json:"Content,omitempty"`
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

// stepEvent is the SSE projection of engine.StepEvent — only fields safe for
// the frontend are included. Args, Display, TraceResult, and cap info are
// intentionally omitted.
type stepEvent struct {
	Type    string `json:"Type"`
	Action  string `json:"Action,omitempty"`
	Message string `json:"Message,omitempty"`
	Index   int    `json:"Index"`
}

// confirmationEvent is the SSE frame that tells the frontend to show a
// confirmation dialog. The frontend sends ConfirmCSAgentAction with the
// ConfirmationId to resolve it.
type confirmationEvent struct {
	ConfirmationID string         `json:"ConfirmationId"`
	Action         string         `json:"Action"`
	Summary        map[string]any `json:"Summary,omitempty"`
	TimeoutSeconds int            `json:"TimeoutSeconds"`
}

const confirmTimeoutSeconds = 60

func stepTypeString(t engine.StepType) string {
	switch t {
	case engine.StepToolCall:
		return "tool_call"
	case engine.StepToolResult:
		return "tool_result"
	case engine.StepConfirmNeeded:
		return "confirm_needed"
	case engine.StepBlocked:
		return "blocked"
	case engine.StepError:
		return "error"
	default:
		return "unknown"
	}
}

// handleChat is the Chat SSE handler. It:
//  1. Validates inputs and session ownership (pre-SSE, errors go via writeError).
//  2. Acquires an engine from the pool before persisting this turn, so cold
//     rehydration only restores prior history.
//  3. Persists user + assistant-placeholder rows.
//  4. Opens the SSE stream and emits event:meta.
//  5. On completion, updates the assistant row and emits event:done or event:error.
func (h *Handlers) handleChat(c *gin.Context, base BaseRequest, raw *simplejson.Json) {
	// -----------------------------------------------------------------------
	// 1. Input validation
	// -----------------------------------------------------------------------
	sessionID := raw.Get("SessionId").MustString()
	if sessionID == "" {
		h.writeError(c, base.Action, base.RequestUUID, ErrInvalidParam.WithMessage("missing SessionId"))
		return
	}

	message := strings.TrimSpace(raw.Get("Message").MustString())
	if message == "" {
		h.writeError(c, base.Action, base.RequestUUID, ErrInvalidParam.WithMessage("missing Message"))
		return
	}
	if len([]rune(message)) > h.cfg.Agent.HTTP.MaxInputLength {
		h.writeError(c, base.Action, base.RequestUUID, ErrInvalidParam.WithMessage("Message exceeds MaxInputLength"))
		return
	}

	sess, err := h.sessions.GetByID(c.Request.Context(), base.Owner, sessionID)
	if err != nil {
		h.writeError(c, base.Action, base.RequestUUID, err)
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
		h.writeError(c, base.Action, base.RequestUUID, ErrSessionTurnLimit)
		return
	}

	// -----------------------------------------------------------------------
	// 1.5 OCR image context extraction
	// -----------------------------------------------------------------------
	var ocrText string
	imageDataURL := strings.TrimSpace(raw.Get("Image").MustString())
	if imageDataURL != "" && h.ocrClient != nil {
		text, valErr := h.processOCR(c.Request.Context(), base.RequestUUID, imageDataURL)
		if valErr != nil {
			h.writeError(c, base.Action, base.RequestUUID, ErrInvalidParam.WithMessage("invalid Image: %v", valErr))
			return
		}
		ocrText = text
	} else if imageDataURL != "" {
		log.Printf("warning: Image provided but OCR not configured (request %s)", base.RequestUUID)
	}

	// -----------------------------------------------------------------------
	// 2. Acquire engine (serialized per session via Lease)
	// -----------------------------------------------------------------------
	if h.pool == nil {
		h.writeError(c, base.Action, base.RequestUUID, ErrInternal.WithMessage("%s", "engine pool not configured"))
		return
	}

	// Build and inject UserContext so downstream tools can perform STS calls
	// with the correct tenant identity.
	userCtx, ucErr := h.buildUserContext(base)
	if ucErr != nil {
		h.writeError(c, base.Action, base.RequestUUID, AsAPIError(ucErr))
		return
	}
	ctx := tools.WithUser(c.Request.Context(), userCtx)

	agent, release, err := h.pool.Lease(ctx, base.Owner, sessionID)
	if err != nil {
		h.writeError(c, base.Action, base.RequestUUID, AsAPIError(err))
		return
	}
	defer release()

	// Hydrate SessionState from the envelope persisted in sessions.context.
	// Order matters:
	//   (1) ClearSessionState wipes whatever the cached Engine carried from
	//       a prior turn — agentpool reuses *engine.Engine across turns, so
	//       without this clear a parse failure below would leave the prior
	//       turn's hydrated=true sticky and §6.2 would persist stale state
	//       on top of the broken row.
	//   (2) ParsePersistedContext returns 3 outcomes: success (hydrate +
	//       persist on done), malformed JSON (log + skip persist), unknown
	//       schema version (log + skip persist — defends forward rollout
	//       where a v2 binary's envelope must NOT be downgraded by an
	//       older binary).
	// sessionStatePersistable is the single boolean the success branch
	// checks before calling UpdateContext. Both error paths set it to
	// false; only a successful parse + SetSessionState sets it to true.
	agent.ClearSessionState()
	var clientCtxPreserve json.RawMessage
	sessionStatePersistable := false
	pc, parseErr := engine.ParsePersistedContext(sess.Context)
	switch {
	case parseErr == nil:
		clientCtxPreserve = pc.ClientContext
		agent.SetSessionState(pc.AgentSessionState, sess.ContextVersion)
		sessionStatePersistable = true
	case errors.Is(parseErr, engine.ErrUnknownSessionStateSchema):
		log.Printf("warning: session %s has unknown SessionState schema_version (will skip persist, leaving row untouched for newer binary): %v",
			sessionID, parseErr)
	default:
		log.Printf("warning: session %s context parse failed (will skip persist): %v",
			sessionID, parseErr)
	}

	clearChatTraceObservers(agent)
	defer clearChatTraceObservers(agent)

	start := time.Now()
	turnIndex := sess.MessageCount/2 + 1
	traceRecorder := newChatTraceRecorder(h.traceWriter, base, sessionID, turnIndex, message, start)
	if traceRecorder != nil {
		traceRecorder.SetRegistryTraceSupplier(agent.RegistryTraceState)
		attachChatTraceObservers(agent, traceRecorder)
	}
	finishTrace := func(err error) {
		if traceRecorder == nil {
			return
		}
		if traceErr := traceRecorder.Finish(err, time.Now()); traceErr != nil {
			log.Printf("warning: HTTP trace write failed: %v", traceErr)
		}
		traceRecorder = nil
	}

	// -----------------------------------------------------------------------
	// 3. Pre-stream persistence
	// -----------------------------------------------------------------------
	userMsgID := uuid.NewString()
	assistantMsgID := uuid.NewString()
	model := h.cfg.Agent.LLM.Model
	reqUUID := base.RequestUUID

	// Persist user message with OCR context included (so the DB record
	// shows what the engine saw). PII filter covers both OCR text and
	// the user's original message.
	persistContent := message
	if ocrText != "" {
		persistContent = "用户上传了一张截图，系统自动识别到以下内容：\n" + ocrText + "\n\n" + message
	}
	if err := h.messages.Append(c.Request.Context(), store.Message{
		ID:          userMsgID,
		SessionID:   sessionID,
		RequestUUID: &reqUUID,
		Role:        "user",
		Content:     guardrails.RedactPII(persistContent),
		Status:      "ok",
	}); err != nil {
		h.writeError(c, base.Action, base.RequestUUID, err)
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
		h.writeError(c, base.Action, base.RequestUUID, err)
		return
	}

	if err := h.sessions.BumpUpdatedAtAndIncCount(c.Request.Context(), base.Owner, sessionID, 2); err != nil {
		h.writeError(c, base.Action, base.RequestUUID, err)
		return
	}

	// -----------------------------------------------------------------------
	// 4. Open SSE response
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
	// 5. Keepalive goroutine
	// -----------------------------------------------------------------------
	var firstToken time.Time
	tokenEmitted := false
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
	stepIndex := 0
	reply, chatErr := agent.ChatWithOptions(ctx, message, func(ev engine.StepEvent) {
		if traceRecorder != nil {
			traceRecorder.OnStep(ev)
		}
		_ = sw.WriteEvent("step", stepEvent{
			Type:    stepTypeString(ev.Type),
			Action:  ev.Action,
			Message: guardrails.RedactOutputLeak(guardrails.RedactPII(ev.Message)),
			Index:   stepIndex,
		})
		stepIndex++
	}, engine.ChatOptions{
		ImageContext: ocrText,
		OnTextDelta: func(s string) {
			if firstToken.IsZero() {
				firstToken = time.Now()
			}
			tokenEmitted = true
			_ = sw.WriteEvent("token", tokenEvent{Text: s})
		},
		OnUsage: func(u llm.TokenUsage) { usage = u },
		ConfirmFunc: func(action string, args map[string]any) bool {
			if h.confirmBroker == nil {
				return false
			}
			confirmID, ch := h.confirmBroker.Register(sessionID, base.Owner)
			defer h.confirmBroker.Cancel(confirmID)
			if err := sw.WriteEvent("confirmation", confirmationEvent{
				ConfirmationID: confirmID,
				Action:         action,
				Summary:        sanitizeConfirmArgs(args),
				TimeoutSeconds: confirmTimeoutSeconds,
			}); err != nil {
				return false
			}
			return WaitForConfirmation(c.Request.Context(), ch, time.Duration(confirmTimeoutSeconds)*time.Second)
		},
	})

	// Signal keepalive goroutine to exit.
	close(done)

	// -----------------------------------------------------------------------
	// 6. Post-stream branching
	// -----------------------------------------------------------------------
	if chatErr == nil && !tokenEmitted && reply != "" {
		if firstToken.IsZero() {
			firstToken = time.Now()
		}
		tokenEmitted = true
		_ = sw.WriteEvent("token", tokenEvent{Text: reply})
	}

	latencyMs := int(time.Since(start).Milliseconds())
	ttftMs := latencyMs
	if !firstToken.IsZero() {
		ttftMs = int(firstToken.Sub(start).Milliseconds())
	}

	// Client disconnected.
	if errors.Is(chatErr, context.Canceled) || errors.Is(c.Request.Context().Err(), context.Canceled) {
		finishTrace(chatErr)
		_ = h.messages.UpdateAssistant(context.Background(), base.Owner, assistantMsgID,
			store.AssistantPatch{Status: "aborted"})
		return
	}

	// LLM error.
	if chatErr != nil {
		finishTrace(chatErr)
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
	finishTrace(nil)
	inputTokens := usage.PromptTokens
	outputTokens := usage.CompletionTokens
	_ = h.messages.UpdateAssistant(context.Background(), base.Owner, assistantMsgID,
		store.AssistantPatch{
			Content:      guardrails.RedactOutputLeak(reply),
			Status:       "ok",
			InputTokens:  &inputTokens,
			OutputTokens: &outputTokens,
			TTFTMs:       &ttftMs,
			LatencyMs:    &latencyMs,
		})
	_ = sw.WriteEvent("done", doneEvent{
		Content:   reply,
		Usage:     usageEvent{InputTokens: inputTokens, OutputTokens: outputTokens},
		LatencyMs: latencyMs,
		TtftMs:    ttftMs,
	})

	// Persist SessionState envelope AFTER the done frame so the client is
	// not blocked on a DB write. Guarded by sessionStatePersistable, which
	// is false on parse failure / unknown schema version (see Lease block
	// above). Persistence failures are warning-only — the assistant reply
	// is already delivered, the worst case is "previous instance" memory
	// loss on the next turn.
	if sessionStatePersistable {
		newState, expectedVer, hydrated := agent.SessionStateSnapshot()
		if hydrated {
			envelope := engine.PersistedContext{
				AgentSessionState: newState,
				ClientContext:     clientCtxPreserve,
			}
			if raw, mErr := json.Marshal(envelope); mErr == nil {
				persistCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
				_, upErr := h.sessions.UpdateContext(persistCtx, base.Owner, sessionID, raw, expectedVer)
				cancel()
				switch {
				case upErr == nil:
					// ok
				case errors.Is(upErr, store.ErrStaleWrite):
					log.Printf("warning: session %s stale context_version on persist (expected=%d)",
						sessionID, expectedVer)
				default:
					log.Printf("warning: session %s UpdateContext failed: %v", sessionID, upErr)
				}
			} else {
				log.Printf("warning: session %s marshal envelope failed: %v", sessionID, mErr)
			}
		}
	}
}

// sanitizeConfirmArgs projects workflow confirm args to a safe subset for the
// frontend confirmation dialog. Sensitive fields (passwords, tokens) are excluded.
func sanitizeConfirmArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	safe := make(map[string]any, len(args))
	for k, v := range args {
		switch k {
		case "Password", "password", "Token", "token", "SecurityToken":
			continue
		}
		safe[k] = v
	}
	return safe
}

const maxOCRTextRunes = 1200

// processOCR validates the image, calls the OCR client, and returns
// PII-filtered, length-capped text. Returns a validation error (caller
// should 400) or ("", nil) on API failure (graceful degradation).
func (h *Handlers) processOCR(ctx context.Context, requestUUID, imageDataURL string) (string, error) {
	if _, err := ocr.ValidateImageDataURL(imageDataURL, h.cfg.Agent.OCR.MaxBytes); err != nil {
		return "", err
	}
	ocrCtx, cancel := context.WithTimeout(ctx, h.cfg.Agent.OCR.Timeout)
	defer cancel()
	text, err := h.ocrClient.Recognize(ocrCtx, imageDataURL)
	if err != nil {
		log.Printf("warning: OCR failed for request %s: %v", requestUUID, err)
		return "", nil
	}
	text = guardrails.RedactPII(text)
	runes := []rune(text)
	if len(runes) > maxOCRTextRunes {
		text = string(runes[:maxOCRTextRunes])
	}
	return text, nil
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
