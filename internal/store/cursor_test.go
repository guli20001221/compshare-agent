package store

import (
	"encoding/base64"
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

func TestEncodeDecodeCursorSubsecondPrecision(t *testing.T) {
	// 999_999_999 ns — maximum nanosecond value, must survive a full round-trip.
	ts := time.Date(2026, 5, 21, 10, 23, 48, 999_999_999, time.UTC)

	cursor, err := EncodeCursor(ts, "msg-sub")
	require.NoError(t, err)

	gotTS, gotID, err := DecodeCursor(cursor)
	require.NoError(t, err)
	assert.True(t, gotTS.Equal(ts), "expected %v got %v", ts, gotTS)
	assert.Equal(t, "msg-sub", gotID)
}

func TestDecodeCursorRejectsInvalidBase64(t *testing.T) {
	_, _, err := DecodeCursor("not base64")
	require.Error(t, err)
}

func TestDecodeCursorRejectsMalformedJSONInValidBase64(t *testing.T) {
	// Valid base64 but the payload is not valid JSON.
	encoded := base64.StdEncoding.EncodeToString([]byte("not-json{{{"))
	_, _, err := DecodeCursor(encoded)
	require.Error(t, err)
}

func TestDecodeCursorRejectsMalformedCreatedAt(t *testing.T) {
	// Valid base64 + valid JSON but created_at is not a parseable timestamp.
	payload := `{"created_at":"not-a-date","id":"msg-xyz"}`
	encoded := base64.StdEncoding.EncodeToString([]byte(payload))
	_, _, err := DecodeCursor(encoded)
	require.Error(t, err)
}

func TestDecodeCursorRejectsMissingFields(t *testing.T) {
	cursor, err := encodeCursorPayload(cursorPayload{ID: "msg-aaa"})
	require.NoError(t, err)

	_, _, err = DecodeCursor(cursor)
	require.Error(t, err)
}
