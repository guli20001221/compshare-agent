package workflow

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// createdAndDescribed puts a sealed create context at the point the workflow has
// finished: the create returned an id, and the optional 查看状态 read came back with
// whatever the platform actually built. The observed row is written RELATIVE to the
// sealed intent, so the test asserts a divergence rather than two hard-coded numbers
// that could both drift with the catalog fixture.
func createdAndDescribed(t *testing.T, mutate func(intended, observed map[string]any)) (*Context, map[string]any) {
	t.Helper()
	wfCtx := draftContext("cn-sh2-02")
	runToTheGate(t, wfCtx)
	confirmAndSeal(t, wfCtx)

	snapshot, err := sealedCreateConfirmation(wfCtx)
	require.NoError(t, err)
	args := snapshot.Execution.Args
	require.NotZero(t, args.CPU, "the fixture must seal a real spec, or there is nothing to compare")
	require.NotZero(t, args.Memory)

	intended := map[string]any{"CPU": int(args.CPU), "Memory": int(args.Memory), "GpuType": args.GpuType}
	observed := map[string]any{
		"UHostId": "uhost-created-1",
		"State":   "Initializing",
		"CPU":     float64(args.CPU),
		"Memory":  float64(args.Memory),
		"GPU":     float64(args.GPU),
		"GpuType": args.GpuType,
		"Zone":    args.Zone,
	}
	if mutate != nil {
		mutate(intended, observed)
	}
	wfCtx.StepResults["创建实例"] = map[string]any{"UHostIds": []any{"uhost-created-1"}}
	wfCtx.StepResults["查看状态"] = map[string]any{"UHostSet": []any{observed}}
	return wfCtx, intended
}

// The production case, 2026-08-23: a create asked for 14 vCPU / 120 GB and the
// platform served 16 vCPU / 96 GB. The reply told the user 14C/120GB — not because
// anything lied, but because the requested spec was the only spec that existed
// anywhere in the turn. stepDescribeInstance had already run, against the exact
// created id; its answer was thrown away one function later.
func TestCreateResultReportsWhatWasServedNotOnlyWhatWasAsked(t *testing.T) {
	wfCtx, intended := createdAndDescribed(t, func(_, observed map[string]any) {
		observed["CPU"] = observed["CPU"].(float64) + 2
		observed["Memory"] = observed["Memory"].(float64) - 24576
	})

	data := createInstanceResultData(wfCtx)
	require.Equal(t, true, data["ActualVerified"])

	served, _ := data["Observed"].([]map[string]any)
	require.Len(t, served, 1)
	require.Equal(t, intended["CPU"].(int)+2, served[0]["CPU"], "the served spec must be the observed one")

	asked, _ := data["Intended"].(map[string]any)
	require.Equal(t, intended["CPU"], asked["CPU"], "and the confirmed spec must still be readable beside it")

	mismatches, _ := data["SpecMismatch"].([]map[string]any)
	fields := map[string]bool{}
	for _, m := range mismatches {
		fields[m["Field"].(string)] = true
		require.Equal(t, "uhost-created-1", m["UHostId"])
	}
	require.True(t, fields["CPU"], "a CPU count that differs must be named")
	require.True(t, fields["Memory"], "so must the memory")
	require.False(t, fields["GpuType"], "a field the platform DID serve must not be reported as drift")
}

// The mirror: when the platform served exactly what was confirmed there is no
// mismatch key at all. Without this the test above passes for a function that
// reports everything as drift.
func TestCreateResultNamesNoDriftWhenThePlatformServedTheContract(t *testing.T) {
	wfCtx, _ := createdAndDescribed(t, nil)
	data := createInstanceResultData(wfCtx)
	require.Equal(t, true, data["ActualVerified"])
	require.NotContains(t, data, "SpecMismatch")
	require.Len(t, data["Observed"], 1)
}

// 查看状态 is Optional. A create that succeeded while the follow-up read did not come
// back is still a created instance — it must not be reported as a failure, and it
// must not be reported as verified either, because nobody looked.
func TestCreateResultSaysTheSpecIsUnverifiedWhenTheReadNeverCameBack(t *testing.T) {
	wfCtx, _ := createdAndDescribed(t, nil)
	delete(wfCtx.StepResults, "查看状态")

	data := createInstanceResultData(wfCtx)
	require.NotNil(t, data["UHostIds"], "the instance exists and its id must still be published")
	require.Equal(t, false, data["ActualVerified"])
	require.NotContains(t, data, "Observed")
	require.NotContains(t, data, "SpecMismatch")
}

// A describe that answered about some OTHER instance is not evidence about this
// create. It must not be projected as the created instance's spec.
func TestCreateResultIgnoresDescribeRowsForOtherInstances(t *testing.T) {
	wfCtx, _ := createdAndDescribed(t, func(_, observed map[string]any) {
		observed["UHostId"] = "uhost-somebody-else"
	})
	data := createInstanceResultData(wfCtx)
	require.Equal(t, false, data["ActualVerified"])
	require.NotContains(t, data, "Observed")
}
