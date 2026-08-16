package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/sshops"
)

func TestMarshalAuditStepsEncodesNoStepsAsNull(t *testing.T) {
	// NULL, not '[]'. "This run recorded no steps" and "this row predates the column" should look
	// the same on read, because commands_ran already separates them and inventing a second way to
	// say it just makes the query wrong in a new way.
	for _, empty := range [][]sshops.PersistedStepSummary{nil, {}} {
		got, err := marshalAuditSteps(empty)
		if err != nil {
			t.Fatalf("unexpected error for %#v: %v", empty, err)
		}
		if got != nil {
			t.Errorf("want nil (SQL NULL) for %#v, got %q", empty, got)
		}
	}
}

func TestMarshalAuditStepsRefusesAnOversizedPayloadRatherThanTheOutcome(t *testing.T) {
	// The producer bounds rows and command length, so reaching this means one of those bounds moved.
	// It must surface as "detail dropped", never as a failed Finish — a Finish that fails loses the
	// disposition, err_class and counts, i.e. the record itself, to save the annotation.
	huge := make([]sshops.PersistedStepSummary, 4000)
	for i := range huge {
		huge[i] = sshops.PersistedStepSummary{Command: strings.Repeat("x", 200), Tier: "read_only", Disposition: "ran"}
	}
	got, err := marshalAuditSteps(huge)
	if err == nil {
		t.Fatalf("want an error above the payload ceiling, got %d bytes", len(got))
	}
	if got != nil {
		t.Errorf("a refused payload must not also be returned: %d bytes", len(got))
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should name the ceiling it hit: %v", err)
	}
}

func TestMarshalAuditStepsRoundTripsTheFieldsAQueryReads(t *testing.T) {
	exit := 137
	encoded, err := marshalAuditSteps([]sshops.PersistedStepSummary{
		{Command: "rm -rf /root/.cache/pip", Tier: "mutating", Disposition: "ran", ExitCode: &exit, Bytes: 42},
		{Command: "systemctl restart vllm", Tier: "mutating", Disposition: "refused", Reason: "refused_client_disconnect"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var back []sshops.PersistedStepSummary
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("the JSONB payload does not decode: %v", err)
	}
	if len(back) != 2 || back[0].ExitCode == nil || *back[0].ExitCode != 137 {
		t.Fatalf("exit status lost in the round trip: %#v", back)
	}
	if back[1].Reason != "refused_client_disconnect" {
		t.Errorf("fine-grained reason lost in the round trip: %#v", back[1])
	}
	// A refused command never produced an exit status, and encoding one as 0 would read as success.
	if back[1].ExitCode != nil {
		t.Errorf("a refused command must have no exit code, got %d", *back[1].ExitCode)
	}
}
