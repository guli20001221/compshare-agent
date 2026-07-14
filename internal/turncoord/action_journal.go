package turncoord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
)

type actionStore interface {
	ListTurnActions(context.Context, store.Owner, string) ([]store.TurnAction, error)
	AbandonUnstartedActions(context.Context, store.Owner, store.ConversationLease) error
	ReserveAction(context.Context, store.Owner, store.ConversationLease, store.ReserveActionInput) (store.TurnAction, bool, error)
	StartAction(context.Context, store.Owner, store.ConversationLease, string) (store.TurnAction, error)
	RecordActionWithContext(context.Context, store.Owner, string, store.ActionStatus, json.RawMessage, *string, *string, json.RawMessage) (store.TurnAction, error)
}

// RestoredActionAdvisory is the only information a later engine may consume
// from a known-success action. It intentionally excludes raw results, args,
// identity, confirmation, and any authorization signal.
type RestoredActionAdvisory struct {
	Index       int
	ActionName  string
	Outcome     string
	ContextHint json.RawMessage
	ErrorCode   string
}

// ActionJournal is one turn's stable sequence of external mutations. It is
// intentionally per-turn: neither SharedDeps nor another session can observe
// or advance its index.
type ActionJournal struct {
	store actionStore
	owner store.Owner
	lease store.ConversationLease

	mu           sync.Mutex
	nextIndex    int
	poisoned     error
	loaded       bool
	baseline     []store.TurnAction
	replayOnly   bool
	replayCursor int
	consumed     map[int]bool
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
	if err := j.loadBaseline(ctx); err != nil {
		return nil, err
	}
	argsHash := canonicalActionArgsHash(args)
	if argsHash == "" {
		return nil, j.poison(fmt.Errorf("action arguments cannot be canonicalized"))
	}
	index, expected, replayOnly, err := j.claimAction(action, argsHash)
	if err != nil {
		return nil, err
	}
	if replayOnly {
		if expected == nil || expected.Index != index || expected.ActionName != action || expected.ArgsHash != argsHash {
			return nil, j.poison(fmt.Errorf("replay action %d differs from durable plan", index))
		}
	}
	reserved, _, err := j.store.ReserveAction(ctx, j.owner, j.lease, store.ReserveActionInput{
		Index: index, ActionName: action, ArgsHash: argsHash, ContextHint: actionContextHint(args, nil),
	})
	if err != nil {
		return nil, j.poison(fmt.Errorf("reserve %s: %w", action, err))
	}
	if replayOnly {
		if reserved.Index != index || reserved.ActionName != action || reserved.ArgsHash != argsHash {
			return nil, j.poison(fmt.Errorf("replay action %d resolved to a different durable action", index))
		}
		j.markConsumed(index)
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
		if _, recordErr := j.store.RecordActionWithContext(ctx, j.owner, started.ExecutionToken, store.ActionStatusSucceeded, raw, nil, requestID, actionContextHint(args, result)); recordErr != nil {
			return nil, j.poison(fmt.Errorf("%s result was not durably recorded: %w", action, recordErr))
		}
		return result, nil
	}

	// A non-zero upstream business response is not proof that the write did not
	// happen. Gate/policy/argument/confirmation rejections occur before this
	// journal boundary; every error returned after StartAction is therefore an
	// unknown external outcome unless a future API supplies a verified upstream
	// idempotency contract.
	return j.recordAmbiguous(ctx, started, action, callErr)
}

// RestoredActionAdvisory consumes only known-success actions from an older
// lease. This lets the next engine answer from a safe outcome summary without
// having to coincidentally call the same write tool again. Replay-only still
// rejects every additional or changed action.
func (j *ActionJournal) RestoredActionAdvisory(ctx context.Context) ([]RestoredActionAdvisory, error) {
	if err := j.loadBaseline(ctx); err != nil {
		return nil, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.replayOnly {
		return nil, nil
	}
	out := make([]RestoredActionAdvisory, 0)
	for _, action := range j.baseline {
		if action.LeaseEpoch == j.lease.Epoch || (action.Status != store.ActionStatusSucceeded && action.Status != store.ActionStatusFailed) {
			continue
		}
		outcome := "succeeded"
		errorCode := ""
		if action.Status == store.ActionStatusFailed {
			outcome = "failed"
			if action.ErrorCode != nil {
				errorCode = *action.ErrorCode
			}
		}
		out = append(out, RestoredActionAdvisory{
			Index: action.Index, ActionName: action.ActionName, Outcome: outcome,
			ContextHint: append(json.RawMessage(nil), action.ContextHint...), ErrorCode: errorCode,
		})
		j.consumed[action.Index] = true
	}
	return out, nil
}

// Err exposes the in-memory commit barrier. Store state alone is insufficient:
// a reservation/start/result transaction may have rolled back while its COMMIT
// acknowledgement was lost, leaving no row that CommitTurn could inspect.
func (j *ActionJournal) Err() error {
	if j == nil {
		return tools.ErrActionJournalRequired
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.poisoned == nil {
		return nil
	}
	return fmt.Errorf("%w: %v", tools.ErrActionOutcomeUncertain, j.poisoned)
}

// VerifyComplete is the coordinator's pre-commit replay barrier. Once a prior
// lease crossed an action's before-call boundary, a takeover may only replay
// the exact durable action list; it cannot omit an old action or add a new one
// with different parameters.
func (j *ActionJournal) VerifyComplete(ctx context.Context) error {
	if err := j.loadBaseline(ctx); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.poisoned != nil {
		return fmt.Errorf("%w: %v", tools.ErrActionOutcomeUncertain, j.poisoned)
	}
	if !j.replayOnly {
		return nil
	}
	for _, action := range j.baseline {
		if !j.consumed[action.Index] {
			j.poisoned = fmt.Errorf("durable action %d was not replayed", action.Index)
			return fmt.Errorf("%w: %v", tools.ErrActionOutcomeUncertain, j.poisoned)
		}
	}
	return nil
}

func (j *ActionJournal) loadBaseline(ctx context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.loaded {
		if j.poisoned != nil {
			return fmt.Errorf("%w: %v", tools.ErrActionOutcomeUncertain, j.poisoned)
		}
		return nil
	}
	if err := j.store.AbandonUnstartedActions(ctx, j.owner, j.lease); err != nil {
		j.poisoned = fmt.Errorf("abandon unstarted durable actions: %w", err)
		j.loaded = true
		return fmt.Errorf("%w: %v", tools.ErrActionOutcomeUncertain, j.poisoned)
	}
	actions, err := j.store.ListTurnActions(ctx, j.owner, j.lease.TurnID)
	if err != nil {
		j.poisoned = fmt.Errorf("load durable action plan: %w", err)
		j.loaded = true
		return fmt.Errorf("%w: %v", tools.ErrActionOutcomeUncertain, j.poisoned)
	}
	j.loaded = true
	j.baseline = make([]store.TurnAction, 0, len(actions))
	j.consumed = make(map[int]bool, len(actions))
	for _, action := range actions {
		if action.Index >= j.nextIndex {
			j.nextIndex = action.Index + 1
		}
		if action.Status == store.ActionStatusAbandoned {
			continue
		}
		j.baseline = append(j.baseline, action)
		if action.LeaseEpoch != j.lease.Epoch && (action.Status != store.ActionStatusReserved || action.InFlight) {
			j.replayOnly = true
		}
	}
	return nil
}

func (j *ActionJournal) claimAction(actionName, argsHash string) (int, *store.TurnAction, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.poisoned != nil {
		return 0, nil, false, fmt.Errorf("%w: turn journal stopped after: %v", tools.ErrActionOutcomeUncertain, j.poisoned)
	}
	if j.replayOnly {
		// Semantic duplicates never create a second durable row. A replay may
		// call X,X,Y while the stored rows are X(index 0),Y(index 2): the second
		// X reuses the consumed result without advancing the durable cursor.
		for _, existing := range j.baseline {
			if j.consumed[existing.Index] && existing.ActionName == actionName && existing.ArgsHash == argsHash {
				expected := existing
				return existing.Index, &expected, true, nil
			}
		}
		for j.replayCursor < len(j.baseline) && j.consumed[j.baseline[j.replayCursor].Index] {
			j.replayCursor++
		}
		if j.replayCursor >= len(j.baseline) {
			j.poisoned = fmt.Errorf("replay attempted an extra action after %d durable actions", len(j.baseline))
			return 0, nil, true, fmt.Errorf("%w: %v", tools.ErrActionOutcomeUncertain, j.poisoned)
		}
		expected := j.baseline[j.replayCursor]
		if expected.ActionName != actionName || expected.ArgsHash != argsHash {
			j.poisoned = fmt.Errorf("replay action differs from durable action index %d", expected.Index)
			return 0, nil, true, fmt.Errorf("%w: %v", tools.ErrActionOutcomeUncertain, j.poisoned)
		}
		j.replayCursor++
		return expected.Index, &expected, true, nil
	}
	index := j.nextIndex
	j.nextIndex++
	return index, nil, false, nil
}

func (j *ActionJournal) markConsumed(index int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.consumed != nil {
		j.consumed[index] = true
	}
}

func (j *ActionJournal) recordAmbiguous(ctx context.Context, started store.TurnAction, action string, cause error) (map[string]any, error) {
	detail, _ := json.Marshal(map[string]any{"error": safeErrorClass(cause)})
	code := safeErrorClass(cause)
	if _, err := j.store.RecordActionWithContext(ctx, j.owner, started.ExecutionToken, store.ActionStatusAmbiguous, detail, &code, nil, started.ContextHint); err != nil {
		return nil, j.poison(fmt.Errorf("%s failed and ambiguity could not be recorded: %w", action, err))
	}
	return nil, j.poison(fmt.Errorf("%s: %w", action, cause))
}

func actionContextHint(args, result map[string]any) json.RawMessage {
	hint := store.ActionContextHint{}
	seen := make(map[string]struct{})
	collect := func(values map[string]any) {}
	collect = func(values map[string]any) {
		for key, value := range values {
			normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", ""))
			switch normalized {
			case "uhostid", "uhostids", "instanceid", "instanceids", "compshareinstanceid", "compshareinstanceids", "resourceid", "resourceids", "imageid", "imageids", "schedulerid":
				for _, id := range hintStrings(value) {
					if len(hint.ResourceIDs) >= 16 || !safeHintAtom(id, 128) {
						continue
					}
					if _, ok := seen[id]; !ok {
						seen[id] = struct{}{}
						hint.ResourceIDs = append(hint.ResourceIDs, id)
					}
				}
			case "region":
				if hint.Region == "" {
					hint.Region = safeHintString(value, 64)
				}
			case "zone", "availabilityzone":
				if hint.Zone == "" {
					hint.Zone = safeHintString(value, 64)
				}
			}
		}
	}
	collect(args)
	collect(result)
	raw, _ := json.Marshal(hint)
	return raw
}

func hintStrings(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{strings.TrimSpace(typed)}
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				out = append(out, strings.TrimSpace(text))
			}
		}
		return out
	default:
		return nil
	}
}

func safeHintString(value any, limit int) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	text = strings.TrimSpace(text)
	if !safeHintAtom(text, limit) {
		return ""
	}
	return text
}

func safeHintAtom(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("._:/-", r) {
			continue
		}
		return false
	}
	return true
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
	for _, key := range []string{
		"RequestId", "RequestID", "request_id", "requestId",
		"RequestUuid", "RequestUUID", "request_uuid", "requestUuid",
	} {
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
		if _, ok := tools.UpstreamAPIErrorFrom(err); ok {
			return "upstream_business_error"
		}
		return "unknown_external_error"
	}
}

var _ tools.ActionJournal = (*ActionJournal)(nil)
