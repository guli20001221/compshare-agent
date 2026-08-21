package sshops

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// The counts alone cannot answer the question a user asks after a disconnect: the lane ships with
// the repair lane, so "8 ran, 1 refused" leaves them unable to tell an approved write that landed
// from a read that did. These pin the detail that answers it — and, just as importantly, pin what
// the detail must NOT grow into.

func TestAuditStepDetailIsRedactedBeforeItReachesAnyWriter(t *testing.T) {
	// Redaction lives at the producer, not in the SQL writer, precisely so this holds for the
	// in-memory writer too. A raw command must never be inside an AuditEvent at all.
	// The token is assembled at runtime on purpose: a literal `Bearer <20+ chars>` in the source
	// trips this repo's own pre-commit secret scanner (scripts/secret_scan.ps1), which is correct
	// of it — a scanner that learned to ignore test files would stop being one.
	token := "sk-" + strings.Repeat("z", 24)
	steps := []Step{
		{Command: `curl -H "Authorization: Bearer ` + token + `" http://127.0.0.1:8188/`, Disposition: "ran"},
		{Command: "grep alice@example.com /var/log/app.log", Disposition: "ran"},
	}
	got := summarizeAuditStepDetail(steps)
	if len(got) != 2 {
		t.Fatalf("want 2 summaries, got %d", len(got))
	}
	joined := got[0].Command + "\n" + got[1].Command
	for _, secret := range []string{token, "alice@example.com"} {
		if strings.Contains(joined, secret) {
			t.Errorf("raw secret %q survived into the persisted step detail:\n%s", secret, joined)
		}
	}
	// ...and the surrounding command must survive, or the column records nothing usable.
	if !strings.Contains(joined, "curl") || !strings.Contains(joined, "/var/log/app.log") {
		t.Errorf("redaction removed the diagnostic content as well:\n%s", joined)
	}
}

func TestAuditStepDetailTruncatesOnRuneBoundariesAndSaysSo(t *testing.T) {
	// A silently shortened command reads as a DIFFERENT command: `rm -rf /root/.cache/pip` and
	// `rm -rf /root` share their first 12 characters.
	long := "cat /数据/" + strings.Repeat("目录", 400) + "/日志.log"
	got := summarizeAuditStepDetail([]Step{{Command: long, Disposition: "ran"}})
	cmd := got[0].Command
	if !utf8.ValidString(cmd) {
		t.Fatalf("truncation split a multi-byte rune: %q", cmd)
	}
	if !strings.HasSuffix(cmd, auditTruncationMarker) {
		t.Errorf("truncated command does not say it was truncated: %q", cmd)
	}
	// The marker is charged AGAINST the bound, not appended past it. A stored value that can exceed
	// the documented cap makes the cap unquotable — every doc would have to say "200, or 205 when it
	// was truncated" — and it is the truncated rows, the long ones, that would carry the overshoot.
	if total := utf8.RuneCountInString(cmd); total != maxAuditStepCommandRunes {
		t.Errorf("a truncated command must be exactly the bound, marker included: want %d runes, got %d",
			maxAuditStepCommandRunes, total)
	}
	if body := strings.TrimSuffix(cmd, auditTruncationMarker); utf8.RuneCountInString(body) != maxAuditStepCommandRunes-utf8.RuneCountInString(auditTruncationMarker) {
		t.Errorf("want %d runes of command before the marker, got %d",
			maxAuditStepCommandRunes-utf8.RuneCountInString(auditTruncationMarker), utf8.RuneCountInString(body))
	}
	// A command that fits is left exactly alone — no marker on an untruncated command.
	short := summarizeAuditStepDetail([]Step{{Command: "df -h /", Disposition: "ran"}})
	if short[0].Command != "df -h /" {
		t.Errorf("short command was altered: %q", short[0].Command)
	}
	// The case that separates a RUNE cap from a BYTE cap, and the only one that does: 100 CJK
	// characters are 300 bytes but well inside a 200-rune bound. An ASCII command cannot tell the
	// two implementations apart — it is short in both units — so without this case the rune
	// counting is untested, and getting it wrong here truncates a perfectly ordinary Chinese path
	// (or panics slicing past the end).
	cjk := strings.Repeat("查", 100)
	fits := summarizeAuditStepDetail([]Step{{Command: cjk, Disposition: "ran"}})
	if fits[0].Command != cjk {
		t.Errorf("a %d-rune / %d-byte command must be stored whole, got %d runes",
			utf8.RuneCountInString(cjk), len(cjk), utf8.RuneCountInString(fits[0].Command))
	}
}

func TestAuditStepDetailIsBoundedByRowCount(t *testing.T) {
	steps := make([]Step, maxAuditStepRows+50)
	for i := range steps {
		steps[i] = Step{Command: "ls /tmp", Disposition: "ran"}
	}
	if got := len(summarizeAuditStepDetail(steps)); got != maxAuditStepRows {
		t.Errorf("want the row bound %d enforced by the producer, got %d", maxAuditStepRows, got)
	}
	if got := summarizeAuditStepDetail(nil); got != nil {
		t.Errorf("no steps must persist as NULL, not an empty array: %#v", got)
	}
}

// TestPersistedStepSummaryCarriesNoOutput is a shape gate, not a value check. INV-6 keeps command
// OUTPUT off every wire but the model's own, and the easiest way for that to be undone is someone
// adding one more "obviously useful" field here. Any new field fails this test on purpose: adding
// it means deciding, in review, that it is not output and not a resume cursor.
func TestPersistedStepSummaryCarriesNoOutput(t *testing.T) {
	want := []string{"bytes", "cmd", "disp", "exit", "reason", "tier"}
	typ := reflect.TypeOf(PersistedStepSummary{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PersistedStepSummary field set changed.\n got: %v\nwant: %v\n\n"+
			"If you are ADDING a field: it must be neither command output (INV-6) nor anything that "+
			"would let this be replayed as a resume cursor. Update `want` deliberately, not to make "+
			"the test pass.", got, want)
	}
	exit := 0
	encoded, err := json.Marshal(PersistedStepSummary{Command: "df -h", Tier: "read_only", Disposition: "ran", ExitCode: &exit})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"output", "stdout", "stderr", "body"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("encoded step carries %q: %s", forbidden, encoded)
		}
	}
}

// TestAKilledRunStillRecordsWhichCommandsRan is the whole point of the column. A client disconnect
// cancels the request ctx mid-run; Finish already survives that (it runs WithoutCancel), and this
// asserts it now carries enough to tell the user the box may have been changed AND by what.
func TestAKilledRunStillRecordsWhichCommandsRan(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	d := stubDescriber{resp: describeResp("ssh -p 23 root@10.0.0.9", b64)}
	ctx, cancel := context.WithCancel(context.Background())
	exit := 0
	runner := &fakeRunner{
		res: Result{Output: "partial", TimedOut: true, Steps: []Step{
			{Command: "df -h /", Tier: "read_only", Disposition: "ran", ExitCode: &exit},
			{Command: "rm -rf /root/.cache/pip", Tier: "mutating", Disposition: "ran", ExitCode: &exit},
			{Command: "systemctl restart vllm", Tier: "mutating", Disposition: "refused", Reason: "refused_client_disconnect"},
		}},
		err:   context.Canceled,
		onRun: cancel,
	}
	audit := &MemAuditWriter{}
	svc := NewService(runner, audit)

	_, _ = svc.Diagnose(ctx, d, Owner{TopOrganizationID: 1, OrganizationID: 2}, "uhost-abc", "", nil, nil)

	if len(audit.Events) < 2 {
		t.Fatalf("want a Begin and a Finish event, got %d", len(audit.Events))
	}
	begin, done := audit.Events[0], audit.Events[1]
	if len(begin.Steps) != 0 {
		t.Errorf("Begin must record no steps — it runs before the harness exists; got %d", len(begin.Steps))
	}
	if len(done.Steps) != 3 {
		t.Fatalf("want 3 persisted steps on the killed run, got %d", len(done.Steps))
	}
	if done.Steps[1].Command != "rm -rf /root/.cache/pip" || done.Steps[1].Tier != "mutating" {
		t.Errorf("the approved write is not recoverable from the row: %#v", done.Steps[1])
	}
	if done.Steps[2].Reason != "refused_client_disconnect" {
		t.Errorf("want the fine-grained refusal reason preserved, got %q", done.Steps[2].Reason)
	}
	// The counts must still agree with the detail — two records of the same run that disagree are
	// worse than one, because a reader cannot tell which is stale.
	if done.CommandsRan != 2 || done.CommandsRefused != 1 {
		t.Errorf("counts disagree with the detail: ran=%d refused=%d vs %d steps",
			done.CommandsRan, done.CommandsRefused, len(done.Steps))
	}
}
