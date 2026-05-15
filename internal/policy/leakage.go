package policy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const RedactedQueryValue = "[REDACTED]"

var (
	staffNamesOnce sync.Once
	staffNames     []string
)

func RedactQueryDerivedValue(value string) string {
	if ContainsQueryLeakage(value) {
		return RedactedQueryValue
	}
	return value
}

func ContainsQueryLeakage(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	lowered := strings.ToLower(trimmed)
	for _, marker := range []string{
		"spt-record",
		"wxwork-spt-record",
		"internal_case",
		"workorder",
		"/cloud/",
		"gitlab.",
		"gitlab.com",
		"feishu.",
		"lark.",
	} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	for _, name := range loadStaffNames() {
		if name != "" && strings.Contains(trimmed, name) {
			return true
		}
	}
	return false
}

func loadStaffNames() []string {
	staffNamesOnce.Do(func() {
		data, err := os.ReadFile(findStaffNamesPath())
		if err != nil {
			staffNames = []string{}
			return
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			staffNames = append(staffNames, line)
		}
	})
	return staffNames
}

func findStaffNamesPath() string {
	const rel = "scripts/rag_w0/staff_names.txt"
	if path, ok := findStaffNamesPathFrom(os.Getwd, rel); ok {
		return path
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		if path, found := findStaffNamesPathFrom(func() (string, error) {
			return filepath.Dir(filepath.Dir(filepath.Dir(file))), nil
		}, rel); found {
			return path
		}
	}
	return rel
}

func findStaffNamesPathFrom(getwd func() (string, error), rel string) (string, bool) {
	dir, err := os.Getwd()
	if getwd != nil {
		dir, err = getwd()
	}
	if err != nil {
		return "", false
	}
	for {
		candidate := filepath.Join(dir, rel)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}
