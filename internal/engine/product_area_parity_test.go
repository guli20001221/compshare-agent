package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestInferredProductAreasAllowedInPython enforces one direction of the
// product_area sync (RAG Phase 1 HIGH gotcha #2): every label the runtime
// inferKnowledgeProductArea can return must be a member of the offline
// validator's ALLOWED_PRODUCT_AREAS (scripts/rag_w0/common.py). If the engine
// learns to boost an area the Python validator rejects, a chunk authored for
// that area would fail offline validation while the runtime still tries to
// affinity-boost it — a silent corpus/runtime split. The reverse direction is
// intentionally NOT asserted: Python may allow areas the engine has no keyword
// for yet.
func TestInferredProductAreasAllowedInPython(t *testing.T) {
	pyPath := filepath.Join("..", "..", "scripts", "rag_w0", "common.py")
	data, err := os.ReadFile(pyPath)
	if err != nil {
		t.Fatalf("read common.py: %v", err)
	}
	allowed := parsePythonProductAreaSet(t, string(data))
	if len(allowed) == 0 {
		t.Fatal("ALLOWED_PRODUCT_AREAS not found or empty in common.py")
	}
	for _, area := range knowledgeInferredProductAreas {
		if _, ok := allowed[area]; !ok {
			t.Errorf("inferKnowledgeProductArea may return %q which is not in common.py ALLOWED_PRODUCT_AREAS", area)
		}
	}
}

func parsePythonProductAreaSet(t *testing.T, src string) map[string]struct{} {
	t.Helper()
	idx := strings.Index(src, "ALLOWED_PRODUCT_AREAS")
	if idx < 0 {
		return nil
	}
	open := strings.Index(src[idx:], "{")
	if open < 0 {
		return nil
	}
	rest := src[idx+open:]
	end := strings.Index(rest, "}")
	if end < 0 {
		return nil
	}
	body := rest[:end+1]
	re := regexp.MustCompile(`["']([^"']+)["']`)
	out := map[string]struct{}{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		out[m[1]] = struct{}{}
	}
	return out
}
