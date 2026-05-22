package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSTSResponse builds the JSON response from the fake STS endpoint.
func fakeSTSResponse(ak, aSecret, token, expiration string) []byte {
	type creds struct {
		AccessKeyId     string
		AccessKeySecret string
		SecurityToken   string
		Expiration      string
	}
	type resp struct {
		RetCode     int
		Message     string
		Credentials creds
	}
	b, _ := json.Marshal(resp{
		RetCode: 0,
		Credentials: creds{
			AccessKeyId:     ak,
			AccessKeySecret: aSecret,
			SecurityToken:   token,
			Expiration:      expiration,
		},
	})
	return b
}

func TestSTSProviderGetReturnsCredentials(t *testing.T) {
	expiration := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	var gotAction, gotRoleUrn, gotPublicKey, gotSig string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		gotAction = r.FormValue("Action")
		gotRoleUrn = r.FormValue("RoleUrn")
		gotPublicKey = r.FormValue("PublicKey")
		gotSig = r.FormValue("Signature")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeSTSResponse("tmp-ak", "tmp-sk", "tmp-token", expiration))
	}))
	defer srv.Close()

	provider := NewSTSProvider("svc-ak", "svc-sk", srv.URL)
	u := UserContext{
		TopOrganizationID: 1,
		OrganizationID:    2,
		RoleUrn:           "ucs:iam::1:role/test",
		SessionName:       "test-session",
	}
	ctx := WithUser(context.Background(), u)

	cred, err := provider.Get(ctx)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if gotAction != "AssumeRole" {
		t.Errorf("expected Action=AssumeRole, got %q", gotAction)
	}
	if gotRoleUrn != u.RoleUrn {
		t.Errorf("expected RoleUrn=%q, got %q", u.RoleUrn, gotRoleUrn)
	}
	if gotPublicKey != "svc-ak" {
		t.Errorf("expected PublicKey=svc-ak, got %q", gotPublicKey)
	}
	if gotSig == "" {
		t.Error("expected Signature to be present")
	}
	if cred.AccessKeyId != "tmp-ak" {
		t.Errorf("expected AccessKeyId=tmp-ak, got %q", cred.AccessKeyId)
	}
	if cred.AccessKeySecret != "tmp-sk" {
		t.Errorf("expected AccessKeySecret=tmp-sk, got %q", cred.AccessKeySecret)
	}
	if cred.SecurityToken != "tmp-token" {
		t.Errorf("expected SecurityToken=tmp-token, got %q", cred.SecurityToken)
	}
}

func TestSTSProviderCachesCredentials(t *testing.T) {
	expiration := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeSTSResponse("tmp-ak", "tmp-sk", "tmp-token", expiration))
	}))
	defer srv.Close()

	provider := NewSTSProvider("svc-ak", "svc-sk", srv.URL)
	u := UserContext{
		TopOrganizationID: 1,
		OrganizationID:    2,
		RoleUrn:           "ucs:iam::1:role/test",
	}
	ctx := WithUser(context.Background(), u)

	_, err := provider.Get(ctx)
	if err != nil {
		t.Fatalf("first Get error: %v", err)
	}
	_, err = provider.Get(ctx)
	if err != nil {
		t.Fatalf("second Get error: %v", err)
	}

	if n := callCount.Load(); n != 1 {
		t.Fatalf("expected 1 STS call, got %d", n)
	}
}

func TestSTSProviderInflightWaitHonorsContextCancellation(t *testing.T) {
	expiration := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if callCount.Add(1) == 1 {
			close(started)
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeSTSResponse("tmp-ak", "tmp-sk", "tmp-token", expiration))
	}))
	defer srv.Close()
	defer releaseOnce.Do(func() { close(release) })

	provider := NewSTSProvider("svc-ak", "svc-sk", srv.URL)
	user := UserContext{
		TopOrganizationID: 1,
		OrganizationID:    2,
		RoleUrn:           "ucs:iam::1:role/test",
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := provider.Get(WithUser(context.Background(), user))
		firstDone <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first STS call")
	}

	waitCtx, cancel := context.WithTimeout(WithUser(context.Background(), user), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := provider.Get(waitCtx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline exceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("inflight waiter ignored cancellation; elapsed=%v", elapsed)
	}

	releaseOnce.Do(func() { close(release) })
	if err := <-firstDone; err != nil {
		t.Fatalf("first Get error: %v", err)
	}
}

func TestSTSProviderMissingUserContextErrors(t *testing.T) {
	provider := NewSTSProvider("svc-ak", "svc-sk", "http://localhost:9999")
	_, err := provider.Get(context.Background())
	if err == nil {
		t.Fatal("expected error when UserContext is missing from ctx")
	}
}

func TestSTSProviderEmptyRoleUrnErrors(t *testing.T) {
	provider := NewSTSProvider("svc-ak", "svc-sk", "http://localhost:9999")
	ctx := WithUser(context.Background(), UserContext{
		TopOrganizationID: 1,
		OrganizationID:    2,
		RoleUrn:           "",
	})
	_, err := provider.Get(ctx)
	if err == nil {
		t.Fatal("expected error when RoleUrn is empty")
	}
}

func TestStaticCredentialProviderReturnsFixed(t *testing.T) {
	expireAt := time.Now().Add(time.Hour)
	fixed := &Credentials{
		AccessKeyId:     "static-ak",
		AccessKeySecret: "static-sk",
		SecurityToken:   "static-token",
		ExpireAt:        expireAt,
	}
	p := StaticCredentialProvider{Cred: fixed}
	got, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != fixed {
		t.Fatalf("expected exact pointer, got different value: %+v", got)
	}

	// Verify format string only to avoid test failing on exact pointer comparison message
	_ = fmt.Sprintf("%+v", got)
}
