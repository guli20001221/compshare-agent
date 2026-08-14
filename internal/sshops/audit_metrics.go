package sshops

import "strings"

const auditFirstCommandNone = "none"

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
