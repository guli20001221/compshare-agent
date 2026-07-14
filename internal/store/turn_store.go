package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PostgresTurnStore is the durable turn/lease/protocol store. It intentionally
// accepts one concrete *sql.DB: AcceptTurn, CommitTurn, and protocol writes must
// share the same transaction manager as sessions and messages. There is no
// non-transactional fallback.
type PostgresTurnStore struct {
	db *sql.DB
}

func NewPostgresTurnStore(db *sql.DB) *PostgresTurnStore {
	if db == nil {
		panic("store: NewPostgresTurnStore requires a non-nil *sql.DB")
	}
	return &PostgresTurnStore{db: db}
}

func (s *PostgresTurnStore) AcceptTurn(ctx context.Context, owner Owner, in AcceptTurnInput) (Turn, bool, error) {
	if in.SessionID == "" || in.ClientTurnID == "" || len(in.ClientTurnID) > 128 || len(in.RequestHash) != 64 || strings.TrimSpace(in.UserContent) == "" {
		return Turn{}, false, fmt.Errorf("%w: invalid turn identity", ErrInvalidArgument)
	}
	canonicalEnvelope, err := canonicalOptionalObject(in.ExecutionEnvelope)
	if err != nil {
		return Turn{}, false, fmt.Errorf("%w: invalid execution envelope", ErrInvalidArgument)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Turn{}, false, fmt.Errorf("accept turn begin: %w", err)
	}
	defer tx.Rollback()

	contextVersion, err := lockSession(ctx, tx, owner, in.SessionID)
	if err != nil {
		return Turn{}, false, err
	}

	existing, err := getTurnByClientIDForUpdate(ctx, tx, owner, in.SessionID, in.ClientTurnID)
	if err == nil {
		if existing.RequestHash != in.RequestHash {
			return Turn{}, false, ErrIdempotencyConflict
		}
		if err := tx.Commit(); err != nil {
			return Turn{}, false, fmt.Errorf("accept existing turn commit: %w", err)
		}
		return existing, false, nil
	}
	if !errors.Is(err, ErrTurnNotFound) {
		return Turn{}, false, err
	}
	// Admission is serialized by the session row lock acquired above. Preserve
	// idempotent retries first, then refuse a different logical turn while any
	// prior turn is still non-terminal. This prevents a second user message from
	// being persisted behind work the client may cancel, retry or replace.
	if err := ensureNoOtherOpenTurn(ctx, tx, owner, in.SessionID); err != nil {
		return Turn{}, false, err
	}
	turnSequence, err := nextTurnSequence(ctx, tx, in.SessionID)
	if err != nil {
		return Turn{}, false, err
	}

	turnID := uuid.NewString()
	userMessageID := uuid.NewString()
	assistantMessageID := uuid.NewString()
	turn, err := insertTurn(ctx, tx, owner, in, turnID, userMessageID, assistantMessageID, contextVersion, turnSequence, canonicalEnvelope)
	if err != nil {
		return Turn{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO messages
  (id, session_id, request_uuid, role, content, status, model, metadata, turn_id, turn_role)
VALUES
  ($1, $2, $3, 'user', $4, 'pending', NULL, $5, $6, 'user'),
  ($7, $2, $3, 'assistant', '', 'pending', $8, NULL, $6, 'assistant')
`, userMessageID, in.SessionID, nullableRequestUUID(in.RequestUUID), in.UserContent,
		nullableJSON(in.UserMetadata), turnID, assistantMessageID, nullableStringPtr(in.AssistantModel)); err != nil {
		return Turn{}, false, fmt.Errorf("accept turn insert pending messages: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Turn{}, false, fmt.Errorf("accept turn commit: %w", err)
	}
	return turn, true, nil
}

func (s *PostgresTurnStore) GetTurn(ctx context.Context, owner Owner, turnID string) (Turn, error) {
	turn, err := scanTurn(s.db.QueryRowContext(ctx, turnSelect+`
WHERE id = $1 AND top_organization_id = $2 AND organization_id = $3
`, turnID, owner.TopOrganizationID, owner.OrganizationID))
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrTurnNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("get turn: %w", err)
	}
	return turn, nil
}

func (s *PostgresTurnStore) FindTurnByClientID(ctx context.Context, owner Owner, sessionID, clientTurnID string) (Turn, error) {
	turn, err := scanTurn(s.db.QueryRowContext(ctx, turnSelect+`
WHERE session_id = $1 AND client_turn_id = $2
  AND top_organization_id = $3 AND organization_id = $4
`, sessionID, clientTurnID, owner.TopOrganizationID, owner.OrganizationID))
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrTurnNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("find turn by client id: %w", err)
	}
	return turn, nil
}

func (s *PostgresTurnStore) GetExecutionEnvelope(ctx context.Context, owner Owner, turnID string) (json.RawMessage, error) {
	turn, err := s.GetTurn(ctx, owner, turnID)
	if err != nil {
		return nil, err
	}
	if len(turn.ExecutionEnvelope) == 0 {
		return nil, ErrExecutionEnvelopeMissing
	}
	return append(json.RawMessage(nil), turn.ExecutionEnvelope...), nil
}

// ListRecoverableTurns is a process-level recovery scan. It returns owner data
// from the turn row so the coordinator can re-enter the normal owner-scoped
// APIs. Active, unexpired leases are excluded; database fencing remains the
// final authority if multiple replicas scan the same orphan concurrently.
func (s *PostgresTurnStore) ListRecoverableTurns(ctx context.Context, limit int) ([]RecoverableTurn, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, turnSelect+`
WHERE (
	status = 'accepted'
	OR (
	  status = 'failed_retryable'
	  AND next_retry_at IS NOT NULL AND next_retry_at <= NOW()
	)
	OR (
      status IN ('running', 'awaiting_confirmation', 'committing')
      AND NOT EXISTS (
        SELECT 1 FROM conversation_leases l
        WHERE l.session_id = chat_turns.session_id
          AND l.active_turn_id = chat_turns.id
          AND l.lease_until > NOW()
	)
  )
  )
ORDER BY updated_at ASC, turn_seq ASC
LIMIT $1
`, limit)
	if err != nil {
		return nil, fmt.Errorf("list recoverable turns: %w", err)
	}
	defer rows.Close()
	out := make([]RecoverableTurn, 0)
	for rows.Next() {
		turn, scanErr := scanTurn(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan recoverable turn: %w", scanErr)
		}
		out = append(out, RecoverableTurn{Turn: turn, ExecutionEnvelope: append(json.RawMessage(nil), turn.ExecutionEnvelope...)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recoverable turns: %w", err)
	}
	return out, nil
}

// ListContinuityAdvisories projects only explanatory outcomes. The result has
// no request arguments, confirmation state, account identity, or authorization
// semantics and therefore must never be used to authorize another action.
func (s *PostgresTurnStore) ListContinuityAdvisories(ctx context.Context, owner Owner, sessionID string, limit int) ([]ContinuityAdvisory, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("%w: missing session", ErrInvalidArgument)
	}
	var exists int
	err := s.db.QueryRowContext(ctx, `
SELECT 1 FROM sessions
WHERE id = $1 AND top_organization_id = $2 AND organization_id = $3
  AND deleted_at IS NULL
`, sessionID, owner.TopOrganizationID, owner.OrganizationID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("verify advisory conversation: %w", err)
	}
	if limit <= 0 || limit > 10 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT kind, turn_id, turn_seq, action_index, action_name, context_hint,
       upstream_request_id, occurred_at
FROM (
  SELECT 'known_success'::text AS kind, t.id AS turn_id, t.turn_seq,
         a.action_index, a.action_name, a.context_hint, a.upstream_request_id,
         a.updated_at AS occurred_at
  FROM chat_turns t
  JOIN turn_actions a ON a.turn_id = t.id AND a.status = 'succeeded'
	  WHERE t.session_id = $1
	    AND t.top_organization_id = $2 AND t.organization_id = $3
	    AND t.status IN ('failed_final', 'ambiguous_after_action', 'aborted')
	    AND t.turn_seq > (SELECT COALESCE(MAX(recent.turn_seq), 0) - 10 FROM chat_turns recent WHERE recent.session_id = $1)
	    AND a.updated_at >= NOW() - INTERVAL '24 hours'

  UNION ALL

  SELECT 'ambiguous'::text, t.id, t.turn_seq, NULL::int, ''::varchar,
         NULL::jsonb, NULL::varchar, COALESCE(t.finished_at, t.updated_at)
  FROM chat_turns t
  WHERE t.session_id = $1
	    AND t.top_organization_id = $2 AND t.organization_id = $3
	    AND t.status = 'ambiguous_after_action'
	    AND t.turn_seq > (SELECT COALESCE(MAX(recent.turn_seq), 0) - 10 FROM chat_turns recent WHERE recent.session_id = $1)
	    AND COALESCE(t.finished_at, t.updated_at) >= NOW() - INTERVAL '24 hours'

  UNION ALL

  SELECT 'aborted'::text, t.id, t.turn_seq, NULL::int, ''::varchar,
         NULL::jsonb, NULL::varchar, COALESCE(t.finished_at, t.updated_at)
  FROM chat_turns t
  WHERE t.session_id = $1
	    AND t.top_organization_id = $2 AND t.organization_id = $3
	    AND t.status = 'aborted'
	    AND t.turn_seq > (SELECT COALESCE(MAX(recent.turn_seq), 0) - 10 FROM chat_turns recent WHERE recent.session_id = $1)
	    AND COALESCE(t.finished_at, t.updated_at) >= NOW() - INTERVAL '24 hours'
) advisories
ORDER BY occurred_at DESC, turn_seq DESC, action_index NULLS LAST
LIMIT $4
`, sessionID, owner.TopOrganizationID, owner.OrganizationID, limit)
	if err != nil {
		return nil, fmt.Errorf("list continuity advisories: %w", err)
	}
	defer rows.Close()
	var out []ContinuityAdvisory
	for rows.Next() {
		var item ContinuityAdvisory
		var kind string
		var actionIndex sql.NullInt64
		var actionName sql.NullString
		var hint []byte
		var upstreamID sql.NullString
		if err := rows.Scan(&kind, &item.TurnID, &item.TurnSequence, &actionIndex,
			&actionName, &hint, &upstreamID, &item.OccurredAt); err != nil {
			return nil, fmt.Errorf("scan continuity advisory: %w", err)
		}
		item.Kind = ContinuityAdvisoryKind(kind)
		if actionIndex.Valid {
			index := int(actionIndex.Int64)
			item.ActionIndex = &index
		}
		if actionName.Valid {
			item.ActionName = actionName.String
		}
		if hint != nil {
			canonicalHint, canonicalErr := canonicalActionContextHint(hint)
			if canonicalErr != nil {
				return nil, fmt.Errorf("validate stored continuity hint: %w", canonicalErr)
			}
			item.ContextHint = append(json.RawMessage(nil), canonicalHint...)
		}
		if upstreamID.Valid {
			value := upstreamID.String
			item.UpstreamRequestID = &value
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate continuity advisories: %w", err)
	}
	return out, nil
}

// ListTurnActions returns the durable action plan/result for one owner-scoped
// turn in stable action-index order. A takeover executor uses this to enter
// replay-only mode after any prior action crossed the before-call boundary.
func (s *PostgresTurnStore) ListTurnActions(ctx context.Context, owner Owner, turnID string) ([]TurnAction, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT a.turn_id, a.action_index, a.lease_epoch, a.action_name, a.args_hash,
       a.execution_token, a.in_flight, a.upstream_request_id, a.status,
       a.result, a.error_code, a.context_hint, a.created_at, a.updated_at
FROM turn_actions a
JOIN chat_turns t ON t.id = a.turn_id
WHERE a.turn_id = $1
  AND t.top_organization_id = $2 AND t.organization_id = $3
ORDER BY a.action_index ASC
`, turnID, owner.TopOrganizationID, owner.OrganizationID)
	if err != nil {
		return nil, fmt.Errorf("list turn actions: %w", err)
	}
	defer rows.Close()
	var actions []TurnAction
	for rows.Next() {
		action, scanErr := scanAction(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan turn action: %w", scanErr)
		}
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate turn actions: %w", err)
	}
	return actions, nil
}

// AbandonUnstartedActions retires reservations from an older lease that never
// crossed StartAction. They cannot have changed external state, so forcing a
// recovered model to reproduce their ordinal positions adds failure without
// adding safety.
func (s *PostgresTurnStore) AbandonUnstartedActions(ctx context.Context, owner Owner, lease ConversationLease) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("abandon actions begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := lockSession(ctx, tx, owner, lease.SessionID); err != nil {
		return err
	}
	if err := lockLease(ctx, tx, lease); err != nil {
		return err
	}
	turn, err := lockTurn(ctx, tx, owner, lease.SessionID, lease.TurnID)
	if err != nil {
		return err
	}
	if err := validateTurnLeaseBinding(turn, lease); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `
UPDATE turn_actions
SET status = 'abandoned'
WHERE turn_id = $1 AND lease_epoch <> $2
  AND status = 'reserved' AND in_flight = FALSE
RETURNING action_index, action_name
`, lease.TurnID, lease.Epoch)
	if err != nil {
		return fmt.Errorf("abandon unstarted actions: %w", err)
	}
	type abandonedAction struct {
		index int
		name  string
	}
	var abandoned []abandonedAction
	for rows.Next() {
		var item abandonedAction
		if err := rows.Scan(&item.index, &item.name); err != nil {
			rows.Close()
			return fmt.Errorf("scan abandoned action: %w", err)
		}
		abandoned = append(abandoned, item)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close abandoned actions: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate abandoned actions: %w", err)
	}
	for _, item := range abandoned {
		payload, marshalErr := json.Marshal(map[string]any{"action_index": item.index, "action_name": item.name})
		if marshalErr != nil {
			return marshalErr
		}
		if _, err := appendEventTx(ctx, tx, turn.ID, lease.Epoch, "action.abandoned", payload, true); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("abandon actions commit: %w", err)
	}
	return nil
}

func (s *PostgresTurnStore) AcquireConversationLease(ctx context.Context, owner Owner, sessionID, turnID, holderID string, ttl time.Duration) (ConversationLease, error) {
	return s.acquireConversationLease(ctx, owner, sessionID, turnID, holderID, ttl, false)
}

// AcquireConversationLeaseForFinalization is the narrow escape hatch for an
// explicit client cancellation. It still takes the same session and turn
// fences, but it need not wait for an execution retry deadline because it will
// never run the model or an upstream action.
func (s *PostgresTurnStore) AcquireConversationLeaseForFinalization(ctx context.Context, owner Owner, sessionID, turnID, holderID string, ttl time.Duration) (ConversationLease, error) {
	return s.acquireConversationLease(ctx, owner, sessionID, turnID, holderID, ttl, true)
}

func (s *PostgresTurnStore) acquireConversationLease(ctx context.Context, owner Owner, sessionID, turnID, holderID string, ttl time.Duration, allowEarlyFinalization bool) (ConversationLease, error) {
	if sessionID == "" || turnID == "" || holderID == "" || len(holderID) > 128 || ttl <= 0 {
		return ConversationLease{}, fmt.Errorf("%w: invalid lease request", ErrInvalidArgument)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return ConversationLease{}, fmt.Errorf("acquire lease begin: %w", err)
	}
	defer tx.Rollback()
	contextVersion, err := lockSession(ctx, tx, owner, sessionID)
	if err != nil {
		return ConversationLease{}, err
	}

	var leaseOwner Owner
	var activeTurn, currentHolder sql.NullString
	var epoch int64
	var leaseUntil time.Time
	var active bool
	err = tx.QueryRowContext(ctx, `
SELECT top_organization_id, organization_id, active_turn_id, holder_id,
       lease_epoch, lease_until, lease_until > NOW()
FROM conversation_leases WHERE session_id = $1 FOR UPDATE
`, sessionID).Scan(&leaseOwner.TopOrganizationID, &leaseOwner.OrganizationID,
		&activeTurn, &currentHolder, &epoch, &leaseUntil, &active)
	if errors.Is(err, sql.ErrNoRows) {
		turn, lockErr := lockTurn(ctx, tx, owner, sessionID, turnID)
		if lockErr != nil {
			return ConversationLease{}, lockErr
		}
		if err := ensureTurnIsQueueHead(ctx, tx, turn); err != nil {
			return ConversationLease{}, err
		}
		lease, insertErr := insertLease(ctx, tx, owner, sessionID, turnID, holderID, 1, ttl)
		if insertErr != nil {
			return ConversationLease{}, insertErr
		}
		if err := startTurnTx(ctx, tx, turn, lease, contextVersion, allowEarlyFinalization); err != nil {
			return ConversationLease{}, err
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return ConversationLease{}, fmt.Errorf("acquire lease commit: %w", commitErr)
		}
		return lease, nil
	}
	if err != nil {
		return ConversationLease{}, fmt.Errorf("acquire lease read: %w", err)
	}
	if leaseOwner != owner {
		return ConversationLease{}, ErrConversationNotFound
	}
	if active && currentHolder.Valid && (currentHolder.String != holderID || !activeTurn.Valid || activeTurn.String != turnID) {
		return ConversationLease{}, ErrLeaseHeld
	}
	if active && currentHolder.Valid && currentHolder.String == holderID && activeTurn.Valid && activeTurn.String == turnID {
		// Re-entry is not renewal: a second handler using the same replica ID
		// must not receive the same execution right. The current executor renews
		// explicitly with RenewConversationLease.
		return ConversationLease{}, ErrLeaseHeld
	}

	// An expired executor may only be taken over for the same active turn. A
	// different queued turn must first reconcile the abandoned one.
	if activeTurn.Valid && activeTurn.String != "" && activeTurn.String != turnID {
		return ConversationLease{}, ErrLeaseHeld
	}
	turn, err := lockTurn(ctx, tx, owner, sessionID, turnID)
	if err != nil {
		return ConversationLease{}, err
	}
	if err := ensureTurnIsQueueHead(ctx, tx, turn); err != nil {
		return ConversationLease{}, err
	}
	epoch++
	lease, err := txUpdateLeaseOwner(ctx, tx, owner, sessionID, turnID, holderID, epoch, ttl)
	if err != nil {
		return ConversationLease{}, err
	}
	if err := startTurnTx(ctx, tx, turn, lease, contextVersion, allowEarlyFinalization); err != nil {
		return ConversationLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return ConversationLease{}, fmt.Errorf("acquire expired lease commit: %w", err)
	}
	return lease, nil
}

func (s *PostgresTurnStore) RenewConversationLease(ctx context.Context, owner Owner, lease ConversationLease, ttl time.Duration) (ConversationLease, error) {
	if ttl <= 0 {
		return ConversationLease{}, fmt.Errorf("%w: lease ttl must be positive", ErrInvalidArgument)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return ConversationLease{}, fmt.Errorf("renew lease begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := lockSession(ctx, tx, owner, lease.SessionID); err != nil {
		return ConversationLease{}, err
	}
	if err := lockLease(ctx, tx, lease); err != nil {
		return ConversationLease{}, err
	}
	turn, err := lockTurn(ctx, tx, owner, lease.SessionID, lease.TurnID)
	if err != nil {
		return ConversationLease{}, err
	}
	if err := validateTurnLeaseBinding(turn, lease); err != nil {
		return ConversationLease{}, err
	}
	renewed, err := updateLeaseDeadline(ctx, tx, lease.SessionID, lease.TurnID, lease.HolderID, lease.Epoch, ttl)
	if err != nil {
		return ConversationLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return ConversationLease{}, fmt.Errorf("renew lease commit: %w", err)
	}
	return renewed, nil
}

func (s *PostgresTurnStore) ReleaseConversationLease(ctx context.Context, owner Owner, lease ConversationLease) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("release lease begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := lockSession(ctx, tx, owner, lease.SessionID); err != nil {
		return err
	}
	if err := lockLease(ctx, tx, lease); err != nil {
		return err
	}
	turn, err := lockTurn(ctx, tx, owner, lease.SessionID, lease.TurnID)
	if err != nil {
		return err
	}
	if err := validateTurnLeaseBinding(turn, lease); err != nil {
		return err
	}
	uncertain, err := turnHasPossibleAction(ctx, tx, turn.ID)
	if err != nil {
		return err
	}
	nextStatus, retryCount, nextRetryAt, exhausted := failureTransition(turn, TurnStatusFailedRetryable, uncertain, time.Now().UTC())
	reason := "lease_released"
	if uncertain {
		reason = "lease_released_after_action"
	} else if exhausted {
		reason = "retry_exhausted"
	}
	if nextStatus.Terminal() {
		if err := lockTurnMessages(ctx, tx, turn.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE messages SET status = 'error', error_code = $1
WHERE turn_id = $2 AND status = 'pending'
`, reason, turn.ID); err != nil {
			return fmt.Errorf("release lease fail pending messages: %w", err)
		}
	}
	payload, _ := json.Marshal(failureEventPayload(reason, nextStatus, retryCount, nextRetryAt))
	if _, err := appendEventTx(ctx, tx, turn.ID, lease.Epoch, "turn.lease_released", payload, !nextStatus.Terminal()); err != nil {
		return err
	}
	finishedExpr := "NULL"
	if nextStatus.Terminal() {
		finishedExpr = "NOW()"
	}
	res, err := tx.ExecContext(ctx, `
UPDATE chat_turns
SET status = $1, executor_id = NULL, error_code = $2, finished_at = `+finishedExpr+`,
	    execution_envelope = CASE WHEN $5 THEN NULL ELSE execution_envelope END,
	    retry_count = $6, next_retry_at = $7
WHERE id = $3 AND lease_epoch = $4
  AND status IN ('running', 'awaiting_confirmation', 'committing')
`, nextStatus, reason, turn.ID, lease.Epoch, nextStatus.Terminal(), retryCount, nextRetryAt)
	if err != nil {
		return fmt.Errorf("release lease update turn: %w", err)
	}
	if !exactlyOneRow(res) {
		return ErrInvalidTurnState
	}
	if err := releaseLeaseTx(ctx, tx, lease); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("release lease commit: %w", err)
	}
	return nil
}

func (s *PostgresTurnStore) AppendEvent(ctx context.Context, owner Owner, lease ConversationLease, eventType string, payload json.RawMessage, provisional bool) (TurnEvent, error) {
	canonicalPayload, err := canonicalJSON(payload)
	if err != nil || eventType == "" || !provisional {
		return TurnEvent{}, fmt.Errorf("%w: invalid event", ErrInvalidArgument)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return TurnEvent{}, fmt.Errorf("append event begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := lockSession(ctx, tx, owner, lease.SessionID); err != nil {
		return TurnEvent{}, err
	}
	if err := lockLease(ctx, tx, lease); err != nil {
		return TurnEvent{}, err
	}
	turn, err := lockTurn(ctx, tx, owner, lease.SessionID, lease.TurnID)
	if err != nil {
		return TurnEvent{}, err
	}
	if err := validateTurnLeaseBinding(turn, lease); err != nil {
		return TurnEvent{}, err
	}
	event, err := appendEventTx(ctx, tx, lease.TurnID, lease.Epoch, eventType, canonicalPayload, true)
	if err != nil {
		return TurnEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return TurnEvent{}, fmt.Errorf("append event commit: %w", err)
	}
	return event, nil
}

func (s *PostgresTurnStore) ListEvents(ctx context.Context, owner Owner, turnID string, afterSeq int64, limit int) ([]TurnEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT e.turn_id, e.seq, e.lease_epoch, e.event_type, e.payload, e.provisional, e.created_at
FROM chat_turn_events e
JOIN chat_turns t ON t.id = e.turn_id
WHERE e.turn_id = $1 AND t.top_organization_id = $2 AND t.organization_id = $3
  AND e.seq > $4
  AND (NOT e.provisional OR e.lease_epoch = t.lease_epoch)
ORDER BY e.seq ASC
LIMIT $5
`, turnID, owner.TopOrganizationID, owner.OrganizationID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list turn events: %w", err)
	}
	defer rows.Close()
	var events []TurnEvent
	for rows.Next() {
		event, scanErr := scanEvent(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan turn event: %w", scanErr)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate turn events: %w", err)
	}
	return events, nil
}

func (s *PostgresTurnStore) CreateInteraction(
	ctx context.Context,
	owner Owner,
	lease ConversationLease,
	key, kind string,
	payload json.RawMessage,
	ttl time.Duration,
) (TurnInteraction, bool, error) {
	canonicalPayload, err := canonicalJSON(payload)
	if err != nil || key == "" || len(key) > 128 || kind == "" || len(kind) > 32 || ttl <= 0 {
		return TurnInteraction{}, false, fmt.Errorf("%w: invalid interaction", ErrInvalidArgument)
	}
	requestHash := HashTurnRequest(kind, string(canonicalPayload))
	interactionEvent, err := marshalInteractionRequestedEvent(key, kind, canonicalPayload)
	if err != nil {
		return TurnInteraction{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return TurnInteraction{}, false, fmt.Errorf("create interaction begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := lockSession(ctx, tx, owner, lease.SessionID); err != nil {
		return TurnInteraction{}, false, err
	}
	if err := lockLease(ctx, tx, lease); err != nil {
		return TurnInteraction{}, false, err
	}
	turn, err := lockTurn(ctx, tx, owner, lease.SessionID, lease.TurnID)
	if err != nil {
		return TurnInteraction{}, false, err
	}
	if turn.Status.Terminal() {
		return TurnInteraction{}, false, ErrInvalidTurnState
	}
	if err := validateTurnLeaseBinding(turn, lease); err != nil {
		return TurnInteraction{}, false, err
	}

	latest, latestErr := getLatestInteractionForUpdate(ctx, tx, lease.TurnID)
	if latestErr != nil && !errors.Is(latestErr, ErrTurnNotFound) {
		return TurnInteraction{}, false, latestErr
	}
	var existing TurnInteraction
	if latestErr == nil && latest.RequestHash == requestHash &&
		(latest.Status == InteractionStatusPending || latest.Status == InteractionStatusResolved) {
		existing = latest
		key = latest.Key
		interactionEvent, err = marshalInteractionRequestedEvent(key, kind, canonicalPayload)
		if err != nil {
			return TurnInteraction{}, false, err
		}
		err = nil
	} else {
		existing, err = getInteractionForUpdate(ctx, tx, lease.TurnID, key)
	}
	if err == nil && existing.RequestHash != requestHash {
		return TurnInteraction{}, false, ErrInteractionConflict
	}
	if err == nil && (existing.Status == InteractionStatusSuperseded || (latestErr == nil && latest.ID != existing.ID)) {
		// Never reactivate a public identity that a stale browser may still
		// hold. The semantic payload is the same, but this is a new occurrence.
		key = kind + "/" + uuid.NewString()
		interactionEvent, err = marshalInteractionRequestedEvent(key, kind, canonicalPayload)
		if err != nil {
			return TurnInteraction{}, false, err
		}
		existing, err = getInteractionForUpdate(ctx, tx, lease.TurnID, key)
	}
	if err == nil {
		if existing.Status == InteractionStatusPending {
			if existing.LeaseEpoch != lease.Epoch {
				existing, err = scanInteraction(tx.QueryRowContext(ctx, `
UPDATE turn_interactions
SET lease_epoch = $1
WHERE id = $2 AND status = 'pending' AND expires_at > NOW()
RETURNING id, turn_id, interaction_key, kind, request_hash, request_payload,
	      lease_epoch, expires_at, status, resolution_hash,
	      response_payload, created_at, resolved_at
	`, lease.Epoch, existing.ID))
				if errors.Is(err, sql.ErrNoRows) {
					return TurnInteraction{}, false, ErrInteractionExpired
				}
				if err != nil {
					return TurnInteraction{}, false, fmt.Errorf("rebind pending interaction: %w", err)
				}
				if _, err := appendEventTx(ctx, tx, lease.TurnID, lease.Epoch, "interaction.requested", interactionEvent, true); err != nil {
					return TurnInteraction{}, false, err
				}
			} else {
				var unexpired bool
				if err := tx.QueryRowContext(ctx, `SELECT expires_at > NOW() FROM turn_interactions WHERE id = $1`, existing.ID).Scan(&unexpired); err != nil {
					return TurnInteraction{}, false, fmt.Errorf("check existing interaction expiry: %w", err)
				}
				if !unexpired {
					return TurnInteraction{}, false, ErrInteractionExpired
				}
			}
			res, err := tx.ExecContext(ctx, `
UPDATE chat_turns SET status = 'awaiting_confirmation'
WHERE id = $1 AND status IN ('running', 'awaiting_confirmation')
`, lease.TurnID)
			if err != nil {
				return TurnInteraction{}, false, fmt.Errorf("restore awaiting interaction state: %w", err)
			}
			if !exactlyOneRow(res) {
				return TurnInteraction{}, false, ErrInvalidTurnState
			}
		}
		if err := tx.Commit(); err != nil {
			return TurnInteraction{}, false, fmt.Errorf("create existing interaction commit: %w", err)
		}
		return existing, false, nil
	}
	if !errors.Is(err, ErrTurnNotFound) {
		return TurnInteraction{}, false, err
	}

	// A different semantic confirmation replaces any older unresolved card.
	// The old row remains for audit, but can no longer authorize an action from
	// a stale browser tab or a fenced executor.
	rows, err := tx.QueryContext(ctx, `
UPDATE turn_interactions
SET status = 'superseded'
WHERE turn_id = $1 AND status = 'pending' AND interaction_key <> $2
RETURNING interaction_key
`, lease.TurnID, key)
	if err != nil {
		return TurnInteraction{}, false, fmt.Errorf("supersede pending interactions: %w", err)
	}
	var superseded []string
	for rows.Next() {
		var oldKey string
		if err := rows.Scan(&oldKey); err != nil {
			rows.Close()
			return TurnInteraction{}, false, fmt.Errorf("scan superseded interaction: %w", err)
		}
		superseded = append(superseded, oldKey)
	}
	if err := rows.Close(); err != nil {
		return TurnInteraction{}, false, fmt.Errorf("close superseded interactions: %w", err)
	}
	if err := rows.Err(); err != nil {
		return TurnInteraction{}, false, fmt.Errorf("iterate superseded interactions: %w", err)
	}
	for _, oldKey := range superseded {
		payload, marshalErr := json.Marshal(map[string]any{
			"interaction_key": oldKey,
			"replaced_by":     key,
		})
		if marshalErr != nil {
			return TurnInteraction{}, false, marshalErr
		}
		if _, err := appendEventTx(ctx, tx, lease.TurnID, lease.Epoch, "interaction.superseded", payload, true); err != nil {
			return TurnInteraction{}, false, err
		}
	}

	interaction, err := scanInteraction(tx.QueryRowContext(ctx, `
INSERT INTO turn_interactions
  (id, turn_id, interaction_key, kind, request_hash, request_payload,
   lease_epoch, expires_at, status)
VALUES ($1, $2, $3, $4, $5, $6, $7,
        NOW() + ($8 * INTERVAL '1 millisecond'), 'pending')
RETURNING id, turn_id, interaction_key, kind, request_hash, request_payload,
	      lease_epoch, expires_at, status, resolution_hash,
	      response_payload, created_at, resolved_at
`, uuid.NewString(), lease.TurnID, key, kind, requestHash, nullableJSON(canonicalPayload),
		lease.Epoch, durationMillis(ttl)))
	if err != nil {
		return TurnInteraction{}, false, fmt.Errorf("create interaction: %w", err)
	}
	if _, err := appendEventTx(ctx, tx, lease.TurnID, lease.Epoch, "interaction.requested", interactionEvent, true); err != nil {
		return TurnInteraction{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE chat_turns SET status = 'awaiting_confirmation' WHERE id = $1
`, lease.TurnID); err != nil {
		return TurnInteraction{}, false, fmt.Errorf("mark turn awaiting interaction: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return TurnInteraction{}, false, fmt.Errorf("create interaction commit: %w", err)
	}
	return interaction, true, nil
}

func (s *PostgresTurnStore) ResolveInteraction(
	ctx context.Context,
	owner Owner,
	turnID, key string,
	response json.RawMessage,
) (TurnInteraction, error) {
	canonicalResponse, err := canonicalJSON(response)
	if err != nil || turnID == "" || key == "" {
		return TurnInteraction{}, fmt.Errorf("%w: invalid interaction resolution", ErrInvalidArgument)
	}
	resolutionHash := hashJSON(canonicalResponse)
	sessionID, err := s.findTurnSession(ctx, owner, turnID)
	if err != nil {
		return TurnInteraction{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return TurnInteraction{}, fmt.Errorf("resolve interaction begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := lockSession(ctx, tx, owner, sessionID); err != nil {
		return TurnInteraction{}, err
	}
	turn, err := lockTurn(ctx, tx, owner, sessionID, turnID)
	if err != nil {
		return TurnInteraction{}, err
	}
	if turn.Status.Terminal() {
		return TurnInteraction{}, ErrInvalidTurnState
	}
	interaction, err := getInteractionForUpdate(ctx, tx, turnID, key)
	if err != nil {
		return TurnInteraction{}, err
	}
	if interaction.Status == InteractionStatusResolved {
		if interaction.ResolutionHash == nil || *interaction.ResolutionHash != resolutionHash {
			return TurnInteraction{}, ErrInteractionConflict
		}
		if err := tx.Commit(); err != nil {
			return TurnInteraction{}, fmt.Errorf("resolve existing interaction commit: %w", err)
		}
		return interaction, nil
	}
	if interaction.Status != InteractionStatusPending {
		return TurnInteraction{}, ErrInteractionConflict
	}
	var unexpired bool
	if err := tx.QueryRowContext(ctx, `
SELECT expires_at > NOW() FROM turn_interactions
WHERE turn_id = $1 AND interaction_key = $2
`, turnID, key).Scan(&unexpired); err != nil {
		return TurnInteraction{}, fmt.Errorf("check interaction expiry: %w", err)
	}
	if !unexpired {
		return TurnInteraction{}, ErrInteractionExpired
	}
	interaction, err = scanInteraction(tx.QueryRowContext(ctx, `
UPDATE turn_interactions
SET status = 'resolved', resolution_hash = $1,
    response_payload = $2, resolved_at = NOW()
WHERE turn_id = $3 AND interaction_key = $4 AND status = 'pending'
RETURNING id, turn_id, interaction_key, kind, request_hash, request_payload,
	      lease_epoch, expires_at, status, resolution_hash,
	      response_payload, created_at, resolved_at
`, resolutionHash, nullableJSON(canonicalResponse), turnID, key))
	if err != nil {
		return TurnInteraction{}, fmt.Errorf("resolve interaction: %w", err)
	}
	resolvedEvent, _ := json.Marshal(map[string]any{
		"interaction_key": interaction.Key,
		"kind":            interaction.Kind,
		"resolved":        true,
	})
	if _, err := appendEventTx(ctx, tx, turnID, interaction.LeaseEpoch, "interaction.resolved", resolvedEvent, true); err != nil {
		return TurnInteraction{}, err
	}
	if err := tx.Commit(); err != nil {
		return TurnInteraction{}, fmt.Errorf("resolve interaction commit: %w", err)
	}
	return interaction, nil
}

func (s *PostgresTurnStore) GetInteraction(ctx context.Context, owner Owner, turnID, key string) (TurnInteraction, error) {
	interaction, err := scanInteraction(s.db.QueryRowContext(ctx, `
SELECT i.id, i.turn_id, i.interaction_key, i.kind, i.request_hash,
	   i.request_payload, i.lease_epoch, i.expires_at, i.status,
	   i.resolution_hash, i.response_payload,
	   i.created_at, i.resolved_at
FROM turn_interactions i
JOIN chat_turns t ON t.id = i.turn_id
WHERE i.turn_id = $1 AND i.interaction_key = $2
  AND t.top_organization_id = $3 AND t.organization_id = $4
`, turnID, key, owner.TopOrganizationID, owner.OrganizationID))
	if errors.Is(err, sql.ErrNoRows) {
		return TurnInteraction{}, ErrTurnNotFound
	}
	if err != nil {
		return TurnInteraction{}, fmt.Errorf("get interaction: %w", err)
	}
	return interaction, nil
}

func (s *PostgresTurnStore) ReserveAction(
	ctx context.Context,
	owner Owner,
	lease ConversationLease,
	in ReserveActionInput,
) (TurnAction, bool, error) {
	if in.Index < 0 || in.ActionName == "" || len(in.ActionName) > 128 || len(in.ArgsHash) != 64 {
		return TurnAction{}, false, fmt.Errorf("%w: invalid action reservation", ErrInvalidArgument)
	}
	canonicalHint, err := canonicalActionContextHint(in.ContextHint)
	if err != nil {
		return TurnAction{}, false, fmt.Errorf("%w: invalid action context hint", ErrInvalidArgument)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return TurnAction{}, false, fmt.Errorf("reserve action begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := lockSession(ctx, tx, owner, lease.SessionID); err != nil {
		return TurnAction{}, false, err
	}
	if err := lockLease(ctx, tx, lease); err != nil {
		return TurnAction{}, false, err
	}
	turn, err := lockTurn(ctx, tx, owner, lease.SessionID, lease.TurnID)
	if err != nil {
		return TurnAction{}, false, err
	}
	if turn.Status.Terminal() {
		return TurnAction{}, false, ErrInvalidTurnState
	}
	if err := validateTurnLeaseBinding(turn, lease); err != nil {
		return TurnAction{}, false, err
	}
	if err := ensureNoPendingInteractions(ctx, tx, turn.ID); err != nil {
		return TurnAction{}, false, err
	}

	existing, err := getActionForUpdate(ctx, tx, lease.TurnID, in.Index)
	if err == nil {
		if existing.ActionName != in.ActionName || existing.ArgsHash != in.ArgsHash {
			return TurnAction{}, false, ErrActionConflict
		}
		if existing.Status == ActionStatusAbandoned {
			existing, err = reactivateAbandonedAction(ctx, tx, existing, lease)
			if err != nil {
				return TurnAction{}, false, err
			}
		}
		if err := tx.Commit(); err != nil {
			return TurnAction{}, false, fmt.Errorf("reserve existing action commit: %w", err)
		}
		return existing, false, nil
	}
	if !errors.Is(err, ErrActionNotFound) {
		return TurnAction{}, false, err
	}
	semantic, err := getActionByIdentityForUpdate(ctx, tx, lease.TurnID, in.ActionName, in.ArgsHash)
	if err == nil {
		if semantic.Status == ActionStatusAbandoned {
			semantic, err = reactivateAbandonedAction(ctx, tx, semantic, lease)
			if err != nil {
				return TurnAction{}, false, err
			}
		}
		if err := tx.Commit(); err != nil {
			return TurnAction{}, false, fmt.Errorf("reserve semantic action commit: %w", err)
		}
		return semantic, false, nil
	}
	if !errors.Is(err, ErrActionNotFound) {
		return TurnAction{}, false, err
	}

	action, err := scanAction(tx.QueryRowContext(ctx, `
INSERT INTO turn_actions
  (turn_id, action_index, lease_epoch, action_name, args_hash,
   execution_token, in_flight, upstream_request_id, status, context_hint)
VALUES ($1, $2, $3, $4, $5, $6, FALSE, $7, 'reserved', $8)
RETURNING turn_id, action_index, lease_epoch, action_name, args_hash,
          execution_token, in_flight, upstream_request_id, status,
          result, error_code, context_hint, created_at, updated_at
`, lease.TurnID, in.Index, lease.Epoch, in.ActionName, in.ArgsHash,
		uuid.NewString(), nullableStringPtr(in.UpstreamRequestID), nullableJSON(canonicalHint)))
	if err != nil {
		return TurnAction{}, false, fmt.Errorf("reserve action: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE chat_turns SET has_external_action = TRUE WHERE id = $1
`, lease.TurnID); err != nil {
		return TurnAction{}, false, fmt.Errorf("mark turn external action: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return TurnAction{}, false, fmt.Errorf("reserve action commit: %w", err)
	}
	return action, true, nil
}

func reactivateAbandonedAction(ctx context.Context, tx *sql.Tx, action TurnAction, lease ConversationLease) (TurnAction, error) {
	reactivated, err := scanAction(tx.QueryRowContext(ctx, `
UPDATE turn_actions
SET lease_epoch = $1, execution_token = $2, status = 'reserved',
    in_flight = FALSE, upstream_request_id = NULL, result = NULL, error_code = NULL
WHERE turn_id = $3 AND action_index = $4 AND status = 'abandoned'
RETURNING turn_id, action_index, lease_epoch, action_name, args_hash,
          execution_token, in_flight, upstream_request_id, status,
          result, error_code, context_hint, created_at, updated_at
`, lease.Epoch, uuid.NewString(), action.TurnID, action.Index))
	if err != nil {
		return TurnAction{}, fmt.Errorf("reactivate abandoned action: %w", err)
	}
	return reactivated, nil
}

// StartAction is the durable before-call boundary. ReserveAction alone never
// means that upstream may have run. A later lease can claim an unstarted
// reservation, while a reservation already marked in-flight must never be
// issued again automatically.
func (s *PostgresTurnStore) StartAction(
	ctx context.Context,
	owner Owner,
	lease ConversationLease,
	executionToken string,
) (TurnAction, error) {
	if executionToken == "" {
		return TurnAction{}, fmt.Errorf("%w: invalid action start", ErrInvalidArgument)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return TurnAction{}, fmt.Errorf("start action begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := lockSession(ctx, tx, owner, lease.SessionID); err != nil {
		return TurnAction{}, err
	}
	if err := lockLease(ctx, tx, lease); err != nil {
		return TurnAction{}, err
	}
	turn, err := lockTurn(ctx, tx, owner, lease.SessionID, lease.TurnID)
	if err != nil {
		return TurnAction{}, err
	}
	if err := validateTurnLeaseBinding(turn, lease); err != nil {
		return TurnAction{}, err
	}
	if err := ensureNoPendingInteractions(ctx, tx, turn.ID); err != nil {
		return TurnAction{}, err
	}
	action, err := getActionByTokenForUpdate(ctx, tx, executionToken)
	if err != nil {
		return TurnAction{}, err
	}
	if action.TurnID != turn.ID {
		return TurnAction{}, ErrActionNotFound
	}
	if action.Status != ActionStatusReserved {
		return TurnAction{}, ErrActionConflict
	}
	if action.InFlight {
		return TurnAction{}, ErrActionUncertain
	}
	action, err = scanAction(tx.QueryRowContext(ctx, `
UPDATE turn_actions
SET in_flight = TRUE, lease_epoch = $1
WHERE execution_token = $2 AND turn_id = $3
  AND status = 'reserved' AND in_flight = FALSE
RETURNING turn_id, action_index, lease_epoch, action_name, args_hash,
          execution_token, in_flight, upstream_request_id, status,
          result, error_code, context_hint, created_at, updated_at
`, lease.Epoch, executionToken, turn.ID))
	if err != nil {
		return TurnAction{}, fmt.Errorf("start action: %w", err)
	}
	if err := tx.Commit(); err != nil {
		// The caller cannot know whether the durable before-call marker committed.
		// It must not call upstream after an acknowledgement failure.
		return TurnAction{}, fmt.Errorf("%w: start action commit: %v", ErrActionUncertain, err)
	}
	return action, nil
}

// RecordAction is intentionally execution-token based and does not require the
// old conversation lease. A process can lose its lease after the upstream side
// effect succeeds; suppressing that result would turn known reality into an
// ambiguous action during reconciliation.
func (s *PostgresTurnStore) RecordAction(
	ctx context.Context,
	owner Owner,
	executionToken string,
	status ActionStatus,
	result json.RawMessage,
	errorCode *string,
	upstreamRequestID ...*string,
) (TurnAction, error) {
	if len(upstreamRequestID) > 1 {
		return TurnAction{}, fmt.Errorf("%w: multiple upstream request ids", ErrInvalidArgument)
	}
	var requestID *string
	if len(upstreamRequestID) == 1 {
		requestID = upstreamRequestID[0]
	}
	return s.RecordActionWithContext(ctx, owner, executionToken, status, result, errorCode, requestID, nil)
}

// RecordActionWithContext atomically records the known upstream outcome and a
// strictly whitelisted conversational breadcrumb. If the turn was already
// terminal, it also emits a non-provisional late-outcome event for reconnecting
// clients; raw upstream results are never copied into that event.
func (s *PostgresTurnStore) RecordActionWithContext(
	ctx context.Context,
	owner Owner,
	executionToken string,
	status ActionStatus,
	result json.RawMessage,
	errorCode *string,
	requestID *string,
	contextHint json.RawMessage,
) (TurnAction, error) {
	canonicalResult, err := canonicalJSON(result)
	if err != nil || executionToken == "" || status == ActionStatusReserved || !status.Valid() {
		return TurnAction{}, fmt.Errorf("%w: invalid action result", ErrInvalidArgument)
	}
	canonicalHint, err := canonicalActionContextHint(contextHint)
	if err != nil {
		return TurnAction{}, fmt.Errorf("%w: invalid action context hint", ErrInvalidArgument)
	}
	var sessionID, turnID string
	err = s.db.QueryRowContext(ctx, `
SELECT t.session_id, a.turn_id
FROM turn_actions a
JOIN chat_turns t ON t.id = a.turn_id
WHERE a.execution_token = $1
  AND t.top_organization_id = $2 AND t.organization_id = $3
`, executionToken, owner.TopOrganizationID, owner.OrganizationID).Scan(&sessionID, &turnID)
	if errors.Is(err, sql.ErrNoRows) {
		return TurnAction{}, ErrActionNotFound
	}
	if err != nil {
		return TurnAction{}, fmt.Errorf("locate action: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return TurnAction{}, fmt.Errorf("record action begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := lockSession(ctx, tx, owner, sessionID); err != nil {
		return TurnAction{}, err
	}
	turn, err := lockTurn(ctx, tx, owner, sessionID, turnID)
	if err != nil {
		return TurnAction{}, err
	}
	action, err := getActionByTokenForUpdate(ctx, tx, executionToken)
	if err != nil {
		return TurnAction{}, err
	}
	if action.Status != ActionStatusReserved {
		if action.Status != status || !sameNullableString(action.ErrorCode, errorCode) || !jsonEqual(action.Result, canonicalResult) {
			return TurnAction{}, ErrActionConflict
		}
		if requestID != nil && action.UpstreamRequestID != nil && !sameNullableString(action.UpstreamRequestID, requestID) {
			return TurnAction{}, ErrActionConflict
		}
		if len(canonicalHint) != 0 && len(action.ContextHint) != 0 && !jsonEqual(action.ContextHint, canonicalHint) {
			return TurnAction{}, ErrActionConflict
		}
		if (requestID != nil && action.UpstreamRequestID == nil) || (len(canonicalHint) != 0 && len(action.ContextHint) == 0) {
			action, err = scanAction(tx.QueryRowContext(ctx, `
UPDATE turn_actions
SET upstream_request_id = COALESCE($1, upstream_request_id),
    context_hint = COALESCE($2, context_hint)
WHERE execution_token = $3 AND status = $4
RETURNING turn_id, action_index, lease_epoch, action_name, args_hash,
          execution_token, in_flight, upstream_request_id, status,
          result, error_code, context_hint, created_at, updated_at

`, nullableStringPtr(requestID), nullableJSON(canonicalHint), executionToken, status))
			if err != nil {
				return TurnAction{}, fmt.Errorf("backfill action context: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return TurnAction{}, fmt.Errorf("record existing action commit: %w", err)
		}
		return action, nil
	}
	if !action.InFlight {
		return TurnAction{}, ErrInvalidTurnState
	}
	action, err = scanAction(tx.QueryRowContext(ctx, `
UPDATE turn_actions
SET status = $1, in_flight = FALSE, result = $2, error_code = $3,
    upstream_request_id = COALESCE($4, upstream_request_id),
    context_hint = COALESCE($5, context_hint)
WHERE execution_token = $6 AND status = 'reserved' AND in_flight = TRUE
RETURNING turn_id, action_index, lease_epoch, action_name, args_hash,
          execution_token, in_flight, upstream_request_id, status,
          result, error_code, context_hint, created_at, updated_at
`, status, nullableJSON(canonicalResult), nullableStringPtr(errorCode), nullableStringPtr(requestID), nullableJSON(canonicalHint), executionToken))
	if err != nil {
		return TurnAction{}, fmt.Errorf("record action: %w", err)
	}
	if turn.Status.Terminal() {
		latePayload, marshalErr := json.Marshal(map[string]any{
			"action_index": action.Index,
			"action_name":  action.ActionName,
			"status":       action.Status,
			"context_hint": action.ContextHint,
		})
		if marshalErr != nil {
			return TurnAction{}, fmt.Errorf("encode late action outcome: %w", marshalErr)
		}
		if _, err := appendEventTx(ctx, tx, turn.ID, action.LeaseEpoch, "action.late_outcome", latePayload, false); err != nil {
			return TurnAction{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return TurnAction{}, fmt.Errorf("record action commit: %w", err)
	}
	return action, nil
}

func (s *PostgresTurnStore) CommitTurn(ctx context.Context, owner Owner, in CommitTurnInput) (Turn, error) {
	canonicalContext, err := canonicalJSON(in.Context)
	if err != nil || !in.ContextWriteMode.Valid() || in.TurnID == "" || in.Lease.TurnID != in.TurnID || in.TerminalEventType == "" || strings.TrimSpace(in.Assistant.Content) == "" {
		return Turn{}, fmt.Errorf("%w: invalid turn commit", ErrInvalidArgument)
	}
	canonicalEvent, err := canonicalJSON(in.TerminalEventPayload)
	if err != nil {
		return Turn{}, fmt.Errorf("%w: invalid terminal event", ErrInvalidArgument)
	}
	commitHash := hashCommit(in, canonicalContext, canonicalEvent)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Turn{}, fmt.Errorf("commit turn begin: %w", err)
	}
	defer tx.Rollback()
	currentVersion, err := lockSession(ctx, tx, owner, in.Lease.SessionID)
	if err != nil {
		return Turn{}, err
	}
	if err := lockLease(ctx, tx, in.Lease); err != nil {
		return Turn{}, err
	}
	turn, err := lockTurn(ctx, tx, owner, in.Lease.SessionID, in.TurnID)
	if err != nil {
		return Turn{}, err
	}
	if turn.Status.Terminal() || turn.Status == TurnStatusFailedRetryable {
		return Turn{}, ErrInvalidTurnState
	}
	if err := validateTurnLeaseBinding(turn, in.Lease); err != nil {
		return Turn{}, err
	}
	if currentVersion != in.ExpectedContextVersion {
		return Turn{}, ErrContextConflict
	}
	if err := lockPendingTurnMessages(ctx, tx, turn); err != nil {
		return Turn{}, err
	}
	if err := ensureNoUncertainActions(ctx, tx, turn.ID); err != nil {
		return Turn{}, err
	}
	if err := ensureNoPendingInteractions(ctx, tx, turn.ID); err != nil {
		return Turn{}, err
	}

	var newContextVersion int
	if in.ContextWriteMode == ContextWriteUpdate {
		if err := tx.QueryRowContext(ctx, `
UPDATE sessions
SET context = $1, context_version = context_version + 1,
    message_count = message_count + 2, updated_at = NOW()
WHERE id = $2 AND top_organization_id = $3 AND organization_id = $4
  AND deleted_at IS NULL AND context_version = $5
RETURNING context_version
`, nullableJSON(canonicalContext), in.Lease.SessionID, owner.TopOrganizationID,
			owner.OrganizationID, in.ExpectedContextVersion).Scan(&newContextVersion); errors.Is(err, sql.ErrNoRows) {
			return Turn{}, ErrContextConflict
		} else if err != nil {
			return Turn{}, fmt.Errorf("commit turn session context: %w", err)
		}
	} else {
		if err := tx.QueryRowContext(ctx, `
UPDATE sessions
SET message_count = message_count + 2, updated_at = NOW()
WHERE id = $1 AND top_organization_id = $2 AND organization_id = $3
  AND deleted_at IS NULL AND context_version = $4
RETURNING context_version
`, in.Lease.SessionID, owner.TopOrganizationID, owner.OrganizationID,
			in.ExpectedContextVersion).Scan(&newContextVersion); errors.Is(err, sql.ErrNoRows) {
			return Turn{}, ErrContextConflict
		} else if err != nil {
			return Turn{}, fmt.Errorf("commit turn preserving session context: %w", err)
		}
	}
	userResult, err := tx.ExecContext(ctx, `
UPDATE messages
SET status = 'ok', error_code = NULL
WHERE id = $1 AND turn_id = $2 AND turn_role = 'user' AND status = 'pending'
`, turn.UserMessageID, turn.ID)
	if err != nil {
		return Turn{}, fmt.Errorf("commit user message: %w", err)
	}
	if !exactlyOneRow(userResult) {
		return Turn{}, ErrInvalidTurnState
	}
	assistantResult, err := tx.ExecContext(ctx, `
UPDATE messages
SET content = $1, status = 'ok', error_code = NULL,
    input_tokens = $2, output_tokens = $3, ttft_ms = $4, latency_ms = $5
WHERE id = $6 AND turn_id = $7 AND turn_role = 'assistant' AND status = 'pending'
`, in.Assistant.Content, nullableIntPtr(in.Assistant.InputTokens), nullableIntPtr(in.Assistant.OutputTokens),
		nullableIntPtr(in.Assistant.TTFTMs), nullableIntPtr(in.Assistant.LatencyMs),
		turn.AssistantMessageID, turn.ID)
	if err != nil {
		return Turn{}, fmt.Errorf("commit assistant message: %w", err)
	}
	if !exactlyOneRow(assistantResult) {
		return Turn{}, ErrInvalidTurnState
	}
	if _, err := appendEventTx(ctx, tx, turn.ID, in.Lease.Epoch, in.TerminalEventType, canonicalEvent, false); err != nil {
		return Turn{}, err
	}
	committed, err := scanTurn(tx.QueryRowContext(ctx, `
UPDATE chat_turns
SET status = 'committed', committed_context_version = $1,
    committed_lease_epoch = $2, commit_hash = $3, error_code = NULL,
    finished_at = NOW(), committed_at = NOW(), execution_envelope = NULL
WHERE id = $4 AND status IN ('running', 'awaiting_confirmation', 'committing')
RETURNING `+turnColumns,
		newContextVersion, in.Lease.Epoch, commitHash, turn.ID))
	if err != nil {
		return Turn{}, fmt.Errorf("commit turn row: %w", err)
	}
	if err := releaseLeaseTx(ctx, tx, in.Lease); err != nil {
		return Turn{}, err
	}
	if err := tx.Commit(); err != nil {
		// The caller must reconcile with GetTurn. Do not run a cleanup write: a
		// commit acknowledgement failure is not evidence that PostgreSQL rolled back.
		return Turn{}, fmt.Errorf("commit turn transaction: %w", err)
	}
	return committed, nil
}

// HashTurnCommit returns the fingerprint persisted by CommitTurn. Callers keep
// it across an uncertain transaction acknowledgement so they can distinguish
// their own durable commit from a later executor's different result.
func HashTurnCommit(in CommitTurnInput) (string, error) {
	canonicalContext, err := canonicalJSON(in.Context)
	if err != nil || !in.ContextWriteMode.Valid() || in.TurnID == "" || in.Lease.TurnID != in.TurnID || in.TerminalEventType == "" {
		return "", fmt.Errorf("%w: invalid turn commit", ErrInvalidArgument)
	}
	canonicalEvent, err := canonicalJSON(in.TerminalEventPayload)
	if err != nil {
		return "", fmt.Errorf("%w: invalid terminal event", ErrInvalidArgument)
	}
	return hashCommit(in, canonicalContext, canonicalEvent), nil
}

// ReconcileCommit is read-only. It is the required follow-up when CommitTurn
// returns an acknowledgement error: success is reported only when the durable
// row contains this exact attempt's fingerprint.
func (s *PostgresTurnStore) ReconcileCommit(ctx context.Context, owner Owner, in CommitTurnInput) (Turn, error) {
	expected, err := HashTurnCommit(in)
	if err != nil {
		return Turn{}, err
	}
	turn, err := s.GetTurn(ctx, owner, in.TurnID)
	if err != nil {
		return Turn{}, err
	}
	if turn.Status != TurnStatusCommitted {
		return Turn{}, ErrInvalidTurnState
	}
	if turn.CommitHash == nil || *turn.CommitHash != expected {
		return Turn{}, ErrCommitConflict
	}
	return turn, nil
}

func (s *PostgresTurnStore) FailTurn(
	ctx context.Context,
	owner Owner,
	lease ConversationLease,
	desired TurnStatus,
	reason string,
) (Turn, error) {
	if desired != TurnStatusFailedRetryable && desired != TurnStatusFailedFinal && desired != TurnStatusAborted {
		return Turn{}, fmt.Errorf("%w: invalid failure status", ErrInvalidArgument)
	}
	return s.failOrReconcileTurn(ctx, owner, lease, desired, reason, "turn.failed")
}

func (s *PostgresTurnStore) ReconcileTurn(ctx context.Context, owner Owner, lease ConversationLease, reason string) (Turn, error) {
	return s.failOrReconcileTurn(ctx, owner, lease, TurnStatusFailedRetryable, reason, "turn.reconciled")
}

func (s *PostgresTurnStore) failOrReconcileTurn(
	ctx context.Context,
	owner Owner,
	lease ConversationLease,
	desired TurnStatus,
	reason, eventType string,
) (Turn, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Turn{}, fmt.Errorf("fail turn begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := lockSession(ctx, tx, owner, lease.SessionID); err != nil {
		return Turn{}, err
	}
	if err := lockLease(ctx, tx, lease); err != nil {
		return Turn{}, err
	}
	turn, err := lockTurn(ctx, tx, owner, lease.SessionID, lease.TurnID)
	if err != nil {
		return Turn{}, err
	}
	if turn.Status.Terminal() {
		return Turn{}, ErrInvalidTurnState
	}
	if err := validateTurnLeaseBinding(turn, lease); err != nil {
		return Turn{}, err
	}
	if err := lockTurnMessages(ctx, tx, turn.ID); err != nil {
		return Turn{}, err
	}
	uncertain, err := turnHasPossibleAction(ctx, tx, turn.ID)
	if err != nil {
		return Turn{}, err
	}
	actual, retryCount, nextRetryAt, exhausted := failureTransition(turn, desired, uncertain, time.Now().UTC())
	if exhausted {
		reason = "retry_exhausted"
	}
	if actual.Terminal() {
		if _, err := tx.ExecContext(ctx, `
UPDATE messages SET status = 'error', error_code = $1
WHERE turn_id = $2 AND status = 'pending'
`, reason, turn.ID); err != nil {
			return Turn{}, fmt.Errorf("fail pending messages: %w", err)
		}
	}
	payload, _ := json.Marshal(failureEventPayload(reason, actual, retryCount, nextRetryAt))
	if _, err := appendEventTx(ctx, tx, turn.ID, lease.Epoch, eventType, payload, !actual.Terminal()); err != nil {
		return Turn{}, err
	}
	finishedExpr := "NULL"
	if actual.Terminal() {
		finishedExpr = "NOW()"
	}
	failed, err := scanTurn(tx.QueryRowContext(ctx, `
UPDATE chat_turns
SET status = $1, error_code = $2, executor_id = NULL, finished_at = `+finishedExpr+`,
	    execution_envelope = CASE WHEN $4 THEN NULL ELSE execution_envelope END,
	    retry_count = $5, next_retry_at = $6
WHERE id = $3 AND status IN ('accepted', 'running', 'awaiting_confirmation', 'committing', 'failed_retryable')
RETURNING `+turnColumns,
		actual, reason, turn.ID, actual.Terminal(), retryCount, nextRetryAt))
	if err != nil {
		return Turn{}, fmt.Errorf("fail turn row: %w", err)
	}
	if err := releaseLeaseTx(ctx, tx, lease); err != nil {
		return Turn{}, err
	}
	if err := tx.Commit(); err != nil {
		return Turn{}, fmt.Errorf("fail turn commit: %w", err)
	}
	return failed, nil
}

func failureTransition(turn Turn, desired TurnStatus, uncertain bool, now time.Time) (TurnStatus, int, *time.Time, bool) {
	if uncertain {
		return TurnStatusAmbiguousAfterAction, turn.RetryCount, nil, false
	}
	if desired != TurnStatusFailedRetryable {
		return desired, turn.RetryCount, nil, false
	}
	nextCount := turn.RetryCount + 1
	if nextCount > MaxTurnRecoveryAttempts {
		return TurnStatusFailedFinal, MaxTurnRecoveryAttempts, nil, true
	}
	next := now.Add(turnRecoveryBackoff(nextCount))
	return TurnStatusFailedRetryable, nextCount, &next, false
}

func turnRecoveryBackoff(retryCount int) time.Duration {
	switch retryCount {
	case 1:
		return 2 * time.Second
	case 2:
		return 10 * time.Second
	default:
		return 30 * time.Second
	}
}

func failureEventPayload(reason string, status TurnStatus, retryCount int, nextRetryAt *time.Time) map[string]any {
	payload := map[string]any{
		"reason":      reason,
		"status":      string(status),
		"retry_count": retryCount,
	}
	if nextRetryAt != nil {
		payload["next_retry_at"] = nextRetryAt.UTC().Format(time.RFC3339Nano)
	}
	return payload
}
