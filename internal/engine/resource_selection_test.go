package engine

import (
	"strconv"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/intent"
)

func TestResourceSelectionPromptRendersCandidateDetails(t *testing.T) {
	p := testPendingResourceSelection([]entity.InstanceSnapshot{
		testInstance("uhost-a", "qa-shadow-4090", "Running"),
		testInstance("uhost-b", "batch-host", "Stopped"),
	})

	got := renderResourceSelectionPrompt(p)

	wantParts := []string{
		"1.",
		"2.",
		"uhost-a",
		"uhost-b",
		"qa-shadow-4090",
		"batch-host",
		"Running",
		"Stopped",
		"GPU=RTX4090 x1",
		"CPU=16",
		"\u5185\u5b58=65536 MB",
		"cn-wlcb-01",
		"charge=Dynamic",
		"1-2",
		"ID",
		"\u5b8c\u6574\u5b9e\u4f8b\u540d\u79f0",
	}
	for _, part := range wantParts {
		if !strings.Contains(got, part) {
			t.Fatalf("prompt missing %q:\n%s", part, got)
		}
	}
}

func TestResourceSelectionPromptRendersDuplicateNamesSeparately(t *testing.T) {
	p := testPendingResourceSelection([]entity.InstanceSnapshot{
		testInstance("uhost-dup-a", "same-name", "Running"),
		testInstance("uhost-dup-b", "same-name", "Running"),
	})

	got := renderResourceSelectionPrompt(p)

	if strings.Count(got, "same-name") != 2 {
		t.Fatalf("duplicate names should render both candidates, got:\n%s", got)
	}
	if !strings.Contains(got, "uhost-dup-a") || !strings.Contains(got, "uhost-dup-b") {
		t.Fatalf("duplicate names should include both IDs, got:\n%s", got)
	}
}

func TestResourceSelectionPromptSanitizesCandidateFields(t *testing.T) {
	p := testPendingResourceSelection([]entity.InstanceSnapshot{
		testInstance("uhost-bad", "bad\n2. fake", "Running"),
		testInstance("uhost-good", "good", "Running"),
	})

	got := renderResourceSelectionPrompt(p)

	if strings.Contains(got, "\n2. fake") {
		t.Fatalf("prompt should not allow candidate field to inject fake ordinal line:\n%s", got)
	}
	if !strings.Contains(got, "1. bad 2. fake (uhost-bad)") {
		t.Fatalf("prompt should keep sanitized name on the candidate line, got:\n%s", got)
	}
	if strings.Count(got, "\n2.") != 1 {
		t.Fatalf("prompt should contain only the real second candidate line, got:\n%s", got)
	}
	if strings.Contains(got, "1/2/3") {
		t.Fatalf("prompt should not suggest only 1/2/3 when candidates may have a wider range, got:\n%s", got)
	}
}

func TestResourceSelectionMatchResolvesOrdinalsIDAndName(t *testing.T) {
	p := testPendingResourceSelection([]entity.InstanceSnapshot{
		testInstance("uhost-first", "first-host", "Running"),
		testInstance("uhost-second", "second-host", "Running"),
	})

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "number", in: "1", want: "uhost-first"},
		{name: "chinese first", in: "\u7b2c\u4e00\u53f0", want: "uhost-first"},
		{name: "chinese second phrase", in: "\u9009\u7b2c\u4e8c\u53f0", want: "uhost-second"},
		{name: "exact id", in: "uhost-second", want: "uhost-second"},
		{name: "exact name", in: "first-host", want: "uhost-first"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchResourceSelection(tt.in, p)
			if !got.ok || got.ambiguous {
				t.Fatalf("matchResourceSelection(%q) = ok %v ambiguous %v, want ok true ambiguous false", tt.in, got.ok, got.ambiguous)
			}
			if got.instance.UHostId != tt.want {
				t.Fatalf("matchResourceSelection(%q) resolved %q, want %q", tt.in, got.instance.UHostId, tt.want)
			}
		})
	}
}

func TestResourceSelectionMatchPrefersExactIDAndNameBeforeOrdinal(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		candidates []entity.InstanceSnapshot
		want       string
		ambiguous  bool
	}{
		{
			name:  "single chinese numeral name",
			input: "\u4e00",
			candidates: []entity.InstanceSnapshot{
				testInstance("uhost-first", "first-host", "Running"),
				testInstance("uhost-name-one", "\u4e00", "Running"),
			},
			want: "uhost-name-one",
		},
		{
			name:  "ordinal shaped name",
			input: "\u7b2c2\u53f0",
			candidates: []entity.InstanceSnapshot{
				testInstance("uhost-first", "first-host", "Running"),
				testInstance("uhost-name-second", "\u7b2c2\u53f0", "Running"),
			},
			want: "uhost-name-second",
		},
		{
			name:  "numeric name conflicts with ordinal",
			input: "1",
			candidates: []entity.InstanceSnapshot{
				testInstance("uhost-first", "first-host", "Running"),
				testInstance("uhost-name-one", "1", "Running"),
			},
			ambiguous: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchResourceSelection(tt.input, testPendingResourceSelection(tt.candidates))
			if tt.ambiguous {
				if got.ok || !got.ambiguous {
					t.Fatalf("matchResourceSelection(%q) = ok %v ambiguous %v, want ambiguous", tt.input, got.ok, got.ambiguous)
				}
				return
			}
			if !got.ok || got.ambiguous {
				t.Fatalf("matchResourceSelection(%q) = ok %v ambiguous %v, want ok true ambiguous false", tt.input, got.ok, got.ambiguous)
			}
			if got.instance.UHostId != tt.want {
				t.Fatalf("matchResourceSelection(%q) resolved %q, want %q", tt.input, got.instance.UHostId, tt.want)
			}
		})
	}
}

func TestResourceSelectionMatchResolvesDoubleDigitChineseOrdinals(t *testing.T) {
	candidates := make([]entity.InstanceSnapshot, 20)
	for i := range candidates {
		candidates[i] = testInstance("uhost-"+strconv.Itoa(i+1), "host-"+strconv.Itoa(i+1), "Running")
	}
	p := testPendingResourceSelection(candidates)

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "ten", in: "\u7b2c\u5341\u53f0", want: "uhost-10"},
		{name: "eleven", in: "\u7b2c\u5341\u4e00\u53f0", want: "uhost-11"},
		{name: "twenty", in: "\u7b2c\u4e8c\u5341\u53f0", want: "uhost-20"},
		{name: "arabic twenty", in: "\u7b2c20\u53f0", want: "uhost-20"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchResourceSelection(tt.in, p)
			if !got.ok || got.ambiguous {
				t.Fatalf("matchResourceSelection(%q) = ok %v ambiguous %v, want ok true ambiguous false", tt.in, got.ok, got.ambiguous)
			}
			if got.instance.UHostId != tt.want {
				t.Fatalf("matchResourceSelection(%q) resolved %q, want %q", tt.in, got.instance.UHostId, tt.want)
			}
		})
	}
}

func TestResourceSelectionMatchInvalidSelection(t *testing.T) {
	p := testPendingResourceSelection([]entity.InstanceSnapshot{
		testInstance("uhost-first", "first-host", "Running"),
	})

	got := matchResourceSelection("not a listed resource", p)
	if got.ok || got.ambiguous {
		t.Fatalf("invalid selection = ok %v ambiguous %v, want both false", got.ok, got.ambiguous)
	}
}

func TestResourceSelectionMatchDuplicateExactNameIsAmbiguous(t *testing.T) {
	p := testPendingResourceSelection([]entity.InstanceSnapshot{
		testInstance("uhost-dup-a", "same-name", "Running"),
		testInstance("uhost-dup-b", "same-name", "Running"),
	})

	got := matchResourceSelection("same-name", p)
	if got.ok || !got.ambiguous {
		t.Fatalf("duplicate exact name = ok %v ambiguous %v, want ok false ambiguous true", got.ok, got.ambiguous)
	}
}

func TestResourceSelectionExpiry(t *testing.T) {
	p := testPendingResourceSelection([]entity.InstanceSnapshot{
		testInstance("uhost-first", "first-host", "Running"),
	})
	p.createdTurn = 10

	tests := []struct {
		currentTurn int
		expired     bool
	}{
		{currentTurn: 11, expired: false},
		{currentTurn: 12, expired: false},
		{currentTurn: 13, expired: true},
	}

	for _, tt := range tests {
		if got := isResourceSelectionExpired(tt.currentTurn, p); got != tt.expired {
			t.Fatalf("isResourceSelectionExpired(%d) = %v, want %v", tt.currentTurn, got, tt.expired)
		}
	}
}

func testSnapshotWithInstances(insts ...entity.InstanceSnapshot) entity.RegistrySnapshot {
	m := make(map[string]entity.InstanceSnapshot, len(insts))
	for _, i := range insts {
		m[i.UHostId] = i
	}
	return entity.RegistrySnapshot{Instances: m}
}

// TestFindExplicitInstanceRef locks the deterministic backstop for the monitor
// flow: when the user names a literal uhost-ID, code resolves it (Rule 5) instead
// of depending on the intent router to extract it into Slots.TargetRefs. WHY it
// matters: a resolved single candidate flows through the len==1 auto-dispatch that
// also populates SelectedInstanceID, so the explicit-ID turn both answers AND
// records context — the all-instances "select one" prompt did neither.
func TestFindExplicitInstanceRef(t *testing.T) {
	snap := testSnapshotWithInstances(
		testInstance("uhost-1qy6d8tkfrl4", "host", "Running"),
		testInstance("uhost-1rkv126dxgiq", "host", "Running"),
	)
	tests := []struct {
		name      string
		msg       string
		wantID    string // matched instance ID, "" if none
		wantNotFd string // unresolved uhost token, "" if none
	}{
		{"explicit ID resolves", "uhost-1qy6d8tkfrl4 的GPU利用率是多少？", "uhost-1qy6d8tkfrl4", ""},
		{"ID with trailing CJK", "查uhost-1qy6d8tkfrl4的内存", "uhost-1qy6d8tkfrl4", ""},
		{"wrong ID surfaces as notFound", "uhost-doesnotexist 的GPU利用率", "", "uhost-doesnotexist"},
		{"mistyped-case ID echoes whole as notFound", "uhost-1QY6D8 的GPU利用率", "", "uhost-1QY6D8"},
		{"no ID at all", "查看GPU利用率", "", ""},
		{"first resolvable wins", "uhost-1qy6d8tkfrl4 还是 uhost-1rkv126dxgiq", "uhost-1qy6d8tkfrl4", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matched, notFound := findExplicitInstanceRef(tt.msg, snap)
			gotID := ""
			if matched != nil {
				gotID = matched.UHostId
			}
			if gotID != tt.wantID {
				t.Fatalf("matched = %q, want %q", gotID, tt.wantID)
			}
			if notFound != tt.wantNotFd {
				t.Fatalf("notFound = %q, want %q", notFound, tt.wantNotFd)
			}
		})
	}
}

// TestResourceSelectionPromptWrongIDPrefix: a typo'd/unowned ID gets an explicit
// "未找到实例 X" notice (distinct from the no-ID-given prompt) but still lists
// candidates so the user can recover.
func TestResourceSelectionPromptWrongIDPrefix(t *testing.T) {
	p := testPendingResourceSelection([]entity.InstanceSnapshot{
		testInstance("uhost-a", "host", "Running"),
		testInstance("uhost-b", "host2", "Stopped"),
	})
	p.notFoundRef = "uhost-typo"
	got := renderResourceSelectionPrompt(p)
	if !strings.Contains(got, "未找到实例 uhost-typo") {
		t.Fatalf("prompt missing wrong-ID notice:\n%s", got)
	}
	if strings.Contains(got, "我需要先确认你要查看哪台实例") {
		t.Fatalf("wrong-ID prompt must not reuse the no-ID lead line:\n%s", got)
	}
	if !strings.Contains(got, "uhost-a") || !strings.Contains(got, "uhost-b") {
		t.Fatalf("wrong-ID prompt must still list candidates so the user can recover:\n%s", got)
	}
}

func testPendingResourceSelection(candidates []entity.InstanceSnapshot) pendingResourceSelection {
	return pendingResourceSelection{
		originalUserMsg: "CPU question",
		plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentMonitorQuery,
		},
		snapshot:    entity.RegistrySnapshot{},
		candidates:  candidates,
		createdTurn: 4,
	}
}

func testInstance(id, name, state string) entity.InstanceSnapshot {
	return entity.InstanceSnapshot{
		UHostId:    id,
		Name:       name,
		State:      state,
		GPU:        1,
		GpuType:    "RTX4090",
		CPU:        16,
		Memory:     65536,
		Zone:       "cn-wlcb-01",
		ChargeType: "Dynamic",
	}
}
