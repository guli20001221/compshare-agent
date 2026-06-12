package tracegate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/compshare-agent/internal/observability"
)

type Labels struct {
	Thresholds Thresholds  `json:"thresholds"`
	Cases      []CaseLabel `json:"cases"`
}

type Thresholds struct {
	RuntimeFormMismatchRateMax float64 `json:"runtime_form_mismatch_rate_max"`
	SchemaValidRateMin         float64 `json:"schema_valid_rate_min,omitempty"`
}

type CaseLabel struct {
	TurnID             string `json:"turn_id,omitempty"`
	TraceID            string `json:"trace_id,omitempty"`
	ExpectedIntent     string `json:"expected_intent,omitempty"`
	ForbiddenIntent    string `json:"forbidden_intent,omitempty"`
	CuratedSchemaValid bool   `json:"curated_schema_valid,omitempty"`
	Note               string `json:"note,omitempty"`
}

type Stats struct {
	TotalRecords             int
	PlannerHandled           int
	SchemaInvalid            int
	CuratedSchemaTotal       int
	CuratedSchemaInvalid     int
	RuntimeFormCompared      int
	RuntimeFormMismatch      int
	EscapedHallucinatedCount int
	LabeledRecords           int
	IntentMismatches         []IntentMismatch
	ForbiddenIntentHits      []ForbiddenIntentHit
}

type IntentMismatch struct {
	Key  string
	Want string
	Got  string
}

type ForbiddenIntentHit struct {
	Key       string
	Forbidden string
}

type GateFailure struct {
	Code    string
	Message string
}

func Aggregate(records []observability.TraceRecord, labels Labels) Stats {
	byKey := labelsByRecordKey(labels.Cases)
	var stats Stats
	for _, record := range records {
		stats.TotalRecords++
		if plannerObserved(record.IntentRouter) {
			stats.PlannerHandled++
			if !record.IntentRouter.SchemaValid {
				stats.SchemaInvalid++
			}
		}
		if mismatch, ok := record.RuntimeFormMismatch(); ok {
			stats.RuntimeFormCompared++
			if mismatch {
				stats.RuntimeFormMismatch++
			}
		}
		stats.EscapedHallucinatedCount += record.Outcome.EscapedHallucinatedCount

		label, ok := byKey[recordKey(record)]
		if !ok {
			continue
		}
		stats.LabeledRecords++
		if label.CuratedSchemaValid {
			stats.CuratedSchemaTotal++
			if !record.IntentRouter.SchemaValid {
				stats.CuratedSchemaInvalid++
			}
		}
		if label.ExpectedIntent != "" && record.IntentRouter.Intent != label.ExpectedIntent {
			stats.IntentMismatches = append(stats.IntentMismatches, IntentMismatch{
				Key:  recordKey(record),
				Want: label.ExpectedIntent,
				Got:  record.IntentRouter.Intent,
			})
		}
		if label.ForbiddenIntent != "" && record.IntentRouter.Intent == label.ForbiddenIntent {
			stats.ForbiddenIntentHits = append(stats.ForbiddenIntentHits, ForbiddenIntentHit{
				Key:       recordKey(record),
				Forbidden: label.ForbiddenIntent,
			})
		}
	}
	return stats
}

func (s Stats) RuntimeFormMismatchRate() float64 {
	if s.RuntimeFormCompared == 0 {
		return 0
	}
	return float64(s.RuntimeFormMismatch) / float64(s.RuntimeFormCompared)
}

func (s Stats) SchemaValidRate() float64 {
	if s.PlannerHandled == 0 {
		return 1
	}
	return float64(s.PlannerHandled-s.SchemaInvalid) / float64(s.PlannerHandled)
}

func GateFailures(stats Stats, labels Labels) []GateFailure {
	var failures []GateFailure
	if stats.EscapedHallucinatedCount != 0 {
		failures = append(failures, GateFailure{
			Code:    "escaped_hallucinated_nonzero",
			Message: fmt.Sprintf("escaped hallucinated count = %d, want 0", stats.EscapedHallucinatedCount),
		})
	}
	if stats.CuratedSchemaInvalid != 0 {
		failures = append(failures, GateFailure{
			Code:    "curated_schema_invalid",
			Message: fmt.Sprintf("curated schema invalid = %d/%d, want 0", stats.CuratedSchemaInvalid, stats.CuratedSchemaTotal),
		})
	}
	if len(stats.IntentMismatches) != 0 {
		failures = append(failures, GateFailure{
			Code:    "intent_mismatch",
			Message: fmt.Sprintf("intent mismatches = %d, want 0", len(stats.IntentMismatches)),
		})
	}
	if len(stats.ForbiddenIntentHits) != 0 {
		failures = append(failures, GateFailure{
			Code:    "forbidden_intent",
			Message: fmt.Sprintf("forbidden intent hits = %d, want 0", len(stats.ForbiddenIntentHits)),
		})
	}
	if stats.RuntimeFormCompared > 0 && stats.RuntimeFormMismatchRate() > labels.Thresholds.RuntimeFormMismatchRateMax {
		failures = append(failures, GateFailure{
			Code: "runtime_form_mismatch_rate",
			Message: fmt.Sprintf("runtime form mismatch rate = %.3f, max %.3f",
				stats.RuntimeFormMismatchRate(), labels.Thresholds.RuntimeFormMismatchRateMax),
		})
	}
	if labels.Thresholds.SchemaValidRateMin > 0 && stats.SchemaValidRate() < labels.Thresholds.SchemaValidRateMin {
		failures = append(failures, GateFailure{
			Code: "schema_valid_rate",
			Message: fmt.Sprintf("schema valid rate = %.3f, min %.3f",
				stats.SchemaValidRate(), labels.Thresholds.SchemaValidRateMin),
		})
	}
	return failures
}

func LoadRecordsJSONL(r io.Reader) ([]observability.TraceRecord, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	var records []observability.TraceRecord
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record observability.TraceRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return nil, fmt.Errorf("decode JSONL line %d: %w", lineNo, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan JSONL: %w", err)
	}
	return records, nil
}

func LoadLabelsJSON(r io.Reader) (Labels, error) {
	var labels Labels
	if err := json.NewDecoder(r).Decode(&labels); err != nil {
		return Labels{}, fmt.Errorf("decode labels: %w", err)
	}
	return labels, nil
}

func labelsByRecordKey(labels []CaseLabel) map[string]CaseLabel {
	out := make(map[string]CaseLabel, len(labels))
	for _, label := range labels {
		key := label.TurnID
		if key == "" {
			key = label.TraceID
		}
		if key == "" {
			continue
		}
		out[key] = label
	}
	return out
}

func recordKey(record observability.TraceRecord) string {
	if record.TurnID != "" {
		return record.TurnID
	}
	return record.TraceID
}

func plannerObserved(trace observability.RouterTrace) bool {
	return trace.Enabled ||
		trace.Model != "" ||
		trace.LatencyMS != 0 ||
		trace.InputTokens != 0 ||
		trace.OutputTokens != 0 ||
		trace.SchemaValid ||
		trace.Intent != "" ||
		trace.PlannedRuntimeForm != "" ||
		len(trace.Skills) > 0 ||
		len(trace.Slots.TargetRefs) > 0 ||
		len(trace.Slots.Metrics) > 0 ||
		trace.Slots.TimeWindow != nil ||
		trace.Confidence != 0 ||
		trace.HardBlockHint ||
		trace.RouteStatus != ""
}
