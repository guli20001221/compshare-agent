package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bitly/go-simplejson"
	"github.com/stretchr/testify/require"
)

// recordedEvent is one frame written by chatStream during a test.
type recordedEvent struct {
	Event string
	Data  any
}

// recordingSink is a streamWriter test double. It captures every frame the
// chat streaming core emits so tests can assert on event order and payload
// content without a live transport (SSE or WS). It replaces the former pattern
// of scraping SSE text out of an httptest recorder.
type recordingSink struct {
	events []recordedEvent
}

func (s *recordingSink) WriteEvent(event string, data any) error {
	s.events = append(s.events, recordedEvent{Event: event, Data: data})
	return nil
}

func (s *recordingSink) WriteKeepalive() error { return nil }

// has reports whether an event of the given name was emitted.
func (s *recordingSink) has(event string) bool {
	for _, e := range s.events {
		if e.Event == event {
			return true
		}
	}
	return false
}

// lastIndexOf returns the index of the last frame with the given event name, or
// -1. Used to assert ordering (e.g. all steps precede done).
func (s *recordingSink) lastIndexOf(event string) int {
	idx := -1
	for i, e := range s.events {
		if e.Event == event {
			idx = i
		}
	}
	return idx
}

func (s *recordingSink) firstIndexOf(event string) int {
	for i, e := range s.events {
		if e.Event == event {
			return i
		}
	}
	return -1
}

// body marshals all recorded frames to a single JSON string, so tests can keep
// using substring assertions (e.g. "the streamed output still contains the raw
// IP") that previously ran against the SSE response body.
func (s *recordingSink) body() string {
	raw, _ := json.Marshal(s.events)
	return string(raw)
}

// runChatJSON parses a gateway request body (the same JSON the SSE tests built)
// and runs the chat turn through a recordingSink, returning the captured frames
// and any pre-stream *APIError (e.g. validation / turn-cap). It is the test
// entry point that replaces `h.Dispatch(c)` for SendCSAgentChat now that chat
// streaming is transport-agnostic.
func runChatJSON(t *testing.T, h *Handlers, jsonBody string) (*recordingSink, *APIError) {
	t.Helper()
	raw, err := simplejson.NewJson([]byte(jsonBody))
	require.NoError(t, err)

	base := BaseRequest{
		Action:      raw.Get("Action").MustString(),
		RequestUUID: raw.Get("request_uuid").MustString(),
		ProjectID:   raw.Get("ProjectId").MustString(),
		UserEmail:   raw.Get("user_email").MustString(),
	}
	base.Owner.TopOrganizationID = uint32(raw.Get("top_organization_id").MustInt64())
	base.Owner.OrganizationID = uint32(raw.Get("organization_id").MustInt64())

	sessionID := raw.Get("SessionId").MustString()
	message := strings.TrimSpace(raw.Get("Message").MustString())
	image := strings.TrimSpace(raw.Get("Image").MustString())

	prep, apiErr := h.prepareChat(context.Background(), base, sessionID, message, image)
	if apiErr != nil {
		return &recordingSink{}, apiErr
	}
	defer prep.release()

	sink := &recordingSink{}
	h.chatStream(context.Background(), sink, base, prep)
	return sink, nil
}
