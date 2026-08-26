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

const abortedAssistantMessage = "本次回复已中止，未完整生成。"

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
// emitted only when the client opts in through SendCSAgentChat Features
// ("confirm_form_v1"). A client that received a Form may
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

// confirmationAckEvent identifies the card whose decision the server accepted.
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

// Card failures use their own event name because they do not terminate the turn.
// ConfirmationId lets the client update only the affected card.
const confirmationErrorEventName = "confirmation_error"

// confirmationFailureMessage never infers whether an already-resolved card ran;
// ErrConfirmationNotFound covers both expiry and an earlier accepted click.
func confirmationFailureMessage(err error) string {
	switch {
	case errors.Is(err, ErrConfirmationOwner):
		// The card is untouched: the claim was rejected before anything moved.
		return "这张授权卡片不属于当前会话，本次点击没有生效。"
	case errors.Is(err, ErrConfirmationNotFound):
		// The id may have expired or an earlier click may already be executing.
		return "这次点击没有生效。该授权卡已失效或已被处理，本轮结果请以后续输出为准，请勿重复提交。"
	default:
		// Override validation: the card is still pending and still answerable, so
		// the sentence must say that rather than read as a dead end.
		return "卡片上的选项没有通过校验，本次没有提交；请调整后再点确认。"
	}
}

// All confirmation cards share one human-response budget. The WebSocket
// interaction allowance is derived from this value.
const confirmWaitTimeout = 120 * time.Second

// confirmTimeoutSeconds is the same budget as it appears on the wire, so the
// countdown the user watches and the deadline the server enforces cannot drift.
const confirmTimeoutSeconds = int(confirmWaitTimeout / time.Second)

// featureConfirmForm is the SendCSAgentChat Features value a client sends to
// opt in to form-bearing confirmations (and Overrides on resolve).
const featureConfirmForm = "confirm_form_v1"

// featureGuidedCreate opts an eligible client into guided GPU creation.
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

func writeVisibleEvent(sw streamWriter, recorder *chatTraceRecorder, event string, data any) error {
	err := sw.WriteEvent(event, data)
	if err == nil && recorder != nil {
		recorder.ObserveFirstVisibleEvent(recorder.clockNow())
	}
	return err
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
		// A notice is not a blocked or failed tool call.
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

	// confirmFormOptIn is set when the client advertises confirm_form_v1. The
	// SSE POST path does not opt in.
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

// A job begun on the last ordinary turn must have a bounded chance to finish. The allowance is
// derived from durable message_count (two rows per attempt), so it survives restarts without a new
// counter/schema and cannot turn an unresolved cursor into unbounded model usage.
const maxSessionBackgroundJobContinuationTurns = 6

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
	// getOrCreateSession necessarily runs before Lease so it can resolve the canonical ID. Another
	// request may then finish while this one waits on the per-session mutex. Re-read under the lease:
	// hydrating the pre-lease snapshot can miss a newly persisted background-job cursor and launch a
	// duplicate whose eventual stale CAS cannot be resumed.
	sess, err = h.sessions.GetByID(ctx, base.Owner, sessionID)
	if err != nil {
		release()
		return nil, AsAPIError(err)
	}

	// Clear cached state before parsing the persisted envelope. A malformed or
	// unknown-version row must not inherit state from the previous lease and must
	// not be overwritten by this binary.
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

	// Each Chat call persists two rows. Ordinarily the configured cap is hard, including aborted
	// turns. A live V8 guest-job cursor gets a small bounded continuation allowance so a job started
	// on the last ordinary turn can be polled and verified; once the cursor clears, or six extra
	// attempts are consumed, the normal cap applies again.
	maxTurns := h.cfg.Agent.HTTP.MaxSessionTurns
	if maxTurns <= 0 {
		maxTurns = config.DefaultMaxSessionTurns
	}
	if sess.MessageCount >= maxTurns*2 {
		continuationTurns := (sess.MessageCount - maxTurns*2) / 2
		// SetSessionState is the authority for schema-version gating and cursor
		// normalization.  Inspect that hydrated value rather than the raw JSON, or
		// a pre-V8/partial cursor could buy continuation turns without a pollable job.
		normalizedState, _, hydrated := agent.SessionStateSnapshot()
		activeJob := hydrated && !normalizedState.PersistedInstanceOpsJob.IsZero()
		if !activeJob || continuationTurns >= maxSessionBackgroundJobContinuationTurns {
			release()
			return nil, ErrSessionTurnLimit
		}
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

	// Persist the same OCR-wrapped input shape the engine saw, after applying the
	// shared conversation redaction boundary.
	persistContent := message
	if ocrText != "" {
		// Match the wrapper the engine saw so persisted replay has the same
		// framing. The persistence boundary below also redacts the user text.
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
		_ = writeVisibleEvent(sw, traceRecorder, "step", stepEvent{
			Type:    stepTypeString(ev.Type),
			Action:  ev.Action,
			Label:   stepActionLabel(ev.Action),
			Message: guardrails.RedactOutputLeak(guardrails.RedactPII(ev.Message)),
			Index:   stepIndex,
		})
		stepIndex++
	}, engine.ChatOptions{
		// Use the request UUID as the evidence-binding turn identity.
		TurnID:       base.RequestUUID,
		ImageContext: prep.ocrText,
		OnTextDelta: func(s string) {
			if firstToken.IsZero() {
				firstToken = time.Now()
			}
			tokenEmitted = true
			_ = writeVisibleEvent(sw, traceRecorder, "token", tokenEvent{Text: s})
		},
		OnUsage: func(u llm.TokenUsage) { usage = u },
		GuidedCreate: prep.confirmFormOptIn &&
			prep.guidedCreateOptIn,
		ConfirmResultFunc: func(action string, args map[string]any) engine.ConfirmationResult {
			if h.confirmBroker == nil {
				return engine.ConfirmationResult{TerminalReason: observability.ConfirmationReasonDeliveryFailed}
			}
			confirmID, ch := h.confirmBroker.Register(sessionID, base.Owner)
			defer h.confirmBroker.Cancel(confirmID)
			// Both in-instance lane cards come through HERE (the plain y/N path); the
			// form path below is create-flow only, so it has nothing to label.
			if err := writeVisibleEvent(sw, traceRecorder, "confirmation", confirmationEvent{
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
	// Every terminal path below (client disconnect, LLM error, success) must
	// persist execution continuity observed before the turn ended. The helper is
	// detached from streamCtx, bounded, best-effort, and still fail-closed when
	// prepareChat could not parse/recognize the stored envelope.
	defer h.persistSessionStateBestEffort(base.Owner, sessionID, agent, prep)

	// -----------------------------------------------------------------------
	// Post-stream branching
	// -----------------------------------------------------------------------
	if chatErr == nil && !tokenEmitted && reply != "" {
		if firstToken.IsZero() {
			firstToken = time.Now()
		}
		tokenEmitted = true
		_ = writeVisibleEvent(sw, traceRecorder, "token", tokenEvent{Text: reply})
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
			store.AssistantPatch{Content: abortedAssistantMessage, Status: "aborted"})
		return
	}

	// LLM error.
	if chatErr != nil {
		apiErr := classifyChatError(chatErr)
		code := apiErr.Code
		_ = writeVisibleEvent(sw, traceRecorder, "error", streamErrorEvent{Code: apiErr.Code, Message: apiErr.Message})
		finishTrace(chatErr)
		_ = h.persistAssistant(base.Owner, assistantMsgID,
			store.AssistantPatch{
				Status:    "error",
				ErrorCode: &code,
				LatencyMs: &latencyMs,
				TTFTMs:    &ttftMs,
			})
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

	// Persist replay metadata after the reply. SessionState persistence is the
	// shared deferred terminal action above. Transcript persistence is bounded and
	// skipped when the assistant row itself did not land.
	h.persistTurnTranscript(base.Owner, assistantMsgID, agent, replyPersistErr)
}

// persistSessionStateBestEffort writes the current execution snapshot from a
// detached, bounded context. It is shared by every chatStream terminus so an
// approved background launch is not forgotten merely because the browser or
// model stream ended before a normal reply. Parse/unknown-schema turns remain
// non-persistable and therefore cannot overwrite their original row.
func (h *Handlers) persistSessionStateBestEffort(owner store.Owner, sessionID string, agent *engine.Engine, prep *chatPrep) {
	if h == nil || h.sessions == nil || agent == nil || prep == nil || !prep.sessionStatePersistable {
		return
	}
	newState, expectedVer, hydrated := agent.SessionStateSnapshot()
	if !hydrated {
		return
	}
	envelope := engine.PersistedContext{
		AgentSessionState: newState,
		ClientContext:     prep.clientCtxPreserve,
	}
	raw, mErr := json.Marshal(envelope)
	if mErr != nil {
		log.Printf("warning: session %s marshal envelope failed: %v", sessionID, mErr)
		return
	}
	persistCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, upErr := h.sessions.UpdateContext(persistCtx, owner, sessionID, raw, expectedVer)
	cancel()
	switch {
	case upErr == nil:
		return
	case errors.Is(upErr, store.ErrStaleWrite):
		// Another writer already advanced this envelope. Retrying our whole stale SessionState would
		// overwrite its active background-job cursor and could make a second job appear trackable.
		// Continuity is best-effort: preserve the winning row and let a later turn rehydrate it.
		log.Printf("warning: session %s stale context_version on persist (expected=%d; latest row preserved)", sessionID, expectedVer)
	default:
		log.Printf("warning: session %s UpdateContext failed: %v", sessionID, upErr)
	}
}

// assistantPersistTimeout prevents a stalled database write from withholding the
// response and the per-session engine lease. The in-memory reply can still be
// delivered if persistence times out.
const assistantPersistTimeout = 5 * time.Second

// persistAssistant applies one bounded assistant-row patch. It returns the
// store error unchanged so callers keep their existing semantics — the two that
// discarded it still discard it, and the success path still reports it to the
// transcript metadata write.
func (h *Handlers) persistAssistant(owner store.Owner, msgID string, patch store.AssistantPatch) error {
	ctx, cancel := context.WithTimeout(context.Background(), assistantPersistTimeout)
	defer cancel()
	err := h.messages.UpdateAssistant(ctx, owner, msgID, patch)
	if err != nil {
		log.Printf("warning: assistant row %s persist failed (reply still delivered): %v", msgID, err)
	}
	return err
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
// confirmEditsFuncFor builds the editable-form confirm gate when the client
// opts into confirm_form_v1. Clients without that feature keep the boolean
// confirmation protocol.
//
// Each call round (including post-edit re-confirms) registers a FRESH
// ConfirmationId carrying the round's form; the broker validates Overrides
// against exactly that form before delivering them.
func (h *Handlers) confirmEditsFuncFor(streamCtx context.Context, sw streamWriter, sessionID string, owner store.Owner, prep *chatPrep) workflow.ConfirmEditsFunc {
	if prep == nil || !prep.confirmFormOptIn || h.confirmBroker == nil {
		return nil
	}
	return func(action string, args map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		confirmID, ch := h.confirmBroker.RegisterWithForm(sessionID, owner, form)
		defer h.confirmBroker.Cancel(confirmID)
		if err := writeVisibleEvent(sw, prep.traceRecorder, "confirmation", confirmationEvent{
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
		if guardrails.IsCredentialKey(k) {
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
		// An unclassified error from the chat runtime is an LLM/agent failure, but its free-form text
		// may be raw provider JSON with request IDs or infrastructure details. Preserve the cause for
		// server-side tracing while returning the stable public message (production case 131).
		modelErr := *ErrModelError
		modelErr.cause = err
		return &modelErr
	}
	return classified
}
