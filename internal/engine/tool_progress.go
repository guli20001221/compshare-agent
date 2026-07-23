package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/diagnosis"
	"github.com/compshare-agent/internal/intent"
)

// repeatableAgentTool no longer lists the local GPU knowledge tools: they were
// deleted along with the static spec table they read, so there is nothing left
// to mark repeatable.
func repeatableAgentTool(action string) bool {
	if action == "SearchKnowledge" || diagnosis.IsDiagnosisTool(action) {
		return true
	}
	_, ok := capability.ReadIntentForTool(action)
	return ok
}

// maxUniqueAgentToolCalls caps only capabilities whose upstream facts do not
// become more authoritative when the model keeps changing equivalent arguments.
// Two attempts leave room for one genuine correction without allowing a monitor
// turn to consume the whole ReAct budget.
func maxUniqueAgentToolCalls(action string) int {
	readIntent, ok := capability.ReadIntentForTool(action)
	if !ok {
		return 0
	}
	switch readIntent {
	case intent.IntentMonitorQuery, intent.IntentMonitorHistory:
		return 2
	default:
		return 0
	}
}

// singleShotAgentTool identifies immutable whole-catalog reads. Once one such
// call succeeds, changing arguments cannot reveal fresher facts in the same
// turn; the next round should answer from the existing observation instead of
// spending model rounds re-filtering the same snapshot.
func singleShotAgentTool(action string) bool {
	readIntent, ok := capability.ReadIntentForTool(action)
	return ok && readIntent == intent.IntentZoneCatalog
}

func completedAgentToolCall(results map[string]string, action string) bool {
	for key, raw := range results {
		recordedAction, _, found := strings.Cut(key, ":")
		if !found || recordedAction != action {
			continue
		}
		var observation struct {
			Status string `json:"status"`
		}
		if json.Unmarshal([]byte(raw), &observation) == nil {
			switch observation.Status {
			case "handled", "empty", "conflict", "unavailable":
				return true
			}
		}
	}
	return false
}

func uniqueAgentToolCalls(results map[string]string, action string) int {
	count := 0
	for key := range results {
		recordedAction, _, found := strings.Cut(key, ":")
		if found && recordedAction == action {
			count++
		}
	}
	return count
}

func toolCallBudgetObservation(action string, limit int) string {
	payload, _ := json.Marshal(map[string]any{
		"status":                 "call_budget_exhausted",
		"action":                 action,
		"max_unique_calls":       limit,
		"required_next_decision": "answer from the existing observations or ask the user for one specific missing field; do not call this capability again this turn",
	})
	return string(payload)
}

func decodeToolArgsForProgress(raw string) (map[string]any, bool) {
	var args map[string]any
	if json.Unmarshal([]byte(raw), &args) != nil {
		return nil, false
	}
	return args, true
}

func toolProgressCallKey(action string, args map[string]any) string {
	payload, _ := json.Marshal(args)
	return action + ":" + digestToolProgress(payload)
}

func digestToolProgress(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func repeatedToolObservation(action, previous string) string {
	var observation any
	if json.Unmarshal([]byte(previous), &observation) != nil {
		observation = previous
	}
	payload, _ := json.Marshal(map[string]any{
		"status":                 "reused_observation",
		"action":                 action,
		"same_call_blocked":      true,
		"observation":            observation,
		"required_next_decision": "do_not_repeat_the_same_arguments; answer, clarify, or use materially different arguments",
	})
	return string(payload)
}
