package sshops

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/opscontext"
	"github.com/stretchr/testify/require"
)

// fakeEchoWrites reports what the write gate looked like from INSIDE the harness. The gate is
// per-task state the harness must not be able to obtain any other way, so the test asserts on what
// crossed the handshake rather than on the Go field.
const fakeEchoWrites = `
import sys, json
conn = json.loads(sys.stdin.readline())
print("<<<VERDICT>>>")
print("ALLOW_WRITES=%r" % (conn.get("allow_writes"),))
print("<<<END>>>")
`

const fakeEchoContext = `
import json, sys
conn = json.loads(sys.stdin.readline())
context = conn.get("context") or {}
facts = context.get("platform_facts") or []
print("@@OUTCOME " + json.dumps({"outcome": "", "err_class": "", "context_applied": True}))
print("<<<VERDICT>>>")
print("CONTEXT_SCHEMA=%r" % context.get("schema_version"))
print("CONTEXT_FACTS=%r" % len(facts))
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
	}
	res, err := sup.RunWithContext(context.Background(), cred("uhost-abc", "1.2.3.4", "root", 23, "S3cr3tPw"), "task", modelContext, nil, nil)
	require.NoError(t, err)
	// The version on the wire is the constant, not a literal: this asserts the supervisor forwards
	// what the producer set, and a hardcoded 1 here would have gone on passing after the v2 bump.
	require.Contains(t, res.Output, fmt.Sprintf("CONTEXT_SCHEMA=%d", opscontext.SchemaVersion))
	require.Contains(t, res.Output, "CONTEXT_FACTS=1")
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
	require.Contains(t, res.Output, "ALLOW_WRITES=False")
	require.False(t, res.ContextApplied)
}

// The harness decides whether to execute a mutating command by reading allow_writes off the
// handshake. If the supervisor omits it, conn.get returns None and the harness silently falls back
// to read-only — a lane configured for repair that quietly only diagnoses, with no error anywhere.
// That failure is invisible from the Go side, so it has to be gated from the harness's point of view.
func TestSupervisorSendsWriteGateOnHandshake(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow bool
		want  string
	}{
		{"writes off", false, "ALLOW_WRITES=False"},
		{"writes on", true, "ALLOW_WRITES=True"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sup := Supervisor{
				Python:      requirePython(t),
				HarnessPath: writeFakeHarness(t, fakeEchoWrites),
				BaseURL:     testAnthropicBaseURL,
				APIKey:      testAnthropicAPIKey,
				Model:       "gpt-5.6-terra",
				Timeout:     30 * time.Second,
				AllowWrites: tc.allow,
			}
			res, err := sup.Run(context.Background(), cred("uhost-abc", "1.2.3.4", "root", 23, "S3cr3tPw"), "t", nil, nil)
			if err != nil {
				t.Fatalf("run: %v (output=%q)", err, res.Output)
			}
			if !strings.Contains(res.Output, tc.want) {
				t.Fatalf("handshake write gate = %q, want %s", res.Output, tc.want)
			}
		})
	}
}

// The audit row is the only durable record that a human authorized entering someone's machine, and
// under what authority. A write session recorded as read_only is not a cosmetic mislabel: it is the
// evidence trail disagreeing with what actually happened on the box, which is exactly what the row
// exists to prevent. Phase must follow the lane's gate, not the commands that happened to run — a
// write-authorized session that issued only reads still entered with write authority.
func TestAuditPhaseRecordsTheAuthorityTheBoxWasEnteredUnder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow bool
		want  string
	}{
		{"read-only lane", false, "read_only"},
		{"write lane", true, "read_write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audit := &MemAuditWriter{}
			// The runner issues no commands at all, so the phase cannot be inferred from behaviour —
			// only from the gate the Service was built with.
			svc := NewService(runnerFunc(func(context.Context, Credential, string, func(Step)) (Result, error) {
				return Result{Output: "done"}, nil
			}), audit, WithWrites(tc.allow))

			// A write lane must be handed a confirmer or Diagnose refuses outright, so supply one even
			// though this test is about the audit phase: the refusal is the subject of its own test.
			var confirm ConfirmFunc
			if tc.allow {
				confirm = func(ConfirmRequest) bool { return true }
			}
			if _, err := svc.Diagnose(context.Background(), stubDescriber{resp: describeResp("ssh root@1.2.3.4", base64.StdEncoding.EncodeToString([]byte("S3cr3tPw")))}, Owner{RequestUUID: "r", TurnID: "t"},
				"uhost-abc", "task", nil, confirm); err != nil {
				t.Fatalf("diagnose: %v", err)
			}
			if len(audit.Events) == 0 {
				t.Fatal("no audit event recorded")
			}
			if got := audit.Events[0].Phase; got != tc.want {
				t.Fatalf("audit phase = %q, want %q", got, tc.want)
			}
		})
	}
}

type runnerFunc func(context.Context, Credential, string, func(Step)) (Result, error)

func (f runnerFunc) Run(ctx context.Context, c Credential, task string, onStep func(Step), _ ConfirmFunc) (Result, error) {
	return f(ctx, c, task, onStep)
}

func (f runnerFunc) RunWithContext(ctx context.Context, c Credential, task string, _ opscontext.Context, onStep func(Step), _ ConfirmFunc) (Result, error) {
	return f(ctx, c, task, onStep)
}
