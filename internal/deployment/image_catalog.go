package deployment

import "strings"

// ImageCatalogSnapshot is the live image catalog, snapshotted by the engine once
// per turn and handed to BOTH the action resolver and the workflow as pure,
// read-only reference data — the exact shape the zone convergence introduced for
// availability zones.
//
// It is the single authority for "which images exist and what they are".
//
// Available()==false means the engine could not obtain the catalog this turn. That
// is NOT an empty catalog: an unavailable snapshot holds no entries AND refuses
// every read, so a consumer that forgets to check Available() cannot silently fall
// back to a guess. A reader must fail (catalog_unavailable) rather than degrade.
//
// It keeps ONE row per image id (id + display name + runtime form + structured
// software facts together), so the name a card shows can never drift from the id
// that executes. It is immutable from the outside:
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
	ID   string
	Name string
	// FamilyID is the source-local identifier of the image family. Community
	// catalogs expose it as CompshareImageGroup.GroupId. Sources without a family
	// relationship deliberately leave it empty; they are represented as singleton
	// families keyed by their concrete image id.
	FamilyID string
	// FamilyName is the community group (series) name — the recognizable name a user
	// refers to ("InfiniteTalk"). "" for platform rows, which carry no group.
	FamilyName string
	// VersionName is the per-version label upstream carries on a community version row
	// ("v26.0201"); "" = honest absence, never a fabricated one. It exists because Name
	// is NOT unique within a family: ParseCommunityImageEntries overwrites every version
	// row's Name with the family name, so a picker labelled by Name alone renders N
	// identical choices and the user selects a version blind.
	VersionName       string
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
	// Zone/ZoneID identify where a custom image currently resides. They are
	// catalog facts, not inferred from its name, and let a clone workflow reject
	// synchronising an image back into its own zone before confirmation.
	Zone   string
	ZoneID uint32
}

// DisplayLabel is the label a picker/card must show for this row: the name plus the
// version when one is known ("InfiniteTalk · v26.0201"). Every version row of a
// community family shares one Name, so a caller that labels by Name alone offers the
// user N indistinguishable choices — selecting a version becomes a blind guess. When
// no version is known the label degrades to the plain name (honest, not fabricated).
func (e ImageCatalogEntry) DisplayLabel() string {
	name := strings.TrimSpace(e.Name)
	version := strings.TrimSpace(e.VersionName)
	if version == "" || strings.EqualFold(name, version) {
		return name
	}
	if name == "" {
		return version
	}
	return name + " · " + version
}

// FamilyKey is the stable, source-scoped key used to keep a family selection
// distinct from a concrete image-version selection. A source that does not publish
// a family relationship intentionally gets one singleton family per image id.
func (e ImageCatalogEntry) FamilyKey() string {
	source := strings.ToLower(strings.TrimSpace(e.Source))
	if source == "" {
		source = "unknown"
	}
	if id := strings.TrimSpace(e.FamilyID); id != "" {
		return source + ":family:" + strings.ToLower(id)
	}
	if name := strings.TrimSpace(e.FamilyName); name != "" {
		return source + ":family-name:" + strings.ToLower(name)
	}
	return source + ":image:" + strings.ToLower(strings.TrimSpace(e.ID))
}

// FamilyLabel is the user-facing name of the image family. It deliberately omits
// a version: a version is a later, concrete choice made from the family.
func (e ImageCatalogEntry) FamilyLabel() string {
	if name := strings.TrimSpace(e.FamilyName); name != "" {
		return name
	}
	return strings.TrimSpace(e.Name)
}

// ImageFamily is a source-provided image family and the concrete versions that
// remain viable for the current request. It gives callers a common hierarchy for
// grouped community responses and flat platform responses alike.
type ImageFamily struct {
	Key      string
	Name     string
	Source   string
	Variants []ImageCatalogEntry
}

// GroupImageFamilies preserves the incoming candidate order while grouping concrete
// image rows into their user-facing families. It never guesses that two flat rows
// belong together: without an upstream family identity, each image remains a
// singleton family.
func GroupImageFamilies(entries []ImageCatalogEntry) []ImageFamily {
	indexByKey := map[string]int{}
	seenVariant := map[string]bool{}
	var out []ImageFamily
	for _, entry := range entries {
		if strings.TrimSpace(entry.ID) == "" {
			continue
		}
		key := entry.FamilyKey()
		// Concrete ids are only unique inside their source. Keeping the source in
		// the dedupe key makes this helper safe for callers that compare catalogs
		// from more than one source in the future.
		variantKey := strings.ToLower(strings.TrimSpace(entry.Source)) + ":" + strings.ToLower(strings.TrimSpace(entry.ID))
		if seenVariant[variantKey] {
			continue
		}
		seenVariant[variantKey] = true

		idx, ok := indexByKey[key]
		if !ok {
			idx = len(out)
			indexByKey[key] = idx
			out = append(out, ImageFamily{
				Key:    key,
				Name:   entry.FamilyLabel(),
				Source: entry.Source,
			})
		}
		out[idx].Variants = append(out[idx].Variants, entry)
	}
	return out
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
		groupID := asString(group, "GroupId")
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
			//
			// Overwriting Name erases the ONLY thing that distinguished two rows of the
			// same family, so capture the version identity first: upstream's VersionName
			// if it sent one, else the row's own name when it actually differs from the
			// family name. Without this the picker shows N identical labels.
			if groupID != "" {
				e.FamilyID = groupID
			}
			if groupName != "" {
				e.FamilyName = groupName
				if e.FamilyID == "" {
					// Some responses omit GroupId. The group name is still the only
					// upstream family identity available, so retain it rather than
					// flattening its versions into unrelated singletons.
					e.FamilyID = groupName
				}
				if e.VersionName == "" && e.Name != "" && !strings.EqualFold(e.Name, groupName) {
					e.VersionName = e.Name
				}
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
	// The per-version label upstream sends on a community version row. Same key order
	// the read capability already uses (communityVersionLabel), minus the "Name"
	// fallback: Name is handled separately below so an absent version stays absent
	// rather than silently echoing the family name.
	version := asString(img, "VersionName")
	if version == "" {
		version = asString(img, "Version")
	}
	e := ImageCatalogEntry{
		ID:                id,
		Name:              name,
		VersionName:       version,
		Source:            source,
		ImageType:         asString(img, "ImageType"),
		Status:            asString(img, "Status"),
		Container:         parseContainerFlag(img["Container"]) || asBool(img, "IsContainer"),
		SupportedGPUTypes: asStringSlice(img["SupportedGpuTypes"]),
		Tags:              normalizeImageTags(asStringSlice(img["Tags"])),
		Description:       asString(img, "Description"),
		SizeMB:            asFloat(img, "Size", "ActualSize", "ImageSize"),
		CreateTime:        asInt64(img, "CreateTime"),
		PubTime:           asInt64(img, "PubTime"),
		Zone:              asString(img, "Zone"),
	}
	if zones, ok := img["ImageSupportZone"].([]any); ok {
		for _, rawZone := range zones {
			zone, ok := rawZone.(map[string]any)
			if !ok {
				continue
			}
			if e.Zone == "" {
				e.Zone = asString(zone, "Zone")
			}
			e.ZoneID = uint32(asInt64FromAny(zone["ZoneId"]))
			if e.ZoneID != 0 || e.Zone != "" {
				break
			}
		}
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

// normalizeImageTags splits compound upstream tag strings so each consumer sees
// one concept per tag.
//
// This is a split, never a rename: no tag text is invented, translated or mapped
// through a keyword table. Spellings that differ only by case collapse (first one
// wins) because the facet builder's own dedup is already case-insensitive, so a
// second spelling could never have produced a distinct option anyway.
//
// Returns nil for an empty result, preserving the Tags contract: nil means "we
// don't know this image's tags", never "matches no tag".
func normalizeImageTags(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	splitTag := func(r rune) bool { return r == '，' || r == ',' }
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, t := range raw {
		for _, part := range strings.FieldsFunc(t, splitTag) {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			k := strings.ToLower(p)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
