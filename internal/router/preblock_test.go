package router

import "testing"

func TestPreBlock_Decide_Empty(t *testing.T) {
	pb := New()
	d := pb.Decide("anything")
	if d.Matched {
		t.Errorf("empty PreBlock should never match, got %+v", d)
	}
}

func TestPreBlock_Decide_Nil(t *testing.T) {
	var pb *PreBlock
	d := pb.Decide("anything")
	if d.Matched {
		t.Errorf("nil PreBlock should not panic and must not match, got %+v", d)
	}
}

func TestPreBlock_Decide_FirstMatchWins(t *testing.T) {
	// Two overlapping rules; first registered MUST win.
	pb := New(
		Rule{
			Match:    func(s string) bool { return s == "overlap" },
			Category: "first",
			Reply:    "first_reply",
		},
		Rule{
			Match:    func(s string) bool { return s == "overlap" },
			Category: "second",
			Reply:    "second_reply",
		},
	)
	d := pb.Decide("overlap")
	if !d.Matched {
		t.Fatalf("expected match, got %+v", d)
	}
	if d.Category != "first" || d.Reply != "first_reply" {
		t.Errorf("first-match invariant broken: %+v", d)
	}
}

func TestPreBlock_Decide_NoMatch_FallsThrough(t *testing.T) {
	pb := New(
		Rule{Match: func(s string) bool { return s == "x" }, Category: "x_cat", Reply: "x_reply"},
	)
	d := pb.Decide("y")
	if d.Matched {
		t.Errorf("expected pass-through, got %+v", d)
	}
	if d.Category != "" || d.Reply != "" {
		t.Errorf("zero-value Decision expected on miss, got %+v", d)
	}
}

func TestPreBlock_Decide_NilMatchSkipped(t *testing.T) {
	// A Rule with nil Match must be silently skipped, not panic.
	pb := New(
		Rule{Match: nil, Category: "broken", Reply: "broken"},
		Rule{Match: func(s string) bool { return s == "hit" }, Category: "ok", Reply: "ok_reply"},
	)
	d := pb.Decide("hit")
	if !d.Matched || d.Category != "ok" {
		t.Errorf("expected fallthrough past nil-match rule, got %+v", d)
	}
}

func TestPreBlock_Categories_Order(t *testing.T) {
	pb := New(
		Rule{Match: func(string) bool { return false }, Category: "a"},
		Rule{Match: func(string) bool { return false }, Category: "b"},
		Rule{Match: func(string) bool { return false }, Category: "c"},
	)
	got := pb.Categories()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("Categories len=%d; want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Categories[%d] = %q; want %q", i, got[i], want[i])
		}
	}
}
