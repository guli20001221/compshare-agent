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
		"refused_not_approved",
		"refused_unconfirmable",
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

// The two that most need to differ, named explicitly: a shape refusal is retryable after splitting,
// a destructive one never is. Collapsing them is what made a live run delete half its probe chain
// and retry instead of splitting (the #516 class).
func TestShapeRefusalIsNotWordedAsAPolicyRefusal(t *testing.T) {
	form := instanceOpsRefusalReason("refused_form")
	if !strings.Contains(form, "命令形式") || !strings.Contains(form, "拆") {
		t.Fatalf("a form refusal must say it is about SHAPE and that splitting fixes it, got %q", form)
	}
	if strings.Contains(form, "高危") {
		t.Fatalf("a form refusal must not be worded as a danger refusal, got %q", form)
	}
	declined := instanceOpsRefusalReason("refused_not_approved")
	if strings.Contains(declined, "高危") || strings.Contains(declined, "命令形式") {
		t.Fatalf("the operator's own decline must not be reported as a policy refusal, got %q", declined)
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

// End to end through the step builder: the specific reason has to reach the user-visible message,
// not merely exist on the struct.
func TestCommandStepMessageCarriesTheSpecificReason(t *testing.T) {
	ev := instanceOpsCommandStep("DiagnoseInstanceInternals", InstanceOpsProgress{
		Kind:        InstanceOpsProgressCommand,
		Command:     "pip install torch",
		Disposition: "refused",
		Reason:      "refused_not_approved",
	})
	if ev.Type != StepBlocked {
		t.Fatalf("a refusal must ride StepBlocked, got %v", ev.Type)
	}
	if !strings.Contains(ev.Message, instanceOpsRefusalReason("refused_not_approved")) {
		t.Fatalf("step message does not carry the specific reason: %q", ev.Message)
	}
	if strings.Contains(ev.Message, "属于高危操作或命令形式不被接受") {
		t.Fatalf("step message fell back to the collapsed sentence: %q", ev.Message)
	}
}
