package deployment

import "testing"

func sw(framework, fver, cuda, os string, idx uint32) SoftwareFacts {
	return SoftwareFacts{Present: true, Framework: framework, FrameworkVersion: fver, CUDAVersion: cuda, OsVersion: os, FrameworkVersionIndex: idx}
}

// resolverCatalog is a small mixed catalog: two PyTorch versions, a TensorFlow
// image sharing an exact name with one of them (cross-framework collision), a bare
// Ubuntu OS image, an offline image, and a VM (non-container) image.
func resolverCatalog() *ImageCatalogSnapshot {
	return NewImageCatalogSnapshot(true, []ImageCatalogEntry{
		{ID: "img-torch-old", Name: "PyTorch", Source: "platform", Status: "Available", Container: true, PubTime: 100, Software: sw("PyTorch", "2.1.0", "11.8", "Ubuntu 20.04", 210)},
		{ID: "img-torch-new", Name: "PyTorch", Source: "platform", Status: "Available", Container: true, PubTime: 200, Software: sw("PyTorch", "2.9.1", "12.8", "Ubuntu 22.04", 291)},
		{ID: "img-ubuntu", Name: "Ubuntu 22.04", Source: "platform", Status: "Available", Container: true, PubTime: 50},
		{ID: "img-tf", Name: "TensorFlow 2.15", Source: "platform", Status: "Available", Container: true, PubTime: 150, Software: sw("TensorFlow", "2.15", "12.2", "Ubuntu 22.04", 215)},
		{ID: "img-offline", Name: "Old CUDA", Source: "platform", Status: "Offline", Container: true, Software: sw("CUDA", "", "11.0", "Ubuntu 18.04", 110)},
		{ID: "img-vm", Name: "Windows Server", Source: "platform", Status: "Available", Container: false, SupportedGPUTypes: []string{"4090"}},
	})
}

func TestResolveImage_ExplicitIDVerifiedResolves(t *testing.T) {
	res := ResolveImage(resolverCatalog(), ImageRequest{ID: "img-torch-new"})
	if res.Status != ResolutionResolved {
		t.Fatalf("verified id must resolve, got %s", res.Status)
	}
	if res.Selection.Provenance != ProvenanceCatalogExactID {
		t.Errorf("provenance must be catalog_exact_id, got %s", res.Selection.Provenance)
	}
	// The name comes from the catalog row, never from a caller-supplied string.
	if res.Selection.Name != "PyTorch" || !res.Selection.Container {
		t.Errorf("selection must project the catalog row: %+v", res.Selection)
	}
}

func TestResolveImage_UnverifiedIDIsNotFoundNeverResolved(t *testing.T) {
	// Invariant 1: an id absent from the catalog must NOT be resolved to an
	// unverified caller name — that is exactly the catalogImageName defect. It is
	// not_found so the caller re-asks or corrects, never seals a stale pair.
	res := ResolveImage(resolverCatalog(), ImageRequest{ID: "img-ghost", Name: "Ghost Image"})
	if res.Status != ResolutionNotFound {
		t.Fatalf("unverified id must be not_found, got %s", res.Status)
	}
	if res.Selection.ID != "" {
		t.Errorf("not_found must carry no sealed selection, got %+v", res.Selection)
	}
}

func TestResolveImage_CatalogUnavailable(t *testing.T) {
	unavail := NewImageCatalogSnapshot(false, nil)
	res := ResolveImage(unavail, ImageRequest{Name: "PyTorch"})
	if res.Status != ResolutionCatalogUnavailable {
		t.Fatalf("unavailable catalog must report catalog_unavailable, got %s", res.Status)
	}
	// A nil snapshot is treated identically.
	if ResolveImage(nil, ImageRequest{Name: "PyTorch"}).Status != ResolutionCatalogUnavailable {
		t.Errorf("nil snapshot must be catalog_unavailable")
	}
}

func TestResolveImage_ExactNamePicksLatestVersionNotTime(t *testing.T) {
	// Two exact-name "PyTorch" rows: the older one (img-torch-old) has a LATER... no,
	// here the newer version also has the newer PubTime, so version and time agree —
	// but the ladder must key on FrameworkVersionIndex, not fall back to time first.
	res := ResolveImage(resolverCatalog(), ImageRequest{Name: "pytorch"})
	if res.Status != ResolutionResolved {
		t.Fatalf("exact name must resolve, got %s", res.Status)
	}
	if res.Selection.Provenance != ProvenanceCatalogExactName {
		t.Errorf("provenance must be catalog_exact_name, got %s", res.Selection.Provenance)
	}
	if res.Selection.ID != "img-torch-new" {
		t.Errorf("must pick highest FrameworkVersionIndex (img-torch-new), got %s", res.Selection.ID)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].ID != "img-torch-old" {
		t.Errorf("the other version must be offered as a candidate, got %+v", res.Candidates)
	}
}

func TestResolveImage_VersionLadderIgnoresLaterTimeAcrossVersions(t *testing.T) {
	// Guard invariant 2 directly: give the OLDER framework version a LATER PubTime.
	// The ladder must still pick the higher FrameworkVersionIndex — time is only the
	// tie-break, never a proxy for "newest software".
	snap := NewImageCatalogSnapshot(true, []ImageCatalogEntry{
		{ID: "v-old", Name: "Box", Source: "platform", Status: "Available", Container: true, PubTime: 999, Software: sw("PyTorch", "2.1", "11.8", "u20", 210)},
		{ID: "v-new", Name: "Box", Source: "platform", Status: "Available", Container: true, PubTime: 1, Software: sw("PyTorch", "2.9", "12.8", "u22", 291)},
	})
	res := ResolveImage(snap, ImageRequest{Name: "Box"})
	if res.Selection.ID != "v-new" {
		t.Errorf("higher FrameworkVersionIndex must win over later PubTime, got %s", res.Selection.ID)
	}
}

func TestResolveImage_VersionLadderUsesStructuredVersionWhenLiveIndexIsZero(t *testing.T) {
	// Measured live: FrameworkVersion is populated while FrameworkVersionIndex is
	// zero on every platform row. The structured dotted version is therefore the
	// only honest "newest" signal; image-name parsing remains forbidden.
	snap := NewImageCatalogSnapshot(true, []ImageCatalogEntry{
		{ID: "v-old", Name: "runtime-old", Source: "platform", Status: "Available", Container: true, Software: sw("PyTorch", "2.9.1", "13.0", "u22", 0)},
		{ID: "v-new", Name: "runtime-new", Source: "platform", Status: "Available", Container: true, Software: sw("PyTorch", "2.13.0", "13.2", "u22", 0)},
	})

	ranked := RankImages(snap, ImageRequest{Framework: "PyTorch", Source: "platform"})
	if len(ranked) != 2 || ranked[0].ID != "v-new" {
		t.Fatalf("structured 2.13.0 must outrank 2.9.1 when both live indexes are zero, got %+v", ranked)
	}
}

func TestCompareDottedVersionRefusesNonNumericCatalogValues(t *testing.T) {
	if compared, ok := compareDottedVersion("nightly", "2.9.1"); ok || compared != 0 {
		t.Fatalf("non-numeric catalog versions must preserve catalog order, got compared=%d ok=%t", compared, ok)
	}
	if compared, ok := compareDottedVersion("v2.9.1+cu130", "2.9"); !ok || compared <= 0 {
		t.Fatalf("plain dotted cores with prefixes/suffixes should compare, got compared=%d ok=%t", compared, ok)
	}
}

func TestResolveImage_CrossFrameworkExactNameIsAmbiguous(t *testing.T) {
	// Same exact name, two different frameworks — a framework-scoped version index
	// cannot order them, so the resolver refuses to guess.
	snap := NewImageCatalogSnapshot(true, []ImageCatalogEntry{
		{ID: "a", Name: "AI Box", Source: "platform", Status: "Available", Container: true, Software: sw("PyTorch", "2.9", "12.8", "u22", 291)},
		{ID: "b", Name: "AI Box", Source: "platform", Status: "Available", Container: true, Software: sw("TensorFlow", "2.15", "12.2", "u22", 215)},
	})
	res := ResolveImage(snap, ImageRequest{Name: "AI Box"})
	if res.Status != ResolutionAmbiguous {
		t.Fatalf("cross-framework same name must be ambiguous, got %s", res.Status)
	}
	if len(res.Candidates) != 2 {
		t.Errorf("both colliding images must be offered, got %d", len(res.Candidates))
	}
}

func TestResolveImage_AcceptanceGate_NoSilentSwap(t *testing.T) {
	// THE acceptance gate: user asks for "Ubuntu-NVIDIA", which does not exist
	// exactly, but similar images do. The resolver must return not_found + ranked
	// candidates showing REAL catalog names — never silently resolve a near match.
	res := ResolveImage(resolverCatalog(), ImageRequest{Name: "Ubuntu-NVIDIA"})
	if res.Status != ResolutionNotFound {
		t.Fatalf("no exact match must be not_found, got %s", res.Status)
	}
	if res.Selection.ID != "" {
		t.Fatalf("not_found must NOT seal a selection (no silent swap), got %+v", res.Selection)
	}
	if len(res.Candidates) == 0 {
		t.Fatalf("similar images exist; candidates must be offered")
	}
	// The top candidate is a real catalog image (Ubuntu shares the "ubuntu" token),
	// carrying its real name — the caller shows this and confirms, never auto-seals.
	top := res.Candidates[0]
	if top.Name == "Ubuntu-NVIDIA" {
		t.Errorf("candidate must carry the REAL catalog name, not the user's text")
	}
	if top.Provenance == ProvenanceCatalogExactName || top.Provenance == ProvenanceCatalogExactID {
		t.Errorf("a near-match candidate must not claim exact provenance, got %s", top.Provenance)
	}
}

func TestResolveImage_NoPreferenceResolvesToDefault(t *testing.T) {
	// No id, no name, no structured preference: the platform default (first viable
	// catalog row) resolves — nothing was asked, so nothing is "not found".
	res := ResolveImage(resolverCatalog(), ImageRequest{})
	if res.Status != ResolutionResolved {
		t.Fatalf("empty request must resolve to the default, got %s", res.Status)
	}
	if res.Selection.ID != "img-torch-old" {
		t.Errorf("default must be the first viable catalog row, got %s", res.Selection.ID)
	}
	if res.Selection.Provenance != ProvenanceStructuredRecommendation {
		t.Errorf("a resolver-chosen default is a recommendation, got %s", res.Selection.Provenance)
	}
}

func TestRankImages_NoImagePreferencePreservesCatalogOrder(t *testing.T) {
	snap := NewImageCatalogSnapshot(true, []ImageCatalogEntry{
		{ID: "older-first", Name: "Older", Source: "platform", Status: "Available", Software: sw("PyTorch", "2.7.1", "", "", 0)},
		{ID: "newer-second", Name: "Newer", Source: "platform", Status: "Available", Software: sw("PyTorch", "2.13.0", "", "", 0)},
	})

	ranked := RankImages(snap, ImageRequest{RequestedGPU: "4090"})
	if len(ranked) != 2 || ranked[0].ID != "older-first" || ranked[1].ID != "newer-second" {
		t.Fatalf("hardware alone is not an image preference; preserve upstream order, got %+v", ranked)
	}
}

func TestResolveImage_PodRequiresContainerImage(t *testing.T) {
	// A pod zone drops the VM image from the viable set entirely.
	res := ResolveImage(resolverCatalog(), ImageRequest{Name: "Windows Server", Zone: ZoneConstraint{IsPod: true}})
	if res.Status == ResolutionResolved {
		t.Fatalf("a VM image must not resolve for a pod zone, got %+v", res.Selection)
	}
}

func TestResolveImage_OfflineImageFilteredFromViable(t *testing.T) {
	// The offline "Old CUDA" row must never be selected or recommended.
	res := ResolveImage(resolverCatalog(), ImageRequest{Name: "Old CUDA"})
	if res.Status == ResolutionResolved && res.Selection.ID == "img-offline" {
		t.Fatalf("an offline image must not resolve")
	}
	for _, c := range res.Candidates {
		if c.ID == "img-offline" {
			t.Fatalf("an offline image must not be recommended")
		}
	}
}

func TestResolveImage_StructuredPreferenceRanksCandidates(t *testing.T) {
	// No exact name, but a structured Framework preference: not_found + candidates
	// ranked so the matching framework leads — using the catalog's real SoftwareFacts,
	// no keyword table.
	res := ResolveImage(resolverCatalog(), ImageRequest{Name: "deep learning box", Framework: "TensorFlow"})
	if res.Status != ResolutionNotFound {
		t.Fatalf("structured-only match is not an exact hit; want not_found, got %s", res.Status)
	}
	if len(res.Candidates) == 0 || res.Candidates[0].ID != "img-tf" {
		t.Errorf("the TensorFlow image must lead on a TensorFlow structured preference, got %+v", res.Candidates)
	}
}

func TestResolveImage_WhollyUnrelatedRequestOffersNothing(t *testing.T) {
	// A specific request that relates to no catalog row: not_found with NO
	// candidates — recovery/create must never swap in a wholly unrelated image.
	res := ResolveImage(resolverCatalog(), ImageRequest{Name: "zzz-nonexistent-xyz"})
	if res.Status != ResolutionNotFound {
		t.Fatalf("want not_found, got %s", res.Status)
	}
	if len(res.Candidates) != 0 {
		t.Errorf("no related image exists; candidates must be empty, got %+v", res.Candidates)
	}
}

func TestResolveImage_SourceScoping(t *testing.T) {
	snap := NewImageCatalogSnapshot(true, []ImageCatalogEntry{
		{ID: "p1", Name: "Shared Name", Source: "platform", Status: "Available", Container: true},
		{ID: "c1", Name: "Shared Name", Source: "community", Status: "Available", Container: true},
	})
	res := ResolveImage(snap, ImageRequest{Name: "Shared Name", Source: "community"})
	if res.Status != ResolutionResolved || res.Selection.ID != "c1" {
		t.Errorf("source scope must confine resolution to community, got %s/%+v", res.Status, res.Selection)
	}
}
