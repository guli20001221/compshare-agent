package intent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlannerTargetRefTypesDoNotExposeUnwiredPlatformEntities(t *testing.T) {
	root := filepath.Join("..", "..", "internal", "intent")
	files := []string{"types.go", "validator.go", "router_schema.go"}
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
