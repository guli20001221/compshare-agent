package turncoord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
)

type actionStore interface {
	ReserveAction(context.Context, store.Owner, store.ConversationLease, store.ReserveActionInput) (store.TurnAction, bool, error)
	StartAction(context.Context, store.Owner, store.ConversationLease, string) (store.TurnAction, error)
	RecordAction(context.Context, store.Owner, string, store.ActionStatus, json.RawMessage, *string, ...*string) (store.TurnAction, error)
}

// ActionJournal is one turn's stable sequence of external mutations. It is
// intentionally per-turn: neither SharedDeps nor another session can observe
// or advance its index.
type ActionJournal struct {
	store actionStore
	owner store.Owner
	lease store.ConversationLease

	mu        sync.Mutex
	nextIndex int
	poisoned  error
}

func NewActionJournal(actions actionStore, owner store.Owner, lease store.ConversationLease) *ActionJournal {
	if actions == nil {
		panic("turncoord: action journal requires a store")
	}
	return &ActionJournal{store: actions, owner: owner, lease: lease}
}

// Execute implements tools.ActionJournal.
func (j *ActionJournal) Execute(ctx context.Context, action string, args map[string]any, call tools.ActionCall) (map[string]any, error) {
	if j == nil || j.store == nil || call == nil || action == "" {
		return nil, fmt.Errorf("%w: invalid action journal", tools.ErrActionJournalRequired)
	}
	index, err := j.claimIndex()
	if err != nil {
		return nil, err
	}
	argsHash := canonicalActionArgsHash(args)
	if argsHash == "" {
		return nil, j.poison(fmt.Errorf("action arguments cannot be canonicalized"))
	}
	reserved, _, err := j.store.ReserveAction(ctx, j.owner, j.lease, store.ReserveActionInput{
		Index: index, ActionName: action, ArgsHash: argsHash,
	})
	if err != nil {
		return nil, j.poison(fmt.Errorf("reserve %s: %w", action, err))
	}

	switch reserved.Status {
	case store.ActionStatusSucceeded:
		result, decodeErr := decodeActionResult(reserved.Result)
		if decodeErr != nil {
			return nil, j.poison(decodeErr)
		}
		return result, nil
	case store.ActionStatusFailed:
		return nil, decodeDefiniteActionError(reserved)
	case store.ActionStatusAmbiguous:
		return nil, j.poison(fmt.Errorf("%s already ambiguous", action))
	case store.ActionStatusReserved:
		if reserved.InFlight {
			return nil, j.poison(fmt.Errorf("%s already started", action))
		}
	default:
		return nil, j.poison(fmt.Errorf("unknown journal status %q", reserved.Status))
	}

	started, err := j.store.StartAction(ctx, j.owner, j.lease, reserved.ExecutionToken)
	if err != nil {
		return nil, j.poison(fmt.Errorf("start %s: %w", action, err))
	}

	result, callErr := call(ctx, action, args)
	if callErr == nil {
		raw, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return j.recordAmbiguous(ctx, started, action, fmt.Errorf("encode action result: %w", marshalErr))
		}
		requestID := upstreamRequestID(result)
		if _, recordErr := j.store.RecordAction(ctx, j.owner, started.ExecutionToken, store.ActionStatusSucceeded, raw, nil, requestID); recordErr != nil {
			return nil, j.poison(fmt.Errorf("%s result was not durably recorded: %w", action, recordErr))
		}
		return result, nil
	}

	if apiErr, ok := tools.UpstreamAPIErrorFrom(callErr); ok {
		raw, _ := json.Marshal(map[string]any{"code": apiErr.Code, "message": apiErr.Message})
		code := "upstream_api:" + strconv.Itoa(apiErr.Code)
		if _, recordErr := j.store.RecordAction(ctx, j.owner, started.ExecutionToken, store.ActionStatusFailed, raw, &code); recordErr != nil {
			return nil, j.poison(fmt.Errorf("%s definite failure was not durably recorded: %w", action, recordErr))
		}
		return nil, callErr
	}
	return j.recordAmbiguous(ctx, started, action, callErr)
}

func (j *ActionJournal) claimIndex() (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.poisoned != nil {
		return 0, fmt.Errorf("%w: turn journal stopped after: %v", tools.ErrActionOutcomeUncertain, j.poisoned)
	}
	index := j.nextIndex
	j.nextIndex++
	return index, nil
}

func (j *ActionJournal) recordAmbiguous(ctx context.Context, started store.TurnAction, action string, cause error) (map[string]any, error) {
	detail, _ := json.Marshal(map[string]any{"error": safeErrorClass(cause)})
	code := safeErrorClass(cause)
	if _, err := j.store.RecordAction(ctx, j.owner, started.ExecutionToken, store.ActionStatusAmbiguous, detail, &code); err != nil {
		return nil, j.poison(fmt.Errorf("%s failed and ambiguity could not be recorded: %w", action, err))
	}
	return nil, j.poison(fmt.Errorf("%s: %w", action, cause))
}

func (j *ActionJournal) poison(cause error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.poisoned == nil {
		j.poisoned = cause
	}
	return fmt.Errorf("%w: %v", tools.ErrActionOutcomeUncertain, j.poisoned)
}

func canonicalActionArgsHash(args map[string]any) string {
	// encoding/json sorts map keys recursively. Hashing those bytes makes a
	// stable identity independent of Go map iteration order without treating
	// the local execution token as an upstream idempotency key.
	raw, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func decodeActionResult(raw json.RawMessage) (map[string]any, error) {
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("%w: stored action result is unreadable", tools.ErrActionOutcomeUncertain)
	}
	return result, nil
}

func decodeDefiniteActionError(action store.TurnAction) error {
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if action.ErrorCode != nil && strings.HasPrefix(*action.ErrorCode, "upstream_api:") && json.Unmarshal(action.Result, &payload) == nil && payload.Code != 0 {
		return tools.NewUpstreamAPIError(payload.Code, payload.Message)
	}
	return fmt.Errorf("action failed deterministically: %s", string(action.Result))
}

func upstreamRequestID(result map[string]any) *string {
	for _, key := range []string{"RequestId", "RequestID", "request_id", "requestId"} {
		if value, ok := result[key].(string); ok && strings.TrimSpace(value) != "" {
			clean := strings.TrimSpace(value)
			return &clean
		}
	}
	return nil
}

func safeErrorClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	default:
		return "unknown_external_error"
	}
}

var _ tools.ActionJournal = (*ActionJournal)(nil)
