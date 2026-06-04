package tracegate

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/observability"
)

func TestAggregateCountsTraceSignalsAndLabels(t *testing.T) {
	records := []observability.TraceRecord{
		{
			TurnID:            "diag-anchor",
			ActualRuntimeForm: observability.RuntimeFormAgent,
			Planner: observability.PlannerTrace{
				Enabled:            true,
				SchemaValid:        true,
				Intent:             "knowledge_qa",
				PlannedRuntimeForm: observability.RuntimeFormAgent,
			},
			Outcome: observability.OutcomeTrace{EscapedHallucinatedCount: 2},
		},
		{
			TurnID: "routing-mismatch",
			Planner: observability.PlannerTrace{
				Enabled:            true,
				SchemaValid:        false,
				Intent:             "resource_info",
				PlannedRuntimeForm: observability.RuntimeFormRouting,
			},
			ToolCalls: []observability.ToolCallTrace{{Source: observability.ToolSourceMainReAct}},
		},
		{
			TurnID:  "unlabeled-no-planner",
			Outcome: observability.OutcomeTrace{},
		},
	}
	labels := Labels{
		Cases: []CaseLabel{
			{
				TurnID:             "diag-anchor",
				ExpectedIntent:     "diagnosis",
				ForbiddenIntent:    "knowledge_qa",
				CuratedSchemaValid: true,
			},
			{
				TurnID:             "routing-mismatch",
				ExpectedIntent:     "resource_info",
				CuratedSchemaValid: true,
			},
		},
	}

	stats := Aggregate(records, labels)

	if stats.TotalRecords != 3 {
		t.Fatalf("TotalRecords = %d, want 3", stats.TotalRecords)
	}
	if stats.PlannerHandled != 2 {
		t.Fatalf("PlannerHandled = %d, want 2", stats.PlannerHandled)
	}
	if stats.SchemaInvalid != 1 {
		t.Fatalf("SchemaInvalid = %d, want 1", stats.SchemaInvalid)
	}
	if stats.CuratedSchemaTotal != 2 || stats.CuratedSchemaInvalid != 1 {
		t.Fatalf("curated schema = %d/%d invalid, want 1/2",
			stats.CuratedSchemaInvalid, stats.CuratedSchemaTotal)
	}
	if stats.RuntimeFormCompared != 2 || stats.RuntimeFormMismatch != 1 {
		t.Fatalf("runtime mismatch = %d/%d, want 1/2",
			stats.RuntimeFormMismatch, stats.RuntimeFormCompared)
	}
	if stats.EscapedHallucinatedCount != 2 {
		t.Fatalf("EscapedHallucinatedCount = %d, want 2", stats.EscapedHallucinatedCount)
	}
	if len(stats.IntentMismatches) != 1 {
		t.Fatalf("IntentMismatches = %d, want 1", len(stats.IntentMismatches))
	}
	if len(stats.ForbiddenIntentHits) != 1 {
		t.Fatalf("ForbiddenIntentHits = %d, want 1", len(stats.ForbiddenIntentHits))
	}
}

func TestGateFailures(t *testing.T) {
	stats := Stats{
		PlannerHandled:           4,
		SchemaInvalid:            2,
		CuratedSchemaTotal:       1,
		CuratedSchemaInvalid:     1,
		RuntimeFormCompared:      4,
		RuntimeFormMismatch:      2,
		EscapedHallucinatedCount: 1,
		IntentMismatches:         []IntentMismatch{{Key: "k", Want: "diagnosis", Got: "knowledge_qa"}},
		ForbiddenIntentHits:      []ForbiddenIntentHit{{Key: "k", Forbidden: "knowledge_qa"}},
	}
	labels := Labels{
		Thresholds: Thresholds{
			RuntimeFormMismatchRateMax: 0.25,
			SchemaValidRateMin:         0.75,
		},
	}

	failures := GateFailures(stats, labels)
	codes := map[string]bool{}
	for _, failure := range failures {
		codes[failure.Code] = true
	}
	for _, code := range []string{
		"escaped_hallucinated_nonzero",
		"curated_schema_invalid",
		"intent_mismatch",
		"forbidden_intent",
		"runtime_form_mismatch_rate",
		"schema_valid_rate",
	} {
		if !codes[code] {
			t.Fatalf("missing gate failure code %q in %#v", code, failures)
		}
	}
}

func TestLoadRecordsJSONLIgnoresBlankLines(t *testing.T) {
	input := strings.NewReader(`
{"turn_id":"turn-1","planner":{"enabled":true,"schema_valid":true,"intent":"diagnosis"}}

{"turn_id":"turn-2","outcome":{"escaped_hallucinated_count":0}}
`)

	records, err := LoadRecordsJSONL(input)
	if err != nil {
		t.Fatalf("LoadRecordsJSONL returned error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records len = %d, want 2", len(records))
	}
	if records[0].TurnID != "turn-1" || records[1].TurnID != "turn-2" {
		t.Fatalf("unexpected turn ids: %#v", records)
	}
}
