package engine

import (
	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/tools"
)

// centralAgentToolWindow is the grouped P6 capability surface. It intentionally
// does not expose the underlying API tools used by deterministic handlers, so
// every platform fact crosses ReadPlatformCapability and its EvidenceEnvelope.
// The two internal capabilities remain shadowed in the legacy registry until
// the grouped P6 rollout is complete; this function is the only opt-in view.
func centralAgentToolWindow(mutatingEnabled bool) []openai.Tool {
	registry := tools.DefaultCapabilityRegistry()
	var out []openai.Tool
	if capability, ok := registry.Lookup(tools.ReadPlatformCapabilityName); ok {
		out = append(out, capability.Tool)
	}
	if capability, ok := registry.Lookup(tools.UpdateTaskStateName); ok {
		out = append(out, capability.Tool)
	}
	if mutatingEnabled {
		if capability, ok := registry.Lookup(tools.ProposeActionName); ok {
			out = append(out, capability.Tool)
		}
	}
	for _, capability := range registry.All() {
		if !capability.ExposedToAgent || capability.Tool.Function == nil {
			continue
		}
		if capability.Name == "SearchKnowledge" || capability.Policy.Route == tools.ActionRouteDiagnosis {
			out = append(out, capability.Tool)
		}
	}
	return out
}

func centralAgentToolNames(mutatingEnabled bool) []string {
	window := centralAgentToolWindow(mutatingEnabled)
	names := make([]string, 0, len(window))
	for _, tool := range window {
		if tool.Function != nil && tool.Function.Name != "" {
			names = append(names, tool.Function.Name)
		}
	}
	return names
}
