package workflow

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// draftInternalKeyRef matches the codec's own key constants — the names for the
// draft's stored format.
var draftInternalKeyRef = regexp.MustCompile(`\b(draftKey(Args|Image|Placement)|argsKey[A-Z]\w*|imageKey[A-Z]\w*|placementKey[A-Z]\w*)\b`)

// TestOnlyTheCodecNamesTheDraftStorageFormat is the boundary that keeps a typed
// domain object from decaying into "two representations, separately maintained".
//
// CreateExecutionDraft is the business object; ToContractMap / ParseCreateExecutionDraft
// are the only encoder and decoder. If stock, price, the card or the create ever
// reach into the stored map by key again, the type becomes decoration: the map is
// the real contract and every reader re-interprets it, which is exactly the defect
// class this convergence removed from the image and spec paths.
//
// Scope, stated honestly: this catches the realistic regression — a future caller
// reusing the codec's key constants, which are right there and unexported. It does
// NOT catch someone hand-writing the raw literal "args", because that string is
// legitimately a card field name elsewhere in this package, so scanning literals
// would fail on correct code. A raw-literal bypass is visible in review; reusing a
// constant is not.
func TestOnlyTheCodecNamesTheDraftStorageFormat(t *testing.T) {
	const codecFile = "create_draft.go"

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// Tests may reach through the format on purpose (tamper/aliasing probes).
		if strings.HasSuffix(name, "_test.go") || name == codecFile {
			continue
		}
		body, err := os.ReadFile(filepath.Clean(name))
		require.NoError(t, err)
		scanned++

		for i, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if m := draftInternalKeyRef.FindString(line); m != "" {
				t.Errorf("%s:%d names the draft's storage format (%s) outside %s:\n\t%s\n"+
					"Read the draft through candidateCreateDraft / ParseCreateExecutionDraft and use the typed fields.",
					name, i+1, m, codecFile, strings.TrimSpace(line))
			}
		}
	}
	require.Greater(t, scanned, 5, "the scan must actually have read this package's sources")
}

// TestTheCodecIsReachableOnlyThroughItsTwoDoors pins the other half: production
// code obtains a draft through candidateCreateDraft (candidate) or by parsing the
// sealed copy, and encodes through ToContractMap. If a second encoder appears,
// the round-trip test stops being a contract for the whole package.
func TestTheCodecIsReachableOnlyThroughItsTwoDoors(t *testing.T) {
	body, err := os.ReadFile(filepath.Clean("create_instance.go"))
	require.NoError(t, err)
	src := string(body)

	// Six crossings, one per boundary function, each structural:
	//
	//   encode: materializeCreateDraft        → the draft step's stored result
	//           materializeCreateConfirmation → the confirmation step's stored result
	//           promoteCreateDraft            → Params, once the gate passes
	//   decode: candidateCreateDraft          → the candidate draft (stock, price)
	//           candidateCreateConfirmation   → the candidate snapshot (card, promote)
	//           createArgsFromSealedDraft     → the sealed snapshot (the create)
	//
	// One more means some other site decided how a draft is stored or read, which
	// is the moment the map becomes the real contract again.
	require.Equal(t, 3, strings.Count(src, ".ToContractMap()"),
		"exactly three encoders belong here: the two resolve steps' results and promoteCreateDraft (into Params)")
	require.Equal(t, 1, strings.Count(src, "ParseCreateExecutionDraft("),
		"the draft is decoded once, in candidateCreateDraft; everyone else asks it")
	require.Equal(t, 2, strings.Count(src, "ParseCreateConfirmationSnapshot("),
		"the snapshot is decoded twice: candidateCreateConfirmation (StepResults) and createArgsFromSealedDraft (the sealed copy)")
}
