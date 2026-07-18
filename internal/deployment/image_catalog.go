package deployment

import "strings"

// ImageCatalogSnapshot is the live image catalog, snapshotted by the engine once
// per turn and handed to BOTH the action resolver and the workflow as pure,
// read-only reference data — the exact shape the zone convergence introduced for
// availability zones.
//
// It is the single authority for "which images exist and what they are". Before
// this type there were three independent interpreters (create's matchPlatformImage,
// the 230 recovery's rankCreateImageCandidates, reinstall's reinstallImageMatches),
// each re-querying and re-ranking the raw DescribeCompShareImages response with its
// own keyword table. One snapshot, resolved once, replaces all three.
//
// Available()==false means the engine could not obtain the catalog this turn. That
// is NOT an empty catalog: an unavailable snapshot holds no entries AND refuses
// every read, so a consumer that forgets to check Available() cannot silently fall
// back to a guess. A reader must fail (catalog_unavailable) rather than degrade.
//
// It keeps ONE row per image id (id + display name + runtime form + structured
// software facts together), so the name a card shows can never drift from the id
// that executes — the id/name divergence catalogImageName papered over cannot be
// reintroduced inside the type meant to end it. It is immutable from the outside:
// accessors return an ImageCatalogEntry (all-scalar, with fresh slices) by value.
type ImageCatalogSnapshot struct {
	available bool
	order     []string                     // CompShareImageId, catalog order
	entries   map[string]ImageCatalogEntry // lower(id) -> the one row
}

// ImageCatalogEntry is one catalog row the engine builds from a live CompShareImage.
// Id/Name/Container/Software are ONE unit — a snapshot stores and replaces them
// together so they cannot diverge.
//
// SupportedGPUTypes is upstream's "推荐的GPU机型" hint (pkg/api CompShareImage:103),
// NOT a strict compatibility matrix: contains = positive evidence, not-contains =
// a warning worth surfacing, empty = unknown (never "unsupported"). Container comes
// from the "True/False" string field (pkg/api:77): a hard runtime-form constraint
// (a pod cannot run a VM image), true only when explicitly "True".
type ImageCatalogEntry struct {
	ID                string
	Name              string
	Source            string // platform | community | custom | sharing
	ImageType         string // System | App | Custom | Community
	Status            string // "" (unknown, treated usable) | Available | Offline | ...
	Container         bool   // true only when upstream Container=="True"
	SupportedGPUTypes []string
	Software          SoftwareFacts
	// Tags is upstream CompShareImage.Tags (镜像标签) — the platform's own tag
	// classification for the image. nil means the upstream row carried no Tags: honest
	// absence (unknown), NEVER "matches no tag". A consumer must read len(Tags)==0 as
	// "we don't know this image's tags", not as a reason to exclude it. It is the real
	// data the central Agent reasons over (deep_learning→whatever real tag exists) and
	// the ONLY thing an explicit ImageTag filter does exact membership against.
	Tags []string
	// Description is upstream CompShareImage.Description (镜像描述). "" = absent. Shown to
	// the Agent as structured context for semantic image selection; never parsed for
	// keywords by the workflow. (README/Readme is deliberately NOT captured here — it is
	// large untrusted rich text, fetched per-id later, out of the catalog.)
	Description string
	SizeMB      float64 // image size in MB (Size), for reinstall disk sizing
	CreateTime  int64
	PubTime     int64
}

// SoftwareFacts mirrors upstream SoftwareDetail (pkg/api describe_compshare_images.go
// SoftwareDetail:136), the structured software metadata the platform returns for an
// image. Present is false when the upstream Softwares pointer was nil — a bare OS
// image with no framework metadata. That is honest absence, NOT a fabricated blank:
// a consumer must read Present before trusting any field, exactly as the recon
// requires ("Softwares=nil 不猜"). FrameworkVersionIndex is upstream's framework
// version sort key and is comparable ONLY within the same Framework.
type SoftwareFacts struct {
	Present               bool
	Framework             string
	FrameworkVersion      string
	CUDAVersion           string
	OsVersion             string
	PythonVersion         string
	FrameworkVersionIndex uint32
}

// NewImageCatalogSnapshot freezes the entries into a read-only catalog, mirroring
// NewZoneCatalogSnapshot. When available is false the entries are discarded
// entirely: a fetch failure cannot masquerade as data. When available is true,
// entries with a blank id are dropped, and a later entry for the same id replaces
// the earlier one WHOLE (id/name/software move together).
func NewImageCatalogSnapshot(available bool, entries []ImageCatalogEntry) *ImageCatalogSnapshot {
	s := &ImageCatalogSnapshot{available: available, entries: map[string]ImageCatalogEntry{}}
	if !available {
		return s
	}
	for _, e := range entries {
		id := strings.TrimSpace(e.ID)
		if id == "" {
			continue
		}
		e.ID = id
		e.Name = strings.TrimSpace(e.Name)
		key := strings.ToLower(id)
		if _, seen := s.entries[key]; !seen {
			s.order = append(s.order, id)
		}
		s.entries[key] = e
	}
	return s
}

// Available reports whether the engine obtained the catalog this turn. A nil
// snapshot (no reference data on the context) is unavailable, so callers treat "no
// snapshot" and "fetch failed" identically — both must refuse rather than guess.
func (s *ImageCatalogSnapshot) Available() bool { return s != nil && s.available }

// ByID returns the single catalog row for a CompShareImageId, matched
// case-insensitively. It is the sole id-verification choke point: an unavailable
// snapshot returns nothing here, so no accessor built on it can leak stale data.
// This is the gate invariant 1 rests on — only an id that resolves HERE may be
// sealed as resolved.
func (s *ImageCatalogSnapshot) ByID(id string) (ImageCatalogEntry, bool) {
	if !s.Available() {
		return ImageCatalogEntry{}, false
	}
	e, ok := s.entries[strings.ToLower(strings.TrimSpace(id))]
	if !ok {
		return ImageCatalogEntry{}, false
	}
	return e.clone(), true
}

// clone returns a deep copy of the entry with fresh SupportedGPUTypes and Tags
// slices, so a caller that mutates either cannot reach the snapshot's stored row.
// Every accessor returns through here, so the immutability the snapshot promises
// holds for the two slice fields too, not only the scalars.
func (e ImageCatalogEntry) clone() ImageCatalogEntry {
	if len(e.SupportedGPUTypes) > 0 {
		gpus := make([]string, len(e.SupportedGPUTypes))
		copy(gpus, e.SupportedGPUTypes)
		e.SupportedGPUTypes = gpus
	}
	if len(e.Tags) > 0 {
		tags := make([]string, len(e.Tags))
		copy(tags, e.Tags)
		e.Tags = tags
	}
	return e
}

// Entries returns the catalog rows in catalog order. The returned slice (and each
// row's SupportedGPUTypes and Tags) is a fresh copy; mutating it cannot reach the
// snapshot.
// An unavailable catalog returns nil. This is what the resolver scans to match a
// name or structured preference.
func (s *ImageCatalogSnapshot) Entries() []ImageCatalogEntry {
	if !s.Available() {
		return nil
	}
	out := make([]ImageCatalogEntry, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.entries[strings.ToLower(id)].clone())
	}
	return out
}

// Len reports the number of rows in the catalog (0 when unavailable).
func (s *ImageCatalogSnapshot) Len() int {
	if !s.Available() {
		return 0
	}
	return len(s.order)
}

// ParsePlatformImageEntries reads a DescribeCompShareImages / custom / shared
// response (all use the flat ImageSet shape) into catalog rows tagged with source.
// It captures the structured Softwares block and the Container runtime-form flag
// that the legacy platformImageCandidates path discarded.
func ParsePlatformImageEntries(result map[string]any, source string) []ImageCatalogEntry {
	if result == nil {
		return nil
	}
	set, ok := result["ImageSet"].([]any)
	if !ok {
		return nil
	}
	out := make([]ImageCatalogEntry, 0, len(set))
	for _, item := range set {
		img, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if e, ok := imageEntryFromMap(img, source); ok {
			out = append(out, e)
		}
	}
	return out
}

// ParseCommunityImageEntries reads a DescribeCommunityImages response (grouped:
// CompshareImageGroup[].Data[]) into catalog rows. The group name backfills the
// display name when a version row carries none, and the source is tagged community.
func ParseCommunityImageEntries(result map[string]any) []ImageCatalogEntry {
	if result == nil {
		return nil
	}
	groups, ok := result["CompshareImageGroup"].([]any)
	if !ok {
		// Some community responses use the flat ImageSet shape.
		return ParsePlatformImageEntries(result, "community")
	}
	var out []ImageCatalogEntry
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			continue
		}
		groupName := asString(group, "ImageName")
		data, ok := group["Data"].([]any)
		if !ok {
			continue
		}
		for _, rawData := range data {
			img, ok := rawData.(map[string]any)
			if !ok {
				continue
			}
			e, ok := imageEntryFromMap(img, "community")
			if !ok {
				continue
			}
			// The group (family) name is the recognizable image name a user refers to
			// and the create card shows ("Stable Diffusion WebUI"), not the per-version
			// row name ("SD WebUI v1.9"). Prefer it; fall back to the row name only when
			// the group carries none.
			if groupName != "" {
				e.Name = groupName
			}
			out = append(out, e)
		}
	}
	return out
}

func imageEntryFromMap(img map[string]any, source string) (ImageCatalogEntry, bool) {
	id := asString(img, "CompShareImageId")
	if id == "" {
		id = asString(img, "ImageId")
	}
	if id == "" {
		return ImageCatalogEntry{}, false
	}
	name := asString(img, "Name")
	if name == "" {
		name = asString(img, "CompShareImageName")
	}
	if name == "" {
		name = asString(img, "ImageName")
	}
	e := ImageCatalogEntry{
		ID:                id,
		Name:              name,
		Source:            source,
		ImageType:         asString(img, "ImageType"),
		Status:            asString(img, "Status"),
		Container:         parseContainerFlag(img["Container"]) || asBool(img, "IsContainer"),
		SupportedGPUTypes: asStringSlice(img["SupportedGpuTypes"]),
		Tags:              asStringSlice(img["Tags"]),
		Description:       asString(img, "Description"),
		SizeMB:            asFloat(img, "Size", "ActualSize", "ImageSize"),
		CreateTime:        asInt64(img, "CreateTime"),
		PubTime:           asInt64(img, "PubTime"),
	}
	if sw, ok := img["Softwares"].(map[string]any); ok && sw != nil {
		e.Software = SoftwareFacts{
			Present:               true,
			Framework:             asString(sw, "Framework"),
			FrameworkVersion:      asString(sw, "FrameworkVersion"),
			CUDAVersion:           asString(sw, "CUDAVersion"),
			OsVersion:             asString(sw, "OsVersion"),
			PythonVersion:         asString(sw, "PythonVersion"),
			FrameworkVersionIndex: uint32(asInt64FromAny(sw["FrameworkVersionIndex"])),
		}
	}
	return e, true
}

// parseContainerFlag reads the upstream Container field, which is the string
// "True"/"False" on live responses but may be a bool in fixtures. Only an explicit
// truthy value counts as a container image; anything else (including absent) is
// treated as non-container, so a pod's container requirement fails safe.
func parseContainerFlag(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(strings.TrimSpace(t), "True")
	default:
		return false
	}
}

func asString(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func asBool(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

func asStringSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func asFloat(m map[string]any, keys ...string) float64 {
	for _, key := range keys {
		switch v := m[key].(type) {
		case float64:
			return v
		case int64:
			return float64(v)
		case int:
			return float64(v)
		}
	}
	return 0
}

func asInt64(m map[string]any, key string) int64 {
	return asInt64FromAny(m[key])
}

func asInt64FromAny(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case uint32:
		return int64(n)
	}
	return 0
}
