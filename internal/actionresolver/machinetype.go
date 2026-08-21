package actionresolver

import (
	"fmt"
	"strings"
)

// MachineTypeCatalog is the live machine-type name set, snapshotted by the engine
// and handed to the resolver as pure data.
//
// The resolver deliberately CANNOT fetch it. A deterministic adjudicator that
// owns network calls, caching and failure policy stops being replayable and
// becomes a second workflow — so the engine queries
// DescribeAvailableCompShareInstanceTypes once per proposal and passes the result
// in.
//
// Available=false means the engine could not obtain the catalog this turn. That
// is NOT the same as an empty catalog, and it must never degrade to a built-in
// table: the platform's machine-type list is a platform fact, and a stale local
// copy of a platform fact is precisely what this package stopped keeping.
type MachineTypeCatalog struct {
	Names     []string
	Available bool
}

type machineTypeStatus int

const (
	machineTypeResolved machineTypeStatus = iota
	machineTypeAmbiguous
	machineTypeUnknown
	machineTypeCatalogUnavailable
)

type machineTypeMatch struct {
	Canonical  string
	Candidates []string
	Status     machineTypeStatus
}

// canonicalMachineType maps an agent-supplied machine-type name onto the live
// catalog. It normalizes FORMAT ONLY — case, spaces, hyphens, underscores — and
// keeps NO alias table.
//
// The distinction is the whole point. "4090 48G" vs the catalog's "4090_48G" is
// the same token punctuated differently, so folding resolves it. "V100" vs a
// catalog holding only "V100S" is a different product name and remains unknown;
// this package never carries a platform alias table.
func canonicalMachineType(raw string, catalog MachineTypeCatalog) machineTypeMatch {
	value := strings.TrimSpace(raw)
	if !catalog.Available {
		return machineTypeMatch{Status: machineTypeCatalogUnavailable}
	}
	if value == "" {
		return machineTypeMatch{Status: machineTypeUnknown}
	}
	if hits := matchCatalog(value, catalog.Names, func(a, b string) bool { return a == b }); len(hits) == 1 {
		return machineTypeMatch{Canonical: hits[0], Status: machineTypeResolved}
	}
	hits := matchCatalog(value, catalog.Names, strings.EqualFold)
	if len(hits) == 0 {
		hits = matchCatalog(value, catalog.Names, func(a, b string) bool {
			return foldMachineType(a) == foldMachineType(b)
		})
	}
	switch len(hits) {
	case 1:
		return machineTypeMatch{Canonical: hits[0], Status: machineTypeResolved}
	case 0:
		return machineTypeMatch{Status: machineTypeUnknown}
	default:
		return machineTypeMatch{Candidates: hits, Status: machineTypeAmbiguous}
	}
}

// canonicalMachineTypeValue adapts canonicalMachineType to the codec pipeline,
// mapping each status onto the refusal channel that tells the truth about WHO
// failed: an unavailable catalog is our outage (dependencyError), several
// matches is a question for the user (ambiguityError), and no match is an
// invalid value (a plain error → Rejected). None of them fall back to a guess.
func (r *Resolver) canonicalMachineTypeValue(text string) (any, error) {
	match := canonicalMachineType(text, r.machineTypes)
	switch match.Status {
	case machineTypeResolved:
		return match.Canonical, nil
	case machineTypeCatalogUnavailable:
		return nil, dependencyError{detail: "机型目录当前不可用，无法确认该机型名称"}
	case machineTypeAmbiguous:
		return nil, ambiguityError{
			detail:     "该名称匹配到多个在售机型，请确认具体机型",
			candidates: match.Candidates,
		}
	default:
		return nil, fmt.Errorf("机型 %q 不在当前机型目录中", text)
	}
}

// matchCatalog returns the deduped catalog names equal to value under equal.
func matchCatalog(value string, names []string, equal func(a, b string) bool) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, name := range names {
		if name == "" || !equal(name, value) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// foldMachineType strips only the separators the platform and its users disagree
// about, then case-folds. It never removes alphanumerics, so V100 and V100S can
// not fold together — a suffix is a different product, not a formatting choice.
func foldMachineType(s string) string {
	return strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "_", "").Replace(strings.TrimSpace(s)))
}

// SpecNeedsMachineTypeCatalog reports whether resolving this operation requires a
// live machine-type snapshot. The engine uses it to skip the upstream query for
// operations that carry no machine-type field.
func SpecNeedsMachineTypeCatalog(spec OperationSpec) bool {
	for _, field := range spec.Fields {
		if field.Codec == CodecMachineType {
			return true
		}
	}
	return false
}
