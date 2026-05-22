package tools

import (
	"context"
	"strings"
	"testing"
)

func TestWithUserAndUserFromRoundTrip(t *testing.T) {
	u := UserContext{
		TopOrganizationID: 123,
		OrganizationID:    456,
		RoleUrn:           "ucs:iam::123:role/test",
		SessionName:       "session-1",
		ProjectId:         "proj-abc",
		Region:            "cn-bj2",
	}
	ctx := WithUser(context.Background(), u)
	got, ok := UserFrom(ctx)
	if !ok {
		t.Fatalf("UserFrom returned ok=false after WithUser")
	}
	if got != u {
		t.Fatalf("UserFrom returned %+v, want %+v", got, u)
	}
}

func TestUserFromMissingReturnsFalse(t *testing.T) {
	_, ok := UserFrom(context.Background())
	if ok {
		t.Fatalf("UserFrom should return ok=false on bare context")
	}
}

func TestRoleUrnFromTemplate(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		tpl := "ucs:iam::%d:role/svc"
		got, err := RoleUrnFromTemplate(tpl, 12345)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "ucs:iam::12345:role/svc"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("zero topOrg", func(t *testing.T) {
		_, err := RoleUrnFromTemplate("ucs:iam::%d:role/svc", 0)
		if err == nil {
			t.Fatalf("expected error for topOrg=0")
		}
	})
	t.Run("empty template", func(t *testing.T) {
		_, err := RoleUrnFromTemplate("", 12345)
		if err == nil {
			t.Fatalf("expected error for empty template")
		}
	})
}

func TestSubjectKeyFromUser(t *testing.T) {
	t.Run("nonzero pair produces deterministic hash", func(t *testing.T) {
		u := UserContext{TopOrganizationID: 10, OrganizationID: 20}
		key1, ok1 := SubjectKeyFromUser(u)
		key2, ok2 := SubjectKeyFromUser(u)
		if !ok1 || !ok2 {
			t.Fatalf("expected ok=true for valid pair")
		}
		if key1 != key2 {
			t.Fatalf("hash not deterministic: %q vs %q", key1, key2)
		}
		if !strings.HasPrefix(key1, "sha256:") {
			t.Fatalf("expected sha256: prefix, got %q", key1)
		}
	})
	t.Run("zero topOrg returns anonymous", func(t *testing.T) {
		key, ok := SubjectKeyFromUser(UserContext{TopOrganizationID: 0, OrganizationID: 20})
		if ok {
			t.Fatalf("expected ok=false for zero TopOrganizationID")
		}
		if key != "anonymous" {
			t.Fatalf("expected 'anonymous', got %q", key)
		}
	})
	t.Run("zero orgId returns anonymous", func(t *testing.T) {
		key, ok := SubjectKeyFromUser(UserContext{TopOrganizationID: 10, OrganizationID: 0})
		if ok {
			t.Fatalf("expected ok=false for zero OrganizationID")
		}
		if key != "anonymous" {
			t.Fatalf("expected 'anonymous', got %q", key)
		}
	})
	t.Run("different pairs produce different keys", func(t *testing.T) {
		k1, _ := SubjectKeyFromUser(UserContext{TopOrganizationID: 1, OrganizationID: 2})
		k2, _ := SubjectKeyFromUser(UserContext{TopOrganizationID: 3, OrganizationID: 4})
		if k1 == k2 {
			t.Fatalf("different pairs must produce different hashes")
		}
	})
}
