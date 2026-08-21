package sshops

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeConfirmHarness plays the harness half of the approval protocol: it asks about two commands
// and reports what came back. stdin is the ONLY channel for the answer, so a supervisor that never
// wrote one leaves this blocked until the deadline — which is itself the failure being tested.
const fakeConfirmHarness = `
import sys, json
sys.stdin.readline()                                   # handshake
results = []
for cmd in ["systemctl restart ollama", "kill 6934"]:
    sys.stdout.write("@@CONFIRM " + json.dumps({"id": "c%d" % (len(results) + 1), "command": cmd}) + "\n")
    sys.stdout.flush()
    line = sys.stdin.readline()
    if not line:
        results.append("EOF")
        continue
    r = json.loads(line)
    results.append("%s:%s:%s" % (r.get("id"), r.get("approved"), r.get("terminal_reason")))
print("<<<VERDICT>>>")
print("REPLIES " + "|".join(results))
print("<<<END>>>")
`

func newConfirmSup(t *testing.T) Supervisor {
	t.Helper()
	return Supervisor{
		Python:      requirePython(t),
		HarnessPath: writeFakeHarness(t, fakeConfirmHarness),
		BaseURL:     testAnthropicBaseURL,
		APIKey:      testAnthropicAPIKey,
		Model:       "gpt-5.6-terra",
		Timeout:     60 * time.Second,
	}
}

// stdin used to be a one-shot reader closed right after the handshake, which is why the lane could
// only ever refuse writes. The reply has to travel back on that same pipe, carrying the id the
// harness asked with — an answer that arrives without the id could authorize whichever command
// happens to be pending.
func TestSupervisorAnswersConfirmRequestsOnStdin(t *testing.T) {
	var asked []string
	sup := newConfirmSup(t)
	res, err := sup.Run(context.Background(), cred("uhost-abc", "1.2.3.4", "root", 23, "S3cr3tPw"), "t", nil,
		func(req ConfirmRequest) ConfirmDecision {
			asked = append(asked, req.Command)
			if req.Command == "kill 6934" {
				return ConfirmDecision{TerminalReason: "timeout"}
			}
			return ConfirmDecision{Approved: true, TerminalReason: "user_confirmed"}
		})
	if err != nil {
		t.Fatalf("run: %v (output=%q)", err, res.Output)
	}
	if len(asked) != 2 || asked[0] != "systemctl restart ollama" || asked[1] != "kill 6934" {
		t.Fatalf("confirmer saw %q; the card must carry the literal command, in order", asked)
	}
	if !strings.Contains(res.Output, "REPLIES c1:True:user_confirmed|c2:False:timeout") {
		t.Fatalf("harness got %q, want terminal reasons paired with matching ids", res.Output)
	}
}

// A missing confirmer is not a product read-only mode. The same diagnosis starts, but Supervisor
// answers every exact-command request with approved=false, so a missing UI channel can never become
// implicit consent.
func TestMissingConfirmerDeniesEveryWriteRequest(t *testing.T) {
	sup := newConfirmSup(t)
	res, err := sup.Run(context.Background(), cred("uhost-abc", "1.2.3.4", "root", 23, "S3cr3tPw"), "t", nil, nil)
	if err != nil {
		t.Fatalf("run: %v (output=%q)", err, res.Output)
	}
	if !strings.Contains(res.Output, "REPLIES c1:False:None|c2:False:None") {
		t.Fatalf("missing confirmer did not fail closed per command: %q", res.Output)
	}
}
