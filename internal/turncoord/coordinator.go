package turncoord

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/guardrails"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/turntrace"
	"github.com/compshare-agent/internal/workflow"
)

const (
	UnknownSchemaWarning  = "系统检测到这段会话由较新版本创建；本轮已按只读模式回答，并保留原有会话状态。"
	CorruptContextWarning = "系统检测到这段会话的部分状态已损坏；本轮已从可用对话记录继续，并重建会话状态。"
)

// TurnEngine is the private, mutable workspace for exactly one durable turn.
// Production uses *engine.Engine; this narrow seam keeps coordinator tests
// independent of model and upstream services.
type TurnEngine interface {
	SetSessionState(engine.SessionState, int)
	SetContinuityAdvisories(engine.ContinuityAdvisories)
	SessionStateSnapshot() (engine.SessionState, int, bool)
	ChatWithOptions(context.Context, string, func(engine.StepEvent), engine.ChatOptions) (string, error)
}

type traceableTurnEngine interface {
	TurnEngine
	AttachTraceHooks(engine.TraceHooks)
	TraceSnapshot(time.Time) engine.TraceSnapshot
}

type EngineFactory interface {
	New(context.Context, store.Owner, string, engine.SessionOptions) (TurnEngine, error)
}

type EngineFactoryFunc func(context.Context, store.Owner, string, engine.SessionOptions) (TurnEngine, error)

func (f EngineFactoryFunc) New(ctx context.Context, owner store.Owner, sessionID string, opts engine.SessionOptions) (TurnEngine, error) {
	return f(ctx, owner, sessionID, opts)
}

// PoolEngineFactory is implemented by agentpool.Pool without coupling this
// package back to agentpool (which would create an import cycle).
type PoolEngineFactory interface {
	NewTurnEngineWithOptions(context.Context, store.Owner, string, engine.SessionOptions) (*engine.Engine, error)
}

func EngineFactoryFromPool(pool PoolEngineFactory) EngineFactory {
	return EngineFactoryFunc(func(ctx context.Context, owner store.Owner, sessionID string, opts engine.SessionOptions) (TurnEngine, error) {
		return pool.NewTurnEngineWithOptions(ctx, owner, sessionID, opts)
	})
}

type turnStore interface {
	actionStore
	AcceptTurn(context.Context, store.Owner, store.AcceptTurnInput) (store.Turn, bool, error)
	GetTurn(context.Context, store.Owner, string) (store.Turn, error)
	FindTurnByClientID(context.Context, store.Owner, string, string) (store.Turn, error)
	ListRecoverableTurns(context.Context, int) ([]store.RecoverableTurn, error)
	ListContinuityAdvisories(context.Context, store.Owner, string, int) ([]store.ContinuityAdvisory, error)
	AcquireConversationLease(context.Context, store.Owner, string, string, string, time.Duration) (store.ConversationLease, error)
	AcquireConversationLeaseForFinalization(context.Context, store.Owner, string, string, string, time.Duration) (store.ConversationLease, error)
	RenewConversationLease(context.Context, store.Owner, store.ConversationLease, time.Duration) (store.ConversationLease, error)
	ReleaseConversationLease(context.Context, store.Owner, store.ConversationLease) error
	AppendEvent(context.Context, store.Owner, store.ConversationLease, string, json.RawMessage, bool) (store.TurnEvent, error)
	ListEvents(context.Context, store.Owner, string, int64, int) ([]store.TurnEvent, error)
	CreateInteraction(context.Context, store.Owner, store.ConversationLease, string, string, json.RawMessage, time.Duration) (store.TurnInteraction, bool, error)
	ResolveInteraction(context.Context, store.Owner, string, string, json.RawMessage) (store.TurnInteraction, error)
	GetInteraction(context.Context, store.Owner, string, string) (store.TurnInteraction, error)
	CommitTurn(context.Context, store.Owner, store.CommitTurnInput) (store.Turn, error)
	ReconcileCommit(context.Context, store.Owner, store.CommitTurnInput) (store.Turn, error)
	FailTurn(context.Context, store.Owner, store.ConversationLease, store.TurnStatus, string) (store.Turn, error)
}

type Options struct {
	ReplicaID            string
	LeaseTTL             time.Duration
	LeaseRenewInterval   time.Duration
	InteractionPoll      time.Duration
	InteractionTTL       time.Duration
	ExecutionTimeout     time.Duration
	RecoveryScanInterval time.Duration
	RecoveryBatchSize    int
	MutatingToolsEnabled bool
	TraceWriter          observability.Writer
	// SecretKey seals turn-scoped workflow secrets in the recoverable envelope.
	// It must be exactly 32 random bytes and identical on every replica.
	SecretKey []byte
}

func (o Options) withDefaults() Options {
	if strings.TrimSpace(o.ReplicaID) == "" {
		o.ReplicaID = "turn-coordinator"
	}
	if o.LeaseTTL <= 0 {
		o.LeaseTTL = 15 * time.Second
	}
	if o.LeaseRenewInterval <= 0 || o.LeaseRenewInterval >= o.LeaseTTL {
		o.LeaseRenewInterval = o.LeaseTTL / 3
	}
	if o.InteractionPoll <= 0 {
		o.InteractionPoll = 100 * time.Millisecond
	}
	if o.InteractionTTL <= 0 {
		o.InteractionTTL = 5 * time.Minute
	}
	if o.ExecutionTimeout <= 0 {
		o.ExecutionTimeout = 10 * time.Minute
	}
	if o.RecoveryScanInterval <= 0 {
		o.RecoveryScanInterval = 2 * time.Second
	}
	if o.RecoveryBatchSize <= 0 || o.RecoveryBatchSize > 1000 {
		o.RecoveryBatchSize = 100
	}
	return o
}

type SubmitInput struct {
	Owner          store.Owner
	SessionID      string
	ClientTurnID   string
	Message        string
	RequestUUID    *string
	AssistantModel *string
	UserMetadata   json.RawMessage
	ImageContext   string
	// ImageDigest is SHA-256 of the uploaded image bytes. OCR text is excluded
	// from request identity because another OCR pass may produce different text.
	ImageDigest  string
	UserContext  tools.UserContext
	ConfirmForm  bool
	GuidedCreate bool
	// SecretInputs exist only after a sealed envelope is opened. Callers must
	// never populate this field directly.
	SecretInputs map[string]string
}

const (
	executionEnvelopeVersion       = 2
	legacyExecutionEnvelopeVersion = 1
)

type executionEnvelope struct {
	Version        int                    `json:"version"`
	Message        string                 `json:"message"`
	OCR            string                 `json:"ocr,omitempty"`
	ImageDigest    string                 `json:"image_digest,omitempty"`
	AssistantModel *string                `json:"assistant_model,omitempty"`
	UserContext    stableExecutionContext `json:"user_context"`
	Features       executionFeatures      `json:"features"`
	SealedSecrets  string                 `json:"sealed_secrets,omitempty"`
	SecretKeyID    string                 `json:"secret_key_id,omitempty"`
	SecretDigest   string                 `json:"secret_digest,omitempty"`
}

type stableExecutionContext struct {
	TopOrganizationID uint32 `json:"top_organization_id"`
	OrganizationID    uint32 `json:"organization_id"`
	CompanyID         uint32 `json:"company_id,omitempty"`
	AccountID         uint32 `json:"account_id,omitempty"`
	Channel           uint32 `json:"channel,omitempty"`
	RoleUrn           string `json:"role_urn,omitempty"`
	ProjectID         string `json:"project_id,omitempty"`
	Region            string `json:"region,omitempty"`
	UserEmail         string `json:"user_email,omitempty"`
	// ClientIP is retained only while a turn is recoverable because the
	// current upstream gateway injects it into every API request. It is excluded
	// from request identity, so a reconnect from another network reuses the
	// first attempt's frozen request instead of forking the turn.
	ClientIP string `json:"client_ip,omitempty"`
}

type executionFeatures struct {
	ConfirmForm  bool `json:"confirm_form"`
	GuidedCreate bool `json:"guided_create"`
}

type Disposition string

const (
	DispositionStarted    Disposition = "started"
	DispositionSubscribed Disposition = "subscribed"
	DispositionReplayed   Disposition = "replayed"
)

type Submission struct {
	Turn        store.Turn
	Disposition Disposition
}

type Event struct {
	TurnID      string
	Seq         int64
	LeaseEpoch  int64
	Type        string
	Payload     json.RawMessage
	Provisional bool
	CreatedAt   time.Time
}

type EventSink func(Event) error

type ConfirmationResponse struct {
	Confirmed bool              `json:"confirmed"`
	Overrides map[string]string `json:"overrides,omitempty"`
}

type subscription struct {
	mu      sync.Mutex
	lastSeq int64
	sink    EventSink
	done    bool
	ctx     context.Context
}

type Coordinator struct {
	turns    turnStore
	sessions store.SessionStore
	factory  EngineFactory
	opts     Options

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	workers map[string]struct{}
	subs    map[string]map[*subscription]struct{}
}

func NewCoordinator(turns turnStore, sessions store.SessionStore, factory EngineFactory, opts Options) *Coordinator {
	if turns == nil || sessions == nil || factory == nil {
		panic("turncoord: coordinator requires durable turns, sessions, and an engine factory")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Coordinator{
		turns: turns, sessions: sessions, factory: factory, opts: opts.withDefaults(),
		ctx: ctx, cancel: cancel, workers: make(map[string]struct{}), subs: make(map[string]map[*subscription]struct{}),
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.recoveryLoop()
	}()
	return c
}

func (c *Coordinator) Close() {
	if c == nil {
		return
	}
	c.cancel()
	c.wg.Wait()
}

func (c *Coordinator) Submit(ctx context.Context, in SubmitInput, sink EventSink) (Submission, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(in.SessionID) == "" || strings.TrimSpace(in.ClientTurnID) == "" || strings.TrimSpace(in.Message) == "" {
		return Submission{}, fmt.Errorf("%w: incomplete turn request", store.ErrInvalidArgument)
	}
	if (in.UserContext.TopOrganizationID != 0 && in.UserContext.TopOrganizationID != in.Owner.TopOrganizationID) ||
		(in.UserContext.OrganizationID != 0 && in.UserContext.OrganizationID != in.Owner.OrganizationID) {
		return Submission{}, fmt.Errorf("%w: request identity does not match owner", store.ErrInvalidArgument)
	}
	envelope, envelopeRaw, err := freezeSubmitInputWithSecretKey(in, c.opts.SecretKey)
	if err != nil {
		return Submission{}, err
	}
	requestHash := hashExecutionEnvelope(in.Owner, in.SessionID, in.ClientTurnID, envelope)
	persistedUserContent := envelope.Message
	if envelope.OCR != "" {
		persistedUserContent = engine.WrapScreenshotContext(envelope.OCR, envelope.Message)
	}
	turn, created, err := c.turns.AcceptTurn(ctx, in.Owner, store.AcceptTurnInput{
		SessionID: in.SessionID, ClientTurnID: in.ClientTurnID, RequestHash: requestHash,
		RequestUUID: in.RequestUUID, UserContent: guardrails.RedactPII(persistedUserContent),
		UserMetadata: in.UserMetadata, AssistantModel: envelope.AssistantModel,
		ExecutionEnvelope: envelopeRaw,
	})
	if err != nil {
		return Submission{}, err
	}
	if sink != nil {
		// Admission cancellation must not cancel durable execution or its local
		// result observer. Transports that need a detachable stream use Subscribe.
		c.subscribe(c.ctx, in.Owner, turn.ID, 0, sink)
	}

	disposition := DispositionSubscribed
	switch turn.Status {
	case store.TurnStatusCommitted, store.TurnStatusFailedFinal, store.TurnStatusAmbiguousAfterAction, store.TurnStatusAborted:
		disposition = DispositionReplayed
		if sink != nil {
			if err := c.replayNow(in.Owner, turn.ID); err != nil {
				return Submission{}, err
			}
		}
		return Submission{Turn: turn, Disposition: disposition}, nil
	case store.TurnStatusAccepted, store.TurnStatusFailedRetryable:
		disposition = DispositionStarted
	default:
		if created {
			disposition = DispositionStarted
		}
	}
	if err := c.ensureWorkerFromTurn(turn); err != nil {
		return Submission{}, err
	}
	return Submission{Turn: turn, Disposition: disposition}, nil
}

func (c *Coordinator) ResolveInteraction(ctx context.Context, owner store.Owner, turnID, key string, response ConfirmationResponse) error {
	interaction, err := c.turns.GetInteraction(ctx, owner, turnID, key)
	if err != nil {
		return err
	}
	if !response.Confirmed {
		response.Overrides = nil
	} else if len(response.Overrides) != 0 {
		var request struct {
			Form *workflow.ConfirmForm `json:"form"`
		}
		if err := json.Unmarshal(interaction.RequestPayload, &request); err != nil || request.Form == nil {
			return fmt.Errorf("%w: this confirmation does not accept overrides", store.ErrInvalidArgument)
		}
		if err := request.Form.ValidateOverrides(response.Overrides); err != nil {
			return fmt.Errorf("%w: %v", store.ErrInvalidArgument, err)
		}
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = c.turns.ResolveInteraction(ctx, owner, turnID, key, raw)
	return err
}

// GetTurn is the owner-scoped status endpoint used by reconnect/reconcile.
func (c *Coordinator) GetTurn(ctx context.Context, owner store.Owner, turnID string) (store.Turn, error) {
	return c.turns.GetTurn(ctx, owner, turnID)
}

func (c *Coordinator) FindTurnByClientID(ctx context.Context, owner store.Owner, sessionID, clientTurnID string) (store.Turn, error) {
	return c.turns.FindTurnByClientID(ctx, owner, sessionID, clientTurnID)
}

// AbortTurn releases an accepted/failed-retryable queue head that the client
// has explicitly abandoned. An actively executing turn remains lease-owned
// and cannot be stolen by this endpoint.
func (c *Coordinator) AbortTurn(ctx context.Context, owner store.Owner, turnID string) (store.Turn, error) {
	turn, err := c.turns.GetTurn(ctx, owner, turnID)
	if err != nil {
		return store.Turn{}, err
	}
	if turn.Status.Terminal() {
		return turn, nil
	}
	if turn.Status != store.TurnStatusAccepted && turn.Status != store.TurnStatusFailedRetryable {
		return store.Turn{}, store.ErrLeaseHeld
	}
	lease, err := c.turns.AcquireConversationLeaseForFinalization(ctx, owner, turn.SessionID, turn.ID, c.opts.ReplicaID+"/abort", c.opts.LeaseTTL)
	if err != nil {
		return store.Turn{}, err
	}
	aborted, err := c.turns.FailTurn(ctx, owner, lease, store.TurnStatusAborted, "client_aborted")
	if err == nil {
		c.publishAvailable(owner, turn.ID)
	}
	return aborted, err
}

// Subscribe replays persisted events strictly after lastSeq and then follows
// the turn until a terminal event or ctx cancellation. For a non-terminal
// orphan it also starts recovery from the frozen database envelope; transport
// cancellation still never cancels execution.
func (c *Coordinator) Subscribe(ctx context.Context, owner store.Owner, turnID string, lastSeq int64, sink EventSink) error {
	if ctx == nil || turnID == "" || lastSeq < 0 || sink == nil {
		return fmt.Errorf("%w: invalid turn subscription", store.ErrInvalidArgument)
	}
	turn, err := c.turns.GetTurn(ctx, owner, turnID)
	if err != nil {
		return err
	}
	c.subscribe(ctx, owner, turnID, lastSeq, sink)
	if !turn.Status.Terminal() {
		if err := c.ensureWorkerFromTurn(turn); err != nil {
			return err
		}
	}
	return nil
}

// RecoverTurn is the explicit resume hook for non-streaming transports.
func (c *Coordinator) RecoverTurn(ctx context.Context, owner store.Owner, turnID string) error {
	turn, err := c.turns.GetTurn(ctx, owner, turnID)
	if err != nil {
		return err
	}
	if turn.Status.Terminal() {
		return nil
	}
	return c.ensureWorkerFromTurn(turn)
}

func (c *Coordinator) recoveryLoop() {
	run := func() {
		ctx, cancel := context.WithTimeout(c.ctx, max(c.opts.RecoveryScanInterval, 2*time.Second))
		defer cancel()
		turns, err := c.turns.ListRecoverableTurns(ctx, c.opts.RecoveryBatchSize)
		if err != nil {
			return
		}
		for _, recoverable := range turns {
			if c.ctx.Err() != nil {
				return
			}
			_ = c.ensureWorkerFromTurn(recoverable.Turn)
		}
	}
	run()
	ticker := time.NewTicker(c.opts.RecoveryScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func (c *Coordinator) ensureWorkerFromTurn(turn store.Turn) error {
	if turn.Status.Terminal() {
		return nil
	}
	c.ensureWorker(turn)
	return nil
}

func (c *Coordinator) ensureWorker(turn store.Turn) {
	turnID := turn.ID
	c.mu.Lock()
	if _, exists := c.workers[turnID]; exists {
		c.mu.Unlock()
		return
	}
	c.workers[turnID] = struct{}{}
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer func() {
			c.mu.Lock()
			delete(c.workers, turnID)
			c.mu.Unlock()
		}()
		c.run(turn)
	}()
}

func (c *Coordinator) run(turn store.Turn) {
	turnID := turn.ID
	owner := turn.Owner
	lease, ok := c.waitForLease(owner, turn.SessionID, turnID)
	if !ok {
		return
	}
	attemptStart := time.Now()
	attemptTrace := turntrace.New(turntrace.Config{
		Writer: c.opts.TraceWriter,
		Tenant: observability.TenantContext{
			TopOrgID: int64(owner.TopOrganizationID), OrgID: int64(owner.OrganizationID),
			ConnectionID: turn.SessionID,
		},
		TraceID: traceAttemptID(turnID, lease.Epoch), TurnID: turnID,
		TurnIndex: int(turn.Sequence), UserMsgHash: "sha256:" + turn.RequestHash,
		Start: attemptStart,
		Continuity: observability.ContinuityTrace{
			SessionIdentityMatch: turn.ID == lease.TurnID && turn.SessionID == lease.SessionID,
			TurnSequence:         turn.Sequence, LeaseEpoch: lease.Epoch,
			EnvelopeParseOutcome: "not_read", ContextParseOutcome: "not_read",
			RetryCount:      turn.RetryCount,
			RecoveryAttempt: turn.RetryCount > 0 || turn.Status == store.TurnStatusFailedRetryable,
		},
	})
	var (
		traceEngine     traceableTurnEngine
		traceReply      string
		traceChatErr    error
		traceAttemptErr error
	)
	// Register trace finalization before the settlement defer below. Defers run
	// in reverse order, so an unexpected exit is first made durable and only
	// then written to observability.
	defer func() {
		if attemptTrace == nil {
			return
		}
		var snapshot engine.TraceSnapshot
		if traceEngine != nil {
			snapshot = traceEngine.TraceSnapshot(time.Now())
		}
		if err := attemptTrace.Finish(traceChatErr, traceAttemptErr, traceReply, snapshot, time.Now()); err != nil {
			log.Printf("warning: durable turn trace write failed: turn=%s epoch=%d: %v", turnID, lease.Epoch, err)
		}
	}()
	settled := false
	settleFailure := func(desired store.TurnStatus, reason string) store.Turn {
		failed := c.failAs(owner, lease, desired, reason)
		traceReason := boundedContinuityReason(reason)
		if failed.Status == store.TurnStatusCommitted && reason == "turn_not_saved" {
			traceAttemptErr = nil
			attemptTrace.SetContinuity(func(trace *observability.ContinuityTrace) {
				trace.CommitOutcome = "late_reconciled_committed"
				trace.CommitReason = ""
			})
			return failed
		}
		if traceAttemptErr == nil {
			traceAttemptErr = errors.New(traceReason)
		}
		attemptTrace.SetContinuity(func(trace *observability.ContinuityTrace) {
			trace.CommitReason = traceReason
			if failed.ID == "" {
				trace.CommitOutcome = "settlement_interrupted"
				return
			}
			trace.CommitOutcome = string(failed.Status)
			trace.RetryCount = failed.RetryCount
		})
		return failed
	}
	defer func() {
		if !settled {
			settleFailure(store.TurnStatusFailedRetryable, "executor_stopped")
		}
	}()

	// The execution envelope is intentionally decoded only after this turn owns
	// the database lease. A malformed row must be terminally cleared by exactly
	// one replica instead of permanently blocking the session queue.
	reloadCtx, reloadCancel := context.WithTimeout(context.Background(), 3*time.Second)
	turn, err := c.turns.GetTurn(reloadCtx, owner, turnID)
	reloadCancel()
	if err != nil {
		settleFailure(store.TurnStatusFailedRetryable, "turn_reload_failed")
		settled = true
		return
	}
	attemptTrace.SetContinuity(func(trace *observability.ContinuityTrace) {
		trace.SessionIdentityMatch = turn.ID == lease.TurnID && turn.SessionID == lease.SessionID && turn.Owner == owner
		trace.TurnSequence = turn.Sequence
		trace.RetryCount = turn.RetryCount
		trace.RecoveryAttempt = turn.RetryCount > 0 || turn.Status == store.TurnStatusFailedRetryable
	})
	in, err := thawSubmitInputWithSecretKey(turn, c.opts.SecretKey)
	if err != nil {
		reason := executionEnvelopeFailureReason(err)
		attemptTrace.SetContinuity(func(trace *observability.ContinuityTrace) { trace.EnvelopeParseOutcome = reason })
		settleFailure(store.TurnStatusFailedFinal, reason)
		settled = true
		return
	}
	attemptTrace.SetUserMessage(in.Message)
	attemptTrace.SetContinuity(func(trace *observability.ContinuityTrace) { trace.EnvelopeParseOutcome = "valid" })

	execCtx, execCancel := context.WithTimeout(c.ctx, c.opts.ExecutionTimeout)
	execCtx, cancelCause := context.WithCancelCause(execCtx)
	defer execCancel()
	defer cancelCause(nil)
	renewDone := make(chan struct{})
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.renewLoop(execCtx, in.Owner, lease, cancelCause, renewDone)
	}()
	defer func() { close(renewDone) }()

	runningEvent, err := c.appendEvent(execCtx, in.Owner, lease, "turn.running", map[string]any{"turn_id": turnID})
	if err != nil {
		settleFailure(store.TurnStatusFailedRetryable, "event_persist_failed")
		settled = true
		return
	}
	c.deliver(runningEvent)

	// The session is deliberately read after the global lease is acquired.
	// AcceptTurn's version is only an admission snapshot; it is not execution
	// authority.
	sess, err := c.sessions.GetByID(execCtx, in.Owner, in.SessionID)
	if err != nil {
		settleFailure(store.TurnStatusFailedRetryable, "context_read_failed")
		settled = true
		return
	}

	state, clientContext, warning, mode, mutations, contextParseOutcome := parseExecutionContext(sess.Context, c.opts.MutatingToolsEnabled)
	attemptTrace.SetContinuity(func(trace *observability.ContinuityTrace) {
		trace.SnapshotContextVersion = sess.ContextVersion
		trace.ContextParseOutcome = contextParseOutcome
	})
	journal := NewActionJournal(c.turns, in.Owner, lease)
	confirm := c.confirmFunc(execCtx, in.Owner, lease, cancelCause)
	var confirmEdits workflow.ConfirmEditsFunc
	if in.ConfirmForm {
		confirmEdits = c.confirmEditsFunc(execCtx, in.Owner, lease, cancelCause)
	}
	eng, err := c.factory.New(execCtx, in.Owner, in.SessionID, engine.SessionOptions{
		ConfirmFn: confirm, MutatingToolsEnabled: mutations,
		InitialCommittedTurns: int(max(turn.Sequence-1, 0)),
		ActionJournal:         journal, RequireActionJournal: true,
	})
	if err != nil {
		settleFailure(store.TurnStatusFailedRetryable, "engine_build_failed")
		settled = true
		return
	}
	if attemptTrace != nil {
		var traceable bool
		traceEngine, traceable = eng.(traceableTurnEngine)
		if !traceable {
			settleFailure(store.TurnStatusFailedFinal, "trace_engine_unsupported")
			settled = true
			return
		}
		traceEngine.AttachTraceHooks(attemptTrace.Hooks())
	}
	eng.SetSessionState(state, sess.ContextVersion)
	sameTurnActions, err := journal.RestoredActionAdvisory(execCtx)
	if err != nil {
		settleFailure(store.TurnStatusFailedRetryable, "action_recovery_read_failed")
		settled = true
		return
	}
	priorOutcomes, err := c.turns.ListContinuityAdvisories(execCtx, in.Owner, in.SessionID, 10)
	if err != nil {
		settleFailure(store.TurnStatusFailedRetryable, "continuity_read_failed")
		settled = true
		return
	}
	eng.SetContinuityAdvisories(buildContinuityAdvisories(
		mode == store.ContextWritePreserve,
		sameTurnActions,
		priorOutcomes,
	))

	var eventErr error
	var eventMu sync.Mutex
	start := time.Now()
	var usage llm.TokenUsage
	reply, chatErr := eng.ChatWithOptions(tools.WithUser(execCtx, normalizedUserContext(in)), in.Message, func(step engine.StepEvent) {
		attemptTrace.OnStep(step)
		eventMu.Lock()
		defer eventMu.Unlock()
		if eventErr != nil {
			return
		}
		payload := map[string]any{
			"type": step.Type, "action": step.Action, "source": step.Source,
			"message": guardrails.RedactOutputLeak(guardrails.RedactPII(step.Message)),
		}
		event, appendErr := c.appendEvent(execCtx, in.Owner, lease, "turn.step", payload)
		if appendErr != nil {
			eventErr = appendErr
			cancelCause(appendErr)
			return
		}
		c.deliver(event)
	}, engine.ChatOptions{
		ImageContext:     in.ImageContext,
		ConfirmFunc:      confirm,
		ConfirmEditsFunc: confirmEdits,
		GuidedCreate:     in.GuidedCreate,
		SecretInputs:     in.SecretInputs,
		OnUsage:          func(value llm.TokenUsage) { usage = value },
	})
	eventMu.Lock()
	persistEventErr := eventErr
	eventMu.Unlock()
	if chatErr != nil || persistEventErr != nil || context.Cause(execCtx) != nil {
		reason := "execution_failed"
		failureStatus := store.TurnStatusFailedRetryable
		cause := context.Cause(execCtx)
		if errors.Is(cause, store.ErrLeaseFenced) {
			reason = "lease_lost"
		} else if errors.Is(cause, store.ErrInteractionExpired) {
			reason = "interaction_expired"
			failureStatus = store.TurnStatusAborted
		}
		if chatErr != nil {
			traceChatErr = chatErr
		} else if persistEventErr != nil {
			traceAttemptErr = persistEventErr
		} else if cause != nil {
			traceAttemptErr = cause
		}
		settleFailure(failureStatus, reason)
		settled = true
		return
	}
	if health, ok := eng.(interface{ ActionJournalError() error }); ok {
		if err := health.ActionJournalError(); err != nil {
			settleFailure(store.TurnStatusFailedRetryable, "action_outcome_uncertain")
			settled = true
			return
		}
	}
	if err := journal.VerifyComplete(execCtx); err != nil {
		settleFailure(store.TurnStatusFailedRetryable, "action_replay_incomplete")
		settled = true
		return
	}
	if err := journal.Err(); err != nil {
		settleFailure(store.TurnStatusFailedRetryable, "action_outcome_uncertain")
		settled = true
		return
	}

	state, version, hydrated := eng.SessionStateSnapshot()
	if !hydrated || version != sess.ContextVersion {
		settleFailure(store.TurnStatusFailedRetryable, "context_snapshot_invalid")
		settled = true
		return
	}
	contextRaw := sess.Context
	if mode == store.ContextWriteUpdate {
		contextRaw, err = json.Marshal(engine.PersistedContext{AgentSessionState: state, ClientContext: clientContext})
		if err != nil {
			settleFailure(store.TurnStatusFailedRetryable, "context_encode_failed")
			settled = true
			return
		}
	}
	finalReply := strings.TrimSpace(reply)
	if warning != "" {
		if finalReply == "" {
			finalReply = warning
		} else {
			finalReply = warning + "\n\n" + finalReply
		}
	}
	finalReply = redactTurnOutput(finalReply)
	if finalReply == "" {
		settleFailure(store.TurnStatusFailedRetryable, "empty_answer")
		settled = true
		return
	}
	traceReply = finalReply
	latency := int(time.Since(start).Milliseconds())
	payload, _ := json.Marshal(map[string]any{
		"turn_id": turnID, "message_id": turn.AssistantMessageID,
		"content": finalReply, "committed": true,
	})
	commitInput := store.CommitTurnInput{
		TurnID: turnID, Lease: lease, ExpectedContextVersion: sess.ContextVersion,
		ContextWriteMode: mode, Context: contextRaw,
		Assistant:         store.AssistantPatch{Content: finalReply, InputTokens: intPtr(usage.PromptTokens), OutputTokens: intPtr(usage.CompletionTokens), LatencyMs: &latency},
		TerminalEventType: "turn.committed", TerminalEventPayload: payload,
	}
	committed, err := c.turns.CommitTurn(execCtx, in.Owner, commitInput)
	reconciled := false
	if err != nil {
		// A transaction error is not evidence of rollback. First prove whether
		// our exact fingerprint committed; only then may we classify the attempt
		// as a safe retry.
		reconcileCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		committed, err = c.turns.ReconcileCommit(reconcileCtx, in.Owner, commitInput)
		cancel()
		reconciled = err == nil
	}
	if err != nil {
		settleFailure(store.TurnStatusFailedRetryable, "turn_not_saved")
		settled = true
		return
	}
	attemptTrace.SetContinuity(func(trace *observability.ContinuityTrace) {
		trace.CommitOutcome = "committed"
		if reconciled {
			trace.CommitOutcome = "reconciled_committed"
		}
		trace.CommitReason = ""
	})
	settled = true
	// Re-read the exact database event. The stored sequence, payload and time
	// are the replay contract; a locally reconstructed approximation can drift.
	publishCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	events, listErr := c.turns.ListEvents(publishCtx, in.Owner, turnID, max(committed.NextEventSeq-2, 0), 10)
	cancel()
	if listErr == nil {
		for _, event := range events {
			if event.Seq == committed.NextEventSeq-1 && event.Type == "turn.committed" {
				c.deliver(eventFromStore(event))
				break
			}
		}
	}
}

func (c *Coordinator) waitForLease(owner store.Owner, sessionID, turnID string) (store.ConversationLease, bool) {
	ticker := time.NewTicker(max(c.opts.InteractionPoll, 20*time.Millisecond))
	defer ticker.Stop()
	for {
		turn, err := c.turns.GetTurn(c.ctx, owner, turnID)
		if err == nil && turn.Status.Terminal() {
			return store.ConversationLease{}, false
		}
		if err == nil {
			lease, acquireErr := c.turns.AcquireConversationLease(c.ctx, owner, sessionID, turnID, c.opts.ReplicaID, c.opts.LeaseTTL)
			if acquireErr == nil {
				return lease, true
			}
			if errors.Is(acquireErr, store.ErrRetryNotDue) || errors.Is(acquireErr, store.ErrRetryExhausted) {
				return store.ConversationLease{}, false
			}
			if errors.Is(acquireErr, store.ErrConversationNotFound) || errors.Is(acquireErr, store.ErrTurnNotFound) {
				return store.ConversationLease{}, false
			}
		} else if errors.Is(err, store.ErrTurnNotFound) || errors.Is(err, store.ErrConversationNotFound) {
			return store.ConversationLease{}, false
		}
		select {
		case <-c.ctx.Done():
			return store.ConversationLease{}, false
		case <-ticker.C:
		}
	}
}

func executionEnvelopeFailureReason(err error) string {
	if errors.Is(err, store.ErrExecutionEnvelopeMissing) {
		return "execution_envelope_missing"
	}
	if strings.Contains(strings.ToLower(err.Error()), "unsupported execution envelope version") {
		return "execution_envelope_unsupported"
	}
	return "execution_envelope_invalid"
}

func (c *Coordinator) renewLoop(ctx context.Context, owner store.Owner, lease store.ConversationLease, cancel context.CancelCauseFunc, done <-chan struct{}) {
	ticker := time.NewTicker(c.opts.LeaseRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			if _, err := c.turns.RenewConversationLease(ctx, owner, lease, c.opts.LeaseTTL); err != nil {
				cancel(fmt.Errorf("%w: %v", store.ErrLeaseFenced, err))
				return
			}
		}
	}
}

func (c *Coordinator) confirmFunc(
	ctx context.Context,
	owner store.Owner,
	lease store.ConversationLease,
	cancel context.CancelCauseFunc,
) engine.ConfirmFunc {
	return func(action string, args map[string]any) bool {
		payload, err := json.Marshal(map[string]any{"action": action, "summary": sanitizeInteractionArgs(args)})
		if err != nil {
			cancel(fmt.Errorf("encode confirmation: %w", err))
			return false
		}
		key := semanticInteractionKey("confirmation", payload)
		response, ok := c.awaitConfirmation(ctx, owner, lease, cancel, key, payload)
		return ok && response.Confirmed
	}
}

func (c *Coordinator) confirmEditsFunc(
	ctx context.Context,
	owner store.Owner,
	lease store.ConversationLease,
	cancel context.CancelCauseFunc,
) workflow.ConfirmEditsFunc {
	return func(action string, args map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		if form == nil {
			cancel(fmt.Errorf("encode confirmation: missing form"))
			return workflow.ConfirmResolution{}
		}
		payload, err := json.Marshal(map[string]any{
			"action": action, "summary": sanitizeInteractionArgs(args), "form": form,
		})
		if err != nil {
			cancel(fmt.Errorf("encode confirmation: %w", err))
			return workflow.ConfirmResolution{}
		}
		key := semanticInteractionKey("confirmation", payload)
		response, ok := c.awaitConfirmation(ctx, owner, lease, cancel, key, payload)
		if !ok {
			return workflow.ConfirmResolution{}
		}
		return workflow.ConfirmResolution{Confirmed: response.Confirmed, Overrides: response.Overrides}
	}
}

func semanticInteractionKey(kind string, payload json.RawMessage) string {
	return kind + "/" + store.HashTurnRequest(kind, string(payload))
}

func (c *Coordinator) awaitConfirmation(
	ctx context.Context,
	owner store.Owner,
	lease store.ConversationLease,
	cancel context.CancelCauseFunc,
	key string,
	payload json.RawMessage,
) (ConfirmationResponse, bool) {
	interaction, _, err := c.turns.CreateInteraction(ctx, owner, lease, key, "confirmation", payload, c.opts.InteractionTTL)
	if err != nil {
		cancel(fmt.Errorf("persist confirmation %s: %w", key, err))
		return ConfirmationResponse{}, false
	}
	key = interaction.Key
	ticker := time.NewTicker(c.opts.InteractionPoll)
	defer ticker.Stop()
	for {
		// ExpiresAt is assigned by PostgreSQL. ResolveInteraction also rechecks
		// NOW() in the same transaction, so no process clock can authorize a
		// stale approval.
		if !interaction.ExpiresAt.After(time.Now()) {
			cancel(fmt.Errorf("confirmation %s: %w", key, store.ErrInteractionExpired))
			return ConfirmationResponse{}, false
		}
		if interaction.Status == store.InteractionStatusResolved {
			var response ConfirmationResponse
			if err := json.Unmarshal(interaction.ResponsePayload, &response); err != nil {
				cancel(fmt.Errorf("decode confirmation %s: %w", key, err))
				return ConfirmationResponse{}, false
			}
			return response, true
		}
		select {
		case <-ctx.Done():
			return ConfirmationResponse{}, false
		case <-ticker.C:
			interaction, err = c.turns.GetInteraction(ctx, owner, lease.TurnID, key)
			if err != nil {
				cancel(fmt.Errorf("read confirmation %s: %w", key, err))
				return ConfirmationResponse{}, false
			}
		}
	}
}

func parseExecutionContext(raw json.RawMessage, globalMutations bool) (engine.SessionState, json.RawMessage, string, store.ContextWriteMode, bool, string) {
	pc, err := engine.ParsePersistedContext(raw)
	if err == nil {
		return pc.AgentSessionState, pc.ClientContext, "", store.ContextWriteUpdate, globalMutations, "valid"
	}
	if errors.Is(err, engine.ErrUnknownSessionStateSchema) {
		return engine.SessionState{SchemaVersion: engine.SessionStateSchemaCurrent}, nil,
			UnknownSchemaWarning, store.ContextWritePreserve, false, "unknown_schema"
	}
	return engine.SessionState{SchemaVersion: engine.SessionStateSchemaCurrent}, pc.ClientContext,
		CorruptContextWarning, store.ContextWriteUpdate, globalMutations, "malformed"
}

func normalizedUserContext(in SubmitInput) tools.UserContext {
	u := in.UserContext
	if u.TopOrganizationID == 0 {
		u.TopOrganizationID = in.Owner.TopOrganizationID
	}
	if u.OrganizationID == 0 {
		u.OrganizationID = in.Owner.OrganizationID
	}
	return u
}

func freezeSubmitInput(in SubmitInput) (executionEnvelope, json.RawMessage, error) {
	return freezeSubmitInputWithSecretKey(in, nil)
}

func freezeSubmitInputWithSecretKey(in SubmitInput, secretKey []byte) (executionEnvelope, json.RawMessage, error) {
	digest, err := normalizeImageDigest(in.ImageDigest)
	if err != nil {
		return executionEnvelope{}, nil, err
	}
	message := strings.TrimSpace(in.Message)
	secretInputs := map[string]string{}
	if password, start, end := engine.ExtractResetPasswordSecret(message); password != "" {
		secretInputs["Password"] = password
		message = message[:start] + guardrails.CredentialRedactedOutput + message[end:]
	}
	message = strings.TrimSpace(guardrails.RedactCredentials(message))
	ocrText := strings.TrimSpace(in.ImageContext)
	if password, start, end := engine.ExtractResetPasswordSecret(ocrText); password != "" {
		ocrText = ocrText[:start] + guardrails.CredentialRedactedOutput + ocrText[end:]
	}
	ocrText = strings.TrimSpace(guardrails.RedactCredentials(ocrText))
	if message == "" {
		return executionEnvelope{}, nil, fmt.Errorf("%w: empty redacted message", store.ErrInvalidArgument)
	}
	if ocrText != "" && digest == "" {
		return executionEnvelope{}, nil, fmt.Errorf("%w: OCR requires a stable image digest", store.ErrInvalidArgument)
	}
	user := in.UserContext
	if user.TopOrganizationID == 0 {
		user.TopOrganizationID = in.Owner.TopOrganizationID
	}
	if user.OrganizationID == 0 {
		user.OrganizationID = in.Owner.OrganizationID
	}
	if user.TopOrganizationID != in.Owner.TopOrganizationID || user.OrganizationID != in.Owner.OrganizationID {
		return executionEnvelope{}, nil, fmt.Errorf("%w: envelope owner mismatch", store.ErrInvalidArgument)
	}
	role := strings.TrimSpace(user.RoleUrn)
	if guardrails.ContainsCredential(role) {
		return executionEnvelope{}, nil, fmt.Errorf("%w: credential-like role value", store.ErrInvalidArgument)
	}
	var model *string
	if in.AssistantModel != nil {
		value := strings.TrimSpace(*in.AssistantModel)
		if len(value) > 128 {
			return executionEnvelope{}, nil, fmt.Errorf("%w: model name too long", store.ErrInvalidArgument)
		}
		model = &value
	}
	envelope := executionEnvelope{
		Version: executionEnvelopeVersion, Message: message, OCR: ocrText,
		ImageDigest: digest, AssistantModel: model,
		UserContext: stableExecutionContext{
			TopOrganizationID: user.TopOrganizationID, OrganizationID: user.OrganizationID,
			CompanyID: user.CompanyID, AccountID: user.AccountID, Channel: user.Channel,
			RoleUrn: role, ProjectID: strings.TrimSpace(user.ProjectId), Region: strings.TrimSpace(user.Region),
			UserEmail: strings.TrimSpace(user.UserEmail), ClientIP: strings.TrimSpace(user.ClientIP),
		},
		Features: executionFeatures{ConfirmForm: in.ConfirmForm, GuidedCreate: in.GuidedCreate},
	}
	if len(secretInputs) != 0 {
		sealed, keyID, digest, sealErr := sealTurnSecrets(secretKey, secretInputs, executionEnvelopeAAD(in.Owner, in.SessionID, in.ClientTurnID))
		if sealErr != nil {
			return executionEnvelope{}, nil, sealErr
		}
		envelope.SealedSecrets = sealed
		envelope.SecretKeyID = keyID
		envelope.SecretDigest = digest
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return executionEnvelope{}, nil, fmt.Errorf("encode execution envelope: %w", err)
	}
	return envelope, raw, nil
}

func thawSubmitInput(turn store.Turn) (SubmitInput, error) {
	return thawSubmitInputWithSecretKey(turn, nil)
}

func thawSubmitInputWithSecretKey(turn store.Turn, secretKey []byte) (SubmitInput, error) {
	var envelope executionEnvelope
	if len(turn.ExecutionEnvelope) == 0 {
		return SubmitInput{}, store.ErrExecutionEnvelopeMissing
	}
	decoder := json.NewDecoder(strings.NewReader(string(turn.ExecutionEnvelope)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return SubmitInput{}, fmt.Errorf("decode execution envelope: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return SubmitInput{}, fmt.Errorf("decode execution envelope: trailing data")
	}
	if envelope.Version != executionEnvelopeVersion && envelope.Version != legacyExecutionEnvelopeVersion {
		return SubmitInput{}, fmt.Errorf("unsupported execution envelope version %d", envelope.Version)
	}
	if envelope.Version == legacyExecutionEnvelopeVersion && (envelope.SealedSecrets != "" || envelope.SecretKeyID != "" || envelope.SecretDigest != "") {
		return SubmitInput{}, fmt.Errorf("%w: legacy envelope contains secret fields", store.ErrInvalidArgument)
	}
	if strings.TrimSpace(envelope.Message) == "" ||
		envelope.UserContext.TopOrganizationID != turn.Owner.TopOrganizationID ||
		envelope.UserContext.OrganizationID != turn.Owner.OrganizationID {
		return SubmitInput{}, fmt.Errorf("%w: execution envelope identity mismatch", store.ErrInvalidArgument)
	}
	digest, err := normalizeImageDigest(envelope.ImageDigest)
	if err != nil || (envelope.OCR != "" && digest == "") {
		return SubmitInput{}, fmt.Errorf("%w: invalid execution image identity", store.ErrInvalidArgument)
	}
	envelope.ImageDigest = digest
	var secretInputs map[string]string
	if envelope.SealedSecrets != "" {
		secretInputs, err = openTurnSecrets(secretKey, envelope.SealedSecrets, envelope.SecretKeyID, envelope.SecretDigest,
			executionEnvelopeAAD(turn.Owner, turn.SessionID, turn.ClientTurnID))
		if err != nil {
			return SubmitInput{}, err
		}
	}
	return SubmitInput{
		Owner: turn.Owner, SessionID: turn.SessionID, ClientTurnID: turn.ClientTurnID,
		Message: envelope.Message, AssistantModel: envelope.AssistantModel,
		ImageContext: envelope.OCR, ImageDigest: envelope.ImageDigest,
		UserContext: tools.UserContext{
			TopOrganizationID: envelope.UserContext.TopOrganizationID,
			OrganizationID:    envelope.UserContext.OrganizationID,
			CompanyID:         envelope.UserContext.CompanyID, AccountID: envelope.UserContext.AccountID,
			Channel: envelope.UserContext.Channel, RoleUrn: envelope.UserContext.RoleUrn,
			SessionName: fmt.Sprintf("%d-%d", turn.Owner.TopOrganizationID, turn.Owner.OrganizationID),
			ProjectId:   envelope.UserContext.ProjectID, Region: envelope.UserContext.Region,
			UserEmail: envelope.UserContext.UserEmail, ClientIP: envelope.UserContext.ClientIP,
		},
		ConfirmForm: envelope.Features.ConfirmForm, GuidedCreate: envelope.Features.GuidedCreate,
		SecretInputs: secretInputs,
	}, nil
}

func hashExecutionEnvelope(owner store.Owner, sessionID, clientTurnID string, envelope executionEnvelope) string {
	hashUserContext := envelope.UserContext
	hashUserContext.ClientIP = ""
	stableUserContext, _ := json.Marshal(hashUserContext)
	features, _ := json.Marshal(envelope.Features)
	model := "<nil>"
	if envelope.AssistantModel != nil {
		model = "<value>" + *envelope.AssistantModel
	}
	// OCR is deliberately absent: image bytes, not a potentially drifting OCR
	// pass, identify the screenshot attached to this semantic request.
	return store.HashTurnRequest(
		fmt.Sprint(owner.TopOrganizationID), fmt.Sprint(owner.OrganizationID),
		sessionID, clientTurnID, envelope.Message, envelope.ImageDigest,
		string(stableUserContext), model, string(features), envelope.SecretKeyID, envelope.SecretDigest,
	)
}

func executionEnvelopeAAD(owner store.Owner, sessionID, clientTurnID string) []byte {
	return []byte(fmt.Sprintf("turn-secret-v1:%d:%d:%s:%s", owner.TopOrganizationID, owner.OrganizationID, sessionID, clientTurnID))
}

func sealTurnSecrets(key []byte, secrets map[string]string, aad []byte) (sealed, keyID, digest string, err error) {
	if len(key) != 32 {
		return "", "", "", fmt.Errorf("%w: durable secret key must be 32 bytes", store.ErrInvalidArgument)
	}
	plain, err := json.Marshal(secrets)
	if err != nil {
		return "", "", "", fmt.Errorf("encode turn secrets: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", "", fmt.Errorf("create turn secret cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", "", fmt.Errorf("create turn secret envelope: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", "", "", fmt.Errorf("create turn secret nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, plain, aad)
	keyHash := sha256.Sum256(key)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(plain)
	return base64.RawStdEncoding.EncodeToString(append(nonce, ciphertext...)),
		hex.EncodeToString(keyHash[:8]), hex.EncodeToString(mac.Sum(nil)), nil
}

func openTurnSecrets(key []byte, sealed, expectedKeyID, expectedDigest string, aad []byte) (map[string]string, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("%w: durable secret key unavailable", store.ErrInvalidArgument)
	}
	keyHash := sha256.Sum256(key)
	if !hmac.Equal([]byte(expectedKeyID), []byte(hex.EncodeToString(keyHash[:8]))) {
		return nil, fmt.Errorf("%w: durable secret key mismatch", store.ErrInvalidArgument)
	}
	payload, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid sealed turn secrets", store.ErrInvalidArgument)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(payload) < gcm.NonceSize() {
		return nil, fmt.Errorf("%w: invalid sealed turn secrets", store.ErrInvalidArgument)
	}
	plain, err := gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], aad)
	if err != nil {
		return nil, fmt.Errorf("%w: open sealed turn secrets", store.ErrInvalidArgument)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(plain)
	if !hmac.Equal([]byte(expectedDigest), []byte(hex.EncodeToString(mac.Sum(nil)))) {
		return nil, fmt.Errorf("%w: turn secret digest mismatch", store.ErrInvalidArgument)
	}
	var secrets map[string]string
	if err := json.Unmarshal(plain, &secrets); err != nil || strings.TrimSpace(secrets["Password"]) == "" {
		return nil, fmt.Errorf("%w: invalid turn secrets", store.ErrInvalidArgument)
	}
	return secrets, nil
}

func normalizeImageDigest(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "sha256:")
	if value == "" {
		return "", nil
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("%w: image digest must be SHA-256", store.ErrInvalidArgument)
	}
	return value, nil
}

// StableImageDigest is the transport helper for raw uploaded image bytes. Raw
// bytes are never retained in the durable envelope.
func StableImageDigest(image []byte) string {
	if len(image) == 0 {
		return ""
	}
	sum := sha256.Sum256(image)
	return hex.EncodeToString(sum[:])
}

func sanitizeInteractionArgs(args map[string]any) map[string]any {
	out := make(map[string]any)
	for key, value := range args {
		lower := strings.ToLower(strings.ReplaceAll(key, "_", ""))
		if strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "accesskey") || strings.Contains(lower, "privatekey") {
			continue
		}
		switch typed := value.(type) {
		case map[string]any:
			out[key] = sanitizeInteractionArgs(typed)
		case string:
			out[key] = redactTurnOutput(typed)
		default:
			out[key] = value
		}
	}
	return out
}

func redactTurnOutput(text string) string {
	return guardrails.RedactOutputLeak(guardrails.RedactPII(text))
}

func buildContinuityAdvisories(
	readOnly bool,
	sameTurn []RestoredActionAdvisory,
	prior []store.ContinuityAdvisory,
) engine.ContinuityAdvisories {
	notices := make([]string, 0, len(sameTurn)+len(prior))
	for _, item := range sameTurn {
		if strings.TrimSpace(item.ActionName) == "" {
			continue
		}
		var notice string
		switch item.Outcome {
		case "succeeded":
			notice = fmt.Sprintf("本轮恢复前，操作 %s 已确认成功%s；不要重复执行，请据此完成回答",
				item.ActionName, continuityHintText(item.ContextHint))
		case "failed":
			notice = fmt.Sprintf("本轮恢复前，操作 %s 已确认失败%s；不要重复执行，请向用户说明失败结果",
				item.ActionName, continuityHintText(item.ContextHint))
		default:
			continue
		}
		notices = append(notices, notice)
	}
	for _, item := range prior {
		switch item.Kind {
		case store.ContinuityAdvisoryKnownSuccess:
			if strings.TrimSpace(item.ActionName) == "" {
				continue
			}
			notices = append(notices, fmt.Sprintf(
				"此前第 %d 轮的操作 %s 后来确认成功%s；这只用于理解后续问题，新操作仍须重新查询并确认",
				item.TurnSequence, item.ActionName, continuityHintText(item.ContextHint),
			))
		case store.ContinuityAdvisoryAmbiguous:
			notices = append(notices, fmt.Sprintf(
				"此前第 %d 轮有操作结果无法确认；不得假定成功或失败，应先查询当前状态",
				item.TurnSequence,
			))
		}
	}
	return engine.ContinuityAdvisories{ReadOnly: readOnly, Notices: notices}
}

func continuityHintText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var hint store.ActionContextHint
	if err := json.Unmarshal(raw, &hint); err != nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if len(hint.ResourceIDs) > 0 {
		parts = append(parts, "资源 "+strings.Join(hint.ResourceIDs, "、"))
	}
	if hint.Region != "" {
		parts = append(parts, "地域 "+hint.Region)
	}
	if hint.Zone != "" {
		parts = append(parts, "可用区 "+hint.Zone)
	}
	if len(parts) == 0 {
		return ""
	}
	return "（" + strings.Join(parts, "，") + "）"
}

func (c *Coordinator) appendEvent(ctx context.Context, owner store.Owner, lease store.ConversationLease, eventType string, payload any) (Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	event, err := c.turns.AppendEvent(ctx, owner, lease, eventType, raw, true)
	if err != nil {
		return Event{}, err
	}
	return eventFromStore(event), nil
}

func eventFromStore(event store.TurnEvent) Event {
	return Event{TurnID: event.TurnID, Seq: event.Seq, LeaseEpoch: event.LeaseEpoch, Type: event.Type, Payload: event.Payload, Provisional: event.Provisional, CreatedAt: event.CreatedAt}
}

func (c *Coordinator) fail(owner store.Owner, lease store.ConversationLease, reason string) store.Turn {
	return c.failAs(owner, lease, store.TurnStatusFailedRetryable, reason)
}

// failAs keeps reconciling while the coordinator is alive. A transient DB
// failure must not leave a running/awaiting turn permanently at the queue
// head. If the old lease expires, this worker reacquires the same turn under a
// new fence before writing the failure; it never skips to a later turn.
func (c *Coordinator) failAs(owner store.Owner, lease store.ConversationLease, desired store.TurnStatus, reason string) store.Turn {
	current := lease
	for {
		select {
		case <-c.ctx.Done():
			return store.Turn{}
		default:
		}
		attemptCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		turn, getErr := c.turns.GetTurn(attemptCtx, owner, current.TurnID)
		if getErr == nil && (turn.Status.Terminal() || turn.Status == desired) {
			cancel()
			c.publishAvailable(owner, turn.ID)
			return turn
		}
		failed, failErr := c.turns.FailTurn(attemptCtx, owner, current, desired, reason)
		cancel()
		if failErr == nil {
			c.publishAvailable(owner, failed.ID)
			return failed
		}
		if errors.Is(failErr, store.ErrLeaseFenced) || errors.Is(failErr, store.ErrInvalidTurnState) || time.Now().After(current.LeaseUntil) {
			acquireCtx, acquireCancel := context.WithTimeout(context.Background(), 2*time.Second)
			replacement, acquireErr := c.turns.AcquireConversationLease(acquireCtx, owner, current.SessionID, current.TurnID, current.HolderID, c.opts.LeaseTTL)
			acquireCancel()
			if acquireErr == nil {
				current = replacement
				continue
			}
		}
		timer := time.NewTimer(max(c.opts.InteractionPoll, 50*time.Millisecond))
		select {
		case <-c.ctx.Done():
			timer.Stop()
			return store.Turn{}
		case <-timer.C:
		}
	}
}

func traceAttemptID(turnID string, leaseEpoch int64) string {
	return fmt.Sprintf("%s:e%d", turnID, leaseEpoch)
}

// Keep persisted protocol reasons enumerable. Store errors may contain SQL,
// model or upstream text; those details are never copied into continuity.
func boundedContinuityReason(reason string) string {
	switch reason {
	case "executor_stopped", "turn_reload_failed", "execution_envelope_missing",
		"execution_envelope_unsupported", "execution_envelope_invalid",
		"event_persist_failed", "context_read_failed", "engine_build_failed",
		"trace_engine_unsupported", "action_recovery_read_failed",
		"continuity_read_failed", "execution_failed", "lease_lost",
		"interaction_expired", "action_outcome_uncertain",
		"action_replay_incomplete", "context_snapshot_invalid",
		"context_encode_failed", "empty_answer", "turn_not_saved":
		return reason
	default:
		return "other"
	}
}

func (c *Coordinator) publishAvailable(owner store.Owner, turnID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	events, err := c.turns.ListEvents(ctx, owner, turnID, 0, 1000)
	if err != nil {
		return
	}
	for _, event := range events {
		c.deliver(eventFromStore(event))
	}
}

func (c *Coordinator) subscribe(ctx context.Context, owner store.Owner, turnID string, lastSeq int64, sink EventSink) {
	sub := &subscription{sink: sink, lastSeq: lastSeq, ctx: ctx}
	c.mu.Lock()
	if c.subs[turnID] == nil {
		c.subs[turnID] = make(map[*subscription]struct{})
	}
	c.subs[turnID][sub] = struct{}{}
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		c.pollSubscription(owner, turnID, sub)
	}()
}

func (c *Coordinator) pollSubscription(owner store.Owner, turnID string, sub *subscription) {
	ticker := time.NewTicker(c.opts.InteractionPoll)
	defer ticker.Stop()
	defer c.removeSubscription(turnID, sub)
	for {
		if c.pollOnce(owner, turnID, sub) {
			return
		}
		select {
		case <-c.ctx.Done():
			return
		case <-sub.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Coordinator) pollOnce(owner store.Owner, turnID string, sub *subscription) bool {
	sub.mu.Lock()
	after := sub.lastSeq
	done := sub.done
	sub.mu.Unlock()
	if done {
		return true
	}
	events, err := c.turns.ListEvents(sub.ctx, owner, turnID, after, 100)
	if err != nil {
		return false
	}
	for _, event := range events {
		if c.deliverTo(sub, eventFromStore(event)) {
			return true
		}
	}
	if len(events) == 0 {
		turn, err := c.turns.GetTurn(sub.ctx, owner, turnID)
		if err == nil && turn.Status.Terminal() {
			terminalSeq := turn.NextEventSeq - 1
			if after < terminalSeq {
				// The first ListEvents may have raced immediately before the commit
				// transaction became visible while GetTurn raced immediately after it.
				// Re-read rather than closing and losing the terminal event.
				committedEvents, listErr := c.turns.ListEvents(sub.ctx, owner, turnID, after, 100)
				if listErr != nil {
					return false
				}
				for _, event := range committedEvents {
					if c.deliverTo(sub, eventFromStore(event)) {
						return true
					}
				}
				return false
			}
			sub.mu.Lock()
			sub.done = true
			sub.mu.Unlock()
			return true
		}
	}
	return false
}

func (c *Coordinator) replayNow(owner store.Owner, turnID string) error {
	c.mu.Lock()
	subs := make([]*subscription, 0, len(c.subs[turnID]))
	for sub := range c.subs[turnID] {
		subs = append(subs, sub)
	}
	c.mu.Unlock()
	events, err := c.turns.ListEvents(context.Background(), owner, turnID, 0, 1000)
	if err != nil {
		return err
	}
	for _, raw := range events {
		for _, sub := range subs {
			c.deliverTo(sub, eventFromStore(raw))
		}
	}
	return nil
}

func (c *Coordinator) deliver(event Event) {
	c.mu.Lock()
	subs := make([]*subscription, 0, len(c.subs[event.TurnID]))
	for sub := range c.subs[event.TurnID] {
		subs = append(subs, sub)
	}
	c.mu.Unlock()
	for _, sub := range subs {
		c.deliverTo(sub, event)
	}
}

func (c *Coordinator) deliverTo(sub *subscription, event Event) bool {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.done || event.Seq <= sub.lastSeq {
		return sub.done
	}
	if err := sub.sink(event); err != nil {
		sub.done = true
		return true
	}
	sub.lastSeq = event.Seq
	if !event.Provisional || event.Type == "turn.failed" || event.Type == "turn.reconciled" {
		sub.done = true
	}
	return sub.done
}

func (c *Coordinator) removeSubscription(turnID string, sub *subscription) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.subs[turnID], sub)
	if len(c.subs[turnID]) == 0 {
		delete(c.subs, turnID)
	}
}

func intPtr(value int) *int {
	if value == 0 {
		return nil
	}
	return &value
}
