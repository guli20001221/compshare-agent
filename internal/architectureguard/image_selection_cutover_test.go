package architectureguard

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestImageSelectionKeywordMatchersDeleted is the reappearance guard for the image
// convergence: the create/reinstall image path now resolves through the ONE typed
// interpreter (deployment.ResolveImage / RankImages) against a per-turn catalog
// snapshot, and the guided form offers real ImageSource/ImageType/ImageTag facets.
// The deleted machinery was three independent keyword/regex matchers plus a fake
// ImagePurpose enum that mapped user text to image names by hardcoded keywords
// (pytorch/cuda→deep_learning, vllm/ollama→llm_inference, …). Reintroducing any of
// them would re-fork image resolution and re-teach the workflow to guess semantics —
// exactly what the central Agent now owns.
//
// The Scan/baseline gate (scanner_test.go) forbids only NEW string_heuristic/regex
// sites; it cannot forbid a DEFINITION reappearing, so this mirrors
// TestProductionCannotReEnableLegacySemanticRuntime with a definition-anchored token
// walk. Tokens are signature-anchored (`func X(` / a symbol name) so the explanatory
// comments that still mention the old names in prose do not false-positive.
func TestImageSelectionKeywordMatchersDeleted(t *testing.T) {
	root, err := FindRepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		// The three deleted keyword/version-regex matchers and their cluster.
		"func matchPlatformImage(",
		"func platformImagesForIntent(",
		"func platformImagesForPurpose(",
		"func platformImagePurposeRelevance(",
		"func platformImageRelevance(",
		"func platformImageVersionKey(",
		"func bestViablePlatformImage(",
		"func firstViablePlatformImage(",
		"func viablePlatformImageIDs(",
		"func platformImageCandidates(",
		"func communityImageCandidates(",
		"func sharesImageSubstring(",
		"func imageMismatchWarnings(",
		"func pickPlatformImageId(",
		"func pickPlatformImageName(",
		"func pickFirstCommunityImageId(",
		"func pickFirstCommunityImageName(",
		// The deleted fake ImagePurpose enum + its normalize/form machinery. The
		// facet split (ImageSource/ImageType/ImageTag) replaced it; a migration MUST
		// NOT keep the purpose keyword field alive.
		"func normalizeImagePurpose(",
		"func imagePurposeValue(",
		"func imagePurposeFormOptions(",
		"func buildGuidedImagePurposeForm(",
		"func applyGuidedImagePurposeOverrides(",
		"func shouldSkipGuidedImagePurposeStep(",
		"imagePurposeDeepLearning",
		"imagePurposeLLMInference",
		// The name-version regexes the ladder replaced with real
		// Softwares.FrameworkVersionIndex.
		"torchTagRE",
		"cudaTagRE",
	}
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, token := range forbidden {
				if strings.Contains(string(data), token) {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("deleted image keyword/purpose matcher %q reappeared in %s", token, filepath.ToSlash(rel))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
