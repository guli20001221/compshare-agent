package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/turncoord"
)

// coordinatorStreamEvent projects one durable coordinator event onto the
// WebSocket envelope. The canonical v2 fields are always present, while the
// legacy event discriminator and flattened display fields keep an older UI
// usable during the cutover. The durable Payload is never rewritten.
func coordinatorStreamEvent(turn store.Turn, event turncoord.Event) (string, map[string]any, error) {
	if strings.TrimSpace(event.TurnID) == "" || strings.TrimSpace(event.Type) == "" || event.Seq <= 0 {
		return "", nil, fmt.Errorf("invalid durable turn event")
	}
	if turn.ID != "" && turn.ID != event.TurnID {
		return "", nil, fmt.Errorf("event turn %s does not match subscribed turn %s", event.TurnID, turn.ID)
	}

	payload := map[string]any{}
	if len(event.Payload) != 0 {
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return "", nil, fmt.Errorf("decode durable event payload: %w", err)
		}
	}

	frame := map[string]any{
		"TurnId":      event.TurnID,
		"Seq":         event.Seq,
		"Type":        event.Type,
		"EventType":   event.Type,
		"Payload":     json.RawMessage(event.Payload),
		"Provisional": event.Provisional,
		"CreatedAt":   event.CreatedAt,
	}
	if turn.SessionID != "" {
		frame["SessionId"] = turn.SessionID
	}

	legacyEvent := event.Type
	switch event.Type {
	case "turn.running":
		legacyEvent = "meta"
		frame["Status"] = string(store.TurnStatusRunning)
		if turn.AssistantMessageID != "" {
			frame["MessageId"] = turn.AssistantMessageID
		}

	case "turn.step":
		legacyEvent = "step"
		copyPayloadField(frame, payload, "Action", "action")
		copyPayloadField(frame, payload, "Message", "message")
		// Type is reserved for the canonical durable event name. New clients
		// read the step subtype from Payload.type; StepType is an additive aid
		// for compatibility adapters.
		copyPayloadField(frame, payload, "StepType", "type")

	case "interaction.requested":
		kind, _ := payload["kind"].(string)
		if strings.EqualFold(kind, "selection") {
			legacyEvent = "selection"
		} else {
			legacyEvent = "confirmation"
		}
		interactionKey, _ := payload["interaction_key"].(string)
		frame["InteractionKey"] = interactionKey
		if legacyEvent == "selection" {
			frame["SelectionId"] = interactionKey
		} else {
			frame["ConfirmationId"] = interactionKey
		}
		inner := objectPayload(payload["payload"])
		copyPayloadField(frame, inner, "Action", "action")
		copyPayloadField(frame, inner, "Summary", "summary")
		copyPayloadField(frame, inner, "Form", "form")
		copyPayloadField(frame, inner, "TimeoutSeconds", "timeout_seconds")

	case "interaction.resolved":
		legacyEvent = "interaction_resolved"
		copyPayloadField(frame, payload, "InteractionKey", "interaction_key")

	case "turn.committed":
		legacyEvent = "done"
		frame["Committed"] = true
		frame["Status"] = string(store.TurnStatusCommitted)
		copyPayloadField(frame, payload, "Content", "content")
		copyPayloadField(frame, payload, "MessageId", "message_id")

	case "turn.failed", "turn.reconciled", "turn.lease_released":
		status, _ := payload["status"].(string)
		if status == string(store.TurnStatusAborted) {
			legacyEvent = "aborted"
		} else {
			legacyEvent = "error"
		}
		frame["Committed"] = false
		if status != "" {
			frame["Status"] = status
		}
		copyPayloadField(frame, payload, "Code", "reason")
		copyPayloadField(frame, payload, "Message", "message")
		if _, ok := frame["Message"]; !ok {
			copyPayloadField(frame, payload, "Message", "reason")
		}
	}

	return legacyEvent, frame, nil
}

func copyPayloadField(dst, src map[string]any, dstKey, srcKey string) {
	if value, ok := src[srcKey]; ok && value != nil {
		dst[dstKey] = value
	}
}

func objectPayload(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case json.RawMessage:
		var out map[string]any
		_ = json.Unmarshal(typed, &out)
		return out
	case string:
		var out map[string]any
		_ = json.Unmarshal([]byte(typed), &out)
		return out
	default:
		return nil
	}
}
