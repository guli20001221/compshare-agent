package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
)

const turnColumns = `
id, session_id, top_organization_id, organization_id, client_turn_id, turn_seq,
request_hash, status, user_message_id, assistant_message_id,
base_context_version, committed_context_version, committed_lease_epoch,
commit_hash, error_code, executor_id, lease_epoch, has_external_action,
next_event_seq, created_at, updated_at, started_at, finished_at, committed_at`

const turnSelect = `SELECT ` + turnColumns + ` FROM chat_turns `

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTurn(row rowScanner) (Turn, error) {
	var out Turn
	var status string
	var committedContextVersion, committedLeaseEpoch, leaseEpoch sql.NullInt64
	var commitHash, errorCode, executorID sql.NullString
	var startedAt, finishedAt, committedAt sql.NullTime
	if err := row.Scan(
		&out.ID, &out.SessionID, &out.Owner.TopOrganizationID, &out.Owner.OrganizationID,
		&out.ClientTurnID, &out.Sequence, &out.RequestHash, &status, &out.UserMessageID,
		&out.AssistantMessageID, &out.BaseContextVersion, &committedContextVersion,
		&committedLeaseEpoch, &commitHash, &errorCode, &executorID, &leaseEpoch,
		&out.HasExternalAction, &out.NextEventSeq, &out.CreatedAt, &out.UpdatedAt,
		&startedAt, &finishedAt, &committedAt,
	); err != nil {
		return Turn{}, err
	}
	out.Status = TurnStatus(status)
	if committedContextVersion.Valid {
		v := int(committedContextVersion.Int64)
		out.CommittedContextVersion = &v
	}
	if committedLeaseEpoch.Valid {
		v := committedLeaseEpoch.Int64
		out.CommittedLeaseEpoch = &v
	}
	if leaseEpoch.Valid {
		v := leaseEpoch.Int64
		out.LeaseEpoch = &v
	}
	if commitHash.Valid {
		out.CommitHash = &commitHash.String
	}
	if errorCode.Valid {
		out.ErrorCode = &errorCode.String
	}
	if executorID.Valid {
		out.ExecutorID = &executorID.String
	}
	if startedAt.Valid {
		out.StartedAt = &startedAt.Time
	}
	if finishedAt.Valid {
		out.FinishedAt = &finishedAt.Time
	}
	if committedAt.Valid {
		out.CommittedAt = &committedAt.Time
	}
	return out, nil
}

func lockSession(ctx context.Context, tx *sql.Tx, owner Owner, sessionID string) (int, error) {
	var version int
	err := tx.QueryRowContext(ctx, `
SELECT context_version
FROM sessions
WHERE id = $1 AND top_organization_id = $2 AND organization_id = $3
  AND deleted_at IS NULL
FOR UPDATE
`, sessionID, owner.TopOrganizationID, owner.OrganizationID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrConversationNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("lock conversation: %w", err)
	}
	return version, nil
}

func getTurnByClientIDForUpdate(
	ctx context.Context,
	tx *sql.Tx,
	owner Owner,
	sessionID, clientTurnID string,
) (Turn, error) {
	out, err := scanTurn(tx.QueryRowContext(ctx, turnSelect+`
WHERE session_id = $1 AND top_organization_id = $2 AND organization_id = $3
  AND client_turn_id = $4
FOR UPDATE
`, sessionID, owner.TopOrganizationID, owner.OrganizationID, clientTurnID))
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrTurnNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("find idempotent turn: %w", err)
	}
	return out, nil
}

func insertTurn(
	ctx context.Context,
	tx *sql.Tx,
	owner Owner,
	in AcceptTurnInput,
	turnID, userMessageID, assistantMessageID string,
	contextVersion int,
	turnSequence int64,
) (Turn, error) {
	out, err := scanTurn(tx.QueryRowContext(ctx, `
INSERT INTO chat_turns
  (id, session_id, top_organization_id, organization_id, client_turn_id,
   turn_seq, request_hash, status, user_message_id, assistant_message_id,
   base_context_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'accepted', $8, $9, $10)
RETURNING `+turnColumns,
		turnID, in.SessionID, owner.TopOrganizationID, owner.OrganizationID,
		in.ClientTurnID, turnSequence, in.RequestHash, userMessageID, assistantMessageID, contextVersion))
	if err != nil {
		return Turn{}, fmt.Errorf("insert turn: %w", err)
	}
	return out, nil
}

func nextTurnSequence(ctx context.Context, tx *sql.Tx, sessionID string) (int64, error) {
	var sequence int64
	if err := tx.QueryRowContext(ctx, `
SELECT GREATEST(
  COALESCE((SELECT MAX(turn_seq) FROM chat_turns WHERE session_id = $1), 0),
  COALESCE((SELECT message_count / 2 FROM sessions WHERE id = $1), 0)
) + 1
`, sessionID).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("allocate conversation turn sequence: %w", err)
	}
	return sequence, nil
}

func lockTurn(ctx context.Context, tx *sql.Tx, owner Owner, sessionID, turnID string) (Turn, error) {
	out, err := scanTurn(tx.QueryRowContext(ctx, turnSelect+`
WHERE id = $1 AND session_id = $2
  AND top_organization_id = $3 AND organization_id = $4
FOR UPDATE
`, turnID, sessionID, owner.TopOrganizationID, owner.OrganizationID))
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrTurnNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("lock turn: %w", err)
	}
	return out, nil
}

func ensureTurnIsQueueHead(ctx context.Context, tx *sql.Tx, turn Turn) error {
	var earlier int
	if err := tx.QueryRowContext(ctx, `
SELECT count(*)
FROM chat_turns
WHERE session_id = $1 AND turn_seq < $2
  AND status IN ('accepted', 'running', 'awaiting_confirmation', 'committing', 'failed_retryable')
`, turn.SessionID, turn.Sequence).Scan(&earlier); err != nil {
		return fmt.Errorf("check conversation turn order: %w", err)
	}
	if earlier != 0 {
		return ErrTurnOutOfOrder
	}
	return nil
}

func insertLease(
	ctx context.Context,
	tx *sql.Tx,
	owner Owner,
	sessionID, turnID, holderID string,
	epoch int64,
	ttl time.Duration,
) (ConversationLease, error) {
	var leaseUntil time.Time
	err := tx.QueryRowContext(ctx, `
INSERT INTO conversation_leases
  (session_id, top_organization_id, organization_id, active_turn_id,
   holder_id, lease_epoch, lease_until)
VALUES ($1, $2, $3, $4, $5, $6, NOW() + ($7 * INTERVAL '1 millisecond'))
RETURNING lease_until
`, sessionID, owner.TopOrganizationID, owner.OrganizationID, turnID, holderID,
		epoch, durationMillis(ttl)).Scan(&leaseUntil)
	if err != nil {
		return ConversationLease{}, fmt.Errorf("insert conversation lease: %w", err)
	}
	return ConversationLease{SessionID: sessionID, TurnID: turnID, HolderID: holderID, Epoch: epoch, LeaseUntil: leaseUntil}, nil
}

func txUpdateLeaseOwner(
	ctx context.Context,
	tx *sql.Tx,
	owner Owner,
	sessionID, turnID, holderID string,
	epoch int64,
	ttl time.Duration,
) (ConversationLease, error) {
	var leaseUntil time.Time
	err := tx.QueryRowContext(ctx, `
UPDATE conversation_leases
SET active_turn_id = $1, holder_id = $2, lease_epoch = $3,
    lease_until = NOW() + ($4 * INTERVAL '1 millisecond')
WHERE session_id = $5 AND top_organization_id = $6 AND organization_id = $7
RETURNING lease_until
`, turnID, holderID, epoch, durationMillis(ttl), sessionID,
		owner.TopOrganizationID, owner.OrganizationID).Scan(&leaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationLease{}, ErrConversationNotFound
	}
	if err != nil {
		return ConversationLease{}, fmt.Errorf("take over conversation lease: %w", err)
	}
	return ConversationLease{SessionID: sessionID, TurnID: turnID, HolderID: holderID, Epoch: epoch, LeaseUntil: leaseUntil}, nil
}

func updateLeaseDeadline(
	ctx context.Context,
	tx *sql.Tx,
	sessionID, turnID, holderID string,
	epoch int64,
	ttl time.Duration,
) (ConversationLease, error) {
	var leaseUntil time.Time
	err := tx.QueryRowContext(ctx, `
UPDATE conversation_leases
SET lease_until = NOW() + ($1 * INTERVAL '1 millisecond')
WHERE session_id = $2 AND active_turn_id = $3 AND holder_id = $4
  AND lease_epoch = $5 AND lease_until > NOW()
RETURNING lease_until
`, durationMillis(ttl), sessionID, turnID, holderID, epoch).Scan(&leaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationLease{}, ErrLeaseFenced
	}
	if err != nil {
		return ConversationLease{}, fmt.Errorf("renew conversation lease: %w", err)
	}
	return ConversationLease{SessionID: sessionID, TurnID: turnID, HolderID: holderID, Epoch: epoch, LeaseUntil: leaseUntil}, nil
}

func lockLease(ctx context.Context, tx *sql.Tx, lease ConversationLease) error {
	var found int
	err := tx.QueryRowContext(ctx, `
SELECT 1
FROM conversation_leases
WHERE session_id = $1 AND active_turn_id = $2 AND holder_id = $3
  AND lease_epoch = $4 AND lease_until > NOW()
FOR UPDATE
`, lease.SessionID, lease.TurnID, lease.HolderID, lease.Epoch).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseFenced
	}
	if err != nil {
		return fmt.Errorf("lock conversation lease: %w", err)
	}
	return nil
}

func startTurnTx(ctx context.Context, tx *sql.Tx, turn Turn, lease ConversationLease, contextVersion int) error {
	if turn.Status.Terminal() {
		return ErrInvalidTurnState
	}
	switch turn.Status {
	case TurnStatusAccepted, TurnStatusFailedRetryable, TurnStatusRunning,
		TurnStatusAwaitingConfirmation, TurnStatusCommitting:
	default:
		return ErrInvalidTurnState
	}
	nextStatus := turn.Status
	if turn.Status == TurnStatusAccepted || turn.Status == TurnStatusFailedRetryable {
		nextStatus = TurnStatusRunning
	}
	res, err := tx.ExecContext(ctx, `
UPDATE chat_turns
SET status = $1, executor_id = $2, lease_epoch = $3,
    base_context_version = $4,
    error_code = NULL, started_at = COALESCE(started_at, NOW())
WHERE id = $5 AND status = $6
`, nextStatus, lease.HolderID, lease.Epoch, contextVersion, turn.ID, turn.Status)
	if err != nil {
		return fmt.Errorf("start turn: %w", err)
	}
	if !exactlyOneRow(res) {
		return ErrInvalidTurnState
	}
	return nil
}

func validateTurnLeaseBinding(turn Turn, lease ConversationLease) error {
	if turn.ID != lease.TurnID || turn.SessionID != lease.SessionID || turn.Status.Terminal() ||
		turn.ExecutorID == nil || *turn.ExecutorID != lease.HolderID ||
		turn.LeaseEpoch == nil || *turn.LeaseEpoch != lease.Epoch {
		return ErrLeaseFenced
	}
	return nil
}

func releaseLeaseTx(ctx context.Context, tx *sql.Tx, lease ConversationLease) error {
	res, err := tx.ExecContext(ctx, `
UPDATE conversation_leases
SET active_turn_id = NULL, holder_id = NULL, lease_until = NOW()
WHERE session_id = $1 AND active_turn_id = $2 AND holder_id = $3
  AND lease_epoch = $4
`, lease.SessionID, lease.TurnID, lease.HolderID, lease.Epoch)
	if err != nil {
		return fmt.Errorf("release conversation lease: %w", err)
	}
	if !exactlyOneRow(res) {
		return ErrLeaseFenced
	}
	return nil
}

func (s *PostgresTurnStore) findTurnSession(ctx context.Context, owner Owner, turnID string) (string, error) {
	var sessionID string
	err := s.db.QueryRowContext(ctx, `
SELECT session_id FROM chat_turns
WHERE id = $1 AND top_organization_id = $2 AND organization_id = $3
`, turnID, owner.TopOrganizationID, owner.OrganizationID).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrTurnNotFound
	}
	if err != nil {
		return "", fmt.Errorf("locate turn conversation: %w", err)
	}
	return sessionID, nil
}

func appendEventTx(
	ctx context.Context,
	tx *sql.Tx,
	turnID string,
	leaseEpoch int64,
	eventType string,
	payload json.RawMessage,
	provisional bool,
) (TurnEvent, error) {
	var seq int64
	err := tx.QueryRowContext(ctx, `
UPDATE chat_turns
SET next_event_seq = next_event_seq + 1
WHERE id = $1
RETURNING next_event_seq - 1
`, turnID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return TurnEvent{}, ErrTurnNotFound
	}
	if err != nil {
		return TurnEvent{}, fmt.Errorf("allocate turn event sequence: %w", err)
	}
	event, err := scanEvent(tx.QueryRowContext(ctx, `
	INSERT INTO chat_turn_events (turn_id, seq, lease_epoch, event_type, payload, provisional)
	VALUES ($1, $2, $3, $4, $5, $6)
	RETURNING turn_id, seq, lease_epoch, event_type, payload, provisional, created_at
`, turnID, seq, leaseEpoch, eventType, nullableJSON(payload), provisional))
	if err != nil {
		return TurnEvent{}, fmt.Errorf("append turn event: %w", err)
	}
	return event, nil
}

func scanEvent(row rowScanner) (TurnEvent, error) {
	var out TurnEvent
	var payload []byte
	if err := row.Scan(&out.TurnID, &out.Seq, &out.LeaseEpoch, &out.Type, &payload, &out.Provisional, &out.CreatedAt); err != nil {
		return TurnEvent{}, err
	}
	if payload != nil {
		out.Payload = append(json.RawMessage(nil), payload...)
	}
	return out, nil
}

func getInteractionForUpdate(ctx context.Context, tx *sql.Tx, turnID, key string) (TurnInteraction, error) {
	out, err := scanInteraction(tx.QueryRowContext(ctx, `
SELECT id, turn_id, interaction_key, kind, request_hash, request_payload,
       lease_epoch, expires_at, status, resolution_hash, response_payload,
       created_at, resolved_at
FROM turn_interactions
WHERE turn_id = $1 AND interaction_key = $2
FOR UPDATE
`, turnID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return TurnInteraction{}, ErrTurnNotFound
	}
	if err != nil {
		return TurnInteraction{}, fmt.Errorf("lock turn interaction: %w", err)
	}
	return out, nil
}

func scanInteraction(row rowScanner) (TurnInteraction, error) {
	var out TurnInteraction
	var status string
	var requestPayload, responsePayload []byte
	var resolutionHash sql.NullString
	var resolvedAt sql.NullTime
	if err := row.Scan(
		&out.ID, &out.TurnID, &out.Key, &out.Kind, &out.RequestHash,
		&requestPayload, &out.LeaseEpoch, &out.ExpiresAt, &status,
		&resolutionHash, &responsePayload, &out.CreatedAt, &resolvedAt,
	); err != nil {
		return TurnInteraction{}, err
	}
	out.Status = InteractionStatus(status)
	if requestPayload != nil {
		out.RequestPayload = append(json.RawMessage(nil), requestPayload...)
	}
	if responsePayload != nil {
		out.ResponsePayload = append(json.RawMessage(nil), responsePayload...)
	}
	if resolutionHash.Valid {
		out.ResolutionHash = &resolutionHash.String
	}
	if resolvedAt.Valid {
		out.ResolvedAt = &resolvedAt.Time
	}
	return out, nil
}

func getActionForUpdate(ctx context.Context, tx *sql.Tx, turnID string, index int) (TurnAction, error) {
	out, err := scanAction(tx.QueryRowContext(ctx, `
SELECT turn_id, action_index, lease_epoch, action_name, args_hash,
       execution_token, in_flight, upstream_request_id, status, result,
       error_code, created_at, updated_at
FROM turn_actions
WHERE turn_id = $1 AND action_index = $2
FOR UPDATE
`, turnID, index))
	if errors.Is(err, sql.ErrNoRows) {
		return TurnAction{}, ErrActionNotFound
	}
	if err != nil {
		return TurnAction{}, fmt.Errorf("lock turn action: %w", err)
	}
	return out, nil
}

func getActionByTokenForUpdate(ctx context.Context, tx *sql.Tx, token string) (TurnAction, error) {
	out, err := scanAction(tx.QueryRowContext(ctx, `
SELECT turn_id, action_index, lease_epoch, action_name, args_hash,
       execution_token, in_flight, upstream_request_id, status, result,
       error_code, created_at, updated_at
FROM turn_actions
WHERE execution_token = $1
FOR UPDATE
`, token))
	if errors.Is(err, sql.ErrNoRows) {
		return TurnAction{}, ErrActionNotFound
	}
	if err != nil {
		return TurnAction{}, fmt.Errorf("lock turn action by token: %w", err)
	}
	return out, nil
}

func getActionByIdentityForUpdate(ctx context.Context, tx *sql.Tx, turnID, actionName, argsHash string) (TurnAction, error) {
	out, err := scanAction(tx.QueryRowContext(ctx, `
SELECT turn_id, action_index, lease_epoch, action_name, args_hash,
       execution_token, in_flight, upstream_request_id, status, result,
       error_code, created_at, updated_at
FROM turn_actions
WHERE turn_id = $1 AND action_name = $2 AND args_hash = $3
FOR UPDATE
`, turnID, actionName, argsHash))
	if errors.Is(err, sql.ErrNoRows) {
		return TurnAction{}, ErrActionNotFound
	}
	if err != nil {
		return TurnAction{}, fmt.Errorf("lock turn action by identity: %w", err)
	}
	return out, nil
}

func scanAction(row rowScanner) (TurnAction, error) {
	var out TurnAction
	var status string
	var upstreamID, errorCode sql.NullString
	var result []byte
	if err := row.Scan(
		&out.TurnID, &out.Index, &out.LeaseEpoch, &out.ActionName, &out.ArgsHash,
		&out.ExecutionToken, &out.InFlight, &upstreamID, &status, &result,
		&errorCode, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return TurnAction{}, err
	}
	out.Status = ActionStatus(status)
	if upstreamID.Valid {
		out.UpstreamRequestID = &upstreamID.String
	}
	if errorCode.Valid {
		out.ErrorCode = &errorCode.String
	}
	if result != nil {
		out.Result = append(json.RawMessage(nil), result...)
	}
	return out, nil
}

func lockPendingTurnMessages(ctx context.Context, tx *sql.Tx, turn Turn) error {
	rows, err := tx.QueryContext(ctx, `
SELECT id, turn_role, status
FROM messages
WHERE turn_id = $1
ORDER BY turn_role
FOR UPDATE
`, turn.ID)
	if err != nil {
		return fmt.Errorf("lock pending turn messages: %w", err)
	}
	defer rows.Close()
	seen := map[string]string{}
	for rows.Next() {
		var id, role, status string
		if err := rows.Scan(&id, &role, &status); err != nil {
			return fmt.Errorf("scan pending turn message: %w", err)
		}
		if status != "pending" {
			return ErrInvalidTurnState
		}
		seen[role] = id
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate pending turn messages: %w", err)
	}
	if len(seen) != 2 || seen["user"] != turn.UserMessageID || seen["assistant"] != turn.AssistantMessageID {
		return ErrInvalidTurnState
	}
	return nil
}

func lockTurnMessages(ctx context.Context, tx *sql.Tx, turnID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM messages WHERE turn_id = $1 FOR UPDATE`, turnID)
	if err != nil {
		return fmt.Errorf("lock turn messages: %w", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan turn message lock: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate turn message locks: %w", err)
	}
	if count != 2 {
		return ErrInvalidTurnState
	}
	return nil
}

func ensureNoUncertainActions(ctx context.Context, tx *sql.Tx, turnID string) error {
	var count int
	if err := tx.QueryRowContext(ctx, `
SELECT count(*) FROM turn_actions
WHERE turn_id = $1 AND status IN ('reserved', 'ambiguous')
`, turnID).Scan(&count); err != nil {
		return fmt.Errorf("check uncertain actions: %w", err)
	}
	if count != 0 {
		return ErrActionUncertain
	}
	return nil
}

func ensureNoPendingInteractions(ctx context.Context, tx *sql.Tx, turnID string) error {
	var expired, active int
	if err := tx.QueryRowContext(ctx, `
SELECT
  count(*) FILTER (WHERE expires_at <= NOW()),
  count(*) FILTER (WHERE expires_at > NOW())
FROM turn_interactions
WHERE turn_id = $1 AND status = 'pending'
`, turnID).Scan(&expired, &active); err != nil {
		return fmt.Errorf("check pending interactions: %w", err)
	}
	if expired != 0 {
		return ErrInteractionExpired
	}
	if active != 0 {
		return ErrInteractionPending
	}
	return nil
}

func turnHasPossibleAction(ctx context.Context, tx *sql.Tx, turnID string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `
SELECT count(*) FROM turn_actions
WHERE turn_id = $1
  AND (status = 'ambiguous' OR (status = 'reserved' AND in_flight = TRUE))
`, turnID).Scan(&count); err != nil {
		return false, fmt.Errorf("check possible turn actions: %w", err)
	}
	return count != 0, nil
}

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("null"), nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

func jsonEqual(a, b json.RawMessage) bool {
	ca, errA := canonicalJSON(a)
	cb, errB := canonicalJSON(b)
	return errA == nil && errB == nil && bytes.Equal(ca, cb)
}

func sameNullableString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func exactlyOneRow(result sql.Result) bool {
	if result == nil {
		return false
	}
	rows, err := result.RowsAffected()
	return err == nil && rows == 1
}

func durationMillis(ttl time.Duration) int64 {
	millis := ttl.Milliseconds()
	if millis < 1 {
		return 1
	}
	return millis
}

func marshalInteractionRequestedEvent(key, kind string, payload json.RawMessage) (json.RawMessage, error) {
	event, err := json.Marshal(struct {
		InteractionKey string          `json:"interaction_key"`
		Kind           string          `json:"kind"`
		Payload        json.RawMessage `json:"payload"`
	}{InteractionKey: key, Kind: kind, Payload: payload})
	if err != nil {
		return nil, fmt.Errorf("encode interaction event: %w", err)
	}
	return event, nil
}

func hashCommit(in CommitTurnInput, contextJSON, eventJSON json.RawMessage) string {
	fields := []string{
		in.TurnID,
		in.Lease.SessionID,
		in.Lease.TurnID,
		in.Lease.HolderID,
		strconv.FormatInt(in.Lease.Epoch, 10),
		strconv.Itoa(in.ExpectedContextVersion),
		string(in.ContextWriteMode),
		string(contextJSON),
		in.Assistant.Content,
		in.TerminalEventType,
		string(eventJSON),
	}
	for _, value := range []*int{
		in.Assistant.InputTokens,
		in.Assistant.OutputTokens,
		in.Assistant.TTFTMs,
		in.Assistant.LatencyMs,
	} {
		if value == nil {
			fields = append(fields, "")
		} else {
			fields = append(fields, strconv.Itoa(*value))
		}
	}
	return HashTurnRequest(fields...)
}
