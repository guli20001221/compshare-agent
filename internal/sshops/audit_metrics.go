package sshops

import (
	"strings"

	"github.com/compshare-agent/internal/guardrails"
)

const auditFirstCommandNone = "none"

// maxAuditStepCommandRunes bounds one persisted display command, MARKER INCLUDED — a stored command
// is never longer than this, so the number can be stated without a footnote. The legacy confirmation
// wire refuses anything over 300 chars (_MAX_CONFIRMABLE_COMMAND in guardrails.py), while the current
// autonomous path has its own command cap; this bound exists so a pathological read cannot make the Finish
// UPDATE large, and it is counted in RUNES rather than bytes because a CJK path would otherwise be
// cut mid-character.
const maxAuditStepCommandRunes = 200

// auditTruncationMarker is charged against that bound rather than appended past it. A cap that a
// stored value can exceed is a cap nobody can quote: every document describing this column would
// have to say "200, or 205 if it was truncated".
const auditTruncationMarker = "…[截断]"

// maxAuditStepRows bounds how many steps reach the row. The supervisor already caps Steps at
// maxHarnessSteps, but that is a different file with a different reason to change, and a
// best-effort Finish that fails on an oversized payload would be logged rather than retried — so
// the writer carries its own bound instead of trusting the producer's.
const maxAuditStepRows = 120

// PersistedStepSummary is the persisted, redacted projection of one Step. It exists so an
// interrupted run can be described afterwards by NAME — which commands ran and how they ended —
// instead of only by count.
//
// Deliberately absent:
//
//   - command OUTPUT. INV-6 keeps output off every wire but the model's own; a column carrying it
//     would be a second copy of the box's contents in a table nobody reads that way.
//   - the RAW command. summarizeAuditSteps has always refused it ("raw commands can carry paths,
//     tokens or user-provided arguments"), and a new column does not change that reasoning. What
//     is stored is the redacted DISPLAY form — the same two redactors, in the same order, that the
//     user already saw this command through in the live activity stream.
//
// It is also NOT a resume cursor, and must not become one. It records what a past run did so a
// human can be told; feeding it to a new harness as "already done, skip these" would need a
// different artifact with stricter properties — complete rather than bounded, and attesting each
// command's EFFECT rather than its exit status.
type PersistedStepSummary struct {
	Command     string `json:"cmd"`
	Tier        string `json:"tier"`
	Disposition string `json:"disp"`
	Reason      string `json:"reason,omitempty"`
	ExitCode    *int   `json:"exit,omitempty"`
	Bytes       int    `json:"bytes,omitempty"`
}

// summarizeAuditStepDetail redacts and bounds the steps for persistence. Redaction happens HERE,
// at the producer, rather than in the SQL writer where the task's redaction lives: AuditEvent is
// handed to every AuditWriter including the in-memory one, so a raw command must never be inside
// it in the first place.
func summarizeAuditStepDetail(steps []Step) []PersistedStepSummary {
	if len(steps) == 0 {
		return nil
	}
	limit := len(steps)
	if limit > maxAuditStepRows {
		limit = maxAuditStepRows
	}
	out := make([]PersistedStepSummary, 0, limit)
	for _, step := range steps[:limit] {
		out = append(out, PersistedStepSummary{
			Command:     truncateRunes(guardrails.RedactOutputLeak(guardrails.RedactPII(step.Command)), maxAuditStepCommandRunes),
			Tier:        step.Tier,
			Disposition: step.Disposition,
			Reason:      step.Reason,
			ExitCode:    step.ExitCode,
			Bytes:       step.Bytes,
		})
	}
	return out
}

// truncateRunes returns at most n runes IN TOTAL and says when it cut, because a silently shortened
// command reads as a different command — `rm -rf /root/.cache/pip` and `rm -rf /root` are the same
// string for the first 12 characters.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	marker := []rune(auditTruncationMarker)
	if n <= len(marker) {
		// Unreachable at the real bound (200 vs 5); a bare cut beats a panic if someone lowers it.
		return string(runes[:n])
	}
	return string(runes[:n-len(marker)]) + auditTruncationMarker
}

// summarizeAuditSteps creates only aggregate/enum audit data. Command text is
// intentionally not copied into the audit row: raw commands can carry paths,
// tokens or user-provided arguments, while the fixed class is enough to measure
// whether context changed the first diagnostic move.
func summarizeAuditSteps(steps []Step) (ran, refused int, firstClass string) {
	firstClass = auditFirstCommandNone
	for index, step := range steps {
		if index == 0 {
			firstClass = auditCommandClass(step.Command)
		}
		switch step.Disposition {
		case "ran":
			ran++
		case "refused":
			refused++
		}
	}
	return ran, refused, firstClass
}

func auditCommandClass(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "unknown"
	}
	switch fields[0] {
	case "uname", "lsb_release", "lscpu", "lspci", "which", "id", "whoami":
		return "environment_discovery"
	case "cat":
		if len(fields) > 1 {
			switch fields[1] {
			case "/etc/os-release", "/etc/issue", "/proc/version":
				return "environment_discovery"
			}
		}
		return "file_inspection"
	case "ls", "find", "tail", "head", "stat", "readlink", "grep":
		return "file_inspection"
	case "nvidia-smi":
		return "gpu_validation"
	case "df", "du", "free", "uptime", "top", "ps", "pgrep", "ss", "netstat", "curl", "systemctl", "supervisorctl", "journalctl", "fuser":
		return "targeted_validation"
	default:
		return "other"
	}
}
