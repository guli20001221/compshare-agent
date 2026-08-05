//go:build linux

package sshops

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// On Linux the harness gets its OWN process group, so a timeout must kill the WHOLE tree — including a
// grandchild the wrapper spawned (a stand-in for the SDK-spawned claude CLI + node). This is the
// resource-leak guard for item 4: without Setpgid + group-kill, killing the direct python child orphans
// the grandchild. Cannot run on the Windows dev box (POSIX process groups are Linux/prod only), so it is
// build-tagged linux and exercised in CI.
func TestSupervisorKillsGrandchildProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "grandchild.pid")
	// The wrapper spawns a `sleep` grandchild in its own (inherited) process group, records its pid,
	// then sleeps far past the harness timeout. The pidfile path rides the stdin task field.
	body := "" +
		"import sys, json, subprocess, time\n" +
		"conn = json.loads(sys.stdin.readline())\n" +
		"gc = subprocess.Popen(['sleep', '120'])\n" +
		"open(conn['task'], 'w').write(str(gc.pid))\n" +
		"time.sleep(120)\n"

	sup := Supervisor{
		Python:      requirePython(t),
		HarnessPath: writeFakeHarness(t, body),
		BaseURL:     testAnthropicBaseURL,
		APIKey:      testAnthropicAPIKey,
		Timeout:     1 * time.Second, // fire the group-kill fast
	}
	c := cred("x", "h", "root", 22, "pw")

	_, err := sup.Run(context.Background(), c, pidfile, nil, nil) // task carries the pidfile path
	if err == nil {
		t.Fatalf("expected timeout error")
	}

	raw, readErr := os.ReadFile(pidfile)
	if readErr != nil {
		t.Fatalf("grandchild pidfile never written (wrapper did not spawn it?): %v", readErr)
	}
	gcPid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil {
		t.Fatalf("bad grandchild pid %q: %v", raw, convErr)
	}

	// Poll: the group-kill must terminate the grandchild. Kill(pid, 0) probes process-table
	// presence, but reports a zombie as present until the container's PID 1 reaps it. A zombie
	// cannot execute and proves the group kill reached the child, so it is terminal for this test.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if grandchildTerminated(gcPid) {
			return
		}
		if time.Now().After(deadline) {
			// Clean up the leak we just proved, so the test host isn't polluted.
			_ = syscall.Kill(gcPid, syscall.SIGKILL)
			t.Fatalf("grandchild pid %d survived the harness timeout — process group was not killed", gcPid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func grandchildTerminated(pid int) bool {
	if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
		return true
	}

	// /proc/<pid>/stat begins with "pid (comm) state ...". Split after the final ')' so a
	// process name containing spaces or ')' cannot move the state field.
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return os.IsNotExist(err)
	}
	lastParen := strings.LastIndex(string(stat), ")")
	if lastParen < 0 {
		return false
	}
	fields := strings.Fields(string(stat)[lastParen+1:])
	return len(fields) > 0 && fields[0] == "Z"
}
