package actionresolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveCatalog mirrors a real DescribeAvailableCompShareInstanceTypes name set:
// a base card, a memory variant of it, and a suffixed family member (V100S with
// no bare V100 on offer) — the three shapes the matching rules have to tell apart.
func liveCatalog() MachineTypeCatalog {
	return MachineTypeCatalog{
		Names:     []string{"4090", "4090_48G", "A100", "V100S", "H20"},
		Available: true,
	}
}

func TestCanonicalMachineType_ExactAndCaseAndFormat(t *testing.T) {
	catalog := liveCatalog()

	// 1. exact
	m := canonicalMachineType("4090", catalog)
	assert.Equal(t, machineTypeResolved, m.Status)
	assert.Equal(t, "4090", m.Canonical)

	// 2. case-insensitive
	m = canonicalMachineType("v100s", catalog)
	assert.Equal(t, machineTypeResolved, m.Status)
	assert.Equal(t, "V100S", m.Canonical, "canonical form comes from the catalog, not the user's casing")

	// 3. separator formatting — the case that used to need a hardcoded
	//    "4090 48G" -> "4090_48G" alias. Same token, different punctuation.
	for _, raw := range []string{"4090 48G", "4090-48G", "4090_48g", "  4090 48g  "} {
		m = canonicalMachineType(raw, catalog)
		assert.Equalf(t, machineTypeResolved, m.Status, "raw=%q", raw)
		assert.Equalf(t, "4090_48G", m.Canonical, "raw=%q", raw)
	}
}

// TestCanonicalMachineType_SuffixIsNotAnAlias is the anti-regression for the
// deleted gpuTypeAliases table, which hardcoded V100 -> V100S. Whether a bare
// V100 means V100S is a PLATFORM fact this repo does not get to assert, so with
// only V100S on offer a bare "V100" must stay unknown and bounce back to the
// agent to ask — not silently resolve to a different product.
func TestCanonicalMachineType_SuffixIsNotAnAlias(t *testing.T) {
	m := canonicalMachineType("V100", liveCatalog())
	assert.Equal(t, machineTypeUnknown, m.Status)
	assert.Empty(t, m.Canonical)
}

// TestCanonicalMachineType_AmbiguityIsAQuestionNotAGuess: when folding maps one
// input onto several catalog entries the resolver must refuse, not pick one.
func TestCanonicalMachineType_AmbiguityIsAQuestionNotAGuess(t *testing.T) {
	// A catalog that punctuates the same token two ways: both fold to "409048G".
	catalog := MachineTypeCatalog{Names: []string{"4090_48G", "4090-48G"}, Available: true}
	m := canonicalMachineType("4090 48G", catalog)
	require.Equal(t, machineTypeAmbiguous, m.Status)
	assert.ElementsMatch(t, []string{"4090_48G", "4090-48G"}, m.Candidates)
	assert.Empty(t, m.Canonical, "an ambiguous match must not yield a value")
}

func TestCanonicalMachineType_UnknownDoesNotDefault(t *testing.T) {
	// "默认推荐 4090" was RecommendGPUType's last-resort branch. Nothing may
	// resurrect it: an unrecognised card is unknown, never a popular guess.
	for _, raw := range []string{"5090", "totally-made-up", ""} {
		m := canonicalMachineType(raw, liveCatalog())
		assert.Equalf(t, machineTypeUnknown, m.Status, "raw=%q", raw)
		assert.Emptyf(t, m.Canonical, "raw=%q must not fall back to a default card", raw)
	}
}

// TestCanonicalMachineType_CatalogUnavailableIsNotUnknown separates OUR outage
// from the user's mistake. An unreachable catalog must not be reported as "no
// such machine type" (blames the user for our failed query) and must not fall
// back to a local table (the whole point of deleting gpuSpecs).
func TestCanonicalMachineType_CatalogUnavailableIsNotUnknown(t *testing.T) {
	for _, catalog := range []MachineTypeCatalog{
		{Available: false},
		{Names: []string{"4090"}, Available: false},
	} {
		m := canonicalMachineType("4090", catalog)
		assert.Equal(t, machineTypeCatalogUnavailable, m.Status)
		assert.Empty(t, m.Canonical, "an unavailable catalog must never yield a machine type")
	}
}

func TestSpecNeedsMachineTypeCatalog(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)

	create, ok := catalog.Lookup("CreateInstanceWorkflow")
	require.True(t, ok)
	assert.Equal(t, CodecMachineType, create.Fields["GpuType"].Codec,
		"GpuType must route through the live-catalog codec, not plain text")
	assert.True(t, SpecNeedsMachineTypeCatalog(create))

	stop, ok := catalog.Lookup("StopInstanceWorkflow")
	require.True(t, ok)
	assert.False(t, SpecNeedsMachineTypeCatalog(stop),
		"operations with no machine-type field must not trigger the upstream query")
}
