package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageStore_ListCommittedBySession_HidesGhostRowsAndPaginatesWholePairs(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 86001, OrganizationID: 86002}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)
	base := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	insertLegacyTailPair(t, db, session.ID, "legacy-good", 1, "ok", "ok", base)
	insertLegacyMessage(t, db, session.ID, "legacy-orphan", "user", "ok", "orphan-user", base.Add(time.Minute))
	insertLegacyTailPair(t, db, session.ID, "legacy-failed", 2, "ok", "error", base.Add(2*time.Minute))
	insertTailFixtureTurnAt(t, db, owner, session.ID, 1, TurnStatusCommitted, "ok", "ok", true, base.Add(3*time.Minute))
	insertTailFixtureTurnAt(t, db, owner, session.ID, 2, TurnStatusAccepted, "pending", "pending", true, base.Add(4*time.Minute))
	insertTailFixtureTurnAt(t, db, owner, session.ID, 3, TurnStatusFailedRetryable, "error", "error", true, base.Add(5*time.Minute))
	insertTailFixtureTurnAt(t, db, owner, session.ID, 4, TurnStatusCommitted, "ok", "ok", true, base.Add(6*time.Minute))

	messageStore := NewMessageStore(db)
	first, cursor, err := messageStore.ListCommittedBySession(ctx, owner, session.ID, 2, "")
	require.NoError(t, err)
	require.Len(t, first, 2)
	assert.Equal(t, []string{"legacy-u-1", "legacy-a-1"}, messageContents(first))
	require.NotEmpty(t, cursor)

	second, cursor, err := messageStore.ListCommittedBySession(ctx, owner, session.ID, 2, cursor)
	require.NoError(t, err)
	require.Len(t, second, 2)
	assert.Equal(t, []string{"u-001", "a-001"}, messageContents(second))
	require.NotEmpty(t, cursor)

	third, cursor, err := messageStore.ListCommittedBySession(ctx, owner, session.ID, 2, cursor)
	require.NoError(t, err)
	require.Len(t, third, 2)
	assert.Equal(t, []string{"u-004", "a-004"}, messageContents(third))
	assert.Empty(t, cursor)

	all := append(append(first, second...), third...)
	assert.NotContains(t, messageContents(all), "orphan-user")
	assert.NotContains(t, messageContents(all), "legacy-u-2")
	assert.NotContains(t, messageContents(all), "u-002")
	assert.NotContains(t, messageContents(all), "u-003")
	for i := 0; i < len(all); i += 2 {
		assert.Equal(t, "user", all[i].Role)
		assert.Equal(t, "assistant", all[i+1].Role)
	}
}

func TestMessageStore_ListCommittedBySession_RejectsInvalidCursor(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	owner := Owner{TopOrganizationID: 86101, OrganizationID: 86102}
	session, err := NewSessionStore(db).Create(context.Background(), owner, nil, nil)
	require.NoError(t, err)

	_, _, err = NewMessageStore(db).ListCommittedBySession(context.Background(), owner, session.ID, 50, "not-a-cursor")
	require.Error(t, err)
	var cursorErr *ErrInvalidCursor
	assert.True(t, errors.As(err, &cursorErr))
}
