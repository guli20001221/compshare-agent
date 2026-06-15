package engine

import (
	"context"
	"sort"
	"strings"

	"github.com/compshare-agent/internal/workflow"
)

// maxCreateRecoveryProbes bounds how many candidate images the create-zone
// recovery probes for availability, so a recovery never hammers the capacity
// API. Ordered by keyword relevance, so the most likely match is probed first.
const maxCreateRecoveryProbes = 8

// isImageUnavailableMessage reports whether a failed workflow message is the
// upstream "image not available in this zone" rejection — RetCode=230 on
// CheckCompShareResourceCapacity ("Params [CompShareImageId] not available").
// Platform images are zone-blind at query time (DescribeCompShareImages has no
// Zone param), so a name-matched image can simply be absent from the resolved
// create-zone; the capacity precheck is the only signal we get.
func isImageUnavailableMessage(msg string) bool {
	return strings.Contains(msg, "230") && strings.Contains(msg, "CompShareImageId")
}

// createImageUnavailable reports whether a CreateInstanceWorkflow result failed
// specifically because the chosen platform image is not available in the
// resolved zone (vs. sold-out stock, insufficient balance, etc.).
func createImageUnavailable(result *workflow.Result) bool {
	return result != nil && !result.Success && isImageUnavailableMessage(result.Message)
}

type createImageCand struct {
	id    string
	name  string
	score int
}

// resolveAvailableCreateImage finds a platform image that (a) relates to the
// user's requested image keyword and (b) is actually creatable (single-card
// stock) in the given zone for gpuType — probing capacity per candidate, first
// available wins. Returns ok=false when no relevant image is creatable, so the
// caller falls back to an honest failure reply. failedID is the image that
// already 230'd and is skipped. This is the engine-side analogue of the
// deploy_model handler's resolve-in-engine-then-thread pattern, but reactive:
// it only runs after the saga's own resolution hit the zone-availability wall.
func (e *Engine) resolveAvailableCreateImage(ctx context.Context, args map[string]any, zone, failedID string) (id, name string, ok bool) {
	keyword, _ := args["ImageName"].(string)
	gpuType, _ := args["GpuType"].(string)
	if strings.TrimSpace(keyword) == "" || strings.TrimSpace(gpuType) == "" {
		return "", "", false
	}
	res := e.querySafeRead(ctx, "DescribeCompShareImages", map[string]any{"Limit": 100})
	imageSet, _ := res["ImageSet"].([]any)
	cands := rankCreateImageCandidates(imageSet, keyword, failedID)
	for i, c := range cands {
		if i >= maxCreateRecoveryProbes {
			break
		}
		// zoneStockState 230s back to zoneUnknown for an image absent from the
		// zone, so zoneInStock means BOTH "image valid here" AND "has stock".
		if e.zoneStockState(ctx, zone, gpuType, c.id) == zoneInStock {
			return c.id, c.name, true
		}
	}
	return "", "", false
}

// rankCreateImageCandidates orders platform images by relevance to keyword,
// keeping only images with some relation: exact name > name contains keyword >
// shares a >=4-char substring (e.g. "PyTorch" ↔ "cuda128_torch291_py312" via
// "torch"). Unrelated images (a Windows image for a "pytorch" request) are
// dropped so recovery never silently swaps in a wholly different kind of image.
// The failed image and entries without an id/name are skipped.
func rankCreateImageCandidates(imageSet []any, keyword, failedID string) []createImageCand {
	kw := strings.ToLower(strings.TrimSpace(keyword))
	var out []createImageCand
	for _, item := range imageSet {
		img, _ := item.(map[string]any)
		id, _ := img["CompShareImageId"].(string)
		nm, _ := img["Name"].(string)
		if id == "" || nm == "" || id == failedID {
			continue
		}
		ln := strings.ToLower(nm)
		score := 0
		switch {
		case ln == kw:
			score = 3
		case kw != "" && strings.Contains(ln, kw):
			score = 2
		case sharesSubstringFold(kw, ln, 4):
			score = 1
		}
		if score == 0 {
			continue
		}
		out = append(out, createImageCand{id: id, name: nm, score: score})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out
}

// sharesSubstringFold reports whether a and b share a common substring of at
// least minLen (case-insensitive). It operates on bytes; image names are ASCII
// so this is exact for them and merely best-effort (never panics) for CJK.
func sharesSubstringFold(a, b string, minLen int) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if len(a) < minLen || len(b) < minLen {
		return false
	}
	for i := 0; i+minLen <= len(a); i++ {
		if strings.Contains(b, a[i:i+minLen]) {
			return true
		}
	}
	return false
}
