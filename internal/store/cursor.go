package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

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
		return time.Time{}, "", fmt.Errorf("decode cursor: %w", err)
	}
	var payload cursorPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return time.Time{}, "", fmt.Errorf("parse cursor: %w", err)
	}
	if payload.CreatedAt == "" || payload.ID == "" {
		return time.Time{}, "", fmt.Errorf("invalid cursor: missing fields")
	}
	ts, err := time.Parse(time.RFC3339Nano, payload.CreatedAt)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("parse cursor created_at: %w", err)
	}
	return ts, payload.ID, nil
}
