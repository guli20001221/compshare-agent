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
	ErrTurnNotFound         = errors.New("store: turn not found")
	ErrConversationNotFound = errors.New("store: conversation not found")
	ErrLeaseFenced          = errors.New("store: conversation lease fenced")
	ErrLeaseHeld            = errors.New("store: conversation lease held by another executor")
	ErrIdempotencyConflict  = errors.New("store: idempotency key reused with different request")
	ErrContextConflict      = errors.New("store: context version conflict")
	ErrCommitConflict       = errors.New("store: committed result differs from this attempt")
	ErrTurnOutOfOrder       = errors.New("store: an earlier turn must finish first")
	ErrInteractionConflict  = errors.New("store: interaction payload conflict")
	ErrInteractionExpired   = errors.New("store: interaction expired")
	ErrActionConflict       = errors.New("store: action reservation conflict")
	ErrActionNotFound       = errors.New("store: action not found")
	ErrActionUncertain      = errors.New("store: action execution outcome is uncertain")
	ErrInteractionPending   = errors.New("store: turn interaction is still pending")
	ErrInvalidTurnState     = errors.New("store: invalid turn state transition")
	ErrInvalidArgument      = errors.New("store: invalid argument")
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

// MayHaveExecuted is deliberately conservative. A reservation can cross a
// process crash after the external side effect but before RecordAction.
func (s ActionStatus) MayHaveExecuted() bool {
	return s == ActionStatusReserved || s == ActionStatusSucceeded || s == ActionStatusAmbiguous
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
	CreatedAt         time.Time
	UpdatedAt         time.Time
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
