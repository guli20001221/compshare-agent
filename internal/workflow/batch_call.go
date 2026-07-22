package workflow

import "fmt"

// MaxBatchCalls bounds one batch step's upstream fan-out. It is a guard against a
// candidate list that grew unexpectedly (a runaway or a cross-product bug), not a
// tuning knob. Its real consumer is the capacity probe (stepProbeZoneCapacity),
// whose fan-out is one call per offered (model, zone) row of the catalog so both
// hardware cards can be gated on real creatability — ~19 rows against the live
// catalog today, not the four zones of a single model an earlier version of this
// comment assumed. The bound MUST stay above that real fan-out: a bound below it
// drops the tail, and a dropped capacity probe reads downstream as "unknown =
// selectable" — which is how a sold-out model at the end of the list gets offered
// as clickable (see TestGPUCardGraysASoldOutModelPastTheCapacityProbeFanOut). 40
// leaves headroom for new regions while still catching a true explosion. Calls
// past the bound are recorded as explicit unknowns, never silently dropped.
const MaxBatchCalls = 40

// batchResultsKey is where a batch step's collected outcomes live inside its
// StepResults entry. It is deliberately not a bare list at the top level: the
// entry stays a map[string]any like every other step result, so nothing
// downstream has to special-case the shape of a step it did not expect.
const batchResultsKey = "BatchResults"

// BatchCall is one upstream call a batch step will make.
type BatchCall struct {
	// Key identifies which candidate this call is about (a zone, for the capacity
	// probe). It travels onto the outcome so a consumer matches results to
	// candidates by identity instead of by position — a call that is never made
	// because of the bound would otherwise shift every later index.
	Key string
	// Args are this call's upstream arguments.
	Args map[string]any
}

// BatchOutcome is what one call in a batch produced. The three states are
// distinct on purpose: OK with a result, a call that ran and failed, and a call
// that was never made. Only the first says anything about the candidate.
type BatchOutcome struct {
	Key    string
	OK     bool
	Result map[string]any
	// Err is the failure text when the call ran and failed, or the reason it was
	// never made. Non-empty exactly when OK is false.
	Err string
}

// BatchResults reads a batch step's outcomes back out of a step result. It
// returns nil for a non-batch or missing result, which callers must treat as "no
// information", never as "no candidate is available".
func BatchResults(result map[string]any) []BatchOutcome {
	raw, _ := result[batchResultsKey].([]any)
	if len(raw) == 0 {
		return nil
	}
	out := make([]BatchOutcome, 0, len(raw))
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key, _ := entry["Key"].(string)
		okFlag, _ := entry["OK"].(bool)
		res, _ := entry["Result"].(map[string]any)
		errText, _ := entry["Err"].(string)
		out = append(out, BatchOutcome{Key: key, OK: okFlag, Result: res, Err: errText})
	}
	return out
}

// encodeBatchOutcomes renders outcomes into the generic map shape a step result
// must have. Kept next to BatchResults so the writer and the reader of this
// shape are one edit apart.
func encodeBatchOutcomes(outcomes []BatchOutcome) map[string]any {
	encoded := make([]any, 0, len(outcomes))
	for _, o := range outcomes {
		entry := map[string]any{"Key": o.Key, "OK": o.OK}
		if o.Result != nil {
			entry["Result"] = o.Result
		}
		if o.Err != "" {
			entry["Err"] = o.Err
		}
		encoded = append(encoded, entry)
	}
	return map[string]any{batchResultsKey: encoded}
}

// batchNotAttempted is the recorded reason for a call the bound stopped. It is a
// sentence rather than a flag because it reaches the same failure record a real
// upstream error would, and a reader of that record should not have to know the
// bound exists to understand why nothing came back.
func batchNotAttempted(limit int) string {
	return fmt.Sprintf("未发起查询：超出单步批量上限（%d）", limit)
}
