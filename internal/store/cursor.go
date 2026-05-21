package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// ErrInvalidCursor is returned by DecodeCursor when the cursor string cannot
// be decoded or parsed. Callers can distinguish this from store/database
// errors using errors.As.
type ErrInvalidCursor struct {
	Reason error
}

func (e *ErrInvalidCursor) Error() string { return "invalid cursor: " + e.Reason.Error() }
func (e *ErrInvalidCursor) Unwrap() error { return e.Reason }

type cursorPayload struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

func EncodeCursor(createdAt time.Time, id string) (string, error) {
	return encodeCursorPayload(cursorPayload{
		CreatedAt: createdAt.UTC().Format(time.RFC3339Nano),
		ID:        id,
	})
}

func encodeCursorPayload(payload cursorPayload) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func DecodeCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", &ErrInvalidCursor{Reason: fmt.Errorf("decode cursor: %w", err)}
	}
	var payload cursorPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return time.Time{}, "", &ErrInvalidCursor{Reason: fmt.Errorf("parse cursor: %w", err)}
	}
	if payload.CreatedAt == "" || payload.ID == "" {
		return time.Time{}, "", &ErrInvalidCursor{Reason: fmt.Errorf("missing fields")}
	}
	ts, err := time.Parse(time.RFC3339Nano, payload.CreatedAt)
	if err != nil {
		return time.Time{}, "", &ErrInvalidCursor{Reason: fmt.Errorf("parse cursor created_at: %w", err)}
	}
	return ts, payload.ID, nil
}
