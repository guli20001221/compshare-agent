package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// TurnCommitter writes the two halves of a finished turn — the assistant's answer and the
// session state it produced — as ONE commit.
//
// They were two independent writes, and the split is not cosmetic. The answer lives in
// `messages`, the state (selected instance, last intent, pending workflow frame) lives in
// `sessions.context`, and a turn that lands one without the other is a turn that lies:
//
//   - answer saved, state lost: the transcript shows the agent picked an instance and the state
//     says it never did. The next turn reads the stale state and contradicts the visible
//     conversation. This is the amnesia, arriving through the back door of a partial write.
//   - state saved, answer lost: the state moves on from a turn the transcript does not contain.
//
// So they commit together or not at all, and the caller may only tell the user "done" once this
// returns nil. On ErrStaleWrite NEITHER half lands: a CAS conflict means another writer moved the
// session out from under us, and shipping our answer on top of their state is how a detectable
// conflict becomes silent, permanent state loss.
type TurnCommitter interface {
	CommitTurn(
		ctx context.Context,
		owner Owner,
		sessionID string,
		msgID string,
		patch AssistantPatch,
		ctxJSON json.RawMessage,
		expectedVersion int,
	) (newVersion int, err error)
}

// dbBacked is exposed by the concrete Postgres stores so a caller holding only the SessionStore
// and MessageStore interfaces can discover that they are, in fact, the same database — which is
// the precondition for committing across both tables in one transaction.
type dbBacked interface{ DB() *sql.DB }

// DB returns the connection behind this store. See dbBacked.
func (s *MySQLSessionStore) DB() *sql.DB { return s.db }

// DB returns the connection behind this store. See dbBacked.
func (s *MySQLMessageStore) DB() *sql.DB { return s.db }

// TurnCommitterFor picks the strongest commit available for the given stores.
//
// When both are backed by the SAME database, the two halves of a turn can be committed in one
// transaction and the partial-write failure mode disappears entirely. That is always the case in
// production, where cmd/server.go builds both from one *sql.DB.
//
// Otherwise (in-memory test doubles, or a future split backend) it falls back to a SEQUENCED
// commit, which satisfies the same contract for every failure the store can REPORT — see
// sequencedTurnCommitter for the one residue it cannot remove, and why that residue is the
// harmless half rather than the harmful one. Both implementations are held to one contract by
// TestTurnCommitterContract.
func TurnCommitterFor(sessions SessionStore, messages MessageStore) TurnCommitter {
	sdb, sok := sessions.(dbBacked)
	mdb, mok := messages.(dbBacked)
	if sok && mok && sdb.DB() != nil && sdb.DB() == mdb.DB() {
		return NewTurnStore(sdb.DB())
	}
	return sequencedTurnCommitter{sessions: sessions, messages: messages}
}

// sequencedTurnCommitter commits the two halves without a transaction, for stores that do not
// share a database.
//
// The order is the whole design. The session state is CASed FIRST:
//
//   - if the CAS loses, the assistant row is never touched, so NEITHER half lands — identical to
//     the transactional behavior;
//   - if the CAS wins but the message write then fails, the state is ahead of the transcript.
//     That is the HARMLESS half: the next turn reasons from correct state about a turn the
//     transcript is missing. The reverse order would leave the transcript ahead of the state —
//     the agent visibly picked an instance and then denies it — which is the amnesia this whole
//     line of work exists to remove.
//
// Either way the caller is told the turn did not commit and must not announce success.
type sequencedTurnCommitter struct {
	sessions SessionStore
	messages MessageStore
}

func (c sequencedTurnCommitter) CommitTurn(
	ctx context.Context,
	owner Owner,
	sessionID string,
	msgID string,
	patch AssistantPatch,
	ctxJSON json.RawMessage,
	expectedVersion int,
) (int, error) {
	newVersion, err := c.sessions.UpdateContext(ctx, owner, sessionID, ctxJSON, expectedVersion)
	if err != nil {
		return 0, err
	}
	if err := c.messages.UpdateAssistant(ctx, owner, msgID, patch); err != nil {
		return 0, err
	}
	return newVersion, nil
}

// PostgresTurnStore implements TurnCommitter over the same *sql.DB the message and session
// stores use — which is what makes a single transaction across the two tables possible at all.
type PostgresTurnStore struct {
	db *sql.DB
}

// NewTurnStore returns a TurnCommitter over db.
func NewTurnStore(db *sql.DB) *PostgresTurnStore { return &PostgresTurnStore{db: db} }

// CommitTurn updates the assistant message and CASes the session context inside one transaction.
//
// Returns ErrStaleWrite when the session's context_version has moved (another writer committed
// first) — and rolls the message update back with it, so the two halves stay together on the
// failure path exactly as they do on the success path. Returns sql.ErrNoRows when the assistant
// row is not this owner's.
func (s *PostgresTurnStore) CommitTurn(
	ctx context.Context,
	owner Owner,
	sessionID string,
	msgID string,
	patch AssistantPatch,
	ctxJSON json.RawMessage,
	expectedVersion int,
) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("commit turn: begin: %w", err)
	}
	// Rollback is a no-op once Commit has succeeded, so this covers every early return without
	// the caller having to reason about which ones.
	defer func() { _ = tx.Rollback() }()

	msgRes, err := tx.ExecContext(ctx, `
UPDATE messages m
SET content = $1, status = $2, error_code = $3, input_tokens = $4, output_tokens = $5, ttft_ms = $6, latency_ms = $7
FROM sessions s
WHERE s.id = m.session_id
  AND m.id = $8 AND m.role = 'assistant'
  AND s.top_organization_id = $9 AND s.organization_id = $10 AND s.deleted_at IS NULL
`, patch.Content, patch.Status,
		nullableStringPtr(patch.ErrorCode),
		nullableIntPtr(patch.InputTokens), nullableIntPtr(patch.OutputTokens),
		nullableIntPtr(patch.TTFTMs), nullableIntPtr(patch.LatencyMs),
		msgID, owner.TopOrganizationID, owner.OrganizationID)
	if err != nil {
		return 0, fmt.Errorf("commit turn: update assistant: %w", err)
	}
	msgN, err := msgRes.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("commit turn: update assistant rows affected: %w", err)
	}
	if msgN == 0 {
		return 0, sql.ErrNoRows
	}

	ctxRes, err := tx.ExecContext(ctx, `
UPDATE sessions
   SET context = $1, context_version = context_version + 1
 WHERE id = $2
   AND top_organization_id = $3
   AND organization_id = $4
   AND deleted_at IS NULL
   AND context_version = $5
`, nullableJSON(ctxJSON), sessionID, owner.TopOrganizationID, owner.OrganizationID, expectedVersion)
	if err != nil {
		return 0, fmt.Errorf("commit turn: update context: %w", err)
	}
	ctxN, err := ctxRes.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("commit turn: update context rows affected: %w", err)
	}
	if ctxN == 0 {
		// The CAS lost. Rolling back takes the ANSWER with it — deliberately. Committing the
		// answer on top of another writer's state is precisely the half-write this type exists
		// to prevent, and it would convert a conflict we can see into state loss we cannot.
		return 0, ErrStaleWrite
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit turn: commit: %w", err)
	}
	return expectedVersion + 1, nil
}
