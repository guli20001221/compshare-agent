package sshops

import (
	"context"
	"encoding/base64"
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
    results.append("%s:%s" % (r.get("id"), r.get("approved")))
print("<<<VERDICT>>>")
print("REPLIES " + "|".join(results))
print("<<<END>>>")
`

func newConfirmSup(t *testing.T) Supervisor {
	t.Helper()
	return Supervisor{
		Python:      pythonBin(),
		HarnessPath: writeFakeHarness(t, fakeConfirmHarness),
		GatewayURL:  "http://127.0.0.1:3456",
		Model:       "deepseek-v4-flash",
		Timeout:     60 * time.Second,
		AllowWrites: true,
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
		func(req ConfirmRequest) bool {
			asked = append(asked, req.Command)
			return req.Command != "kill 6934" // approve the restart, decline the kill
		})
	if err != nil {
		t.Fatalf("run: %v (output=%q)", err, res.Output)
	}
	if len(asked) != 2 || asked[0] != "systemctl restart ollama" || asked[1] != "kill 6934" {
		t.Fatalf("confirmer saw %q; the card must carry the literal command, in order", asked)
	}
	if !strings.Contains(res.Output, "REPLIES c1:True|c2:False") {
		t.Fatalf("harness got %q, want c1 approved and c2 declined with matching ids", res.Output)
	}
}

// A write-enabled lane with no confirmer is a lane no human is watching. Refusing up front beats
// running with every write auto-denied: that run would spend its whole budget proposing repairs
// that can never be approved, and the user would read it as the model failing to fix anything.
func TestWriteLaneWithoutConfirmerRefusesBeforeTouchingTheBox(t *testing.T) {
	audit := &MemAuditWriter{}
	entered := false
	svc := NewService(runnerFunc(func(context.Context, Credential, string, func(Step)) (Result, error) {
		entered = true
		return Result{Output: "should never run"}, nil
	}), audit, WithWrites(true))

	d := stubDescriber{resp: describeResp("ssh root@1.2.3.4", base64.StdEncoding.EncodeToString([]byte("S3cr3tPw")))}
	_, err := svc.Diagnose(context.Background(), d, Owner{RequestUUID: "r", TurnID: "t"}, "uhost-abc", "task", nil, nil)
	if err == nil {
		t.Fatal("write lane ran with no confirmer wired")
	}
	if entered {
		t.Fatal("refusal happened after the harness had already been spawned")
	}
	// The read-only lane has no such requirement — it never asks anything, so a nil confirmer is fine.
	roSvc := NewService(runnerFunc(func(context.Context, Credential, string, func(Step)) (Result, error) {
		return Result{Output: "ok"}, nil
	}), &MemAuditWriter{})
	if _, err := roSvc.Diagnose(context.Background(), d, Owner{RequestUUID: "r2", TurnID: "t2"},
		"uhost-abc", "task", nil, nil); err != nil {
		t.Fatalf("read-only lane must not require a confirmer: %v", err)
	}
}
