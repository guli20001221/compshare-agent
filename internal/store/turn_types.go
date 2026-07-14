package store

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"time"
)

var (
	ErrTurnNotFound             = errors.New("store: turn not found")
	ErrConversationNotFound     = errors.New("store: conversation not found")
	ErrLeaseFenced              = errors.New("store: conversation lease fenced")
	ErrLeaseHeld                = errors.New("store: conversation lease held by another executor")
	ErrIdempotencyConflict      = errors.New("store: idempotency key reused with different request")
	ErrContextConflict          = errors.New("store: context version conflict")
	ErrCommitConflict           = errors.New("store: committed result differs from this attempt")
	ErrTurnOutOfOrder           = errors.New("store: an earlier turn must finish first")
	ErrInteractionConflict      = errors.New("store: interaction payload conflict")
	ErrInteractionExpired       = errors.New("store: interaction expired")
	ErrActionConflict           = errors.New("store: action reservation conflict")
	ErrActionNotFound           = errors.New("store: action not found")
	ErrActionUncertain          = errors.New("store: action execution outcome is uncertain")
	ErrInteractionPending       = errors.New("store: turn interaction is still pending")
	ErrExecutionEnvelopeMissing = errors.New("store: durable execution envelope missing")
	ErrInvalidTurnState         = errors.New("store: invalid turn state transition")
	ErrInvalidArgument          = errors.New("store: invalid argument")
)

type TurnStatus string

const (
	TurnStatusAccepted             TurnStatus = "accepted"
	TurnStatusRunning              TurnStatus = "running"
	TurnStatusAwaitingConfirmation TurnStatus = "awaiting_confirmation"
	TurnStatusCommitting           TurnStatus = "committing"
	TurnStatusCommitted            TurnStatus = "committed"
	TurnStatusFailedRetryable      TurnStatus = "failed_retryable"
	TurnStatusAmbiguousAfterAction TurnStatus = "ambiguous_after_action"
	TurnStatusAborted              TurnStatus = "aborted"
)

func (s TurnStatus) Valid() bool {
	switch s {
	case TurnStatusAccepted, TurnStatusRunning, TurnStatusAwaitingConfirmation,
		TurnStatusCommitting, TurnStatusCommitted, TurnStatusFailedRetryable,
		TurnStatusAmbiguousAfterAction, TurnStatusAborted:
		return true
	default:
		return false
	}
}

func (s TurnStatus) Terminal() bool {
	switch s {
	case TurnStatusCommitted, TurnStatusAmbiguousAfterAction, TurnStatusAborted:
		return true
	default:
		return false
	}
}

type ActionStatus string

const (
	ActionStatusReserved  ActionStatus = "reserved"
	ActionStatusSucceeded ActionStatus = "succeeded"
	ActionStatusFailed    ActionStatus = "failed"
	ActionStatusAmbiguous ActionStatus = "ambiguous"
)

func (s ActionStatus) Valid() bool {
	switch s {
	case ActionStatusReserved, ActionStatusSucceeded, ActionStatusFailed, ActionStatusAmbiguous:
		return true
	default:
		return false
	}
}

type InteractionStatus string

const (
	InteractionStatusPending  InteractionStatus = "pending"
	InteractionStatusResolved InteractionStatus = "resolved"
)

// ContextWriteMode makes context mutation an explicit part of the durable
// commit contract. The zero value is intentionally invalid: callers handling
// a state schema they cannot understand must choose Preserve rather than
// accidentally overwriting it with an empty/default state.
type ContextWriteMode string

const (
	ContextWriteUpdate   ContextWriteMode = "update"
	ContextWritePreserve ContextWriteMode = "preserve"
)

func (m ContextWriteMode) Valid() bool {
	return m == ContextWriteUpdate || m == ContextWritePreserve
}

type Turn struct {
	ID                      string
	SessionID               string
	Owner                   Owner
	ClientTurnID            string
	Sequence                int64
	RequestHash             string
	Status                  TurnStatus
	UserMessageID           string
	AssistantMessageID      string
	BaseContextVersion      int
	CommittedContextVersion *int
	CommittedLeaseEpoch     *int64
	CommitHash              *string
	ErrorCode               *string
	ExecutorID              *string
	LeaseEpoch              *int64
	HasExternalAction       bool
	ExecutionEnvelope       json.RawMessage
	NextEventSeq            int64
	CreatedAt               time.Time
	UpdatedAt               time.Time
	StartedAt               *time.Time
	FinishedAt              *time.Time
	CommittedAt             *time.Time
}

type ConversationLease struct {
	SessionID  string
	TurnID     string
	HolderID   string
	Epoch      int64
	LeaseUntil time.Time
}

type TurnEvent struct {
	TurnID      string
	Seq         int64
	LeaseEpoch  int64
	Type        string
	Payload     json.RawMessage
	Provisional bool
	CreatedAt   time.Time
}

type TurnAction struct {
	TurnID            string
	Index             int
	LeaseEpoch        int64
	ActionName        string
	ArgsHash          string
	ExecutionToken    string
	InFlight          bool
	UpstreamRequestID *string
	Status            ActionStatus
	Result            json.RawMessage
	ErrorCode         *string
	// ContextHint is a strictly whitelisted continuity aid. It is never proof
	// of identity, ownership, confirmation, or permission to execute an action.
	ContextHint json.RawMessage
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ActionContextHint is the complete allowlist for action-derived conversational
// context. It carries resource breadcrumbs only, never permissions or approval.
type ActionContextHint struct {
	ResourceIDs []string `json:"resource_ids,omitempty"`
	Region      string   `json:"region,omitempty"`
	Zone        string   `json:"zone,omitempty"`
}

type TurnInteraction struct {
	ID              string
	TurnID          string
	Key             string
	Kind            string
	RequestHash     string
	RequestPayload  json.RawMessage
	LeaseEpoch      int64
	ExpiresAt       time.Time
	Status          InteractionStatus
	ResolutionHash  *string
	ResponsePayload json.RawMessage
	CreatedAt       time.Time
	ResolvedAt      *time.Time
}

type AcceptTurnInput struct {
	SessionID      string
	ClientTurnID   string
	RequestHash    string
	RequestUUID    *string
	UserContent    string
	UserMetadata   json.RawMessage
	AssistantModel *string
	// ExecutionEnvelope is the frozen, secret-free request needed by a later
	// replica to resume this turn. It is nullable only for migration compatibility.
	ExecutionEnvelope json.RawMessage
}

type CommitTurnInput struct {
	TurnID                 string
	Lease                  ConversationLease
	ExpectedContextVersion int
	ContextWriteMode       ContextWriteMode
	Context                json.RawMessage
	Assistant              AssistantPatch
	TerminalEventType      string
	TerminalEventPayload   json.RawMessage
}

type ReserveActionInput struct {
	Index             int
	ActionName        string
	ArgsHash          string
	UpstreamRequestID *string
	ContextHint       json.RawMessage
}

type RecoverableTurn struct {
	Turn              Turn
	ExecutionEnvelope json.RawMessage
}

type ContinuityAdvisoryKind string

const (
	ContinuityAdvisoryAmbiguous    ContinuityAdvisoryKind = "ambiguous"
	ContinuityAdvisoryAborted      ContinuityAdvisoryKind = "aborted"
	ContinuityAdvisoryKnownSuccess ContinuityAdvisoryKind = "known_success"
)

// ContinuityAdvisory is explanatory history only. Deliberately absent are
// arguments, confirmation state, identity assertions, and authorization flags.
type ContinuityAdvisory struct {
	Kind              ContinuityAdvisoryKind
	TurnID            string
	TurnSequence      int64
	ActionIndex       *int
	ActionName        string
	ContextHint       json.RawMessage
	UpstreamRequestID *string
	OccurredAt        time.Time
}

// MayHaveExecuted is true only once StartAction has crossed the durable
// before-call boundary, or when reconciliation already marked the outcome
// ambiguous. A plain reservation has not called upstream and is safe to claim
// under a later lease.
func (a TurnAction) MayHaveExecuted() bool {
	return (a.Status == ActionStatusReserved && a.InFlight) || a.Status == ActionStatusAmbiguous
}

// HashTurnRequest hashes framed fields rather than their concatenation, so
// ("ab", "c") and ("a", "bc") can never alias.
func HashTurnRequest(fields ...string) string {
	h := sha256.New()
	for _, field := range fields {
		writeHashField(h, []byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashJSON(value json.RawMessage) string {
	h := sha256.New()
	writeHashField(h, value)
	return hex.EncodeToString(h.Sum(nil))
}

func writeHashField(h hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(value)
}
