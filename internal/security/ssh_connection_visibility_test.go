package security

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SshLoginCommand is named in CLAUDE.md as the source of truth for SSH facts —
// explicitly NOT DescribeCompShareSoftwarePort, which returns image app ports.
// It reached the model as [REDACTED] because IsCredentialKey matches any key
// containing "ssh" and "command", so the agent was denied the one field it is
// told to use, while IP, PrivateIP, and the identical string sitting in an
// ordinary Remark all passed through untouched.
func TestRedactForLLM_KeepsSSHLoginCommandUsable(t *testing.T) {
	// Every shape the upstream is known to return; see the fixtures in
	// internal/diagnosis/ssh_failure_test.go and internal/capability.
	for _, command := range []string{
		"ssh root@1.2.3.4 -p 22",
		"ssh ubuntu@1.2.3.5",
		"ssh -p 23120 root@cpod-abc.podtcp.compshare.cn",
		"ssh root@example.invalid",
	} {
		out := RedactForLLM(map[string]any{
			"UHostId":         "uhost-abc123",
			"IP":              "106.75.12.34",
			"PrivateIP":       "10.0.0.4",
			"SshLoginCommand": command,
		})
		m, ok := out.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, command, m["SshLoginCommand"],
			"the model must be able to hand the user a command it can paste")
		assert.Equal(t, "106.75.12.34", m["IP"], "unchanged")
		assert.Equal(t, "10.0.0.4", m["PrivateIP"], "unchanged")
	}
}

// The exception fails CLOSED. Only a recognizable bare ssh line passes; anything
// else keeps the old blanking. This is the guard that makes it safe, because
// credential redaction alone would NOT be — those patterns match `key=value`,
// not command flags, so `sshpass -p hunter2 ...` would sail straight through
// them untouched.
func TestRedactForLLM_SSHExceptionFailsClosed(t *testing.T) {
	for name, value := range map[string]string{
		"password-carrying wrapper":  "sshpass -p Abc12345 ssh root@106.75.12.34 -p 23",
		"trailing shell command":     "ssh root@1.2.3.4 -p 22 && cat /root/.ssh/id_rsa",
		"proxy option with a secret": "ssh root@1.2.3.4 -o ProxyCommand='curl -H Authorization:tok_abcdefghij'",
		"empty host":                 "ssh ubuntu@",
		"not an ssh command at all":  "tok_abcdefghijklmnopqrst",
		// The comment on isPlainSSHLoginCommand says "at most one -p"; without a
		// seenPort flag it did not, and an allowlist that admits inputs its own
		// contract excludes is not an allowlist.
		"repeated port flag":   "ssh root@host -p 22 -p 23",
		"repeated port, split": "ssh -p 22 root@host -p 23",
		"port zero":            "ssh root@host -p 0",
		"port above 65535":     "ssh root@host -p 99999",
		// ssh would read this token as a flag, so accepting it means this check
		// and ssh disagree about what the string says.
		"user part looks like a flag": "ssh -root@host -p 22",
	} {
		out := RedactForLLM(map[string]any{"SshLoginCommand": value})
		assert.Equal(t, redactedValue, out.(map[string]any)["SshLoginCommand"],
			"%s: an unrecognized value must stay redacted, not be passed through", name)
	}
}

// The key allowlist is exact, not a substring rule — a substring rule is the
// mistake being corrected. A field that merely mentions ssh, or that is a real
// credential, keeps being blanked.
func TestRedactForLLM_SSHExceptionKeyAllowlistIsExact(t *testing.T) {
	out := RedactForLLM(map[string]any{
		"SSHCommand":      "ssh root@1.2.3.4 -p 22",
		"SshCommandToken": "tok_abcdefghijklmnopqrst",
		"SshPrivateKey":   "-----BEGIN OPENSSH PRIVATE KEY-----",
		"SshPassword":     "hunter2",
	})
	m := out.(map[string]any)
	for _, k := range []string{"SSHCommand", "SshCommandToken", "SshPrivateKey", "SshPassword"} {
		assert.Equal(t, redactedValue, m[k],
			"%s is not the authoritative field; widening the allowlist must be a deliberate act", k)
	}
}

// Scoped to the model path on purpose: persisted traces keep their current
// behaviour, so this change moves nothing at rest.
func TestRedactForTrace_SSHExceptionDoesNotApply(t *testing.T) {
	out := RedactForTrace(map[string]any{
		"SshLoginCommand": "ssh root@106.75.12.34 -p 23",
	})
	assert.Equal(t, redactedValue, out.(map[string]any)["SshLoginCommand"],
		"trace redaction is unchanged")
}
