package sshops

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLateMutationAndJobSurviveDisplayDetailLimit(t *testing.T) {
	var wire strings.Builder
	for i := 0; i < 125; i++ {
		fmt.Fprintln(&wire, `@@STEP {"command":"ps aux","tier":"read_only","disposition":"ran","exit":0}`)
	}
	jobID := "job-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fmt.Fprintf(&wire, "@@JOB {\"job_id\":%q,\"job_state\":\"unknown\",\"purpose\":\"install requested dependency\"}\n", jobID)
	fmt.Fprintf(&wire, "@@STEP {\"command\":\"install requested dependency\",\"tier\":\"mutating\",\"disposition\":\"ran\",\"job_id\":%q,\"job_state\":\"started\"}\n", jobID)
	fmt.Fprintln(&wire, `@@STEP {"command":"invalid request","tier":"mutating","disposition":"refused"}`)
	var live []Step
	_, steps, _, err := parseHarnessStream(strings.NewReader(wire.String()), func(step Step) {
		live = append(live, step)
	}, nil, nil)
	require.NoError(t, err)
	require.Len(t, steps, 127)
	require.Len(t, live, 128)
	require.True(t, live[125].JobLifecycleOnly)
	require.Equal(t, jobID, live[125].JobID)
	require.Equal(t, "mutating", live[126].Tier)
	require.Equal(t, jobID, live[126].JobID)

	// Exercise the real Service finalization as well as the stream parser: a
	// disconnected run keeps exact aggregate counts but bounds stored detail.
	runner := &fakeRunner{res: Result{Steps: steps}, err: context.Canceled}
	audit := &MemAuditWriter{}
	svc := NewService(runner, audit)
	d := stubDescriber{resp: describeResp("ssh -p 23 root@10.0.0.9", base64.StdEncoding.EncodeToString([]byte(secretPW)))}
	_, err = svc.Diagnose(context.Background(), d, Owner{TurnID: "late-tail"}, "uhost-abc", "repair dependency", nil, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, audit.Events, 2)
	finished := audit.Events[1]
	require.Equal(t, 126, finished.CommandsRan)
	require.Equal(t, 1, finished.CommandsRefused)
	require.Equal(t, "error", finished.Disposition)
	require.Len(t, finished.Steps, maxAuditStepRows)
	require.Len(t, runner.res.Steps, 127)
}

func TestSequentialJobsNeverLoseTheNextPrelaunchHandle(t *testing.T) {
	var wire strings.Builder
	// Only the final job is still active. Completed jobs must not consume a
	// cumulative sideband allowance that hides its pre-launch recovery handle.
	for i := 1; i <= 33; i++ {
		fmt.Fprintf(&wire, "@@JOB {\"job_id\":\"job-%032x\",\"job_state\":\"unknown\"}\n", i)
		if i < 33 {
			fmt.Fprintf(&wire, "@@STEP {\"command\":\"poll_background_job\",\"tier\":\"read_only\",\"disposition\":\"ran\",\"job_id\":\"job-%032x\",\"job_state\":\"succeeded\"}\n", i)
		}
	}
	var handles []string
	_, steps, _, err := parseHarnessStream(strings.NewReader(wire.String()), func(step Step) {
		if step.JobLifecycleOnly {
			handles = append(handles, step.JobID)
		}
	}, nil, nil)
	require.NoError(t, err)
	require.Len(t, steps, 32)
	require.Len(t, handles, 33)
	require.Equal(t, fmt.Sprintf("job-%032x", 33), handles[32])
}
