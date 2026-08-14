package sshops

import (
	"strings"
	"testing"
)

// The harness classifies a refusal six ways and the wire carried three, so everything downstream —
// the user's activity stream, the audit row — could only say "refused". `reason` carries the
// specific one. These pin the two halves that can silently rot: that the field is actually read off
// the wire, and that its ABSENCE degrades instead of turning into a wrong specific value.
func TestStepCarriesTheSpecificRefusalReason(t *testing.T) {
	in := strings.Join([]string{
		`@@STEP {"command":"rm -rf /","tier":"destructive","disposition":"refused","reason":"refused_destructive","exit":null,"bytes":0}`,
		`@@STEP {"command":"a $(b)","tier":"mutating","disposition":"refused","reason":"refused_form","exit":null,"bytes":0}`,
		`@@STEP {"command":"pip install x","tier":"mutating","disposition":"refused","reason":"refused_not_approved","exit":null,"bytes":0}`,
		`@@STEP {"command":"systemctl restart x","tier":"mutating","disposition":"refused","reason":"refused_confirmation_timeout","exit":null,"bytes":0}`,
		"<<<VERDICT>>>", "结论", "<<<END>>>",
	}, "\n") + "\n"

	_, steps, _, err := parseHarnessStream(strings.NewReader(in), nil, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"refused_destructive", "refused_form", "refused_not_approved", "refused_confirmation_timeout"}
	if len(steps) != len(want) {
		t.Fatalf("want %d steps, got %d", len(want), len(steps))
	}
	for i, w := range want {
		if steps[i].Reason != w {
			t.Fatalf("step %d: Reason = %q, want %q", i, steps[i].Reason, w)
		}
		// The coarse field must NOT have been rewritten to make room for the fine one: the audit
		// row and every existing consumer still key off it.
		if steps[i].Disposition != "refused" {
			t.Fatalf("step %d: Disposition = %q, want refused", i, steps[i].Disposition)
		}
	}
}

// A harness older than this server emits no `reason`. That has to read as "unknown", never as one of
// the real values — a blank must not become a confident wrong sentence downstream.
func TestAnOlderHarnessLeavesTheReasonEmptyRatherThanWrong(t *testing.T) {
	in := `@@STEP {"command":"rm -rf /","tier":"destructive","disposition":"refused","exit":null,"bytes":0}` +
		"\n<<<VERDICT>>>\n结论\n<<<END>>>\n"

	_, steps, _, err := parseHarnessStream(strings.NewReader(in), nil, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(steps))
	}
	if steps[0].Reason != "" {
		t.Fatalf("Reason = %q, want empty for a harness that does not send it", steps[0].Reason)
	}
	if steps[0].Disposition != "refused" {
		t.Fatalf("Disposition = %q, want refused", steps[0].Disposition)
	}
}
