package store

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestTruncateAuditTaskKeepsTheTaskWhenItFits(t *testing.T) {
	const task = "检查实例 cpod-test 的 /v1/models，Authorization: Bearer example-0123456789，期望 HTTP 200"
	require.Equal(t, task, truncateAuditTask(task, maxAuditTaskBytes),
		"the audit row records the task the run was given, not a rewritten one")
}

func TestTruncateAuditTaskCutsOnARuneBoundary(t *testing.T) {
	task := strings.Repeat("诊断", 3000) // 18000 bytes, 3 bytes per rune
	got := truncateAuditTask(task, maxAuditTaskBytes)

	require.LessOrEqual(t, len(got), maxAuditTaskBytes)
	require.True(t, utf8.ValidString(got), "a cut inside a CJK rune would make PostgreSQL refuse the INSERT")
	require.Equal(t, 0, len(got)%3, "every kept rune is whole")
}
