package engine

import (
	"context"
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

func TestResourceSelectionEmbeddedOrdinalReference(t *testing.T) {
	candidates := make([]entity.InstanceSnapshot, 12)
	for i := range candidates {
		id := "uhost-" + strings.Repeat("0", 2-len(strconv.Itoa(i+1))) + strconv.Itoa(i+1)
		candidates[i] = testInstance(id, "host-"+strconv.Itoa(i+1), "Running")
	}
	p := testPendingResourceSelection(candidates)

	got, exact := matchResourceSelectionReference("\u7b2c11\u53f0 GPU \u5fd9\u4e0d\u5fd9", p)
	if exact {
		t.Fatal("embedded ordinal in a question should not be treated as a pure selection reply")
	}
	if !got.ok || got.instance.UHostId != "uhost-11" {
		t.Fatalf("embedded ordinal resolved %+v, want uhost-11", got)
	}

	got, _ = matchResourceSelectionReference("\u7b2c11\u4e2a\u95ee\u9898\u662f\u4ec0\u4e48", p)
	if got.ok || got.ambiguous {
		t.Fatalf("non-instance ordinal phrase should not resolve a resource: %+v", got)
	}
}

func TestPendingSelectionRoundTripsThroughSessionState(t *testing.T) {
	e := newEngineForSessionStateTest(t)
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)
	e.userTurn = 7
	e.recordPendingInstanceSelection([]entity.InstanceSnapshot{
		testInstance("uhost-a", "host-a", "Running"),
		testInstance("uhost-b", "host-b", "Stopped"),
	}, intent.IntentResourceInfo, "\u6211\u6709\u54ea\u4e9b\u5b9e\u4f8b", 2, false)

	state, _, hydrated := e.SessionStateSnapshot()
	if !hydrated {
		t.Fatal("session state should be hydrated")
	}
	if state.PendingSelectionKind != pendingSelectionKindInstance {
		t.Fatalf("pending kind = %q, want %q", state.PendingSelectionKind, pendingSelectionKindInstance)
	}
	if len(state.PendingSelectionItems) != 2 {
		t.Fatalf("pending items = %d, want 2", len(state.PendingSelectionItems))
	}

	e2 := newEngineForSessionStateTest(t)
	e2.SetSessionState(state, 2)
	pending, ok := e2.pendingResourceSelectionFromSession()
	if !ok {
		t.Fatal("expected pending selection restored from session state")
	}
	match := matchResourceSelection("2", *pending)
	if !match.ok || match.instance.UHostId != "uhost-b" {
		t.Fatalf("restored selection resolved %+v, want uhost-b", match)
	}
}

func TestRecordInstanceStateFactsResourceInfoMultiHostDoesNotStorePendingSelection(t *testing.T) {
	e := newEngineForSessionStateTest(t)
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)
	e.userTurn = 5
	e.lastPlannerIntentThisTurn = intent.IntentResourceInfo
	rawHosts := make([]any, 12)
	for i := range rawHosts {
		n := i + 1
		id := "uhost-" + strings.Repeat("0", 2-len(strconv.Itoa(n))) + strconv.Itoa(n)
		rawHosts[i] = map[string]any{
			"UHostId": id,
			"Name":    "host-" + strconv.Itoa(n),
			"State":   "Running",
			"GpuType": "4090",
			"GPU":     float64(1),
		}
	}

	e.recordInstanceStateFacts(map[string]any{"UHostSet": rawHosts})

	state, _, _ := e.SessionStateSnapshot()
	if state.SelectedInstanceID != "" {
		t.Fatalf("multi-host list must not select a current instance, got %q", state.SelectedInstanceID)
	}
	if len(state.PendingSelectionItems) != 0 {
		t.Fatalf("raw multi-host fact writer must not persist pending selection before display truncation, got %d", len(state.PendingSelectionItems))
	}
}

func TestRecordInstanceStateFactsNonResourceMultiHostDoesNotStorePendingSelection(t *testing.T) {
	e := newEngineForSessionStateTest(t)
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)
	e.userTurn = 5
	e.lastPlannerIntentThisTurn = intent.IntentMonitorQuery
	rawHosts := []any{
		map[string]any{"UHostId": "uhost-a", "Name": "host-a", "State": "Running"},
		map[string]any{"UHostId": "uhost-b", "Name": "host-b", "State": "Running"},
	}

	e.recordInstanceStateFacts(map[string]any{"UHostSet": rawHosts})

	state, _, _ := e.SessionStateSnapshot()
	if len(state.PendingSelectionItems) != 0 {
		t.Fatalf("non-resource multi-host result must not persist pending selection, got %d", len(state.PendingSelectionItems))
	}
}

func TestRecordPendingSelectionFromDisplayedDescribeResultCapsAtDisplayCap(t *testing.T) {
	e := newEngineForSessionStateTest(t)
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)
	e.userTurn = 5
	e.lastPlannerIntentThisTurn = intent.IntentResourceInfo
	rawHosts := make([]any, 12)
	for i := range rawHosts {
		n := i + 1
		id := "uhost-" + strings.Repeat("0", 2-len(strconv.Itoa(n))) + strconv.Itoa(n)
		rawHosts[i] = map[string]any{
			"UHostId": id,
			"Name":    "host-" + strconv.Itoa(n),
			"State":   "Running",
			"GpuType": "4090",
			"GPU":     float64(1),
		}
	}

	e.recordPendingSelectionFromDisplayedDescribeResult(map[string]any{
		"TotalCount": float64(12),
		"Truncated":  true,
		"UHostSet":   rawHosts,
	})
	e.commitDisplayedResourceSelectionIfVisible("1. host-1\n2. host-2")

	state, _, _ := e.SessionStateSnapshot()
	if len(state.PendingSelectionItems) != intent.DefaultMaxInstancesPerDisplay {
		t.Fatalf("pending items = %d, want display cap %d", len(state.PendingSelectionItems), intent.DefaultMaxInstancesPerDisplay)
	}
	if state.PendingSelectionItems[9].ID != "uhost-10" {
		t.Fatalf("10th pending item = %+v, want uhost-10", state.PendingSelectionItems[9])
	}
}

func TestRecordPendingSelectionFromResourceHandlerUsesHandlerCandidates(t *testing.T) {
	e := newEngineForSessionStateTest(t)
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)
	e.userTurn = 5
	displayed := []entity.InstanceSnapshot{
		testInstance("uhost-running", "running", "Running"),
		testInstance("uhost-stopped", "stopped", "Stopped"),
	}

	e.recordPendingSelectionFromHandlerResult(intent.HandlerResult{
		Status:                      intent.HandlerStatusHandled,
		ResourceSelectionCandidates: displayed,
	}, intent.IntentRoute{Intent: intent.IntentResourceInfo}, "\u6211\u6709\u54ea\u4e9b\u5b9e\u4f8b")

	state, _, _ := e.SessionStateSnapshot()
	if len(state.PendingSelectionItems) != 2 {
		t.Fatalf("pending items = %d, want 2", len(state.PendingSelectionItems))
	}
	if state.PendingSelectionItems[0].ID != "uhost-running" || state.PendingSelectionItems[1].ID != "uhost-stopped" {
		t.Fatalf("pending order = %+v, want displayed order", state.PendingSelectionItems)
	}
}

func TestRecordPendingSelectionFromResourceHandlerCapsOrdinalAtDisplayCap(t *testing.T) {
	e := newEngineForSessionStateTest(t)
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)
	e.userTurn = 5
	candidates := make([]entity.InstanceSnapshot, 12)
	for i := range candidates {
		id := "uhost-" + strings.Repeat("0", 2-len(strconv.Itoa(i+1))) + strconv.Itoa(i+1)
		candidates[i] = testInstance(id, "host-"+strconv.Itoa(i+1), "Running")
	}

	e.recordPendingSelectionFromHandlerResult(intent.HandlerResult{
		Status:                      intent.HandlerStatusHandled,
		ResourceSelectionCandidates: candidates,
	}, intent.IntentRoute{Intent: intent.IntentResourceInfo}, "\u6211\u6709\u54ea\u4e9b\u5b9e\u4f8b")

	e2 := newEngineForSessionStateTest(t)
	state, _, _ := e.SessionStateSnapshot()
	e2.SetSessionState(state, 2)
	pending, ok := e2.pendingResourceSelectionFromSession()
	if !ok {
		t.Fatal("expected pending selection restored")
	}
	got, exact := matchResourceSelectionReference("\u7b2c10\u53f0 GPU \u5fd9\u4e0d\u5fd9", *pending)
	if exact {
		t.Fatal("embedded ordinal question should continue normal routing after binding")
	}
	if !got.ok || got.instance.UHostId != "uhost-10" {
		t.Fatalf("resolved %+v, want uhost-10", got)
	}
	got, exact = matchResourceSelectionReference("\u7b2c11\u53f0 GPU \u5fd9\u4e0d\u5fd9", *pending)
	if got.ok || exact {
		t.Fatalf("11th item was not displayed and must not resolve, got %+v exact=%v", got, exact)
	}
}

func TestRecordSelectedInstanceClearsPendingSelection(t *testing.T) {
	e := newEngineForSessionStateTest(t)
	e.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaV1,
		PendingSelectionKind: pendingSelectionKindInstance,
		PendingSelectionItems: []PendingSelectionItem{{
			Index: 1,
			ID:    "uhost-old",
			Name:  "old",
		}},
	}, 1)

	e.recordSelectedInstanceID("uhost-new", "new")

	state, _, _ := e.SessionStateSnapshot()
	if state.PendingSelectionKind != "" || len(state.PendingSelectionItems) != 0 {
		t.Fatalf("pending selection should be cleared after selecting an instance: %+v", state)
	}
}

func TestTryResumeResourceSelectionEmbeddedOrdinalBindsAndContinues(t *testing.T) {
	candidates := make([]entity.InstanceSnapshot, 12)
	for i := range candidates {
		id := "uhost-" + strings.Repeat("0", 2-len(strconv.Itoa(i+1))) + strconv.Itoa(i+1)
		candidates[i] = testInstance(id, "host-"+strconv.Itoa(i+1), "Running")
	}
	e := newEngineForSessionStateTest(t)
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)
	e.userTurn = 3
	e.recordPendingInstanceSelection(candidates, intent.IntentResourceInfo, "\u6211\u6709\u54ea\u4e9b\u5b9e\u4f8b", len(candidates), false)

	reply, handled := e.tryResumeResourceSelection(context.Background(), "\u7b2c10\u53f0 GPU \u5fd9\u4e0d\u5fd9", noopStep)
	if !handled {
		t.Fatalf("embedded ordinal monitor question should be answered immediately")
	}
	if strings.Contains(reply, "\u8bf7\u9009\u62e9") {
		t.Fatalf("embedded ordinal monitor question must not ask the user to choose again: %q", reply)
	}
	if e.sessionState.SelectedInstanceID != "uhost-10" {
		t.Fatalf("selected = %q, want uhost-10", e.sessionState.SelectedInstanceID)
	}
	if e.sessionState.PendingSelectionKind != "" || len(e.sessionState.PendingSelectionItems) != 0 {
		t.Fatalf("pending selection should be cleared after binding, state=%+v", e.sessionState)
	}
}

func TestTryResumeResourceSelectionEmbeddedOrdinalGPUInfoAnswersSelectedInstance(t *testing.T) {
	candidates := []entity.InstanceSnapshot{
		testInstance("uhost-01", "host-1", "Running"),
		testInstance("uhost-02", "host-2", "Running"),
	}
	e := newEngineForSessionStateTest(t)
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)
	e.userTurn = 3
	e.recordPendingInstanceSelection(candidates, intent.IntentResourceInfo, "\u6211\u6709\u54ea\u4e9b\u5b9e\u4f8b", len(candidates), false)

	reply, handled := e.tryResumeResourceSelection(context.Background(), "\u7b2c1\u53f0 GPU \u578b\u53f7\u662f\u4ec0\u4e48", noopStep)
	if !handled {
		t.Fatal("embedded ordinal GPU info question should be answered from the selected instance")
	}
	if !strings.Contains(reply, "RTX4090") {
		t.Fatalf("reply should include selected instance GPU type, got %q", reply)
	}
	if !strings.Contains(reply, "数量 1 张") {
		t.Fatalf("reply should include selected instance GPU count, got %q", reply)
	}
	if strings.Contains(reply, "\u8bf7\u9009\u62e9") || strings.Contains(reply, "\u76d1\u63a7") {
		t.Fatalf("GPU info question should not ask selection again or become monitor query: %q", reply)
	}
	if e.sessionState.SelectedInstanceID != "uhost-01" {
		t.Fatalf("selected = %q, want uhost-01", e.sessionState.SelectedInstanceID)
	}
	if e.sessionState.PendingSelectionKind != "" || len(e.sessionState.PendingSelectionItems) != 0 {
		t.Fatalf("pending selection should be cleared after binding, state=%+v", e.sessionState)
	}
}

func TestTryResumeResourceSelectionEmbeddedOrdinalGPUHowToContinuesRouting(t *testing.T) {
	candidates := []entity.InstanceSnapshot{
		testInstance("uhost-01", "host-1", "Running"),
		testInstance("uhost-02", "host-2", "Running"),
	}
	e := newEngineForSessionStateTest(t)
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)
	e.userTurn = 3
	e.recordPendingInstanceSelection(candidates, intent.IntentResourceInfo, "\u6211\u6709\u54ea\u4e9b\u5b9e\u4f8b", len(candidates), false)

	reply, handled := e.tryResumeResourceSelection(context.Background(), "\u7b2c1\u53f0 GPU \u578b\u53f7\u600e\u4e48\u67e5", noopStep)
	if handled {
		t.Fatalf("embedded ordinal GPU how-to question should continue normal routing, got reply %q", reply)
	}
	if e.sessionState.SelectedInstanceID != "uhost-01" {
		t.Fatalf("selected = %q, want uhost-01", e.sessionState.SelectedInstanceID)
	}
	if e.sessionState.PendingSelectionKind != "" || len(e.sessionState.PendingSelectionItems) != 0 {
		t.Fatalf("pending selection should be cleared after binding, state=%+v", e.sessionState)
	}
}

func TestTryResumeResourceSelectionEmbeddedOrdinalTaskRequestsContinueRouting(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{name: "deploy", text: "\u7528\u7b2c1\u53f0 GPU \u90e8\u7f72 DeepSeek"},
		{name: "create_like", text: "\u6309\u7b2c1\u53f0\u914d\u7f6e\u521b\u5efa\u4e00\u53f0"},
		{name: "advice", text: "\u7b2c1\u53f0 GPU \u80fd\u8dd1 DeepSeek \u5417"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidates := []entity.InstanceSnapshot{
				testInstance("uhost-01", "host-1", "Running"),
				testInstance("uhost-02", "host-2", "Running"),
			}
			e := newEngineForSessionStateTest(t)
			e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)
			e.userTurn = 3
			e.recordPendingInstanceSelection(candidates, intent.IntentResourceInfo, "\u6211\u6709\u54ea\u4e9b\u5b9e\u4f8b", len(candidates), false)

			reply, handled := e.tryResumeResourceSelection(context.Background(), tc.text, noopStep)
			if handled {
				t.Fatalf("task request after ordinal selection should continue normal routing, got reply %q", reply)
			}
			if e.sessionState.SelectedInstanceID != "uhost-01" {
				t.Fatalf("selected = %q, want uhost-01", e.sessionState.SelectedInstanceID)
			}
			if e.sessionState.PendingSelectionKind != "" || len(e.sessionState.PendingSelectionItems) != 0 {
				t.Fatalf("pending selection should be cleared after binding, state=%+v", e.sessionState)
			}
		})
	}
}

func TestTryResumeResourceSelectionIgnoresUnrelatedQuestion(t *testing.T) {
	e := newEngineForSessionStateTest(t)
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)
	e.userTurn = 3
	e.recordPendingInstanceSelection([]entity.InstanceSnapshot{
		testInstance("uhost-a", "host-a", "Running"),
		testInstance("uhost-b", "host-b", "Stopped"),
	}, intent.IntentResourceInfo, "\u6211\u6709\u54ea\u4e9b\u5b9e\u4f8b", 2, false)

	reply, handled := e.tryResumeResourceSelection(context.Background(), "4090 \u591a\u5c11\u94b1", noopStep)
	if handled {
		t.Fatalf("unrelated price question should continue to normal routing, got reply %q", reply)
	}
	if e.sessionState.SelectedInstanceID != "" {
		t.Fatalf("unrelated question must not select an instance, got %q", e.sessionState.SelectedInstanceID)
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

func TestResolveContinuationInstanceFindsNameOutsideDisplayedCandidates(t *testing.T) {
	instances := make([]entity.InstanceSnapshot, 0, maxResourceSelectionCandidates+1)
	for i := 0; i < maxResourceSelectionCandidates; i++ {
		instances = append(instances, testInstance("uhost-visible-"+strconv.Itoa(i), "visible-"+strconv.Itoa(i), "Running"))
	}
	hidden := testInstance("uhost-zzzz-hidden", "claude-write-test", "Stopped")
	instances = append(instances, hidden)
	snapshot := testSnapshotWithInstances(instances...)
	candidates, truncated := sortAndLimitResourceSelectionCandidates(instances)
	if !truncated {
		t.Fatal("test setup should produce a truncated candidate list")
	}
	for _, candidate := range candidates {
		if candidate.UHostId == hidden.UHostId {
			t.Fatal("test setup should keep target outside the displayed candidate list")
		}
	}

	got, ok := resolveContinuationInstance("claude-write-test 这台关机", snapshot)
	if !ok {
		t.Fatalf("resolveContinuationInstance should search the full snapshot, not only displayed candidates")
	}
	if got.UHostId != hidden.UHostId {
		t.Fatalf("resolved %q, want %q", got.UHostId, hidden.UHostId)
	}
}

func TestResolveLoopCeilingInstanceUsesSelectedInstanceForFollowups(t *testing.T) {
	selected := testInstance("uhost-selected", "train-box", "Running")
	other := testInstance("uhost-other", "other-box", "Stopped")
	snapshot := testSnapshotWithInstances(selected, other)

	for _, msg := range []string{"这台现在状态呢", "它还能重启吗", "？", "?"} {
		t.Run(msg, func(t *testing.T) {
			got, ok := resolveLoopCeilingInstance(msg, snapshot, selected.UHostId)
			if !ok {
				t.Fatalf("resolveLoopCeilingInstance(%q) did not resolve selected instance", msg)
			}
			if got.UHostId != selected.UHostId {
				t.Fatalf("resolved %q, want %q", got.UHostId, selected.UHostId)
			}
		})
	}
}

func TestResolveLoopCeilingInstanceDoesNotGuessWithoutFollowupSignal(t *testing.T) {
	selected := testInstance("uhost-selected", "train-box", "Running")
	other := testInstance("uhost-other", "other-box", "Stopped")
	snapshot := testSnapshotWithInstances(selected, other)

	if got, ok := resolveLoopCeilingInstance("现在怎么样", snapshot, selected.UHostId); ok {
		t.Fatalf("ambiguous text should not guess selected instance, got %s", got.UHostId)
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
