package deployment

import "testing"

// platformImageResult mirrors a live DescribeCompShareImages ImageSet row, numbers
// decoded to float64 as encoding/json does, Container the "True"/"False" string,
// Softwares a nested object.
func platformImageResult() map[string]any {
	return map[string]any{
		"ImageSet": []any{
			map[string]any{
				"CompShareImageId":  "img-torch",
				"Name":              "PyTorch 2.9.1",
				"ImageType":         "System",
				"ImageOwnerAlias":   "Official",
				"Status":            "Available",
				"Container":         "True",
				"Size":              float64(51200),
				"CreateTime":        float64(1700000000),
				"PubTime":           float64(1700000100),
				"SupportedGpuTypes": []any{"4090", "5090"},
				"Softwares": map[string]any{
					"Framework":             "PyTorch",
					"FrameworkVersion":      "2.9.1",
					"CUDAVersion":           "12.8",
					"OsVersion":             "Ubuntu 22.04",
					"PythonVersion":         "3.12",
					"FrameworkVersionIndex": float64(291),
				},
			},
			map[string]any{
				// A bare OS image: no Softwares block, Container "False".
				"CompShareImageId": "img-ubuntu",
				"Name":             "Ubuntu 22.04",
				"ImageType":        "System",
				"Status":           "Available",
				"Container":        "False",
			},
		},
	}
}

func TestParsePlatformImageEntries_CapturesSoftwaresAndContainer(t *testing.T) {
	entries := ParsePlatformImageEntries(platformImageResult(), "platform")
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	torch := entries[0]
	if torch.ID != "img-torch" || torch.Name != "PyTorch 2.9.1" {
		t.Fatalf("id/name mismatch: %+v", torch)
	}
	if !torch.Container {
		t.Errorf("Container=\"True\" must parse as container=true")
	}
	if torch.Source != "platform" {
		t.Errorf("source not tagged: %q", torch.Source)
	}
	if got := torch.SupportedGPUTypes; len(got) != 2 || got[0] != "4090" || got[1] != "5090" {
		t.Errorf("SupportedGpuTypes not captured: %v", got)
	}
	// The whole point of S0: the structured Softwares block the legacy
	// platformImageCandidates path DISCARDED is now captured, so downstream ranking
	// reads real fields instead of parsing the Name string.
	sw := torch.Software
	if !sw.Present {
		t.Fatalf("Softwares present but Present=false")
	}
	if sw.Framework != "PyTorch" || sw.CUDAVersion != "12.8" || sw.OsVersion != "Ubuntu 22.04" {
		t.Errorf("software facts mismatch: %+v", sw)
	}
	if sw.FrameworkVersionIndex != 291 {
		t.Errorf("FrameworkVersionIndex not captured: %d", sw.FrameworkVersionIndex)
	}
	if torch.SizeMB != 51200 {
		t.Errorf("SizeMB mismatch: %v", torch.SizeMB)
	}
}

func TestParsePlatformImageEntries_HonestAbsenceForBareImage(t *testing.T) {
	entries := ParsePlatformImageEntries(platformImageResult(), "platform")
	ubuntu := entries[1]
	if ubuntu.Container {
		t.Errorf("Container=\"False\" must parse as container=false")
	}
	// A nil Softwares pointer is honest absence, NOT a fabricated blank: Present is
	// the flag a consumer must read before trusting any software field.
	if ubuntu.Software.Present {
		t.Errorf("bare image has no Softwares block; Present must be false, got %+v", ubuntu.Software)
	}
	if ubuntu.Software.Framework != "" || ubuntu.Software.CUDAVersion != "" {
		t.Errorf("absent software must stay zero-valued, got %+v", ubuntu.Software)
	}
}

func TestParseCommunityImageEntries_GroupedShapeAndNameBackfill(t *testing.T) {
	result := map[string]any{
		"CompshareImageGroup": []any{
			map[string]any{
				"ImageName": "Stable Diffusion WebUI",
				"Data": []any{
					map[string]any{
						"CompShareImageId": "img-sd-v1",
						"Name":             "SD WebUI v1",
						"Container":        "True",
					},
					map[string]any{
						// Version row with no Name — group name backfills it.
						"CompShareImageId": "img-sd-v2",
					},
				},
			},
		},
	}
	entries := ParseCommunityImageEntries(result)
	if len(entries) != 2 {
		t.Fatalf("want 2 community entries, got %d", len(entries))
	}
	if entries[0].Source != "community" {
		t.Errorf("community source not tagged: %q", entries[0].Source)
	}
	if entries[1].Name != "Stable Diffusion WebUI" {
		t.Errorf("group name must backfill a version row with no Name, got %q", entries[1].Name)
	}
}

func TestImageCatalogSnapshot_ByIDCaseInsensitiveAndUnavailableRefuses(t *testing.T) {
	entries := ParsePlatformImageEntries(platformImageResult(), "platform")
	snap := NewImageCatalogSnapshot(true, entries)
	if snap.Len() != 2 {
		t.Fatalf("want 2 rows, got %d", snap.Len())
	}
	if _, ok := snap.ByID("IMG-TORCH"); !ok {
		t.Errorf("ByID must match case-insensitively")
	}
	if _, ok := snap.ByID("nope"); ok {
		t.Errorf("ByID must miss an absent id")
	}

	// An unavailable snapshot is NOT an empty catalog: it refuses every read, so a
	// consumer that forgets Available() still cannot silently read stale/absent data.
	unavail := NewImageCatalogSnapshot(false, entries)
	if unavail.Available() {
		t.Fatalf("available=false snapshot reports Available")
	}
	if _, ok := unavail.ByID("img-torch"); ok {
		t.Errorf("unavailable snapshot must refuse ByID even for a real id")
	}
	if unavail.Entries() != nil || unavail.Len() != 0 {
		t.Errorf("unavailable snapshot must expose no entries")
	}
	// A nil snapshot is treated identically to a failed fetch.
	var nilSnap *ImageCatalogSnapshot
	if nilSnap.Available() {
		t.Errorf("nil snapshot must be unavailable")
	}
}

func TestImageCatalogSnapshot_RepeatIDReplacesWhole(t *testing.T) {
	// A later row for the same id replaces the earlier one whole: id/name/software
	// move together and cannot diverge.
	first := ImageCatalogEntry{ID: "img-x", Name: "old", Software: SoftwareFacts{Present: true, Framework: "TF"}}
	second := ImageCatalogEntry{ID: "img-x", Name: "new"}
	snap := NewImageCatalogSnapshot(true, []ImageCatalogEntry{first, second})
	if snap.Len() != 1 {
		t.Fatalf("repeat id must collapse to one row, got %d", snap.Len())
	}
	got, _ := snap.ByID("img-x")
	if got.Name != "new" {
		t.Errorf("later row must win: %q", got.Name)
	}
	if got.Software.Present {
		t.Errorf("replace must be WHOLE — the second row's empty software must not inherit the first's")
	}
}

func TestImageCatalogSnapshot_EntriesReturnsFreshCopy(t *testing.T) {
	entries := ParsePlatformImageEntries(platformImageResult(), "platform")
	snap := NewImageCatalogSnapshot(true, entries)
	got := snap.Entries()
	if len(got) == 0 || len(got[0].SupportedGPUTypes) == 0 {
		t.Fatalf("expected entries with gpu types")
	}
	got[0].SupportedGPUTypes[0] = "mutated"
	fresh := snap.Entries()
	if fresh[0].SupportedGPUTypes[0] == "mutated" {
		t.Errorf("Entries must return a fresh copy; mutation leaked into snapshot")
	}
}
