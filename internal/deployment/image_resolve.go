package deployment

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
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
	// Tag is one exact upstream image tag. It is catalog data, not a semantic
	// alias: callers may use it to rank runtime-named rows such as
	// cuda128_torch291_py312 when the row itself carries the literal "pytorch"
	// tag the user wrote.
	Tag string

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
//	within a tier: same-framework version index (or structured dotted version when
//	the live index is zero), then same-series PubTime, then raw catalog order
//	(invariant 2 — never cross-framework, never time-as-newest)
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
			if !ImageStatusUsable(e.Source, e.Status) || (req.Zone.IsPod && !e.Container) {
				return ImageResolution{Status: ResolutionNotFound}
			}
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
		strings.TrimSpace(req.PythonVersion) != "" ||
		strings.TrimSpace(req.Tag) != ""
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

// viableEntries applies the two HARD gates (and only those): source-aware image
// status usability, and a pod zone requiring a container image. A GPU-support mismatch is deliberately
// NOT a filter — SupportedGpuTypes is a recommendation hint (empty = unknown, not
// unsupported), so it only bumps ranking, never rejects (recon redline #2).
func viableEntries(entries []ImageCatalogEntry, req ImageRequest) []ImageCatalogEntry {
	var out []ImageCatalogEntry
	for _, e := range entries {
		if !ImageStatusUsable(e.Source, e.Status) {
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
// framework version first (the upstream index when populated, otherwise its
// structured dotted version), then newest publish time, then catalog order
// (stable). Time is only the tie-break — never a stand-in for "newest software"
// across images.
func sortByVersionLadder(entries []ImageCatalogEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if sameFramework(a, b) {
			if a.Software.FrameworkVersionIndex != b.Software.FrameworkVersionIndex {
				return a.Software.FrameworkVersionIndex > b.Software.FrameworkVersionIndex
			}
			if compared, ok := compareDottedVersion(a.Software.FrameworkVersion, b.Software.FrameworkVersion); ok && compared != 0 {
				return compared > 0
			}
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
	// scoredByRequest records whether the request expressed a preference ABOUT THE
	// IMAGE. When it did not, every ordering beyond the catalog's own is our
	// invention rather than the user's preference — see the tiebreak below.
	//
	// A GPU bump deliberately does NOT count. SupportedGpuTypes is a compatibility
	// hint about hardware, not a statement about which image the user wants, and the
	// guided flow almost always knows the GPU by the time it builds the picker — so
	// letting it set this flag handed every ordinary create turn to the PubTime
	// tiebreak and threw away the popularity order the caller asked upstream for.
	// Live symptom: InfiniteTalk (16,969 deploys, February build) lost its place to
	// a 3-deploy image published later.
	scoredByRequest := false
	for i, e := range entries {
		preference := nameSimilarity(want, e.Name) + structuredScore(req, e)
		s := preference + gpuBump(req.RequestedGPU, e.SupportedGPUTypes)
		if s <= 0 && !keepAll {
			continue
		}
		if preference > 0 {
			scoredByRequest = true
		}
		out = append(out, scored{e: e, score: s, idx: i})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		a, b := out[i].e, out[j].e
		if scoredByRequest && sameFramework(a, b) {
			if a.Software.FrameworkVersionIndex != b.Software.FrameworkVersionIndex {
				return a.Software.FrameworkVersionIndex > b.Software.FrameworkVersionIndex
			}
			if compared, ok := compareDottedVersion(a.Software.FrameworkVersion, b.Software.FrameworkVersion); ok && compared != 0 {
				return compared > 0
			}
		}
		// Two entries the REQUEST could not tell apart are ordered by the catalog,
		// not by publication date. Recency is our own opinion, and it overrode the
		// caller's: the community browse explicitly asks upstream to sort by
		// CreatedCount descending, and re-sorting equal scores by PubTime threw that
		// away — a browse card showed the ten newest images instead of the ten most
		// deployed, so "InfiniteTalk" (16.8k deploys, latest version February) lost
		// its place to images with a fraction of the usage but a newer date.
		//
		// PubTime still breaks ties among entries the request DID discriminate,
		// where the caller expressed a preference and the newest match is the right
		// one of several equally-good matches.
		if scoredByRequest && a.PubTime != b.PubTime {
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

// compareDottedVersion compares catalog-provided framework versions such as
// "2.13.0" and "2.9.1". It deliberately accepts only a plain dotted numeric core
// (with optional v prefix and build/prerelease suffix). Anything else returns
// ok=false and preserves the upstream catalog order rather than guessing from an
// image name.
func compareDottedVersion(a, b string) (compared int, ok bool) {
	parse := func(raw string) ([]uint64, bool) {
		raw = strings.TrimSpace(raw)
		raw = strings.TrimPrefix(strings.TrimPrefix(raw, "v"), "V")
		for offset, r := range raw {
			if r == '+' || r == '-' {
				raw = raw[:offset]
				break
			}
		}
		if raw == "" {
			return nil, false
		}
		parts := strings.Split(raw, ".")
		out := make([]uint64, len(parts))
		for i, part := range parts {
			if part == "" {
				return nil, false
			}
			value, err := strconv.ParseUint(part, 10, 64)
			if err != nil {
				return nil, false
			}
			out[i] = value
		}
		return out, true
	}
	left, leftOK := parse(a)
	right, rightOK := parse(b)
	if !leftOK || !rightOK {
		return 0, false
	}
	size := len(left)
	if len(right) > size {
		size = len(right)
	}
	for i := 0; i < size; i++ {
		var lv, rv uint64
		if i < len(left) {
			lv = left[i]
		}
		if i < len(right) {
			rv = right[i]
		}
		if lv < rv {
			return -1, true
		}
		if lv > rv {
			return 1, true
		}
	}
	return 0, true
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

// DirectImageNameMatch answers the stricter identity question used when a
// historical exact id is carried alongside a name the user supplied now. The
// ordinary ranker deliberately admits shared-token and shared-substring near
// matches so a picker can offer useful alternatives (for example, FaceFusion may
// show SVC-Fusion). A near match is not strong enough to let that historical id
// become the default for the user's newer name, so only exact/contains relations
// qualify here. Formatting-only whitespace is ignored before that strict check:
// a user can restate "最强 AI 数字人 InfiniteTalk" while the upstream label is
// "最强AI数字人InfiniteTalk-图片和视频数字人" without losing the exact id
// already grounded in the conversation. Punctuation, tokens, and word order are
// deliberately NOT normalized, so this does not turn a fuzzy picker match into
// identity. DisplayLabel includes a community version, allowing a copied version
// such as "v3.6" to identify its row without an image-specific alias.
func DirectImageNameMatch(entry ImageCatalogEntry, name string) bool {
	return nameSimilarity(directImageNameKey(name), directImageNameKey(entry.DisplayLabel())) >= 150
}

// directImageNameKey removes only Unicode whitespace before the strict
// identity comparison. It intentionally leaves punctuation and every
// non-whitespace rune intact: this is presentation normalization, not a
// synonym/keyword matcher.
func directImageNameKey(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, strings.TrimSpace(value))
}

// structuredScore rewards catalog rows whose real SoftwareFacts or exact upstream
// Tags match the request's structured preferences. Absent software metadata
// (Present==false) contributes no software score — honest absence, never a
// fabricated match — while tags remain independently usable catalog evidence.
func structuredScore(req ImageRequest, entry ImageCatalogEntry) int {
	score := 0
	if entry.Software.Present {
		if req.Framework != "" && strings.EqualFold(strings.TrimSpace(entry.Software.Framework), strings.TrimSpace(req.Framework)) {
			score += 80
		}
		if req.CUDAVersion != "" && versionRelated(entry.Software.CUDAVersion, req.CUDAVersion) {
			score += 40
		}
		if req.OsVersion != "" && substringFold(entry.Software.OsVersion, req.OsVersion) {
			score += 40
		}
		if req.PythonVersion != "" && versionRelated(entry.Software.PythonVersion, req.PythonVersion) {
			score += 20
		}
	}
	if req.Tag != "" {
		for _, tag := range entry.Tags {
			if strings.EqualFold(strings.TrimSpace(tag), strings.TrimSpace(req.Tag)) {
				score += 60
				break
			}
		}
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
