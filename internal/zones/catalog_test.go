package zones

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fakeExec returns a canned DescribeCompShareSupportZone response, recording the
// args it was called with and how many times.
type fakeExec struct {
	resp     map[string]any
	err      error
	calls    int
	lastArgs map[string]any
}

func (f *fakeExec) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	f.calls++
	f.lastArgs = args
	if action != "DescribeCompShareSupportZone" {
		return nil, errors.New("unexpected action " + action)
	}
	return f.resp, f.err
}

func liveZonesResp() map[string]any {
	return map[string]any{
		"ZoneInfo": []any{
			map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
			map[string]any{"Zone": "cn-sh2-02", "Region": "cn-sh2", "RegionId": float64(3002), "ZoneId": float64(8200), "Describe": "上海二B"},
			map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true},
		},
	}
}

func liveZones() []ZoneInfo {
	z, _ := FetchSupportZones(context.Background(), &fakeExec{resp: liveZonesResp()}, 1, 2)
	return z
}

func TestFetchSupportZones_ParsesAndForwardsTenant(t *testing.T) {
	f := &fakeExec{resp: liveZonesResp()}
	got, err := FetchSupportZones(context.Background(), f, 66391350, 64404856)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 zones, got %d", len(got))
	}
	if got[2].Zone != "cn-bj2-03" || got[2].Describe != "华北一C" || got[2].ZoneID != 5001 || got[2].RegionID != 3003 {
		t.Fatalf("bj2-03 parsed wrong: %+v", got[2])
	}
	if got[0].IsPod || !got[2].IsPod {
		t.Fatalf("IsPod parsed wrong: got[0]=%v got[2]=%v", got[0].IsPod, got[2].IsPod)
	}
	// organization_id MUST be forwarded — the action 230s without it.
	if f.lastArgs["organization_id"] != uint32(64404856) || f.lastArgs["top_organization_id"] != uint32(66391350) {
		t.Fatalf("tenant identity not forwarded: %+v", f.lastArgs)
	}
}

func TestCatalog_CachesWithinTTLAndRefetchesAfter(t *testing.T) {
	f := &fakeExec{resp: liveZonesResp()}
	clock := time.Unix(1000, 0)
	c := NewCatalog(5 * time.Minute)
	c.now = func() time.Time { return clock }

	if _, err := c.Get(context.Background(), f, 1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), f, 1, 2); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatalf("expected 1 fetch within TTL, got %d", f.calls)
	}
	clock = clock.Add(6 * time.Minute)
	if _, err := c.Get(context.Background(), f, 1, 2); err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 {
		t.Fatalf("expected refetch after TTL, got %d calls", f.calls)
	}
}

func TestCatalog_ServesStaleOnRefreshError(t *testing.T) {
	f := &fakeExec{resp: liveZonesResp()}
	clock := time.Unix(1000, 0)
	c := NewCatalog(time.Minute)
	c.now = func() time.Time { return clock }
	if _, err := c.Get(context.Background(), f, 1, 2); err != nil {
		t.Fatal(err)
	}
	// Make the next refresh fail; advance past TTL.
	f.err = errors.New("boom")
	clock = clock.Add(2 * time.Minute)
	got, err := c.Get(context.Background(), f, 1, 2)
	if err != nil {
		t.Fatalf("should serve stale, got err: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("stale cache not served: %d", len(got))
	}
}

func TestExactZone(t *testing.T) {
	list := liveZones()
	cases := []struct {
		msg, want string
		ok        bool
	}{
		{"部署一台华北一C的4090", "cn-bj2-03", true},
		{"华北一 C 的实例", "cn-bj2-03", true}, // space-insensitive on describe
		{"用 cn-sh2-02 创建", "cn-sh2-02", true},
		{"华北二A 有货吗", "cn-wlcb-01", true},
		{"随便给我来一台4090", "", false}, // no zone named
		{"华北一区的4090", "", false},   // partial — not an exact literal, defer to LLM
	}
	for _, c := range cases {
		got, ok := ExactZone(list, c.msg)
		if got != c.want || ok != c.ok {
			t.Errorf("ExactZone(%q) = (%q,%v), want (%q,%v)", c.msg, got, ok, c.want, c.ok)
		}
	}
}

func TestMentions(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"华北一区的4090", true},  // region-word token
		{"用cn-bj2-03", true}, // cn- token
		{"上海二B 机房", true},    // 机房 token
		{"XX可用区创建一台", true},  // 可用区 token
		// A non-existent zone phrased with a region word is still detected, so it
		// reaches the matcher and gets challenged (the matcher names what IS supported).
		{"创建华北十区的4090", true},
		{"部署一个qwen 32b", false}, // no zone reference at all
	}
	for _, c := range cases {
		if got := Mentions(c.msg); got != c.want {
			t.Errorf("Mentions(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

func TestDescribeForAndLabel(t *testing.T) {
	list := liveZones()
	if d := DescribeFor(list, "cn-bj2-03"); d != "华北一C" {
		t.Errorf("DescribeFor = %q", d)
	}
	if l := Label(list, "cn-bj2-03"); l != "华北一C (cn-bj2-03)" {
		t.Errorf("Label = %q", l)
	}
	if l := Label(list, "cn-unknown-99"); l != "cn-unknown-99" {
		t.Errorf("unknown zone Label should be bare id, got %q", l)
	}
}

func TestParseDecision(t *testing.T) {
	list := liveZones()
	pj := func(s string, v any) error { return json.Unmarshal([]byte(s), v) }

	if d := ParseDecision(`{"decision":"exact","zone":"cn-bj2-03"}`, list, pj); d.Kind != "exact" || d.Zone != "cn-bj2-03" {
		t.Errorf("exact: %+v", d)
	}
	// Hallucinated zone not in list → downgraded to none (never reaches saga).
	if d := ParseDecision(`{"decision":"exact","zone":"cn-gd-99"}`, list, pj); d.Kind != "none" {
		t.Errorf("hallucination should be none: %+v", d)
	}
	if d := ParseDecision(`{"decision":"clarify","clarify":"您是指 华北一C 吗？"}`, list, pj); d.Kind != "clarify" || d.Clarify == "" {
		t.Errorf("clarify: %+v", d)
	}
	if d := ParseDecision(`{"decision":"none"}`, list, pj); d.Kind != "none" {
		t.Errorf("none: %+v", d)
	}
	// clarify with empty text degrades to none (nothing to show).
	if d := ParseDecision(`{"decision":"clarify","clarify":""}`, list, pj); d.Kind != "none" {
		t.Errorf("empty clarify: %+v", d)
	}
	if d := ParseDecision(`garbage`, list, pj); d.Kind != "none" {
		t.Errorf("garbage: %+v", d)
	}
}

func TestMatchSystemPrompt_ListsLiveZones(t *testing.T) {
	p := MatchSystemPrompt(liveZones())
	for _, want := range []string{"华北一C", "cn-bj2-03", "华北二A", "上海二B"} {
		if !contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
