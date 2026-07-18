package deployment

import (
	"sort"
	"strings"
)

// ResolutionStatus and Provenance are TWO orthogonal dimensions of an image
// resolution, kept separate on purpose (the recon's redline #4). Status answers
// "could we resolve the request against the catalog"; Provenance answers "where did
// the resolved value come from". Flattening them into one enum cannot express the
// exact state the legacy catalogImageName path produced — a value that WAS returned
// but was NOT catalog-verified — which is the defect this convergence removes.
type ResolutionStatus string

const (
	// ResolutionResolved: exactly one catalog-verified image answers the request. It
	// is the ONLY status that may be sealed into a create/reinstall contract.
	ResolutionResolved ResolutionStatus = "resolved"
	// ResolutionAmbiguous: several genuinely-different catalog images match equally
	// (e.g. the same exact name across two different frameworks) and the resolver
	// refuses to guess between them.
	ResolutionAmbiguous ResolutionStatus = "ambiguous"
	// ResolutionNotFound: the request named/described something the catalog does not
	// contain exactly. Candidates carries the ranked near-matches to offer — but the
	// caller must confirm before any of them is written (the acceptance gate).
	ResolutionNotFound ResolutionStatus = "not_found"
	// ResolutionCatalogUnavailable: the engine could not obtain the catalog this
	// turn. Not the user's fault, not a rejection — refuse and retry, never guess.
	ResolutionCatalogUnavailable ResolutionStatus = "catalog_unavailable"
)

// Provenance records WHERE a selected image's identity came from. Invariant 1
// forbids one specific pairing — a value written to a create contract must never be
// (resolved, caller_supplied_unverified): only a catalog-verified id may be sealed,
// so the resolver never emits that combination. caller_supplied_unverified exists
// only to LABEL an unverified value the caller passed (for explanation), never to
// bless it.
type Provenance string

const (
	ProvenanceCatalogExactID           Provenance = "catalog_exact_id"
	ProvenanceCatalogExactName         Provenance = "catalog_exact_name"
	ProvenanceStructuredRecommendation Provenance = "structured_recommendation"
	ProvenanceCallerSuppliedUnverified Provenance = "caller_supplied_unverified"
)

// ImageSelection is the final, all-scalar image decision that a CreateExecutionDraft
// may seal (invariant 3): id + display name + source + runtime form + how the id was
// established. Every field is a scalar copied out of ONE catalog row, so the name a
// card shows can never describe a different image than the id that executes.
type ImageSelection struct {
	ID         string
	Name       string
	Source     string
	Container  bool
	Provenance Provenance
}

// ImageRequest is what the agent/handler asks the resolver to find. Name is the
// image name or keyword the user referenced; the Framework/CUDA/OS/Python fields
// are STRUCTURED preferences matched against the catalog's real SoftwareFacts — the
// resolver never re-derives "deep learning → {pytorch,torch,cuda,...}" from a
// keyword table. RequestedGPU and Zone gate viability (a pod requires a container
// image); Source scopes the search to one listing when set.
type ImageRequest struct {
	ID   string
	Name string

	Framework     string
	CUDAVersion   string
	OsVersion     string
	PythonVersion string

	RequestedGPU string
	Zone         ZoneConstraint
	Source       string

	// Prefiltered marks a request whose Name was ALREADY applied by the upstream
	// query (the create flow queries DescribeCompShareImages with Name= / community
	// with FuzzySearch=, so every returned row is a legitimate name match). When set,
	// a non-exact request recommends the best of the viable rows rather than applying
	// a second, stricter client-side name filter that would reject the API's own
	// hits. It never turns a not_found into a resolved — the recommendation still
	// rides the confirm gate.
	Prefiltered bool
}

// ImageResolution is the resolver's typed answer. Selection is populated only when
// Status==ResolutionResolved. Candidates carries the ranked near-matches for a
// not_found/ambiguous outcome — Candidates[0] is the strongest recommendation a
// confirm-gated caller may PROPOSE (never auto-seal).
type ImageResolution struct {
	Status     ResolutionStatus
	Selection  ImageSelection
	Candidates []ImageSelection
}

// ResolveImage is the SINGLE image interpreter. Create, the 230-recovery and
// reinstall all call it against one per-turn ImageCatalogSnapshot, replacing the
// three keyword-table matchers (matchPlatformImage / rankCreateImageCandidates /
// reinstallImageMatches) with one deterministic ladder:
//
//	① explicit CompShareImageId, verified against the catalog
//	② exact image name (case-insensitive)
//	③ structured-field match (Framework / CUDA / OS / Python) + general name similarity
//	within a tier: same-framework FrameworkVersionIndex, then same-series PubTime,
//	then raw catalog order (invariant 2 — never cross-framework, never time-as-newest)
//
// Only ① and ② produce ResolutionResolved. A named/described request with no exact
// hit is ResolutionNotFound plus ranked Candidates — the resolver never silently
// swaps in a near match (the acceptance gate). A request with no id/name/structured
// preference at all resolves to the platform default (first viable catalog row).
func ResolveImage(snap *ImageCatalogSnapshot, req ImageRequest) ImageResolution {
	if !snap.Available() {
		return ImageResolution{Status: ResolutionCatalogUnavailable}
	}

	// ① Explicit id: only a catalog-verified id resolves (invariant 1). An id absent
	// from the catalog is NOT resolved to an unverified name — it is not_found, so the
	// caller re-asks or corrects rather than sealing a stale pair.
	if id := strings.TrimSpace(req.ID); id != "" {
		if e, ok := snap.ByID(id); ok {
			return ImageResolution{Status: ResolutionResolved, Selection: selectionFrom(e, ProvenanceCatalogExactID)}
		}
		return ImageResolution{Status: ResolutionNotFound}
	}

	viable := viableEntries(scopeBySource(snap.Entries(), req.Source), req)
	if len(viable) == 0 {
		return ImageResolution{Status: ResolutionNotFound}
	}

	// ② Exact name.
	if exact := exactNameMatches(viable, req.Name); len(exact) > 0 {
		sortByVersionLadder(exact)
		if len(exact) >= 2 && crossFrameworkCollision(exact[0], exact[1]) {
			// The same exact name spans two different frameworks — a framework-scoped
			// version index cannot order them, so ask instead of guessing.
			return ImageResolution{Status: ResolutionAmbiguous, Candidates: selections(exact, ProvenanceCatalogExactName)}
		}
		return ImageResolution{
			Status:     ResolutionResolved,
			Selection:  selectionFrom(exact[0], ProvenanceCatalogExactName),
			Candidates: selections(exact[1:], ProvenanceCatalogExactName),
		}
	}

	// No exact match. A request with no specific ask resolves to the platform default;
	// a specific ask that missed becomes not_found + ranked recommendations.
	if !hasSpecificRequest(req) {
		return ImageResolution{Status: ResolutionResolved, Selection: selectionFrom(viable[0], ProvenanceStructuredRecommendation)}
	}
	// keepAll when the upstream query already filtered by name: every viable row is a
	// legitimate hit, so recommend the best rather than drop rows a stricter
	// client-side similarity would reject.
	ranked := rankRecommendations(viable, req, req.Prefiltered)
	return ImageResolution{Status: ResolutionNotFound, Candidates: selections(ranked, ProvenanceStructuredRecommendation)}
}

// RankImages returns the viable images ranked by the same ladder ResolveImage picks
// its winner from — for a form that must present an option list rather than one
// selection. Viability (status usable, pod⇒container) is applied; a GPU-recommendation
// mismatch is NOT filtered (the form shows it disabled), so the caller reads each
// row's SupportedGPUTypes to decide.
//
// When the request names a SPECIFIC image (a Name or a structured preference), the
// list is FILTERED to the related candidates — the form-list analogue of ResolveImage
// returning not_found plus only ranked near-matches for a specific miss, so a card
// that asked for "torch" never offers a wholly unrelated Windows image beside it. A
// bare browse (no specific ask) keeps every viable row. An exact name match leads;
// otherwise the order is structured-match + name similarity, then the version ladder,
// then catalog order. Empty when the catalog is unavailable.
func RankImages(snap *ImageCatalogSnapshot, req ImageRequest) []ImageSelection {
	if !snap.Available() {
		return nil
	}
	viable := viableEntries(scopeBySource(snap.Entries(), req.Source), req)
	return selections(rankRecommendations(viable, req, !hasSpecificRequest(req)), ProvenanceStructuredRecommendation)
}

func hasSpecificRequest(req ImageRequest) bool {
	return strings.TrimSpace(req.Name) != "" ||
		strings.TrimSpace(req.Framework) != "" ||
		strings.TrimSpace(req.CUDAVersion) != "" ||
		strings.TrimSpace(req.OsVersion) != "" ||
		strings.TrimSpace(req.PythonVersion) != ""
}

func scopeBySource(entries []ImageCatalogEntry, source string) []ImageCatalogEntry {
	source = strings.TrimSpace(source)
	if source == "" {
		return entries
	}
	out := entries[:0:0]
	for _, e := range entries {
		if strings.EqualFold(e.Source, source) {
			out = append(out, e)
		}
	}
	return out
}

// viableEntries applies the two HARD gates (and only those), mirroring
// SelectImageCandidates: an image whose Status is set and not Available is dropped,
// and a pod zone requires a container image. A GPU-support mismatch is deliberately
// NOT a filter — SupportedGpuTypes is a recommendation hint (empty = unknown, not
// unsupported), so it only bumps ranking, never rejects (recon redline #2).
func viableEntries(entries []ImageCatalogEntry, req ImageRequest) []ImageCatalogEntry {
	var out []ImageCatalogEntry
	for _, e := range entries {
		if e.Status != "" && !strings.EqualFold(e.Status, ImageStatusAvailable) {
			continue
		}
		if req.Zone.IsPod && !e.Container {
			continue
		}
		out = append(out, e)
	}
	return out
}

func exactNameMatches(entries []ImageCatalogEntry, name string) []ImageCatalogEntry {
	want := strings.ToLower(strings.TrimSpace(name))
	if want == "" {
		return nil
	}
	var out []ImageCatalogEntry
	for _, e := range entries {
		if strings.ToLower(strings.TrimSpace(e.Name)) == want {
			out = append(out, e)
		}
	}
	return out
}

// sortByVersionLadder orders a group assumed to be versions of one image: newest
// framework version first (FrameworkVersionIndex, comparable only within the same
// framework), then newest publish time, then catalog order (stable). Time is only
// the tie-break — never a stand-in for "newest software" across images.
func sortByVersionLadder(entries []ImageCatalogEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if sameFramework(a, b) && a.Software.FrameworkVersionIndex != b.Software.FrameworkVersionIndex {
			return a.Software.FrameworkVersionIndex > b.Software.FrameworkVersionIndex
		}
		if a.PubTime != b.PubTime {
			return a.PubTime > b.PubTime
		}
		return false
	})
}

func sameFramework(a, b ImageCatalogEntry) bool {
	return a.Software.Present && b.Software.Present && strings.EqualFold(a.Software.Framework, b.Software.Framework)
}

// crossFrameworkCollision reports two same-name images that belong to DIFFERENT
// frameworks — the one case where an exact-name match is genuinely ambiguous.
func crossFrameworkCollision(a, b ImageCatalogEntry) bool {
	return a.Software.Present && b.Software.Present && !strings.EqualFold(a.Software.Framework, b.Software.Framework)
}

// rankRecommendations scores every viable entry by general name similarity plus
// structured-field match plus a GPU-recommendation bump, and orders them by (score,
// version ladder, catalog order). It uses NO domain keyword table — similarity is
// generic substring/token overlap, and the structured signal reads the catalog's
// real SoftwareFacts.
//
// keepAll=false (the default, un-prefiltered path) drops entries that relate to the
// request in no way, so a recommendation is never a wholly unrelated image. keepAll=
// true (a Prefiltered request, where the upstream query already applied the name)
// keeps every viable row, since the API already judged them relevant — the score
// only orders them.
func rankRecommendations(entries []ImageCatalogEntry, req ImageRequest, keepAll bool) []ImageCatalogEntry {
	type scored struct {
		e     ImageCatalogEntry
		score int
		idx   int
	}
	want := strings.ToLower(strings.TrimSpace(req.Name))
	var out []scored
	for i, e := range entries {
		s := nameSimilarity(want, e.Name) + structuredScore(req, e.Software) + gpuBump(req.RequestedGPU, e.SupportedGPUTypes)
		if s <= 0 && !keepAll {
			continue
		}
		out = append(out, scored{e: e, score: s, idx: i})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		a, b := out[i].e, out[j].e
		if sameFramework(a, b) && a.Software.FrameworkVersionIndex != b.Software.FrameworkVersionIndex {
			return a.Software.FrameworkVersionIndex > b.Software.FrameworkVersionIndex
		}
		if a.PubTime != b.PubTime {
			return a.PubTime > b.PubTime
		}
		return out[i].idx < out[j].idx
	})
	res := make([]ImageCatalogEntry, len(out))
	for i := range out {
		res[i] = out[i].e
	}
	return res
}

// nameSimilarity scores how related two names are, generically (no keyword table):
// equal > contains > contained-by > shared delimited token > shared 4-char run.
func nameSimilarity(want, name string) int {
	if want == "" {
		return 0
	}
	nm := strings.ToLower(strings.TrimSpace(name))
	if nm == "" {
		return 0
	}
	switch {
	case nm == want:
		return 300
	case strings.Contains(nm, want):
		return 200
	case strings.Contains(want, nm):
		return 150
	case sharesToken(want, nm):
		return 120
	case sharedSubstring(want, nm, 4):
		return 100
	default:
		return 0
	}
}

// structuredScore rewards catalog rows whose real SoftwareFacts match the agent's
// structured preferences. Absent software metadata (Present==false) scores zero —
// honest absence, never a fabricated match.
func structuredScore(req ImageRequest, sw SoftwareFacts) int {
	if !sw.Present {
		return 0
	}
	score := 0
	if req.Framework != "" && strings.EqualFold(strings.TrimSpace(sw.Framework), strings.TrimSpace(req.Framework)) {
		score += 80
	}
	if req.CUDAVersion != "" && versionRelated(sw.CUDAVersion, req.CUDAVersion) {
		score += 40
	}
	if req.OsVersion != "" && substringFold(sw.OsVersion, req.OsVersion) {
		score += 40
	}
	if req.PythonVersion != "" && versionRelated(sw.PythonVersion, req.PythonVersion) {
		score += 20
	}
	return score
}

func gpuBump(gpu string, supported []string) int {
	gpu = strings.TrimSpace(gpu)
	if gpu == "" {
		return 0
	}
	for _, s := range supported {
		if strings.EqualFold(strings.TrimSpace(s), gpu) {
			return 10
		}
	}
	return 0
}

func selectionFrom(e ImageCatalogEntry, prov Provenance) ImageSelection {
	return ImageSelection{ID: e.ID, Name: e.Name, Source: e.Source, Container: e.Container, Provenance: prov}
}

func selections(entries []ImageCatalogEntry, prov Provenance) []ImageSelection {
	if len(entries) == 0 {
		return nil
	}
	out := make([]ImageSelection, 0, len(entries))
	for _, e := range entries {
		out = append(out, selectionFrom(e, prov))
	}
	return out
}

// --- generic string helpers (no domain keyword tables) ----------------------

func versionRelated(have, want string) bool {
	have = strings.TrimSpace(strings.ToLower(have))
	want = strings.TrimSpace(strings.ToLower(want))
	if have == "" || want == "" {
		return false
	}
	return have == want || strings.HasPrefix(have, want) || strings.HasPrefix(want, have)
}

func substringFold(haystack, needle string) bool {
	haystack = strings.ToLower(strings.TrimSpace(haystack))
	needle = strings.ToLower(strings.TrimSpace(needle))
	return haystack != "" && needle != "" && strings.Contains(haystack, needle)
}

// sharesToken reports a shared whitespace/underscore/hyphen-delimited token of at
// least 3 characters — generic overlap, not a curated synonym list.
func sharesToken(a, b string) bool {
	fields := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool {
			return r == ' ' || r == '_' || r == '-' || r == '.' || r == '/'
		})
	}
	set := map[string]struct{}{}
	for _, t := range fields(a) {
		if len(t) >= 3 {
			set[t] = struct{}{}
		}
	}
	for _, t := range fields(b) {
		if len(t) >= 3 {
			if _, ok := set[t]; ok {
				return true
			}
		}
	}
	return false
}

// sharedSubstring reports whether a and b share a common run of at least minLen
// characters (case-insensitive, byte-wise — exact for ASCII image names).
func sharedSubstring(a, b string, minLen int) bool {
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
