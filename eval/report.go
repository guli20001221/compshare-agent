package eval

import (
	"fmt"
	"strings"
	"time"
)

// CaseResult records the outcome of a single evaluation case.
type CaseResult struct {
	CaseID         string
	Category       string
	Input          string
	ExpectedIntent string
	GotIntent      string
	IntentCorrect  bool
	ExpectedTools  []string
	GotTool        string
	ToolCorrect    bool
	ToolApplicable bool // false for knowledge_qa cases that expect no tool call
	// Content quality (knowledge_qa only)
	ContentCorrect bool // true if keyword check passes
	KeywordHits    int
	KeywordTotal   int
	Error          string
}

// ModelReport aggregates results for one model.
type ModelReport struct {
	ModelName      string
	Results        []CaseResult
	IntentCorrect  int
	IntentTotal    int
	ToolCorrect    int
	ToolTotal      int // excludes non-applicable cases
	ContentCorrect int
	ContentTotal   int // knowledge_qa cases with keywords
	Duration       time.Duration
}

// IntentAccuracy returns the intent accuracy as a percentage.
func (r *ModelReport) IntentAccuracy() float64 {
	if r.IntentTotal == 0 {
		return 0
	}
	return float64(r.IntentCorrect) / float64(r.IntentTotal) * 100
}

// ToolAccuracy returns the tool accuracy as a percentage.
func (r *ModelReport) ToolAccuracy() float64 {
	if r.ToolTotal == 0 {
		return 0
	}
	return float64(r.ToolCorrect) / float64(r.ToolTotal) * 100
}

// PassFail counts cases that pass ALL applicable checks vs those that fail any.
// A case passes only if: intent correct AND (tool correct or N/A) AND (content correct or N/A).
func (r *ModelReport) PassFail() (pass, fail int) {
	for _, c := range r.Results {
		ok := c.IntentCorrect
		if ok && c.ToolApplicable && !c.ToolCorrect {
			ok = false
		}
		if ok && c.KeywordTotal > 0 && !c.ContentCorrect {
			ok = false
		}
		if ok {
			pass++
		} else {
			fail++
		}
	}
	return
}

// ContentAccuracy returns the content quality accuracy for knowledge_qa cases.
func (r *ModelReport) ContentAccuracy() float64 {
	if r.ContentTotal == 0 {
		return 0
	}
	return float64(r.ContentCorrect) / float64(r.ContentTotal) * 100
}

// Tally recomputes aggregate counts from Results.
func (r *ModelReport) Tally() {
	r.IntentCorrect, r.IntentTotal = 0, 0
	r.ToolCorrect, r.ToolTotal = 0, 0
	r.ContentCorrect, r.ContentTotal = 0, 0
	for _, c := range r.Results {
		r.IntentTotal++
		if c.IntentCorrect {
			r.IntentCorrect++
		}
		if c.ToolApplicable {
			r.ToolTotal++
			if c.ToolCorrect {
				r.ToolCorrect++
			}
		}
		if c.KeywordTotal > 0 {
			r.ContentTotal++
			if c.ContentCorrect {
				r.ContentCorrect++
			}
		}
	}
}

// FormatReport generates a Markdown report from one or more model reports.
func FormatReport(reports []ModelReport) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Eval Report — %s\n\n", time.Now().Format("2006-01-02")))

	// Summary table
	sb.WriteString("| Model | Intent Acc | Tool Acc | Content Acc | Pass | Fail | Time |\n")
	sb.WriteString("|-------|:----------:|:--------:|:-----------:|:----:|:----:|-----:|\n")
	for _, r := range reports {
		pass, fail := r.PassFail()
		sb.WriteString(fmt.Sprintf("| %s | %.1f%% | %.1f%% | %.1f%% (%d/%d) | %d | %d | %s |\n",
			r.ModelName, r.IntentAccuracy(), r.ToolAccuracy(),
			r.ContentAccuracy(), r.ContentCorrect, r.ContentTotal,
			pass, fail, r.Duration.Round(time.Second)))
	}

	// Failure details
	sb.WriteString("\n### Failures\n\n")
	sb.WriteString("| Case ID | Model | Type | Expected | Got | Detail |\n")
	sb.WriteString("|---------|-------|------|----------|-----|--------|\n")
	hasFailures := false
	for _, r := range reports {
		for _, c := range r.Results {
			if !c.IntentCorrect {
				hasFailures = true
				sb.WriteString(fmt.Sprintf("| %s | %s | intent | %s | %s | tool=%s |\n",
					c.CaseID, r.ModelName, c.ExpectedIntent, c.GotIntent, c.GotTool))
			}
			if c.ToolApplicable && !c.ToolCorrect {
				hasFailures = true
				sb.WriteString(fmt.Sprintf("| %s | %s | tool | %s | %s | |\n",
					c.CaseID, r.ModelName, strings.Join(c.ExpectedTools, "/"), c.GotTool))
			}
			if c.KeywordTotal > 0 && !c.ContentCorrect {
				hasFailures = true
				sb.WriteString(fmt.Sprintf("| %s | %s | content | %d/%d keywords | hit %d | |\n",
					c.CaseID, r.ModelName, 2, c.KeywordTotal, c.KeywordHits))
			}
		}
	}
	if !hasFailures {
		sb.WriteString("| (none) | | | | | |\n")
	}

	return sb.String()
}
