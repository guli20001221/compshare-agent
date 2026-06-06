package knowledge

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSourceOriginEnumMatchesPython enforces the cross-language source_origin
// contract: the Go allowedSourceOrigins set (the runtime loader gate) must equal
// the Python ALLOWED_SOURCE_ORIGINS set (the offline corpus validator gate). If
// they drift, a chunk can pass one gate and fail the other — e.g. an external_*
// chunk the Python validator accepts but the Go loader rejects at boot, or vice
// versa. Asserted under `go test ./...`, the merge gate, so neither side can be
// widened without the other.
func TestSourceOriginEnumMatchesPython(t *testing.T) {
	pyPath := filepath.Join("..", "..", "scripts", "rag_w0", "common.py")
	data, err := os.ReadFile(pyPath)
	require.NoError(t, err)

	pySet := parsePythonStringSet(t, string(data), "ALLOWED_SOURCE_ORIGINS")
	require.NotEmpty(t, pySet, "ALLOWED_SOURCE_ORIGINS not found or empty in common.py")

	goSet := map[string]struct{}{}
	for k := range allowedSourceOrigins {
		goSet[k] = struct{}{}
	}
	assert.Equal(t, goSet, pySet, "Go allowedSourceOrigins (loader.go) must match Python ALLOWED_SOURCE_ORIGINS (common.py)")
}

// parsePythonStringSet extracts the quoted string literals from a Python set
// literal `NAME = { "a", "b", ... }`. Intentionally minimal: find the
// assignment, then the next balanced {...} (the set has no nested braces), then
// every "..."/'...' inside.
func parsePythonStringSet(t *testing.T, src, name string) map[string]struct{} {
	t.Helper()
	idx := strings.Index(src, name)
	require.GreaterOrEqual(t, idx, 0, "%s not found", name)
	open := strings.Index(src[idx:], "{")
	require.GreaterOrEqual(t, open, 0, "no { after %s", name)
	rest := src[idx+open:]
	end := strings.Index(rest, "}")
	require.GreaterOrEqual(t, end, 0, "no } closing %s", name)
	body := rest[:end+1]
	re := regexp.MustCompile(`["']([^"']+)["']`)
	out := map[string]struct{}{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		out[m[1]] = struct{}{}
	}
	return out
}
