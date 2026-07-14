package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/turncoord"
)

// durableTurnCoordinator is the single execution authority used by the HTTP
// transport. Both legacy and v2 wire shapes enter this same interface once it
// is configured; the old in-memory chat writer is never a second writer.
type durableTurnCoordinator interface {
	Submit(context.Context, turncoord.SubmitInput, turncoord.EventSink) (turncoord.Submission, error)
	Subscribe(context.Context, store.Owner, string, int64, turncoord.EventSink) error
	GetTurn(context.Context, store.Owner, string) (store.Turn, error)
	FindTurnByClientID(context.Context, store.Owner, string, string) (store.Turn, error)
	ResolveInteraction(context.Context, store.Owner, string, string, turncoord.ConfirmationResponse) error
	AbortTurn(context.Context, store.Owner, string) (store.Turn, error)
}

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
		if status == string(store.TurnStatusFailedRetryable) {
			// failed_retryable is deliberately non-terminal in the store and the
			// coordinator starts it again from the frozen envelope. Project it as
			// reconciliation, otherwise the client would permanently block the
			// session and never subscribe to the eventual committed event.
			frame["ServerStatus"] = status
			frame["Status"] = "reconciling"
			frame["Code"] = "TurnNotSaved"
			frame["Message"] = "本轮尚未确认保存，正在恢复"
		} else if status == string(store.TurnStatusFailedFinal) {
			frame["ServerStatus"] = status
			frame["Status"] = status
			frame["Code"] = "TurnNotSaved"
			frame["Message"] = "本轮未能保存，请重新发送"
		} else if status != "" {
			frame["Status"] = status
		}
		if status != string(store.TurnStatusFailedRetryable) && status != string(store.TurnStatusFailedFinal) {
			copyPayloadField(frame, payload, "Code", "reason")
			copyPayloadField(frame, payload, "Message", "message")
			if _, ok := frame["Message"]; !ok {
				copyPayloadField(frame, payload, "Message", "reason")
			}
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
