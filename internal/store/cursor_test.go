package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeDecodeCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 5, 21, 10, 23, 48, 123_000_000, time.UTC)

	cursor, err := EncodeCursor(ts, "msg-aaa")
	require.NoError(t, err)
	require.NotEmpty(t, cursor)

	gotTS, gotID, err := DecodeCursor(cursor)
	require.NoError(t, err)
	assert.True(t, gotTS.Equal(ts), gotTS)
	assert.Equal(t, "msg-aaa", gotID)
}

func TestDecodeCursorRejectsInvalidBase64(t *testing.T) {
	_, _, err := DecodeCursor("not base64")
	require.Error(t, err)
}

func TestDecodeCursorRejectsMissingFields(t *testing.T) {
	cursor, err := encodeCursorPayload(cursorPayload{ID: "msg-aaa"})
	require.NoError(t, err)

	_, _, err = DecodeCursor(cursor)
	require.Error(t, err)
}
