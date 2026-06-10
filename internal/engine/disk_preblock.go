package engine

import (
	"regexp"
	"strings"

	"github.com/compshare-agent/internal/textutil"
)

var existingDiskIDPattern = regexp.MustCompile(`(?i)\budisk-[a-z0-9_-]+`)

func isExistingDiskAttachUnsupported(userMsg string) bool {
	n := textutil.Normalize(userMsg)
	compact := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(n)

	if strings.Contains(compact, "attachcompsharedisk") {
		return true
	}

	hasAttachVerb := strings.Contains(compact, "挂载") ||
		strings.Contains(compact, "挂到") ||
		strings.Contains(compact, "挂上") ||
		strings.Contains(n, "attach") ||
		strings.Contains(n, "mount")
	if !hasAttachVerb && !strings.Contains(compact, "挂") {
		return false
	}

	if existingDiskIDPattern.MatchString(n) {
		return true
	}

	existingMarkers := []string{
		"已有数据盘",
		"现有数据盘",
		"已有云盘",
		"现有云盘",
		"已有磁盘",
		"现有磁盘",
		"已有盘",
		"现有盘",
		"旧盘",
		"原来的盘",
	}
	for _, marker := range existingMarkers {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	return false
}
