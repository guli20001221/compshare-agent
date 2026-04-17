package prompt

import (
	"testing"
)

func TestClassifyUser_NoInstances(t *testing.T) {
	stage := ClassifyUser(map[string]any{})
	if stage != NewUser {
		t.Errorf("no instances = %v, want NewUser", stage)
	}

	stage = ClassifyUser(map[string]any{"UHostSet": []any{}})
	if stage != NewUser {
		t.Errorf("empty UHostSet = %v, want NewUser", stage)
	}
}

func TestClassifyUser_ActiveUser(t *testing.T) {
	result := map[string]any{
		"UHostSet": []any{
			map[string]any{"State": "Running"},
			map[string]any{"State": "Stopped"},
		},
	}
	stage := ClassifyUser(result)
	if stage != ActiveUser {
		t.Errorf("has running = %v, want ActiveUser", stage)
	}
}

func TestClassifyUser_InactiveUser(t *testing.T) {
	result := map[string]any{
		"UHostSet": []any{
			map[string]any{"State": "Stopped"},
			map[string]any{"State": "Stopped"},
		},
	}
	stage := ClassifyUser(result)
	if stage != InactiveUser {
		t.Errorf("all stopped = %v, want InactiveUser", stage)
	}
}

func TestGetSuggestions_NewUser(t *testing.T) {
	suggestions := GetSuggestions(NewUser)
	if len(suggestions) != 3 {
		t.Fatalf("NewUser suggestions count = %d, want 3", len(suggestions))
	}
	if suggestions[0].Text != "帮我推荐一个入门配置" {
		t.Errorf("first suggestion = %q, unexpected", suggestions[0].Text)
	}
}

func TestGetSuggestions_ActiveUser(t *testing.T) {
	suggestions := GetSuggestions(ActiveUser)
	if len(suggestions) != 3 {
		t.Fatalf("ActiveUser suggestions count = %d, want 3", len(suggestions))
	}
	if suggestions[0].Text != "查看实例状态" {
		t.Errorf("first suggestion = %q, unexpected", suggestions[0].Text)
	}
	// All suggestions must map to existing tools
	texts := map[string]bool{}
	for _, s := range suggestions {
		texts[s.Text] = true
	}
	if texts["设置定时关机"] {
		t.Error("should not suggest '设置定时关机' — no scheduler tool registered")
	}
	if texts["查看余额"] {
		t.Error("should not suggest '查看余额' — no account-info tool registered")
	}
}

func TestGetSuggestions_InactiveUser(t *testing.T) {
	suggestions := GetSuggestions(InactiveUser)
	if len(suggestions) != 3 {
		t.Fatalf("InactiveUser suggestions count = %d, want 3", len(suggestions))
	}
	if suggestions[0].Text != "开机" {
		t.Errorf("first suggestion = %q, unexpected", suggestions[0].Text)
	}
	for _, s := range suggestions {
		if s.Text == "查看余额" {
			t.Error("should not suggest '查看余额' — no account-info tool registered")
		}
	}
}
