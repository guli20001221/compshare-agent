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
	turnSequence, err := nextTurnSequence(ctx, tx, in.SessionID)
	if err != nil {
		return Turn{}, false, err
	}

	turnID := uuid.NewString()
	userMessageID := uuid.NewString()
	assistantMessageID := uuid.NewString()
	turn, err := insertTurn(ctx, tx, owner, in, turnID, userMessageID, assistantMessageID, contextVersion, turnSequence)
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

func (s *PostgresTurnStore) AcquireConversationLease(ctx context.Context, owner Owner, sessionID, turnID, holderID string, ttl time.Duration) (ConversationLease, error) {
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
		if err := startTurnTx(ctx, tx, turn, lease, contextVersion); err != nil {
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
	if err := startTurnTx(ctx, tx, turn, lease, contextVersion); err != nil {
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
	nextStatus := TurnStatusFailedRetryable
	reason := "lease_released"
	if uncertain {
		nextStatus = TurnStatusAmbiguousAfterAction
		reason = "lease_released_after_action"
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
	payload, _ := json.Marshal(map[string]string{"reason": reason, "status": string(nextStatus)})
	if _, err := appendEventTx(ctx, tx, turn.ID, lease.Epoch, "turn.lease_released", payload, !nextStatus.Terminal()); err != nil {
		return err
	}
	finishedExpr := "NULL"
	if nextStatus.Terminal() {
		finishedExpr = "NOW()"
	}
	res, err := tx.ExecContext(ctx, `
UPDATE chat_turns
SET status = $1, executor_id = NULL, error_code = $2, finished_at = `+finishedExpr+`
WHERE id = $3 AND lease_epoch = $4
  AND status IN ('running', 'awaiting_confirmation', 'committing')
`, nextStatus, reason, turn.ID, lease.Epoch)
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

	existing, err := getInteractionForUpdate(ctx, tx, lease.TurnID, key)
	if err == nil {
		if existing.RequestHash != requestHash {
			return TurnInteraction{}, false, ErrInteractionConflict
		}
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
	if _, err := lockTurn(ctx, tx, owner, sessionID, turnID); err != nil {
		return TurnInteraction{}, err
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
		if err := tx.Commit(); err != nil {
			return TurnAction{}, false, fmt.Errorf("reserve existing action commit: %w", err)
		}
		return existing, false, nil
	}
	if !errors.Is(err, ErrActionNotFound) {
		return TurnAction{}, false, err
	}

	action, err := scanAction(tx.QueryRowContext(ctx, `
INSERT INTO turn_actions
  (turn_id, action_index, lease_epoch, action_name, args_hash,
   execution_token, in_flight, upstream_request_id, status)
VALUES ($1, $2, $3, $4, $5, $6, FALSE, $7, 'reserved')
RETURNING turn_id, action_index, lease_epoch, action_name, args_hash,
          execution_token, in_flight, upstream_request_id, status,
          result, error_code, created_at, updated_at
`, lease.TurnID, in.Index, lease.Epoch, in.ActionName, in.ArgsHash,
		uuid.NewString(), nullableStringPtr(in.UpstreamRequestID)))
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
          result, error_code, created_at, updated_at
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
	canonicalResult, err := canonicalJSON(result)
	if err != nil || executionToken == "" || status == ActionStatusReserved || !status.Valid() {
		return TurnAction{}, fmt.Errorf("%w: invalid action result", ErrInvalidArgument)
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
	if _, err := lockTurn(ctx, tx, owner, sessionID, turnID); err != nil {
		return TurnAction{}, err
	}
	action, err := getActionByTokenForUpdate(ctx, tx, executionToken)
	if err != nil {
		return TurnAction{}, err
	}
	var requestID *string
	if len(upstreamRequestID) > 0 {
		requestID = upstreamRequestID[0]
	}
	if len(upstreamRequestID) > 1 {
		return TurnAction{}, fmt.Errorf("%w: multiple upstream request ids", ErrInvalidArgument)
	}
	if action.Status != ActionStatusReserved {
		if action.Status != status || !sameNullableString(action.ErrorCode, errorCode) || !jsonEqual(action.Result, canonicalResult) {
			return TurnAction{}, ErrActionConflict
		}
		if requestID != nil {
			if action.UpstreamRequestID != nil && !sameNullableString(action.UpstreamRequestID, requestID) {
				return TurnAction{}, ErrActionConflict
			}
			if action.UpstreamRequestID == nil {
				action, err = scanAction(tx.QueryRowContext(ctx, `
UPDATE turn_actions SET upstream_request_id = $1
WHERE execution_token = $2 AND upstream_request_id IS NULL AND status = $3
RETURNING turn_id, action_index, lease_epoch, action_name, args_hash,
          execution_token, in_flight, upstream_request_id, status,
          result, error_code, created_at, updated_at
`, *requestID, executionToken, status))
				if err != nil {
					return TurnAction{}, fmt.Errorf("backfill upstream request id: %w", err)
				}
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
    upstream_request_id = COALESCE($4, upstream_request_id)
WHERE execution_token = $5 AND status = 'reserved' AND in_flight = TRUE
RETURNING turn_id, action_index, lease_epoch, action_name, args_hash,
          execution_token, in_flight, upstream_request_id, status,
          result, error_code, created_at, updated_at
`, status, nullableJSON(canonicalResult), nullableStringPtr(errorCode), nullableStringPtr(requestID), executionToken))
	if err != nil {
		return TurnAction{}, fmt.Errorf("record action: %w", err)
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
    finished_at = NOW(), committed_at = NOW()
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
	if desired != TurnStatusFailedRetryable && desired != TurnStatusAborted {
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
	actual := desired
	if uncertain {
		actual = TurnStatusAmbiguousAfterAction
	}
	if actual.Terminal() {
		if _, err := tx.ExecContext(ctx, `
UPDATE messages SET status = 'error', error_code = $1
WHERE turn_id = $2 AND status = 'pending'
`, reason, turn.ID); err != nil {
			return Turn{}, fmt.Errorf("fail pending messages: %w", err)
		}
	}
	payload, _ := json.Marshal(map[string]string{"reason": reason, "status": string(actual)})
	if _, err := appendEventTx(ctx, tx, turn.ID, lease.Epoch, eventType, payload, !actual.Terminal()); err != nil {
		return Turn{}, err
	}
	finishedExpr := "NULL"
	if actual.Terminal() {
		finishedExpr = "NOW()"
	}
	failed, err := scanTurn(tx.QueryRowContext(ctx, `
UPDATE chat_turns
SET status = $1, error_code = $2, executor_id = NULL, finished_at = `+finishedExpr+`
WHERE id = $3 AND status IN ('accepted', 'running', 'awaiting_confirmation', 'committing', 'failed_retryable')
RETURNING `+turnColumns,
		actual, reason, turn.ID))
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
