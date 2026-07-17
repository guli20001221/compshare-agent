package intent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidatedTargetRefTypesDoNotExposeUnwiredPlatformEntities(t *testing.T) {
	root := filepath.Join("..", "..", "internal", "intent")
	// validator.go was deleted with ValidateRoute; types.go still carries the
	// TargetRef* enum this guard is actually about.
	files := []string{"types.go"}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		text := string(data)
		for _, forbidden := range []string{"TargetRefZone", "TargetRefImage", "TargetRefGPUModel", "gpu_model"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s still contains unwired platform target ref %q", file, forbidden)
			}
		}
	}
}
