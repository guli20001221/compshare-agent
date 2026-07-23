package intent

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSlotsIsFrozen pins intent.Slots to its exact field set. Slots is a
// migration-era compatibility carrier that must NOT grow: every new structured
// signal belongs on a typed Capability request (internal/capability/read_*.go),
// where it gets its own schema/validation/renderer contract — not as a new field
// on the general slot bag. This is the "intent.Slots 停止扩张" invariant (P6). A
// new field here fails loudly so the growth is a conscious, reviewed decision,
// not another patch on the shared bag; if growth is genuinely required, update
// this frozen set in the same change.
func TestSlotsIsFrozen(t *testing.T) {
	frozen := []string{
		"target_refs", "metrics", "time_window", "image_source", "search_query",
		"list_mode", "price_kind", "gpu_count", "cfs_kind", "size_gb", "zone",
		"charge_type", "detail_level", "action",
	}
	sort.Strings(frozen)

	typ := reflect.TypeOf(Slots{})
	got := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		got = append(got, name)
	}
	sort.Strings(got)

	require.Equal(t, frozen, got,
		"intent.Slots is frozen — a new structured signal belongs on a typed Capability request, not a new Slots field (P6: intent.Slots 停止扩张)")
}
