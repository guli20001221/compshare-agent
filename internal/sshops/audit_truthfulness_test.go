package sshops

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

// The production ssh_ops_audit table recorded 12 attempts as disposition='ok', exit_code=0 — including
// every attempt whose dial never landed. Reading it therefore required separating two clusters by hand
// (measured 2026-08-06: entered = 2958..4074 B over 95..161 s; refused = 205..456 B over 15.6..16.3 s),
// and err_class — declared on AuditEvent, present in the UPDATE, calibrated in the harness — was NULL
// on all 12 rows because nothing ever assigned it.
//
// These tests pin the two halves of the fix: the harness declares the outcome, and Diagnose writes it.

// lastFinish returns the AuditEvent from the final Finish call MemAuditWriter recorded.
func lastFinish(t *testing.T, m *MemAuditWriter) AuditEvent {
	t.Helper()
	if len(m.Events) < 2 {
		t.Fatalf("expected a Begin and a Finish event, got %d", len(m.Events))
	}
	return m.Events[len(m.Events)-1]
}

func diagnoseWith(t *testing.T, res Result, runErr error) AuditEvent {
	t.Helper()
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	d := stubDescriber{resp: describeResp("ssh -p 23 root@10.0.0.9", b64)}
	audit := &MemAuditWriter{}
	svc := NewService(&fakeRunner{res: res, err: runErr}, audit)
	_, _ = svc.Diagnose(context.Background(), d, Owner{TopOrganizationID: 1, OrganizationID: 2},
		"uhost-abc", "", nil, nil)
	return lastFinish(t, audit)
}

// WHY: this is the exact row shape that made the production table assert we had entered boxes we
// never reached. A refusal is an ORDERLY exit 0 from the harness, so nothing about the process status
// distinguishes it — only the harness's own declaration does.
func TestPreflightRefusalIsAuditedAsErrorNotOk(t *testing.T) {
	got := diagnoseWith(t, Result{
		Output:          "⚠ 只读诊断未能开始：无法建立 SSH 连接…",
		PreflightFailed: true,
		ErrClass:        "TimeoutError",
	}, nil)

	if got.Disposition != "error" {
		t.Errorf("disposition = %q, want \"error\": a dial that never landed must not be recorded as a success", got.Disposition)
	}
	if got.ErrClass != "TimeoutError" {
		t.Errorf("err_class = %q, want \"TimeoutError\" (the class the harness measured)", got.ErrClass)
	}
}

// Production case 131: the SSH/model run had already executed five reads when Claude CLI returned
// an errored ResultMessage. The harness exits zero deliberately so its deterministic partial verdict
// survives; AgentFailed is therefore the only truthful signal that this is not an `ok` diagnosis.
func TestAgentFailureAfterCommandsIsAuditedAsErrorNotOk(t *testing.T) {
	got := diagnoseWith(t, Result{
		Output:      "诊断中断：没有形成经验证的最终结论。",
		AgentFailed: true,
		ErrClass:    "server_error",
		Steps: []Step{
			{Command: "systemctl status app", Tier: "read_only", Disposition: "ran"},
			{Command: "ss -lntp", Tier: "read_only", Disposition: "ran"},
		},
	}, nil)

	if got.Disposition != "error" {
		t.Errorf("disposition = %q, want error for an incomplete inner-agent run", got.Disposition)
	}
	if got.ErrClass != "server_error" {
		t.Errorf("err_class = %q, want bounded model failure class", got.ErrClass)
	}
	if got.CommandsRan != 2 {
		t.Errorf("commands_ran = %d, want settled activity retained", got.CommandsRan)
	}
}

// WHY: the whole point of the change is that the two outcomes become distinguishable. A run that
// entered the box must stay 'ok' with an EMPTY class, or the column just moves the ambiguity.
func TestEnteredRunStaysOkWithNoErrClass(t *testing.T) {
	got := diagnoseWith(t, Result{
		Output: "GPU 正常，显存占用 2.1G/24G …",
		Steps:  []Step{{Command: "nvidia-smi", Tier: "read_only", Disposition: "ran"}},
	}, nil)

	if got.Disposition != "ok" {
		t.Errorf("disposition = %q, want \"ok\"", got.Disposition)
	}
	if got.ErrClass != "" {
		t.Errorf("err_class = %q, want empty on a run that entered the box", got.ErrClass)
	}
}

// WHY: a supervisor-level failure has no paramiko class, and leaving err_class empty there would make
// it indistinguishable from a clean run — the same defect one level up.
func TestSupervisorFailuresGetTheirOwnErrClass(t *testing.T) {
	for _, tc := range []struct {
		name     string
		res      Result
		wantErr  string
		wantDisp string
	}{
		{"harness timed out", Result{TimedOut: true}, "harness_timeout", "error"},
		{"harness exited badly", Result{ExitCode: 2}, "harness_failed", "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := diagnoseWith(t, tc.res, context.DeadlineExceeded)
			if got.Disposition != tc.wantDisp {
				t.Errorf("disposition = %q, want %q", got.Disposition, tc.wantDisp)
			}
			if got.ErrClass != tc.wantErr {
				t.Errorf("err_class = %q, want %q", got.ErrClass, tc.wantErr)
			}
		})
	}
}

// WHY: @@OUTCOME is a NEW wire line. A harness image that predates it must keep behaving exactly as
// today — absence means "entered", never "unknown". Otherwise a rollout ordering (new binary, old
// harness) would relabel every successful diagnosis as a failed dial.
func TestAbsentOutcomeLineMeansTheBoxWasEntered(t *testing.T) {
	stream := "@@STEP {\"command\":\"nvidia-smi\",\"tier\":\"read_only\",\"disposition\":\"ran\",\"bytes\":42}\n" +
		"<<<VERDICT>>>\nGPU 正常\n<<<END>>>\n"
	_, steps, outcome, err := parseHarnessStream(strings.NewReader(stream), nil, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(steps))
	}
	if outcome.Outcome != "" || outcome.ErrClass != "" || outcome.ContextApplied {
		t.Errorf("outcome = %+v, want zero value (entered) when no @@OUTCOME line is present", outcome)
	}
}

func TestOutcomeLineIsParsed(t *testing.T) {
	stream := "@@OUTCOME {\"outcome\":\"preflight_failed\",\"err_class\":\"NoValidConnectionsError\",\"context_applied\":false}\n" +
		"<<<VERDICT>>>\n⚠ 只读诊断未能开始\n<<<END>>>\n"
	verdict, steps, outcome, err := parseHarnessStream(strings.NewReader(stream), nil, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(steps) != 0 {
		t.Errorf("steps = %d, want 0 — a refused dial runs no commands", len(steps))
	}
	if outcome.Outcome != outcomePreflightFailed {
		t.Errorf("outcome = %q, want %q", outcome.Outcome, outcomePreflightFailed)
	}
	if outcome.ErrClass != "NoValidConnectionsError" {
		t.Errorf("err_class = %q", outcome.ErrClass)
	}
	if outcome.ContextApplied {
		t.Errorf("context_applied = true, want false for a preflight refusal")
	}
	if !strings.Contains(verdict, "未能开始") {
		t.Errorf("verdict = %q — the @@OUTCOME line must not leak into the answer body", verdict)
	}
}

func TestOutcomeLineRecordsContextReceipt(t *testing.T) {
	stream := "@@OUTCOME {\"outcome\":\"\",\"err_class\":\"\",\"context_applied\":true}\n" +
		"<<<VERDICT>>>\nGPU 正常\n<<<END>>>\n"
	_, _, outcome, err := parseHarnessStream(strings.NewReader(stream), nil, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if outcome.Outcome != "" || !outcome.ContextApplied {
		t.Errorf("outcome = %+v, want normal run with a context receipt", outcome)
	}
}

func TestAgentFailureOutcomeIsParsedWithoutLeakingIntoVerdict(t *testing.T) {
	stream := "@@OUTCOME {\"outcome\":\"agent_failed\",\"err_class\":\"server_error\",\"context_applied\":true}\n" +
		"<<<VERDICT>>>\n诊断中断：没有形成经验证的最终结论\n<<<END>>>\n"
	verdict, _, outcome, err := parseHarnessStream(strings.NewReader(stream), nil, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if outcome.Outcome != outcomeAgentFailed || outcome.ErrClass != "server_error" || !outcome.ContextApplied {
		t.Fatalf("outcome = %+v, want bounded agent failure with context receipt", outcome)
	}
	if strings.Contains(verdict, "server_error") || !strings.Contains(verdict, "诊断中断") {
		t.Fatalf("verdict = %q, wire metadata must not become customer prose", verdict)
	}
}

// WHY: fail OPEN on a malformed payload. The field feeds an audit label; mislabelling a real
// diagnosis as a dial that never happened is worse than losing the label.
func TestMalformedOutcomeLineIsTreatedAsEntered(t *testing.T) {
	stream := "@@OUTCOME not-json-at-all\n<<<VERDICT>>>\nGPU 正常\n<<<END>>>\n"
	_, _, outcome, err := parseHarnessStream(strings.NewReader(stream), nil, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if outcome.Outcome != "" {
		t.Errorf("outcome = %q, want empty (entered) for an unparseable payload", outcome.Outcome)
	}
}
