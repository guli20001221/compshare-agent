package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/diagnosis"
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
