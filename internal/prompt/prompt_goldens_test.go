package prompt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/compshare-agent/internal/intent"
)

type promptGoldenCase struct {
	ID             string   `json:"id"`
	Question       string   `json:"question"`
	ExpectIntent   string   `json:"expect_intent"`
	ExpectedForm   string   `json:"expected_runtime_form"`
	AllowedActions []string `json:"allowed_actions"`
	ForbiddenTools []string `json:"forbid_actions"`
	Boundary       string   `json:"boundary"`
}

func TestPromptGoldenCasesSchema(t *testing.T) {
	cases := loadPromptGoldenCases(t)
	if len(cases) < 10 {
		t.Fatalf("prompt golden cases = %d, want at least 10", len(cases))
	}
	validIntents := map[string]bool{}
	for _, i := range intent.RuntimeIntents() {
		validIntents[string(i)] = true
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		if tc.ID == "" || tc.Question == "" || tc.ExpectIntent == "" || tc.ExpectedForm == "" || tc.Boundary == "" {
			t.Fatalf("case has required empty field: %#v", tc)
		}
		if seen[tc.ID] {
			t.Fatalf("duplicate prompt golden id %s", tc.ID)
		}
		seen[tc.ID] = true
		if !validIntents[tc.ExpectIntent] {
			t.Fatalf("case %s has invalid intent %s", tc.ID, tc.ExpectIntent)
		}
	}
}

func TestPromptGoldenCasesCoverCreateImageSelection(t *testing.T) {
	cases := loadPromptGoldenCases(t)
	byID := map[string]promptGoldenCase{}
	for _, tc := range cases {
		byID[tc.ID] = tc
	}
	for _, id := range []string{
		"create_instance_framework_prefers_platform_image",
		"create_instance_app_uses_community_image",
	} {
		tc, ok := byID[id]
		if !ok {
			t.Fatalf("prompt golden case %s is required to guard operation image selection", id)
		}
		if tc.ExpectIntent != string(intent.IntentOperationLifecycle) {
			t.Fatalf("case %s intent = %s, want %s", id, tc.ExpectIntent, intent.IntentOperationLifecycle)
		}
		if !containsString(tc.AllowedActions, "CreateInstanceWorkflow") {
			t.Fatalf("case %s must allow CreateInstanceWorkflow: %#v", id, tc)
		}
	}
}

func loadPromptGoldenCases(t *testing.T) []promptGoldenCase {
	t.Helper()
	path := filepath.Join("..", "..", "eval", "prompt_goldens", "cases.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read prompt golden cases: %v", err)
	}
	var cases []promptGoldenCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("parse prompt golden cases: %v", err)
	}
	return cases
}
