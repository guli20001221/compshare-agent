package observability

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBucketFactCacheAge pins the bucket boundaries and the negative→"" omission
// (the only way the field is dropped from the trace).
func TestBucketFactCacheAge(t *testing.T) {
	cases := []struct {
		seconds int
		want    string
	}{
		{-1, ""},
		{0, "le_60s"},
		{60, "le_60s"},
		{61, "le_180s"},
		{180, "le_180s"},
		{181, "le_300s"},
		{300, "le_300s"},
	}
	for _, c := range cases {
		if got := BucketFactCacheAge(c.seconds); got != c.want {
			t.Fatalf("BucketFactCacheAge(%d) = %q, want %q", c.seconds, got, c.want)
		}
	}
}

// TestTraceStateObserved_ZeroIsOmitted is the SHA-stability guard: a zero
// StateTrace must not be observed, so a record that never ran the engine
// marshals without a "state" block (byte-identical to pre-1b).
func TestTraceStateObserved_ZeroIsOmitted(t *testing.T) {
	if traceStateObserved(StateTrace{}) {
		t.Fatal("a zero StateTrace must not be observed (would break SHA stability)")
	}
	// A finalized engine turn always sets ResolutionSource (≥ "unresolved"), so it
	// MUST be observed/emitted.
	if !traceStateObserved(StateTrace{ResolutionSource: ResolutionSourceUnresolved}) {
		t.Fatal("a turn with resolution_source must be observed so the state block is emitted")
	}
	// session_state_hydrated=false alone (no other field) is NOT observed — false
	// is the zero value and would be ambiguous with "field absent"; the block is
	// only present once ResolutionSource (or another field) is set.
	if traceStateObserved(StateTrace{SessionStateHydrated: false}) {
		t.Fatal("hydrated=false alone must not force the state block")
	}
}

// TestTraceRecord_StateMarshaling verifies the state block appears only when
// observed, and that within an observed block session_state_hydrated serializes
// even when false (the denominator must never silently vanish).
func TestTraceRecord_StateMarshaling(t *testing.T) {
	// No State → no "state" key.
	clean, err := json.Marshal(TraceRecord{SchemaVersion: SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(clean), `"state"`) {
		t.Fatalf("clean record must not emit a state block: %s", clean)
	}

	// hydrated=false but resolution_source set → block present, hydrated:false shown.
	rec := TraceRecord{SchemaVersion: SchemaVersion}
	rec.State = StateTrace{
		SessionStateHydrated: false,
		ResolutionSource:     ResolutionSourceUnresolved,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"resolution_source":"unresolved"`) {
		t.Fatalf("expected resolution_source in: %s", s)
	}
	if !strings.Contains(s, `"session_state_hydrated":false`) {
		t.Fatalf("session_state_hydrated:false must serialize (the denominator): %s", s)
	}
	// Empty optional fields stay omitted.
	if strings.Contains(s, "selected_instance_id") || strings.Contains(s, "fact_cache_oldest_age_bucket") {
		t.Fatalf("empty optional state fields must be omitted: %s", s)
	}
}
