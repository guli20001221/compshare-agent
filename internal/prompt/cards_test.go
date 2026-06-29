package prompt

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/diagnosis"
	"github.com/compshare-agent/internal/tools"
)

func TestRenderWorkflowSelectionCardUsesRegistryAndToolDescriptions(t *testing.T) {
	card := renderWorkflowSelectionCard()
	if strings.Contains(card, "CreateInstanceWorkflow") {
		t.Fatalf("workflow card must not expose create workflow after create_instance became the create entry:\n%s", card)
	}
	for _, name := range reactWorkflowActions() {
		if !strings.Contains(card, name+"：") {
			t.Fatalf("workflow card missing action name %s:\n%s", name, card)
		}
		desc := toolDescriptionForTest(t, name)
		if !strings.Contains(card, desc) {
			t.Fatalf("workflow card missing tool description for %s:\n%s", name, card)
		}
	}
	assertStableOrder(t, card, reactWorkflowActions())
}

func TestRenderDiagnosisSelectionCardUsesRegistryAndToolDescriptions(t *testing.T) {
	card := renderDiagnosisSelectionCard()
	for _, name := range diagnosis.RegisteredDiagnosisActions() {
		if !strings.Contains(card, name+"：") {
			t.Fatalf("diagnosis card missing action name %s:\n%s", name, card)
		}
		desc := toolDescriptionForTest(t, name)
		if !strings.Contains(card, desc) {
			t.Fatalf("diagnosis card missing tool description for %s:\n%s", name, card)
		}
	}
	assertStableOrder(t, card, diagnosis.RegisteredDiagnosisActions())
}

func TestWorkflowNoPretextActionListUsesRegistryOrder(t *testing.T) {
	list := renderWorkflowActionNameList()
	want := strings.Join(reactWorkflowActions(), " / ")
	if list != want {
		t.Fatalf("workflow no-pretext list = %q, want %q", list, want)
	}
}

func toolDescriptionForTest(t *testing.T, name string) string {
	t.Helper()
	for _, tool := range tools.Registry {
		if tool.Function != nil && tool.Function.Name == name {
			return tool.Function.Description
		}
	}
	t.Fatalf("tool %s not found", name)
	return ""
}

func assertStableOrder(t *testing.T, text string, names []string) {
	t.Helper()
	last := -1
	for _, name := range names {
		idx := strings.Index(text, name)
		if idx < 0 {
			t.Fatalf("%s missing from text:\n%s", name, text)
		}
		if idx <= last {
			t.Fatalf("%s appears out of order in:\n%s", name, text)
		}
		last = idx
	}
}
