package observability

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTraceSessionObserved_ZeroIsOmitted is the SHA-stability guard: a zero
// SessionTrace must not be observed, so every record that never went through the
// HTTP session layer (CLI turns, raw fixtures) marshals without a "session" block
// — byte-identical to before this block existed.
func TestTraceSessionObserved_ZeroIsOmitted(t *testing.T) {
	if traceSessionObserved(SessionTrace{}) {
		t.Fatal("a zero SessionTrace must not be observed (would break SHA stability)")
	}
	// Swapped alone MUST be observed. A swap is the single most consequential fact
	// in the block — the turn ran on a session the user cannot see — so it can
	// never be dropped for want of a second populated field.
	if !traceSessionObserved(SessionTrace{Swapped: true}) {
		t.Fatal("swapped=true must be observed or a silent session swap goes unrecorded")
	}
	// Each remaining field independently makes the block real, so a producer that
	// records only one of them cannot have it silently discarded.
	for name, trace := range map[string]SessionTrace{
		"requested_hash":   {RequestedSessionIDHash: "sha256:aaa"},
		"session_hash":     {SessionIDHash: "sha256:bbb"},
		"swap_reason":      {SwapReason: SessionSwapNotFound},
		"turn_index":       {TurnIndexInSession: 1},
		"max_turns":        {MaxSessionTurns: 10},
		"rehydrate_source": {RehydrateSource: RehydrateSourceCold},
		"rehydrated_count": {RehydratedMessageCount: 4},
		"version_in":       {ContextVersionIn: 3},
		"version_out":      {ContextVersionOut: 4},
		"cas_conflict":     {CASConflict: true},
		"state_save":       {StateSaveFailed: true},
	} {
		if !traceSessionObserved(trace) {
			t.Fatalf("%s alone must mark the session block observed", name)
		}
	}
}

// TestTraceRecord_SessionMarshaling pins the two properties the block's whole
// attribution value rests on:
//
//  1. an unobserved block emits no "session" key at all (absent = not recorded);
//  2. an observed block ALWAYS carries "swapped", including when false — false and
//     "not written" must be distinguishable or the denominator is lost and the swap
//     rate cannot be computed.
func TestTraceRecord_SessionMarshaling(t *testing.T) {
	clean, err := json.Marshal(TraceRecord{SchemaVersion: SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(clean), `"session"`) {
		t.Fatalf("clean record must not emit a session block: %s", clean)
	}

	// A normal continuing turn: no swap, but the block is present because the turn
	// recorded its continuity facts.
	rec := TraceRecord{SchemaVersion: SchemaVersion}
	rec.Session = SessionTrace{
		RequestedSessionIDHash: "sha256:aaa",
		SessionIDHash:          "sha256:aaa",
		Swapped:                false,
		TurnIndexInSession:     3,
		MaxSessionTurns:        10,
		RehydrateSource:        RehydrateSourceHot,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"swapped":false`) {
		t.Fatalf("swapped:false must serialize (the denominator): %s", s)
	}
	if !strings.Contains(s, `"rehydrate_source":"hot"`) {
		t.Fatalf("expected rehydrate_source in: %s", s)
	}
	// Genuinely optional fields stay omitted — never defaulted. Absent means "this
	// did not happen": no swap, no CAS collision, no failed save.
	for _, absent := range []string{"swap_reason", "cas_conflict", "state_save_failed"} {
		if strings.Contains(s, absent) {
			t.Fatalf("field %s was not observed and must be omitted, not defaulted: %s", absent, s)
		}
	}
	// The two version fields are the exception, and they are deliberately NOT
	// omitempty. 0 is a LEGAL value on both — a brand-new session reads version 0,
	// and a turn whose save failed wrote version 0 — so omitting a zero would make
	// "turn 1 of a fresh session" indistinguishable from "version not observed",
	// which is the precise ambiguity this whole block exists to remove. They must
	// serialize whenever the block does.
	for _, present := range []string{`"context_version_in":`, `"context_version_out":`} {
		if !strings.Contains(s, present) {
			t.Fatalf("%s must serialize even at 0 — absent would read as 'not observed': %s", present, s)
		}
	}
}

// TestTraceRecord_SwappedTurnCarriesBothHashes pins the redaction contract: a swap
// records the two ids as HASHES (a session id is a customer identifier and must
// never enter a trace in the clear) and they differ, because the turn ran on a
// session the client never asked for.
func TestTraceRecord_SwappedTurnCarriesBothHashes(t *testing.T) {
	requested, err := HashTracePayload("synthetic-requested-session")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := HashTracePayload("synthetic-replacement-session")
	if err != nil {
		t.Fatal(err)
	}
	rec := TraceRecord{SchemaVersion: SchemaVersion}
	rec.Session = SessionTrace{
		RequestedSessionIDHash: requested,
		SessionIDHash:          actual,
		Swapped:                true,
		SwapReason:             SessionSwapNotFound,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"swapped":true`) || !strings.Contains(s, `"swap_reason":"not_found"`) {
		t.Fatalf("swap must be recorded with its reason: %s", s)
	}
	if requested == actual {
		t.Fatal("a swap must produce two different hashes")
	}
	if strings.Contains(s, "synthetic-requested-session") || strings.Contains(s, "synthetic-replacement-session") {
		t.Fatalf("raw session ids must never reach the trace: %s", s)
	}
	// absent is a normal first turn, NOT a defect — it must stay distinguishable
	// from not_found, which is a real silent swap.
	if SessionSwapAbsent == SessionSwapNotFound {
		t.Fatal("absent (no id sent) and not_found (id unknown) must remain distinct")
	}
}

// TestExistingFixturesGainNoSessionBlock is the fixture guard the task named: a
// pre-session trace re-marshals without a session key, so archived records and
// the golden fixtures stay byte-identical. If this fails, the GATING is wrong —
// do not touch the fixture.
func TestExistingFixturesGainNoSessionBlock(t *testing.T) {
	for _, name := range []string{"trace_v0_1_minimal.json", "trace_v0_2_cap_fields.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		var record TraceRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatalf("unmarshal %s: %v", name, err)
		}
		if traceSessionObserved(record.Session) {
			t.Fatalf("%s must not observe a session block", name)
		}
		out, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		if strings.Contains(string(out), `"session"`) {
			t.Fatalf("%s must re-marshal without a session key: %s", name, out)
		}
	}
}
