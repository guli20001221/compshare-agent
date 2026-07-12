package engine

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

var entityIDRE = regexp.MustCompile(`uhost-[a-z0-9]+`)

// A zero Engine is enough: attachDeterministicInstanceTable touches only the flag, the
// payload, and instanceTableThisTurn.
var eng = &Engine{}

// describePayload builds a DescribeCompShareInstance result holding n instances with
// predictable IDs, mirroring the shape the live API returns.
func describePayload(n int) map[string]any {
	set := make([]any, 0, n)
	for i := 0; i < n; i++ {
		set = append(set, map[string]any{
			"UHostId": fmt.Sprintf("uhost-1smcp%05d", i),
			"Name":    fmt.Sprintf("host-%d", i),
			"State":   "Running",
			"Zone":    "cn-wlcb-01",
			"GpuType": "4090",
			"GPU":     1.0,
			"CPU":     16.0,
			"Memory":  64.0,
		})
	}
	return map[string]any{"TotalCount": float64(n), "UHostSet": set}
}

// The regression, stated as a test. Asked 我目前部署的实例 against a 13-instance payload,
// the agent loop wrote a table naming three of them and INVENTED a fourteenth
// (uhost-1exampleaa05 — the real prefix of an ID it had just been shown, with a
// confabulated suffix), silently dropping the other ten.
//
// The contract is not "every instance appears" — the display deliberately caps at
// DefaultMaxInstancesPerDisplay, exactly as the fast path does. It is the two things the
// model got wrong: every ID shown must come FROM THE PAYLOAD, and any instance withheld
// must be DISCLOSED rather than silently dropped.
func TestDeterministicTableInventsNothingAndDisclosesWhatItWithheld(t *testing.T) {
	SetAgentDeterministicRenderEnabled(true)
	defer SetAgentDeterministicRenderEnabled(false)

	const n = 13
	result := describePayload(n)
	if !eng.attachDeterministicInstanceTable("DescribeCompShareInstance", result, result) {
		t.Fatal("expected a table to be attached for a non-empty instance payload")
	}
	table, _ := result[renderedInstanceTableKey].(string)
	if table == "" {
		t.Fatal("attached an empty table")
	}

	real := map[string]bool{}
	for i := 0; i < n; i++ {
		real[fmt.Sprintf("uhost-1smcp%05d", i)] = true
	}
	// Nothing in the table may be absent from the payload. This is the assertion the
	// agent loop failed.
	for _, id := range entityIDRE.FindAllString(table, -1) {
		if !real[id] {
			t.Errorf("rendered table names %s, which the tool never returned", id)
		}
	}
	// The instances it did not show must be accounted for, not quietly lost.
	if !strings.Contains(table, "13") {
		t.Errorf("table withheld instances without disclosing the true total; got:\n%s", table)
	}
	// And the model is told, alongside the data, to reference the table by placeholder
	// rather than retype it.
	if instr, _ := result[displayInstructionKey].(string); !strings.Contains(instr, instanceTablePlaceholder) {
		t.Errorf("directive does not tell the model to use the placeholder; got %q", instr)
	}
}

// The placeholder is the whole safety property: whatever the model writes, the table the
// user sees is the one WE rendered from the payload. Even a model that "helpfully"
// rewrites the surrounding prose cannot alter a single instance ID, because it never
// types one.
func TestPlaceholderIsSubstitutedWithOurTableNotTheModels(t *testing.T) {
	const ours = "实例ID=uhost-real0001, 名称=host-0\n（已显示 1/1 台）"
	modelReply := "您好，以下是您的实例：\n\n" + instanceTablePlaceholder + "\n\n需要关机的话告诉我。"

	got, ok := substituteInstanceTable(modelReply, ours)
	if !ok {
		t.Fatal("placeholder was not substituted")
	}
	if !strings.Contains(got, "uhost-real0001") {
		t.Errorf("our rendered table did not reach the user; got:\n%s", got)
	}
	if strings.Contains(got, instanceTablePlaceholder) {
		t.Errorf("placeholder survived into the user-visible reply; got:\n%s", got)
	}
	// The prose the model wrote around it is preserved — we replace the list, not the answer.
	if !strings.Contains(got, "需要关机的话告诉我") {
		t.Errorf("substitution destroyed the model's prose; got:\n%s", got)
	}
}

// If the model ignores the placeholder and hand-writes a list anyway, we must NOT quietly
// append our table on top — that would hide the fact that the contract was disobeyed, and
// the disobedience is the measurement this experiment exists to take.
func TestDisobeyedPlaceholderIsLeftAloneSoItCanBeMeasured(t *testing.T) {
	handWritten := "您有 3 台实例：uhost-aaa、uhost-bbb、uhost-ccc。"
	got, ok := substituteInstanceTable(handWritten, "实例ID=uhost-real0001")
	if ok {
		t.Error("reported a substitution when the model never used the placeholder")
	}
	if got != handWritten {
		t.Errorf("reply was altered despite no placeholder; got:\n%s", got)
	}
}

// Boot-only gate, Go-package default off — the current production path must be
// byte-identical until the A/B says otherwise.
func TestDeterministicRenderIsOffByDefault(t *testing.T) {
	result := describePayload(3)
	if eng.attachDeterministicInstanceTable("DescribeCompShareInstance", result, result) {
		t.Fatal("deterministic render must be off unless the flag is set at boot")
	}
	if _, present := result[renderedInstanceTableKey]; present {
		t.Fatal("flag off must leave the tool result untouched")
	}
}

// Only instance lookups. A stock or price payload has no instance table to render, and
// silently attaching an empty one would put a directive in front of the model pointing at
// nothing.
func TestDeterministicRenderIgnoresOtherToolsAndEmptyPayloads(t *testing.T) {
	SetAgentDeterministicRenderEnabled(true)
	defer SetAgentDeterministicRenderEnabled(false)

	other := describePayload(3)
	if eng.attachDeterministicInstanceTable("DescribeAvailableCompShareInstanceTypes", other, other) {
		t.Error("attached an instance table to a non-instance tool result")
	}
	empty := map[string]any{"TotalCount": 0.0, "UHostSet": []any{}}
	if eng.attachDeterministicInstanceTable("DescribeCompShareInstance", empty, empty) {
		t.Error("attached a table for a payload with no instances")
	}
	if _, present := empty[displayInstructionKey]; present {
		t.Error("left a display directive with no table to point at")
	}
}
