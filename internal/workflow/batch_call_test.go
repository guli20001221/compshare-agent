package workflow

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// perCallExecutor answers by call index so a test can make exactly one call in a
// batch fail. mockExecutor keys on the action name, which cannot express "the
// second of three identical calls failed" — the case that decides whether a
// batch step degrades per candidate or as a whole.
type perCallExecutor struct {
	failIdx map[int]bool
	calls   []map[string]any
}

func (e *perCallExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	idx := len(e.calls)
	e.calls = append(e.calls, args)
	if e.failIdx[idx] {
		return nil, fmt.Errorf("upstream refused call %d", idx)
	}
	return map[string]any{"Echo": args["Zone"]}, nil
}

func batchStep(keys []string, opts ...func(*Step)) Step {
	s := Step{
		Name: "批量探测",
		Type: StepToolCall,
		Tool: "CheckCompShareResourceCapacity",
		BuildArgsBatch: func(*Context) ([]BatchCall, error) {
			calls := make([]BatchCall, 0, len(keys))
			for _, k := range keys {
				calls = append(calls, BatchCall{Key: k, Args: map[string]any{"Zone": k}})
			}
			return calls, nil
		},
	}
	for _, o := range opts {
		o(&s)
	}
	return s
}

func runBatch(t *testing.T, exec *perCallExecutor, step Step) (*Result, *Context) {
	t.Helper()
	def := &Definition{Name: "batch-test", Steps: []Step{step}}
	eng := NewEngine(exec, func(string, map[string]any) bool { return true }, nil)
	// RunOption is handed the fresh Context, which is the only way to read
	// StepResults back out of a completed run.
	var wfCtx *Context
	res, err := eng.Run(context.Background(), def, map[string]any{}, func(c *Context) { wfCtx = c })
	require.NoError(t, err)
	require.NotNil(t, wfCtx)
	return res, wfCtx
}

// TestBatchStepAsksOncePerCandidateAndKeepsAnswersMatchedToThem is the whole
// point of the mechanism: creatability is a property of (image, GPU, zone), so
// the only way to know which zones are creatable is to ask about each one. The
// answers must stay addressable BY CANDIDATE — a consumer that matched results
// to candidates positionally would silently mis-attribute every answer after the
// first call the bound or an error removed.
func TestBatchStepAsksOncePerCandidateAndKeepsAnswersMatchedToThem(t *testing.T) {
	exec := &perCallExecutor{}
	res, wfCtx := runBatch(t, exec, batchStep([]string{"cn-sh2-02", "cn-wlcb-03", "cn-wlcb-01"}))

	require.Empty(t, res.StoppedAt)
	require.Len(t, exec.calls, 3, "one upstream call per candidate")
	assert.Equal(t, "cn-sh2-02", exec.calls[0]["Zone"])
	assert.Equal(t, "cn-wlcb-03", exec.calls[1]["Zone"])
	assert.Equal(t, "cn-wlcb-01", exec.calls[2]["Zone"])

	byKey := map[string]BatchOutcome{}
	for _, o := range BatchResults(wfCtx.Result("批量探测")) {
		byKey[o.Key] = o
	}
	require.Len(t, byKey, 3)
	for _, zone := range []string{"cn-sh2-02", "cn-wlcb-03", "cn-wlcb-01"} {
		require.Contains(t, byKey, zone)
		assert.True(t, byKey[zone].OK, "%s answered successfully", zone)
		assert.Equal(t, zone, byKey[zone].Result["Echo"],
			"%s must carry ITS OWN answer, not another candidate's", zone)
	}
}

// TestBatchStepTreatsAFailedCallAsUnknownNotAsUnavailable holds the line the
// option builders already hold for a missing capacity signal: absence of an
// answer is not a negative answer. If one zone's probe errors, that zone is
// unknown — the other zones keep their real verdicts and the step continues. The
// alternative (fail the step) would throw away three good answers because of one
// timeout, and the alternative-alternative (treat the error as "sold out") would
// gray out a zone the user can actually create in.
func TestBatchStepTreatsAFailedCallAsUnknownNotAsUnavailable(t *testing.T) {
	exec := &perCallExecutor{failIdx: map[int]bool{1: true}}
	res, wfCtx := runBatch(t, exec, batchStep([]string{"cn-sh2-02", "cn-wlcb-03", "cn-wlcb-01"}))

	require.Empty(t, res.StoppedAt, "one failed candidate must not stop the workflow")
	require.Len(t, res.Steps, 1)
	assert.Equal(t, "success", res.Steps[0].Status)

	outcomes := BatchResults(wfCtx.Result("批量探测"))
	require.Len(t, outcomes, 3, "the failed candidate must still be REPORTED, not omitted")
	byKey := map[string]BatchOutcome{}
	for _, o := range outcomes {
		byKey[o.Key] = o
	}
	assert.True(t, byKey["cn-sh2-02"].OK)
	assert.True(t, byKey["cn-wlcb-01"].OK)

	unknown := byKey["cn-wlcb-03"]
	assert.False(t, unknown.OK, "a call that errored cannot claim a verdict")
	assert.NotEmpty(t, unknown.Err, "the reason must travel with the unknown so a card can say why")
	assert.Nil(t, unknown.Result, "an errored candidate must carry no result to misread")
}

// TestBatchStepFailsOnlyWhenEveryCallFailed separates the two failures a batch
// can have. Some candidates unknown describes those candidates; ALL candidates
// unknown describes the upstream, and continuing from it would build a card
// whose every option is "unknown" — indistinguishable from a healthy answer of
// "everything is available".
func TestBatchStepFailsOnlyWhenEveryCallFailed(t *testing.T) {
	t.Run("all fail, required step → workflow stops", func(t *testing.T) {
		exec := &perCallExecutor{failIdx: map[int]bool{0: true, 1: true}}
		res, _ := runBatch(t, exec, batchStep([]string{"cn-sh2-02", "cn-wlcb-03"}))
		assert.Equal(t, "批量探测", res.StoppedAt)
		require.NotNil(t, res.Failure, "a total failure must record what was sent")
		sent, _ := res.Failure.Args["Batch"].([]any)
		assert.Len(t, sent, 2, "the record names every call actually made")
	})

	t.Run("all fail, Optional step → workflow continues without a result", func(t *testing.T) {
		exec := &perCallExecutor{failIdx: map[int]bool{0: true, 1: true}}
		step := batchStep([]string{"cn-sh2-02", "cn-wlcb-03"}, func(s *Step) { s.Optional = true })
		res, wfCtx := runBatch(t, exec, step)
		assert.Empty(t, res.StoppedAt)
		assert.Nil(t, BatchResults(wfCtx.Result("批量探测")),
			"an optional probe that learned nothing must leave NO signal, not an empty verdict")
	})

	t.Run("one succeeds → the step succeeds", func(t *testing.T) {
		exec := &perCallExecutor{failIdx: map[int]bool{0: true}}
		res, _ := runBatch(t, exec, batchStep([]string{"cn-sh2-02", "cn-wlcb-03"}))
		assert.Empty(t, res.StoppedAt)
	})
}

// TestBatchStepRecordsCallsBeyondTheBoundInsteadOfDroppingThem guards the
// silent-truncation failure mode. A bound that shortened the outcome list would
// leave downstream unable to tell "this candidate was checked and is fine" from
// "this candidate was never checked" — the second rendered as the first is
// exactly how a card offers a zone nobody verified.
func TestBatchStepRecordsCallsBeyondTheBoundInsteadOfDroppingThem(t *testing.T) {
	keys := make([]string, 0, MaxBatchCalls+3)
	for i := 0; i < MaxBatchCalls+3; i++ {
		keys = append(keys, fmt.Sprintf("zone-%02d", i))
	}
	exec := &perCallExecutor{}
	res, wfCtx := runBatch(t, exec, batchStep(keys))

	require.Empty(t, res.StoppedAt)
	assert.Len(t, exec.calls, MaxBatchCalls, "the bound must actually stop the fan-out")

	outcomes := BatchResults(wfCtx.Result("批量探测"))
	require.Len(t, outcomes, len(keys), "every candidate must appear, checked or not")
	for i, o := range outcomes {
		assert.Equal(t, keys[i], o.Key)
		if i < MaxBatchCalls {
			assert.True(t, o.OK, "%s was within the bound", o.Key)
			continue
		}
		assert.False(t, o.OK, "%s was never asked about and must not claim a verdict", o.Key)
		assert.Contains(t, o.Err, "批量上限", "the record must say WHY it is unknown")
	}
}

// TestBatchStepWithNoCandidatesSucceedsWithoutCallingUpstream: an empty candidate
// list means there was nothing to ask, which is not an upstream failure. Failing
// here would stop a create whose GPU is offered in zero zones before the step
// that explains that properly.
func TestBatchStepWithNoCandidatesSucceedsWithoutCallingUpstream(t *testing.T) {
	exec := &perCallExecutor{}
	res, wfCtx := runBatch(t, exec, batchStep(nil))
	assert.Empty(t, res.StoppedAt)
	assert.Empty(t, exec.calls)
	assert.Empty(t, BatchResults(wfCtx.Result("批量探测")))
}

// TestBatchStepCheckResultSeesTheWholeBatch pins that a batch step can still
// reject — the gate reads the collected outcomes, not one response — and that a
// rejection carries its typed reason onto the record like any other CheckResult.
func TestBatchStepCheckResultSeesTheWholeBatch(t *testing.T) {
	exec := &perCallExecutor{}
	step := batchStep([]string{"cn-sh2-02", "cn-wlcb-03"}, func(s *Step) {
		s.CheckResult = func(_ *Context, result map[string]any) CheckOutcome {
			if len(BatchResults(result)) != 2 {
				return CheckFailed("检查未拿到完整批量结果")
			}
			return CheckFailedBecause(ReasonCapacitySoldOut, "所选配置在所有可用区均无库存")
		}
	})
	res, _ := runBatch(t, exec, step)

	require.Equal(t, "批量探测", res.StoppedAt)
	assert.Equal(t, "所选配置在所有可用区均无库存", res.Message)
	require.NotNil(t, res.Failure)
	assert.Equal(t, ReasonCapacitySoldOut, res.Failure.Reason,
		"a batch rejection classifies itself like any other, so callers branch on the reason not the sentence")
}
