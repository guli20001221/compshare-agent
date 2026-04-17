package eval

import (
	"strings"
	"testing"
	"time"
)

func TestModelReportPassFail_UsesAllApplicableChecks(t *testing.T) {
	report := ModelReport{
		Results: []CaseResult{
			// Full pass.
			{
				CaseID:         "ok_all",
				IntentCorrect:  true,
				ToolApplicable: true,
				ToolCorrect:    true,
				KeywordTotal:   2,
				ContentCorrect: true,
			},
			// Tool failure should fail the case even when intent is correct.
			{
				CaseID:         "bad_tool",
				IntentCorrect:  true,
				ToolApplicable: true,
				ToolCorrect:    false,
			},
			// Content failure should fail the case even when intent/tool are correct.
			{
				CaseID:         "bad_content",
				IntentCorrect:  true,
				ToolApplicable: false,
				ToolCorrect:    true,
				KeywordTotal:   3,
				ContentCorrect: false,
			},
			// Intent failure should fail regardless of other fields.
			{
				CaseID:         "bad_intent",
				IntentCorrect:  false,
				ToolApplicable: true,
				ToolCorrect:    true,
				KeywordTotal:   2,
				ContentCorrect: true,
			},
		},
	}

	pass, fail := report.PassFail()
	if pass != 1 || fail != 3 {
		t.Fatalf("PassFail() = (%d, %d), want (1, 3)", pass, fail)
	}
}

func TestFormatReport_SummaryUsesPassFailCounts(t *testing.T) {
	report := ModelReport{
		ModelName:      "test-model",
		IntentCorrect:  3,
		IntentTotal:    4,
		ToolCorrect:    1,
		ToolTotal:      2,
		ContentCorrect: 1,
		ContentTotal:   2,
		Duration:       3 * time.Second,
		Results: []CaseResult{
			{
				CaseID:         "ok_all",
				IntentCorrect:  true,
				ToolApplicable: true,
				ToolCorrect:    true,
				KeywordTotal:   2,
				ContentCorrect: true,
			},
			{
				CaseID:         "bad_tool",
				IntentCorrect:  true,
				ToolApplicable: true,
				ToolCorrect:    false,
			},
			{
				CaseID:         "bad_content",
				IntentCorrect:  true,
				ToolApplicable: false,
				ToolCorrect:    true,
				KeywordTotal:   2,
				ContentCorrect: false,
				KeywordHits:    1,
			},
			{
				CaseID:         "bad_intent",
				IntentCorrect:  false,
				ToolApplicable: false,
				ToolCorrect:    true,
			},
		},
	}

	out := FormatReport([]ModelReport{report})

	if !strings.Contains(out, "| test-model | 75.0% | 50.0% | 50.0% (1/2) | 1 | 3 | 3s |") {
		t.Fatalf("summary row did not use PassFail counts:\n%s", out)
	}
}
