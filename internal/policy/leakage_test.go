package policy

import "testing"

func TestRedactQueryDerivedValueRedactsStaffNameFromSharedList(t *testing.T) {
	got := RedactQueryDerivedValue("请张慧帮我看实例启动失败")
	if got != RedactedQueryValue {
		t.Fatalf("RedactQueryDerivedValue() = %q, want %q", got, RedactedQueryValue)
	}
}

func TestRedactQueryDerivedValueKeepsPublicProductTerms(t *testing.T) {
	got := RedactQueryDerivedValue("Feishu 使用 FAQ 怎么看")
	if got != "Feishu 使用 FAQ 怎么看" {
		t.Fatalf("RedactQueryDerivedValue() = %q, want original query", got)
	}
}

func TestRedactQueryDerivedValueRedactsInternalMarkers(t *testing.T) {
	for _, query := range []string{
		"wxwork-spt-record-2026-05 case",
		"查看 /cloud/xxx/snapshotter 路径",
		"gitlab.internal.example/doc",
	} {
		if got := RedactQueryDerivedValue(query); got != RedactedQueryValue {
			t.Fatalf("RedactQueryDerivedValue(%q) = %q, want %q", query, got, RedactedQueryValue)
		}
	}
}
