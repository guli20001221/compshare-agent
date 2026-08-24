package engine

import (
	"strings"
	"testing"
)

// Until 2026-08-08 every write-mode refusal printed one sentence — 「属于高危操作或命令形式不被接受」 —
// covering the destructive tier, the shape gate, an over-long command and the operator's own decline.
// That is unactionable in both directions: the operator cannot tell a policy refusal from their own
// click, and the model cannot tell "never going to work" from "resend it as two commands".
//
// The four must be DISTINGUISHABLE, which is a stronger property than any one wording, so assert
// pairwise distinctness rather than four string literals a reword would have to chase.
func TestEachRefusalReasonIsDistinguishable(t *testing.T) {
	reasons := []string{
		"refused_destructive",
		"refused_form",
		"refused_user_declined",
		"refused_confirmation_timeout",
		"refused_client_disconnect",
		"refused_confirmation_delivery_failed",
		"refused_confirmation_broker_cancelled",
		"refused_not_approved",
		"refused_unconfirmable",
		"refused_precondition",
		"refused_mutating_phase1",
	}
	seen := map[string]string{}
	for _, r := range reasons {
		got := instanceOpsRefusalReason(r)
		if strings.TrimSpace(got) == "" {
			t.Fatalf("%s produced an empty reason", r)
		}
		if prev, dup := seen[got]; dup {
			t.Fatalf("%s and %s both render as %q — the operator cannot tell them apart", prev, r, got)
		}
		seen[got] = r
	}
}

// The two that most need to differ, named explicitly: a command-substitution shape refusal can be
// rewritten without substitution, while a destructive one never becomes executable by respelling it.
func TestShapeRefusalIsNotWordedAsAPolicyRefusal(t *testing.T) {
	form := instanceOpsRefusalReason("refused_form")
	if !strings.Contains(form, "命令形式") || !strings.Contains(form, "命令替换") {
		t.Fatalf("a form refusal must identify command substitution as the unsupported shape, got %q", form)
	}
	if strings.Contains(form, "多行") || strings.Contains(form, "拆成单条") {
		t.Fatalf("a form refusal must not reject supported multiline commands, got %q", form)
	}
	if strings.Contains(form, "高危") {
		t.Fatalf("a form refusal must not be worded as a danger refusal, got %q", form)
	}
	declined := instanceOpsRefusalReason("refused_user_declined")
	if strings.Contains(declined, "高危") || strings.Contains(declined, "命令形式") {
		t.Fatalf("the user's own decline must not be reported as a policy refusal, got %q", declined)
	}
	timeout := instanceOpsRefusalReason("refused_confirmation_timeout")
	if !strings.Contains(timeout, "等待你的确认") || strings.Contains(timeout, "未批准") {
		t.Fatalf("a timed-out card must not be reported as a user decline, got %q", timeout)
	}
	precondition := instanceOpsRefusalReason("refused_precondition")
	if !strings.Contains(precondition, "前置条件") || !strings.Contains(precondition, "重新读取") ||
		strings.Contains(precondition, "高危") || strings.Contains(precondition, "只读模式") {
		t.Fatalf("a stale or invalid precondition must tell the operator how to retry, got %q", precondition)
	}
}

// An unknown or absent reason (a harness older than this server) must degrade to the previous generic
// wording. A blank here would be a step line that says 已拒绝： and then stops.
func TestUnknownRefusalReasonDegradesInsteadOfBlanking(t *testing.T) {
	for _, r := range []string{"", "refused_something_added_later", "ran"} {
		got := instanceOpsRefusalReason(r)
		if strings.TrimSpace(got) == "" {
			t.Fatalf("reason %q produced an empty string", r)
		}
	}
}

// refused_not_approved is what a harness emits when it can prove only that no approval arrived: an
// EOF, a malformed reply, a stale id, or a Go supervisor too old to send terminal_reason at all.
// That last one is not hypothetical — the binary and the harness are separate deploy artifacts
// (agent.ssh_ops.harness_path), so a rolling upgrade runs one old half against one new half, and
// this branch is the one it walks through.
//
// Absence of an approval is not a decision by the user. The wording therefore may not attribute the
// refusal to them, and must still state the fact that matters: nothing ran. Asserted as forbidden
// substrings rather than one literal, because the failure mode is a REWORD back toward blame, not a
// specific sentence.
func TestTheCompatibilityDegradeDoesNotInventAUserDecision(t *testing.T) {
	got := instanceOpsRefusalReason("refused_not_approved")
	for _, blamed := range []string{"未批准", "拒绝", "取消", "你不同意"} {
		if strings.Contains(got, blamed) {
			t.Fatalf("an absent approval was reported as the user's own %q: %q", blamed, got)
		}
	}
	if !strings.Contains(got, "未执行") {
		t.Fatalf("the degraded reason must still say the command did not run, got %q", got)
	}
	// And it has to stay distinguishable from the case where the user really did decline —
	// otherwise the degrade is just the old bug spelled differently.
	if got == instanceOpsRefusalReason("refused_user_declined") {
		t.Fatalf("the degrade and a real decline render identically: %q", got)
	}
}

// End to end through the step builder: the specific reason has to reach the user-visible message,
// not merely exist on the struct.
func TestCommandStepMessageCarriesTheSpecificReason(t *testing.T) {
	ev := instanceOpsCommandStep("DiagnoseInstanceInternals", InstanceOpsProgress{
		Kind:        InstanceOpsProgressCommand,
		Command:     "pip install torch",
		Disposition: "refused",
		Reason:      "refused_confirmation_timeout",
	})
	if ev.Type != StepBlocked {
		t.Fatalf("a refusal must ride StepBlocked, got %v", ev.Type)
	}
	if !strings.Contains(ev.Message, instanceOpsRefusalReason("refused_confirmation_timeout")) {
		t.Fatalf("step message does not carry the specific reason: %q", ev.Message)
	}
	if strings.Contains(ev.Message, "属于高危操作或命令形式不被接受") {
		t.Fatalf("step message fell back to the collapsed sentence: %q", ev.Message)
	}
	if ev.ErrorCode != "SSH_REFUSED_CONFIRMATION_TIMEOUT" {
		t.Fatalf("trace code = %q, want the mechanically normalized harness reason", ev.ErrorCode)
	}
}

func TestCommandStepTraceDistinguishesExecutionFailureWithoutParsingText(t *testing.T) {
	failed := instanceOpsCommandStep("DiagnoseInstanceInternals", InstanceOpsProgress{
		Command: "opaque user command", Disposition: "failed",
	})
	if failed.ErrorCode != "SSH_COMMAND_FAILED" {
		t.Fatalf("failed command trace code = %q", failed.ErrorCode)
	}
	unknown := instanceOpsCommandStep("DiagnoseInstanceInternals", InstanceOpsProgress{
		Command: "password=hunter2", Disposition: "refused", Reason: "refused_future_reason",
	})
	if unknown.ErrorCode != "_OTHER" {
		t.Fatalf("malformed structured reason = %q, want _OTHER", unknown.ErrorCode)
	}
	if strings.Contains(unknown.ErrorCode, "hunter2") {
		t.Fatalf("command text leaked into trace code: %q", unknown.ErrorCode)
	}
}
