package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeUAccountReq(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	return m
}

func TestUAccountClientIGetRoleSuccess(t *testing.T) {
	var gotAction, gotRoleName string
	var gotCompanyID float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decodeUAccountReq(t, r)
		gotAction = body["Action"].(string)
		gotRoleName = body["RoleName"].(string)
		gotCompanyID = body["CompanyId"].(float64)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"RetCode": 0,
			"Role": map[string]any{
				"RoleName": "ucs-service-role/ServiceRoleForCompshare",
				"RoleURN":  "ucs:iam::42:role/ucs-service-role/ServiceRoleForCompshare",
			},
		})
	}))
	defer srv.Close()

	c := NewUAccountClient(srv.URL)
	urn, err := c.IGetRole(context.Background(), 42, "ucs-service-role/ServiceRoleForCompshare")
	if err != nil {
		t.Fatalf("IGetRole error: %v", err)
	}
	if gotAction != "IGetRole" {
		t.Errorf("Action = %q, want IGetRole", gotAction)
	}
	if gotRoleName != "ucs-service-role/ServiceRoleForCompshare" {
		t.Errorf("RoleName = %q", gotRoleName)
	}
	if gotCompanyID != 42 {
		t.Errorf("CompanyId = %v, want 42", gotCompanyID)
	}
	if urn != "ucs:iam::42:role/ucs-service-role/ServiceRoleForCompshare" {
		t.Errorf("RoleURN = %q", urn)
	}
}

func TestUAccountClientIGetRoleRoleNotExistMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"RetCode": 230,
			"Message": "Role Not Exist",
		})
	}))
	defer srv.Close()

	c := NewUAccountClient(srv.URL)
	_, err := c.IGetRole(context.Background(), 42, "ucs-service-role/ServiceRoleForCompshare")
	if !errors.Is(err, ErrRoleNotExist) {
		t.Fatalf("err = %v, want ErrRoleNotExist", err)
	}
}

func TestUAccountClientIGetRoleEmptyURNTreatedAsMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"RetCode": 0,
			"Role":    map[string]any{"RoleName": "", "RoleURN": ""},
		})
	}))
	defer srv.Close()

	c := NewUAccountClient(srv.URL)
	_, err := c.IGetRole(context.Background(), 42, "x")
	if !errors.Is(err, ErrRoleNotExist) {
		t.Fatalf("err = %v, want ErrRoleNotExist for empty RoleURN", err)
	}
}

func TestUAccountClientIGetRoleOtherRetCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"RetCode": 500,
			"Message": "boom",
		})
	}))
	defer srv.Close()

	c := NewUAccountClient(srv.URL)
	_, err := c.IGetRole(context.Background(), 42, "x")
	if err == nil || errors.Is(err, ErrRoleNotExist) {
		t.Fatalf("err = %v, want non-nil non-RoleNotExist", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err should mention RetCode 500, got %v", err)
	}
}

func TestUAccountClientICreateServiceLinkedRoleSuccess(t *testing.T) {
	var gotAction, gotService string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decodeUAccountReq(t, r)
		gotAction = body["Action"].(string)
		gotService = body["ServiceName"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0})
	}))
	defer srv.Close()

	c := NewUAccountClient(srv.URL)
	if err := c.ICreateServiceLinkedRole(context.Background(), 42, "compshare"); err != nil {
		t.Fatalf("ICreateServiceLinkedRole error: %v", err)
	}
	if gotAction != "ICreateServiceLinkedRole" {
		t.Errorf("Action = %q", gotAction)
	}
	if gotService != "compshare" {
		t.Errorf("ServiceName = %q", gotService)
	}
}

func TestUAccountClientICreateServiceLinkedRoleError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 403, "Message": "denied"})
	}))
	defer srv.Close()

	c := NewUAccountClient(srv.URL)
	err := c.ICreateServiceLinkedRole(context.Background(), 42, "compshare")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("err should mention RetCode 403, got %v", err)
	}
}
