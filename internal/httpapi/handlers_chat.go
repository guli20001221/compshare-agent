package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/compshare-agent/internal/agentprotocol"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/guardrails"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/ocr"
	"github.com/compshare-agent/internal/security"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
	"github.com/google/uuid"
)

// metaEvent is the first frame emitted when a chat turn starts streaming.
type metaEvent struct {
	RequestID string `json:"RequestId"`
	SessionID string `json:"SessionId"`
	MessageID string `json:"MessageId"`
}

// tokenEvent carries a single text delta from the LLM.
type tokenEvent struct {
	Text string `json:"Text"`
}

// doneEvent is the final frame on a successful completion.
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

// streamErrorEvent is the error frame emitted when the LLM call fails after
// streaming has started (so a normal JSON error response is no longer possible).
type streamErrorEvent struct {
	Code    string `json:"Code"`
	Message string `json:"Message"`
}

// stepEvent is the streamed projection of engine.StepEvent — only fields safe
// for the frontend are included. Args, Display, TraceResult, and cap info are
// intentionally omitted.
//
// Label is the display name for Action (step_label.go). It is additive and
// omitted when the action has no label, so a client that ignores it sees the
// exact frame it saw before.
type stepEvent struct {
	Type    string `json:"Type"`
	Action  string `json:"Action,omitempty"`
	Label   string `json:"Label,omitempty"`
	Message string `json:"Message,omitempty"`
	Index   int    `json:"Index"`
}

// confirmationEvent is the frame that tells the frontend to show a confirmation
// dialog. The frontend sends ConfirmCSAgentAction with the ConfirmationId to
// resolve it (over the same WebSocket).
//
// Form is the optional editable selection form (create-flow 表单化). It is
// emitted ONLY when COMPSHARE_CONFIRM_FORM is on AND the client opted in via
// SendCSAgentChat Features ("confirm_form_v1") — absent otherwise, keeping the
// frame byte-identical for legacy clients. A client that received a Form may
// attach select-only Overrides to its ConfirmCSAgentAction.
// Label is the card's title when the server is the only party that can know it
// (serverOwnedConfirmLabel). Absent for every workflow, whose title the console
// already has right — so those frames stay byte-identical. A client that does not
// read Label is unaffected; one that does must prefer it over its own map.
type confirmationEvent struct {
	ConfirmationID string                `json:"ConfirmationId"`
	Action         string                `json:"Action"`
	Summary        map[string]any        `json:"Summary,omitempty"`
	TimeoutSeconds int                   `json:"TimeoutSeconds"`
	Label          string                `json:"Label,omitempty"`
	Form           *workflow.ConfirmForm `json:"Form,omitempty"`
}

// confirmationAckEvent is the POSITIVE outcome of a ConfirmCSAgentAction frame,
// carrying back the id it resolved so a client can tell WHICH card it applies to.
//
// It exists because the WS transport had no success reply at all: only failures
// wrote a frame, so a client's only honest option was to treat "the bytes left
// the socket" as "the server accepted it". The console did exactly that and
// marked cards 已处理 on ws.send(), which is how an expired card could sit next
// to a [NotFound] error saying the opposite. The POST transport has always
// returned this (confirmResponse); this is the same acknowledgement on the
// socket the console actually uses.
//
// Additive: a client that ignores unknown events is unaffected, and no existing
// frame changes shape.
type confirmationAckEvent struct {
	ConfirmationID string `json:"ConfirmationId"`
	Accepted       bool   `json:"Accepted"`
}

// confirmationErrorEvent is streamErrorEvent plus the card the failure belongs to.
type confirmationErrorEvent struct {
	Code           string `json:"Code"`
	Message        string `json:"Message"`
	ConfirmationID string `json:"ConfirmationId"`
}

// confirmationErrorEventName is deliberately NOT "error". A card-scoped failure is
// not a failure of the turn — whatever the card was gating is still running behind
// it — and the durable path has always said so with a name of its own
// ("interaction_error", ws_durable.go::writeDurableInteractionError, whose comment
// reads "without ending the observed turn"). The legacy path, the one production
// runs, kept "error".
//
// That cost a customer a 30-minute diagnosis on 2026-08-17. They clicked 确认 on an
// SSH authorization card that had already timed out; the server answered "error" /
// NotFound; the console does settled = true plus ws.close() on ANY "error" frame,
// which cancelled the still-running in-instance run; and the raw English sentence
// "confirmation not found or already resolved" was left standing as the answer.
//
// #548 added ConfirmationId to the frame so a client COULD scope it, and kept the
// event name for compatibility. The compatibility was the bug: a client that does
// not know about the id cannot ignore the frame, it can only fail the turn. A
// distinct name inverts the default — an unknown event is dropped by every client
// we have (the console's switch has a default that ignores unknown frames; wsprobe
// prints them) — so an old client now survives the late click instead of being
// killed by it, and a new client can put the message on the card it names.
const confirmationErrorEventName = "confirmation_error"

// confirmationFailureMessage is what a PERSON reads when their click on an
// authorization card did not take effect. The sentinels' own Error() strings are
// developer English and were being forwarded to the console verbatim.
//
// Every branch states the same two facts, because they are what the reader needs:
// the click did not take effect, and nothing was executed because of it.
func confirmationFailureMessage(err error) string {
	switch {
	case errors.Is(err, ErrConfirmationOwner):
		return "这张授权卡片不属于当前会话，本次点击没有生效。"
	case errors.Is(err, ErrConfirmationNotFound):
		return "这张授权卡片已经失效（等待确认超时，或已经处理过一次），本次点击没有生效，也没有执行任何操作。需要的话请重新发送一次指令。"
	default:
		// Override validation: the card is still pending and still answerable, so
		// the sentence must say that rather than read as a dead end.
		return "卡片上的选项没有通过校验，本次没有提交；请调整后再点确认。"
	}
}

// confirmWaitTimeout is how long the server waits for a person to answer ONE
// authorization card. It is a single number on purpose: a plain y/N card and a
// form-bearing card are both "a human is reading something before authorizing
// it", and the split (60s plain / 120s form) had no reader-side justification —
// it only meant the cards that authorize writing to a customer's box got the
// SHORTER window.
//
// 60 -> 120 on 2026-08-12, from production traces rather than taste: over 30
// days, 36 of 292 cards (12.3%) ended at exactly 60000ms, while real answers
// landed at 59832ms and 59760ms — 168ms and 240ms before the wall — and one
// user actively DECLINED at 59292ms. A distribution that is still dense right
// up against a boundary is being clipped by it, not abandoned before it. The
// per-write cards inside the in-instance lane were worst hit (24 of 140 =
// 17.1%), which is the wrong place to lose a turn: the box has usually already
// been changed by then.
//
// Raising this cost the transport nothing only because wsInteractionAllowance
// now buys the socket its own room for human time; the two must move together.
const confirmWaitTimeout = 120 * time.Second

// confirmTimeoutSeconds is the same budget as it appears on the wire, so the
// countdown the user watches and the deadline the server enforces cannot drift.
const confirmTimeoutSeconds = int(confirmWaitTimeout / time.Second)

// featureConfirmForm is the SendCSAgentChat Features value a client sends to
// opt in to form-bearing confirmations (and Overrides on resolve).
const featureConfirmForm = "confirm_form_v1"

// featureGuidedCreate opts an eligible client into the guided GPU-create order
// flow. The backend only honors it when COMPSHARE_CONFIRM_FORM and
// COMPSHARE_GUIDED_CREATE are both on.
const featureGuidedCreate = "guided_create_v1"

// featureKnowledgeOnly is an authorization-reducing client feature for
// untrusted public chat adapters. It can only remove capabilities.
const featureKnowledgeOnly = "knowledge_only_v1"

// featureFeishuPublicPlatformReadOnly is an authorization-reducing public
// channel feature. It exposes a small, fixed subset of public platform reads;
// it never restores account, instance, diagnostic, or mutating capabilities.
const featureFeishuPublicPlatformReadOnly = agentprotocol.FeatureFeishuPublicPlatformReadOnly

// featureFeishuConsoleHandoff is meaningful only to the Feishu adapter: it
// allows the model to mark that a public-channel reply needs an authenticated
// console diagnosis. It does not enable any extra engine capability.
const featureFeishuConsoleHandoff = agentprotocol.FeatureFeishuConsoleHandoff

// streamWriter is the transport-agnostic sink for Chat streaming frames. The
// production ws.Writer satisfies it, as does the test recordingSink, so the
// streaming core in chatStream is written once and is independent of transport.
type streamWriter interface {
	WriteEvent(event string, data any) error
	WriteKeepalive() error
}

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
	case engine.StepUserNotice:
		// Its own wire word, not "blocked": the console renders this as a notice, and a client
		// that counts errors must not count it.
		//
		// This is a NEW ENUM VALUE on the existing step frame — not a new frame, not a new
		// handshake, and nothing negotiates it. A frontend that predates it falls through its own
		// switch and shows the frame's Label alone, which loses the notice's detail but breaks
		// nothing; the frontend's one-line `case 'user_notice'` is what recovers the detail.
		return "user_notice"
	default:
		return "unknown"
	}
}

// chatPrep carries the validated, persisted, engine-leased state from
// prepareChat into chatStream. release MUST be called by the caller (it returns
// the engine lease).
type chatPrep struct {
	sessionID      string
	message        string
	ocrText        string
	agent          *engine.Engine
	release        func()
	assistantMsgID string
	owner          store.Owner
	requestUUID    string
	action         string

	traceRecorder           *chatTraceRecorder
	clientCtxPreserve       json.RawMessage
	sessionStatePersistable bool
	start                   time.Time

	// confirmFormOptIn is set by the WS read loop when the turn's
	// SendCSAgentChat carried Features:["confirm_form_v1"]. Combined with the
	// boot flag (Handlers.confirmFormEnabled) it gates the editable confirm
	// form; the SSE POST path never sets it.
	confirmFormOptIn bool
	// guidedCreateOptIn is set by the WS read loop when the client supports the
	// guided GPU-create cards.
	guidedCreateOptIn bool
	// knowledgeOnlyOptIn removes every non-knowledge capability for this turn.
	knowledgeOnlyOptIn bool
	// publicPlatformReadOnlyOptIn exposes only the public platform catalog
	// subset. KnowledgeOnly takes precedence if a client sends both flags.
	publicPlatformReadOnlyOptIn bool
	// feishuConsoleHandoffOptIn adds an adapter-private prompt contract for a
	// console-diagnosis marker. It cannot grant tools or user identity.
	feishuConsoleHandoffOptIn bool
}

// prepareChat performs all pre-stream work shared by the SSE and WS paths:
// input validation, OCR extraction, engine lease, session-state hydration, and
// the user + assistant-placeholder row inserts. It returns either a ready
// chatPrep (caller must defer prep.release()) or an *APIError to surface before
// any stream framing. The returned context for streaming is the caller's;
// prepareChat uses ctx only for its own pre-stream DB/engine work.
func (h *Handlers) prepareChat(ctx context.Context, base BaseRequest, sessionID, message, imageDataURL string) (*chatPrep, *APIError) {
	// -----------------------------------------------------------------------
	// 1. Input validation
	// -----------------------------------------------------------------------
	if sessionID == "" {
		return nil, ErrInvalidParam.WithMessage("missing SessionId")
	}
	if message == "" {
		return nil, ErrInvalidParam.WithMessage("missing Message")
	}
	if len([]rune(message)) > h.cfg.Agent.HTTP.MaxInputLength {
		return nil, ErrInvalidParam.WithMessage("Message exceeds MaxInputLength")
	}

	sess, _, err := h.getOrCreateSession(ctx, base.Owner, sessionID)
	if err != nil {
		return nil, AsAPIError(err)
	}
	sessionID = sess.ID

	// Enforce per-session turn cap. Each completed Chat call persists exactly
	// two rows (user + assistant), so message_count == max_session_turns * 2
	// means the cap has been reached. Aborted / errored turns still count —
	// resource-wise they consumed a slot.
	maxTurns := h.cfg.Agent.HTTP.MaxSessionTurns
	if maxTurns <= 0 {
		maxTurns = config.DefaultMaxSessionTurns
	}
	if sess.MessageCount >= maxTurns*2 {
		return nil, ErrSessionTurnLimit
	}

	// -----------------------------------------------------------------------
	// 1.5 OCR image context extraction
	// -----------------------------------------------------------------------
	var ocrText string
	if imageDataURL != "" && h.ocrClient != nil {
		text, valErr := h.processOCR(ctx, base.RequestUUID, imageDataURL)
		if valErr != nil {
			return nil, ErrInvalidParam.WithMessage("invalid Image: %v", valErr)
		}
		ocrText = text
	} else if imageDataURL != "" {
		log.Printf("warning: Image provided but OCR not configured (request %s)", base.RequestUUID)
	}

	// -----------------------------------------------------------------------
	// 2. Acquire engine (serialized per session via Lease)
	// -----------------------------------------------------------------------
	if h.pool == nil {
		return nil, ErrInternal.WithMessage("%s", "engine pool not configured")
	}

	// Build and inject UserContext so downstream tools can perform STS calls
	// with the correct tenant identity.
	userCtx, ucErr := h.buildUserContext(base)
	if ucErr != nil {
		return nil, AsAPIError(ucErr)
	}
	leaseCtx := tools.WithUser(ctx, userCtx)

	agent, release, err := h.pool.Lease(leaseCtx, base.Owner, sessionID)
	if err != nil {
		return nil, AsAPIError(err)
	}

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

	start := time.Now()
	turnIndex := sess.MessageCount/2 + 1
	traceRecorder := newChatTraceRecorder(h.traceWriter, base, sessionID, turnIndex, message, start)
	if traceRecorder != nil {
		traceRecorder.SetRegistryTraceSupplier(agent.RegistryTraceState)
		attachChatTraceObservers(agent, traceRecorder)
	}

	// -----------------------------------------------------------------------
	// 3. Pre-stream persistence
	// -----------------------------------------------------------------------
	userMsgID := uuid.NewString()
	assistantMsgID := uuid.NewString()
	model := h.cfg.Agent.LLM.Model
	reqUUID := base.RequestUUID

	// Persist user message with OCR context included (so the DB record
	// shows what the engine saw). The shared durable redaction covers both OCR
	// text and the user's original message.
	persistContent := message
	if ocrText != "" {
		// Same wrapper the engine uses for the live turn, so the rehydrated
		// copy re-fed to the LLM on later turns matches the live-turn framing
		// (the durable conversation boundary below additionally scrubs the
		// user-message portion).
		persistContent = engine.WrapScreenshotContext(ocrText, message)
	}
	if err := h.messages.Append(ctx, store.Message{
		ID:          userMsgID,
		SessionID:   sessionID,
		RequestUUID: &reqUUID,
		Role:        "user",
		Content:     security.RedactUserConversationText(persistContent),
		Status:      "ok",
	}); err != nil {
		clearChatTraceObservers(agent)
		release()
		return nil, AsAPIError(err)
	}

	if err := h.messages.Append(ctx, store.Message{
		ID:          assistantMsgID,
		SessionID:   sessionID,
		RequestUUID: &reqUUID,
		Role:        "assistant",
		Content:     "",
		Status:      "pending",
		Model:       &model,
	}); err != nil {
		clearChatTraceObservers(agent)
		release()
		return nil, AsAPIError(err)
	}

	// First-turn title derivation: name the session after the user's first
	// message so the history sidebar shows e.g. "4090现在有连存吗?". Uses message
	// (not persistContent, which carries the OCR prefix). Gated on the
	// turn-start snapshot sess.Title being empty so from turn 2 onward we skip a
	// guaranteed 0-row UPDATE on the hot chat path; the store's `title IS NULL`
	// predicate stays as a concurrency backstop. Explicit client titles win.
	// Best-effort: a failure here must not fail the turn.
	if sess.Title == nil {
		if derived := deriveSessionTitle(message); derived != "" {
			if err := h.sessions.SetTitleIfEmpty(ctx, base.Owner, sessionID, derived); err != nil {
				log.Printf("warning: session %s title derivation failed (non-fatal): %v", sessionID, err)
			}
		}
	}

	if err := h.sessions.BumpUpdatedAtAndIncCount(ctx, base.Owner, sessionID, 2); err != nil {
		clearChatTraceObservers(agent)
		release()
		return nil, AsAPIError(err)
	}

	return &chatPrep{
		sessionID:               sessionID,
		message:                 message,
		ocrText:                 ocrText,
		agent:                   agent,
		release:                 release,
		assistantMsgID:          assistantMsgID,
		owner:                   base.Owner,
		requestUUID:             base.RequestUUID,
		action:                  base.Action,
		traceRecorder:           traceRecorder,
		clientCtxPreserve:       clientCtxPreserve,
		sessionStatePersistable: sessionStatePersistable,
		start:                   start,
	}, nil
}

// chatStream runs the LLM turn and streams frames over sw (SSE or WS). It owns
// everything from the meta frame through the done/error frame plus post-stream
// persistence. streamCtx scopes the turn — cancelling it (client disconnect)
// aborts the engine and unblocks any pending confirmation. The engine lease is
// released by the caller's deferred prep.release().
func (h *Handlers) chatStream(streamCtx context.Context, sw streamWriter, base BaseRequest, prep *chatPrep) {
	agent := prep.agent
	sessionID := prep.sessionID
	assistantMsgID := prep.assistantMsgID
	traceRecorder := prep.traceRecorder
	start := prep.start

	defer clearChatTraceObservers(agent)

	// Declared before finishTrace so the closure can read the final reply (for the
	// empty_reply terminus) and the post-turn ReAct counters off the engine.
	var reply string
	var chatErr error

	finishTrace := func(err error) {
		if traceRecorder == nil {
			return
		}
		traceRecorder.SetTerminalSignals(observability.FinishSignals{
			ReplyEmpty:                strings.TrimSpace(reply) == "",
			ReactRounds:               agent.ReactRoundsThisTurn(),
			RoundCeilingHit:           agent.ReactCeilingHitThisTurn(),
			ActionProposalDisposition: agent.ActionProposalDispositionThisTurn(),
		})
		sessState, _, hydrated := agent.SessionStateSnapshot()
		selectedSourceAtStart, selectedFreshnessAtStart := agent.SelectedInstanceProvenanceAtTurnStart()
		traceRecorder.SetStateTrace(observability.StateTrace{
			SessionStateHydrated:                 hydrated,
			ResolutionSource:                     agent.InstanceResolutionSource(),
			SelectedInstanceID:                   sessState.SelectedInstanceID,
			SelectedInstanceIDAtTurnStart:        agent.SelectedInstanceIDAtTurnStart(),
			SelectedInstanceSource:               sessState.SelectedInstanceSource,
			SelectedInstanceFreshness:            sessState.SelectedInstanceFreshness,
			SelectedInstanceSourceAtTurnStart:    selectedSourceAtStart,
			SelectedInstanceFreshnessAtTurnStart: selectedFreshnessAtStart,
		})
		traceRecorder.SetEngineSnapshot(agent.TraceSnapshot(time.Now()))
		if traceErr := traceRecorder.Finish(err, time.Now()); traceErr != nil {
			log.Printf("warning: HTTP trace write failed: %v", traceErr)
		}
		traceRecorder = nil
	}

	ctx := tools.WithUser(streamCtx, mustUserContext(h, base))

	_ = sw.WriteEvent("meta", metaEvent{
		RequestID: base.RequestUUID,
		SessionID: sessionID,
		MessageID: assistantMsgID,
	})

	// -----------------------------------------------------------------------
	// Keepalive goroutine
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
			case <-streamCtx.Done():
				return
			case <-done:
				return
			}
		}
	}()

	// -----------------------------------------------------------------------
	// LLM streaming call
	// -----------------------------------------------------------------------
	stepIndex := 0
	reply, chatErr = agent.ChatWithOptions(ctx, prep.message, func(ev engine.StepEvent) {
		if traceRecorder != nil {
			traceRecorder.OnStep(ev)
		}
		_ = sw.WriteEvent("step", stepEvent{
			Type:    stepTypeString(ev.Type),
			Action:  ev.Action,
			Label:   stepActionLabel(ev.Action),
			Message: guardrails.RedactOutputLeak(guardrails.RedactPII(ev.Message)),
			Index:   stepIndex,
		})
		stepIndex++
	}, engine.ChatOptions{
		// Legacy (non-durable) transport turn identity. Durable turns carry a
		// client/gateway turn id; this path has none, so pass the per-request uuid
		// as a real correlation id for the engine's evidence-binding turn id. The
		// engine backfills an ephemeral id if this is ever empty, so correctness
		// never depends on this line — it only makes the id meaningful.
		TurnID:       base.RequestUUID,
		ImageContext: prep.ocrText,
		OnTextDelta: func(s string) {
			if firstToken.IsZero() {
				firstToken = time.Now()
			}
			tokenEmitted = true
			_ = sw.WriteEvent("token", tokenEvent{Text: s})
		},
		OnUsage: func(u llm.TokenUsage) { usage = u },
		GuidedCreate: h.confirmFormEnabled &&
			h.guidedCreateEnabled &&
			prep.confirmFormOptIn &&
			prep.guidedCreateOptIn,
		ConfirmResultFunc: func(action string, args map[string]any) engine.ConfirmationResult {
			if h.confirmBroker == nil {
				return engine.ConfirmationResult{TerminalReason: observability.ConfirmationReasonDeliveryFailed}
			}
			confirmID, ch := h.confirmBroker.Register(sessionID, base.Owner)
			defer h.confirmBroker.Cancel(confirmID)
			// Both in-instance lane cards come through HERE (the plain y/N path); the
			// form path below is create-flow only, so it has nothing to label.
			if err := sw.WriteEvent("confirmation", confirmationEvent{
				ConfirmationID: confirmID,
				Action:         action,
				Summary:        sanitizeConfirmArgs(args),
				TimeoutSeconds: confirmTimeoutSeconds,
				Label:          serverOwnedConfirmLabel(action),
			}); err != nil {
				return engine.ConfirmationResult{TerminalReason: observability.ConfirmationReasonDeliveryFailed}
			}
			decision, reason := WaitForConfirmationOutcome(streamCtx, ch, confirmWaitTimeout)
			return engine.ConfirmationResult{Confirmed: decision.Confirmed, TerminalReason: reason}
		},
		ConfirmEditsFunc:       h.confirmEditsFuncFor(streamCtx, sw, sessionID, base.Owner, prep),
		KnowledgeOnly:          prep.knowledgeOnlyOptIn,
		PublicPlatformReadOnly: prep.publicPlatformReadOnlyOptIn,
		FeishuConsoleHandoff:   prep.feishuConsoleHandoffOptIn,
	})

	// Signal keepalive goroutine to exit.
	close(done)

	// -----------------------------------------------------------------------
	// Post-stream branching
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
	if errors.Is(chatErr, context.Canceled) || errors.Is(streamCtx.Err(), context.Canceled) {
		finishTrace(chatErr)
		_ = h.persistAssistant(base.Owner, assistantMsgID,
			store.AssistantPatch{Status: "aborted"})
		return
	}

	// LLM error.
	if chatErr != nil {
		finishTrace(chatErr)
		apiErr := classifyChatError(chatErr)
		code := apiErr.Code
		_ = h.persistAssistant(base.Owner, assistantMsgID,
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
	replyPersistErr := h.persistAssistant(base.Owner, assistantMsgID,
		store.AssistantPatch{
			Content:      security.RedactAssistantConversationText(reply),
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
	// is false on parse failure / unknown schema version (see prepareChat).
	// Persistence failures are warning-only — the assistant reply is already
	// delivered, the worst case is "previous instance" memory loss on the
	// next turn.
	if prep.sessionStatePersistable {
		newState, expectedVer, hydrated := agent.SessionStateSnapshot()
		if hydrated {
			envelope := engine.PersistedContext{
				AgentSessionState: newState,
				ClientContext:     prep.clientCtxPreserve,
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
					h.retryPersistSessionContext(base.Owner, sessionID, newState, prep.clientCtxPreserve)
				default:
					log.Printf("warning: session %s UpdateContext failed: %v", sessionID, upErr)
				}
			} else {
				log.Printf("warning: session %s marshal envelope failed: %v", sessionID, mErr)
			}
		}
	}

	// Shadow-persist the canonical tool transcript. LAST: after the done frame so
	// the client never waits on it, and after SessionState so an optional
	// migration probe can never delay a write the next turn actually depends on.
	// Swallowing the error was never enough on its own — an unbounded statement
	// against a blocked database withheld `done` for as long as the database
	// took, which is a user-visible hang rather than a silent failure.
	//
	// Note this still runs while the per-session engine lease is held (released
	// by `defer prep.release()` in the WS turn goroutine), so a blocked database
	// can still delay THIS session's next turn by up to shadowPersistTimeout.
	// Bounding it caps that at 3s; taking it fully off the lease means
	// snapshotting the payload and writing after release, which is a larger
	// change to the turn's shape and deliberately not folded in here.
	// replyPersistErr keeps its historical ignored-error semantics on the main
	// path; it is consulted only to avoid stamping metadata onto a row whose
	// reply never landed.
	h.shadowPersistTranscript(base.Owner, assistantMsgID, agent, replyPersistErr)
}

// assistantPersistTimeout bounds every assistant-row write on the turn path.
//
// These three writes used to run on context.Background() with no deadline. That
// is not a slow path, it is an unbounded one: a stalled database holds the
// SUCCESS write open before the `done` frame and the ERROR write open before the
// error frame, so the client is told nothing at all for as long as the database
// takes — and the per-session engine lease is held throughout, so the user's
// next turn queues behind it too. Every turn does one of these writes, so the
// blast radius is the whole service rather than one optional probe.
//
// Five seconds is chosen to be longer than any healthy write and short enough
// that a user gets an answer. Losing the row is the lesser failure: the reply
// itself is already computed and is delivered from memory, so a timeout costs
// history, not the response.
const assistantPersistTimeout = 5 * time.Second

// persistAssistant applies one bounded assistant-row patch. It returns the
// store error unchanged so callers keep their existing semantics — the two that
// discarded it still discard it, and the success path still reports it to the
// shadow write.
func (h *Handlers) persistAssistant(owner store.Owner, msgID string, patch store.AssistantPatch) error {
	ctx, cancel := context.WithTimeout(context.Background(), assistantPersistTimeout)
	defer cancel()
	err := h.messages.UpdateAssistant(ctx, owner, msgID, patch)
	if err != nil {
		log.Printf("warning: assistant row %s persist failed (reply still delivered): %v", msgID, err)
	}
	return err
}

func (h *Handlers) retryPersistSessionContext(owner store.Owner, sessionID string, newState engine.SessionState, fallbackClientCtx json.RawMessage) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	current, err := h.sessions.GetByID(ctx, owner, sessionID)
	if err != nil {
		log.Printf("warning: session %s refetch after stale context persist failed: %v", sessionID, err)
		return
	}
	clientCtx := fallbackClientCtx
	if pc, parseErr := engine.ParsePersistedContext(current.Context); parseErr == nil {
		clientCtx = pc.ClientContext
	}
	raw, err := json.Marshal(engine.PersistedContext{
		AgentSessionState: newState,
		ClientContext:     clientCtx,
	})
	if err != nil {
		log.Printf("warning: session %s marshal retry envelope failed: %v", sessionID, err)
		return
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel2()
	if _, err := h.sessions.UpdateContext(ctx2, owner, sessionID, raw, current.ContextVersion); err != nil {
		log.Printf("warning: session %s retry UpdateContext failed: %v", sessionID, err)
	}
}

// mustUserContext rebuilds the UserContext for the streaming context. prepareChat
// already validated it succeeds (same base), so an error here is not possible;
// it falls back to an empty context defensively rather than panicking.
func mustUserContext(h *Handlers, base BaseRequest) tools.UserContext {
	uc, err := h.buildUserContext(base)
	if err != nil {
		return tools.UserContext{}
	}
	return uc
}

// sanitizeConfirmArgs projects workflow confirm args to a safe subset for the
// frontend confirmation dialog. Sensitive fields (passwords, tokens) are excluded.
// confirmEditsFuncFor builds the editable-form confirm gate for this turn, or
// returns nil when either gate is closed (boot flag COMPSHARE_CONFIRM_FORM /
// client Features opt-in) — nil keeps the engine on the legacy boolean
// ConfirmFunc, so legacy clients see byte-identical confirmation frames.
//
// Each call round (including post-edit re-confirms) registers a FRESH
// ConfirmationId carrying the round's form; the broker validates Overrides
// against exactly that form before delivering them.
func (h *Handlers) confirmEditsFuncFor(streamCtx context.Context, sw streamWriter, sessionID string, owner store.Owner, prep *chatPrep) workflow.ConfirmEditsFunc {
	if !h.confirmFormEnabled || prep == nil || !prep.confirmFormOptIn || h.confirmBroker == nil {
		return nil
	}
	return func(action string, args map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		confirmID, ch := h.confirmBroker.RegisterWithForm(sessionID, owner, form)
		defer h.confirmBroker.Cancel(confirmID)
		if err := sw.WriteEvent("confirmation", confirmationEvent{
			ConfirmationID: confirmID,
			Action:         action,
			Summary:        sanitizeConfirmArgs(args),
			TimeoutSeconds: confirmTimeoutSeconds,
			Form:           form,
		}); err != nil {
			return workflow.ConfirmResolution{TerminalReason: observability.ConfirmationReasonDeliveryFailed}
		}
		d, reason := WaitForConfirmationOutcome(streamCtx, ch, confirmWaitTimeout)
		return workflow.ConfirmResolution{Confirmed: d.Confirmed, Overrides: d.Overrides, TerminalReason: reason}
	}
}

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
