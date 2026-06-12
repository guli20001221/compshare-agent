package tracegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/observability"
)

func TestContextPromptBaselineGate(t *testing.T) {
	fixtures := filepath.Join("fixtures")
	recordFile, err := os.Open(filepath.Join(fixtures, "context_prompt_baseline.jsonl"))
	if err != nil {
		t.Fatalf("open baseline fixture: %v", err)
	}
	defer recordFile.Close()
	labelFile, err := os.Open(filepath.Join(fixtures, "context_prompt_baseline.labels.json"))
	if err != nil {
		t.Fatalf("open label fixture: %v", err)
	}
	defer labelFile.Close()

	records, err := LoadRecordsJSONL(recordFile)
	if err != nil {
		t.Fatalf("load records: %v", err)
	}
	labels, err := LoadLabelsJSON(labelFile)
	if err != nil {
		t.Fatalf("load labels: %v", err)
	}
	stats := Aggregate(records, labels)
	failures := GateFailures(stats, labels)
	if len(failures) != 0 {
		t.Fatalf("baseline gate failed: %s", formatFailures(failures))
	}
	if stats.LabeledRecords != len(labels.Cases) {
		t.Fatalf("labeled records = %d, labels = %d", stats.LabeledRecords, len(labels.Cases))
	}
}

func TestDiagnosisAnchorFailsWhenRoutedToKnowledgeQA(t *testing.T) {
	records := loadBaselineRecords(t)
	labels := loadBaselineLabels(t)

	// Locate the #123 diagnosis anchor by its label semantics (expected
	// diagnosis, forbidden knowledge_qa), not a hardcoded turn_id, so the gate
	// keeps protecting #123 when the fixture is refreshed from a real capture
	// (which produces agent-generated turn_ids).
	anchorKey := ""
	for _, c := range labels.Cases {
		if c.ExpectedIntent == "diagnosis" && c.ForbiddenIntent == "knowledge_qa" {
			anchorKey = c.TurnID
			if anchorKey == "" {
				anchorKey = c.TraceID
			}
			break
		}
	}
	if anchorKey == "" {
		t.Fatal("no diagnosis anchor (expected_intent=diagnosis, forbidden_intent=knowledge_qa) found in labels")
	}

	mutated := false
	for i := range records {
		if recordKey(records[i]) == anchorKey {
			records[i].IntentRouter.Intent = "knowledge_qa"
			mutated = true
		}
	}
	if !mutated {
		t.Fatalf("diagnosis anchor %q not present in records", anchorKey)
	}

	stats := Aggregate(records, labels)
	failures := GateFailures(stats, labels)
	if !hasFailure(failures, "intent_mismatch") || !hasFailure(failures, "forbidden_intent") {
		t.Fatalf("diagnosis anchor did not fail as expected: %#v", failures)
	}
}

func loadBaselineRecords(t *testing.T) []observability.TraceRecord {
	t.Helper()
	path := filepath.Join("fixtures", "context_prompt_baseline.jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open records: %v", err)
	}
	defer f.Close()
	records, err := LoadRecordsJSONL(f)
	if err != nil {
		t.Fatalf("load records: %v", err)
	}
	return records
}

func loadBaselineLabels(t *testing.T) Labels {
	t.Helper()
	path := filepath.Join("fixtures", "context_prompt_baseline.labels.json")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open labels: %v", err)
	}
	defer f.Close()
	labels, err := LoadLabelsJSON(f)
	if err != nil {
		t.Fatalf("load labels: %v", err)
	}
	return labels
}

func hasFailure(failures []GateFailure, code string) bool {
	for _, failure := range failures {
		if failure.Code == code {
			return true
		}
	}
	return false
}

func formatFailures(failures []GateFailure) string {
	var parts []string
	for _, failure := range failures {
		parts = append(parts, failure.Code+": "+failure.Message)
	}
	return strings.Join(parts, "; ")
}
