package prompt

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/diagnosis"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
)

func TestPromptRegistryParity_Workflows(t *testing.T) {
	registered := workflow.RegisteredWorkflowActions()
	requireNoDuplicateStrings(t, "workflow registry", registered)

	toolNames := registryNamesMatching(func(name string) bool {
		return workflow.IsWorkflowTool(name)
	})
	assertSameStringSet(t, "workflow registry vs tools.Registry", registered, toolNames)

	subset := intent.IntentToolSubset(intent.IntentOperationLifecycle)
	for _, name := range registered {
		if !containsString(subset, name) {
			t.Fatalf("operation_lifecycle subset missing workflow %s", name)
		}
	}
}

func TestPromptRegistryParity_Diagnosis(t *testing.T) {
	registered := diagnosis.RegisteredDiagnosisActions()
	requireNoDuplicateStrings(t, "diagnosis registry", registered)

	toolNames := registryNamesMatching(func(name string) bool {
		return diagnosis.IsDiagnosisTool(name)
	})
	assertSameStringSet(t, "diagnosis registry vs tools.Registry", registered, toolNames)

	subset := intent.IntentToolSubset(intent.IntentDiagnosis)
	for _, name := range registered {
		if !containsString(subset, name) {
			t.Fatalf("diagnosis subset missing tool %s", name)
		}
	}
}

func TestPromptSurfaceContainsRegisteredActions(t *testing.T) {
	mutatingPrompt := BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: true})
	readOnlyPrompt := BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: false})

	for _, name := range workflow.RegisteredWorkflowActions() {
		if !strings.Contains(mutatingPrompt, name) {
			t.Fatalf("mutating prompt missing workflow %s", name)
		}
		if strings.Contains(readOnlyPrompt, name) {
			t.Fatalf("read-only prompt unexpectedly contains workflow %s", name)
		}
	}
	for _, name := range diagnosis.RegisteredDiagnosisActions() {
		if !strings.Contains(mutatingPrompt, name) {
			t.Fatalf("mutating prompt missing diagnosis %s", name)
		}
		if !strings.Contains(readOnlyPrompt, name) {
			t.Fatalf("read-only prompt missing diagnosis %s", name)
		}
	}
}

func registryNamesMatching(match func(string) bool) []string {
	var names []string
	for _, tool := range tools.Registry {
		if tool.Function == nil {
			continue
		}
		name := tool.Function.Name
		if match(name) {
			names = append(names, name)
		}
	}
	return names
}

func assertSameStringSet(t *testing.T, label string, left, right []string) {
	t.Helper()
	leftSet := make(map[string]bool, len(left))
	for _, name := range left {
		leftSet[name] = true
	}
	rightSet := make(map[string]bool, len(right))
	for _, name := range right {
		rightSet[name] = true
	}
	for name := range leftSet {
		if !rightSet[name] {
			t.Fatalf("%s: %s missing from right set\nleft=%v\nright=%v", label, name, left, right)
		}
	}
	for name := range rightSet {
		if !leftSet[name] {
			t.Fatalf("%s: %s missing from left set\nleft=%v\nright=%v", label, name, left, right)
		}
	}
}

func requireNoDuplicateStrings(t *testing.T, label string, names []string) {
	t.Helper()
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			t.Fatalf("%s has duplicate %s: %v", label, name, names)
		}
		seen[name] = true
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
