package sshops

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/compshare-agent/internal/opscontext"
	"github.com/stretchr/testify/require"
)

// fakeEchoWrites reports whether the removed deployment mode leaked back onto
// the handshake. An older harness safely ignores the missing key.
const fakeEchoWrites = `
import sys, json
conn = json.loads(sys.stdin.readline())
print("<<<VERDICT>>>")
print("HAS_ALLOW_WRITES=%r" % ("allow_writes" in conn,))
print("<<<END>>>")
`

const fakeEchoContext = `
import json, sys
conn = json.loads(sys.stdin.readline())
context = conn.get("context") or {}
facts = context.get("platform_facts") or []
targets = conn.get("endpoint_targets") or []
pending = conn.get("pending_background_job") or {}
print("@@OUTCOME " + json.dumps({"outcome": "", "err_class": "", "context_applied": True}))
print("<<<VERDICT>>>")
print("CONTEXT_SCHEMA=%r" % context.get("schema_version"))
print("CONTEXT_FACTS=%r" % len(facts))
print("CONTEXT_HAS_PRIVATE_TARGETS=%r" % ("endpoint_targets" in context,))
print("ENDPOINT_TARGETS=%r" % len(targets))
print("ENDPOINT_FIRST=%r" % ((targets[0].get("id") if targets else None),))
print("CONTEXT_HAS_PENDING_JOB=%r" % ("pending_background_job" in context,))
print("PENDING_JOB=%r" % pending.get("job_id"))
print("PENDING_JOB_PURPOSE=%r" % pending.get("purpose"))
print("<<<END>>>")
`

func TestSupervisorSendsReferenceContextOnHandshake(t *testing.T) {
	sup := Supervisor{
		Python:      requirePython(t),
		HarnessPath: writeFakeHarness(t, fakeEchoContext),
		BaseURL:     testAnthropicBaseURL,
		APIKey:      testAnthropicAPIKey,
		Model:       "gpt-5.6-terra",
		Timeout:     30 * time.Second,
	}
	modelContext := opscontext.Context{
		SchemaVersion: opscontext.SchemaVersion,
		PlatformFacts: []opscontext.Fact{{
			Key: "platform.instance_port_hints", Value: map[string]any{"http": []int{8188}},
			Source: "DescribeCompShareInstance", ObservedAt: "2026-08-13T00:00:00Z", Status: opscontext.StatusKnown,
		}},
		EndpointTargets: []opscontext.EndpointTarget{{
			ID: "platform-http-1", Kind: "http", Label: "ComfyUI platform entry",
			Source: "DescribeCompShareInstance.Softwares.URL", URL: "https://example.invalid/?token=private",
		}},
		PendingBackgroundJob: &opscontext.BackgroundJob{
			JobID: "job-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: "running", Purpose: "download model weights",
		},
	}
	res, err := sup.RunWithContext(context.Background(), cred("uhost-abc", "1.2.3.4", "root", 23, "S3cr3tPw"), "task", modelContext, nil, nil)
	require.NoError(t, err)
	// The version on the wire is the constant, not a literal: this asserts the supervisor forwards
	// what the producer set, and a hardcoded 1 here would have gone on passing after the v2 bump.
	require.Contains(t, res.Output, fmt.Sprintf("CONTEXT_SCHEMA=%d", opscontext.SchemaVersion))
	require.Contains(t, res.Output, "CONTEXT_FACTS=1")
	require.Contains(t, res.Output, "CONTEXT_HAS_PRIVATE_TARGETS=False")
	require.Contains(t, res.Output, "ENDPOINT_TARGETS=1")
	require.Contains(t, res.Output, "ENDPOINT_FIRST='platform-http-1'")
	require.Contains(t, res.Output, "CONTEXT_HAS_PENDING_JOB=False")
	require.Contains(t, res.Output, "PENDING_JOB='job-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'")
	require.Contains(t, res.Output, "PENDING_JOB_PURPOSE='download model weights'")
	require.NotContains(t, res.Output, "private")
	require.True(t, res.ContextApplied)
}

// An image rollout can temporarily pair a new Go supervisor with an older harness that ignores
// the context handshake key and never emits the receipt. The diagnosis must still run, but its
// finished audit record must not claim contextual delivery; Service enforces the latter from this
// false result (covered separately in TestFinishedAuditClearsUnconfirmedContextReceipt).
func TestSupervisorOldHarnessDegradesContextReceiptToFalse(t *testing.T) {
	sup := Supervisor{
		Python:      requirePython(t),
		HarnessPath: writeFakeHarness(t, fakeEchoWrites),
		BaseURL:     testAnthropicBaseURL,
		APIKey:      testAnthropicAPIKey,
		Model:       "gpt-5.6-terra",
		Timeout:     30 * time.Second,
	}
	res, err := sup.RunWithContext(context.Background(), cred("uhost-abc", "1.2.3.4", "root", 23, "S3cr3tPw"), "task",
		opscontext.Context{SchemaVersion: opscontext.SchemaVersion}, nil, nil)
	require.NoError(t, err)
	require.Contains(t, res.Output, "HAS_ALLOW_WRITES=False")
	require.False(t, res.ContextApplied)
}

func TestSupervisorOmitsTheRemovedReadOnlyModeFromHandshake(t *testing.T) {
	sup := Supervisor{
		Python:      requirePython(t),
		HarnessPath: writeFakeHarness(t, fakeEchoWrites),
		BaseURL:     testAnthropicBaseURL,
		APIKey:      testAnthropicAPIKey,
		Model:       "gpt-5.6-terra",
		Timeout:     30 * time.Second,
	}
	res, err := sup.Run(context.Background(), cred("uhost-abc", "1.2.3.4", "root", 23, "S3cr3tPw"), "t", nil, nil)
	require.NoError(t, err)
	require.Contains(t, res.Output, "HAS_ALLOW_WRITES=False")
}

// The audit row is the only persisted record that a human authorized entering someone's machine, and
// under what authority. A write session recorded as read_only is not a cosmetic mislabel: it is the
// evidence trail disagreeing with what actually happened on the box, which is exactly what the row
// exists to prevent. Phase must follow the lane's gate, not the commands that happened to run — a
// write-authorized session that issued only reads still entered with write authority.
func TestAuditPhaseAlwaysRecordsRepairAuthority(t *testing.T) {
	audit := &MemAuditWriter{}
	svc := NewService(runnerFunc(func(context.Context, Credential, string, func(Step)) (Result, error) {
		return Result{Output: "done"}, nil
	}), audit)
	confirm := func(ConfirmRequest) ConfirmDecision { return ConfirmDecision{Approved: true} }
	_, err := svc.Diagnose(context.Background(), stubDescriber{resp: describeResp("ssh root@1.2.3.4", base64.StdEncoding.EncodeToString([]byte("S3cr3tPw")))}, Owner{RequestUUID: "r", TurnID: "t"},
		"uhost-abc", "task", nil, confirm)
	require.NoError(t, err)
	require.NotEmpty(t, audit.Events)
	require.Equal(t, "read_write", audit.Events[0].Phase)
}

type runnerFunc func(context.Context, Credential, string, func(Step)) (Result, error)

func (f runnerFunc) Run(ctx context.Context, c Credential, task string, onStep func(Step), _ ConfirmFunc) (Result, error) {
	return f(ctx, c, task, onStep)
}

func (f runnerFunc) RunWithContext(ctx context.Context, c Credential, task string, _ opscontext.Context, onStep func(Step), _ ConfirmFunc) (Result, error) {
	return f(ctx, c, task, onStep)
}
