package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestServiceLinkedRoleBootstrapperSuccessOnFirstAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0})
	}))
	defer srv.Close()

	b := NewServiceLinkedRoleBootstrapper(NewUAccountClient(srv.URL),
		"compshare", "ucs-service-role/ServiceRoleForCompshare")
	b.PropagateWait = 0 // keep test fast

	if err := b.Bootstrap(context.Background(), 42); err != nil {
		t.Fatalf("Bootstrap error: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("ICreate calls = %d, want 1", got)
	}
}

func TestServiceLinkedRoleBootstrapperRetriesOnError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 500, "Message": "transient"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0})
	}))
	defer srv.Close()

	b := NewServiceLinkedRoleBootstrapper(NewUAccountClient(srv.URL),
		"compshare", "ucs-service-role/ServiceRoleForCompshare")
	b.CreateBackoff = time.Millisecond
	b.PropagateWait = 0

	if err := b.Bootstrap(context.Background(), 42); err != nil {
		t.Fatalf("Bootstrap error: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("ICreate calls = %d, want 3", got)
	}
}

func TestServiceLinkedRoleBootstrapperGivesUpAfterRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 403, "Message": "denied"})
	}))
	defer srv.Close()

	b := NewServiceLinkedRoleBootstrapper(NewUAccountClient(srv.URL),
		"compshare", "ucs-service-role/ServiceRoleForCompshare")
	b.CreateRetries = 3
	b.CreateBackoff = time.Millisecond
	b.PropagateWait = 0

	err := b.Bootstrap(context.Background(), 42)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("ICreate calls = %d, want 3", got)
	}
}

func TestServiceLinkedRoleBootstrapperHonorsContextDuringBackoff(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 500, "Message": "transient"})
	}))
	defer srv.Close()

	b := NewServiceLinkedRoleBootstrapper(NewUAccountClient(srv.URL),
		"compshare", "ucs-service-role/ServiceRoleForCompshare")
	b.CreateRetries = 10
	b.CreateBackoff = 100 * time.Millisecond
	b.PropagateWait = 0

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := b.Bootstrap(ctx, 42)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("backoff did not abort on ctx done; elapsed=%v", elapsed)
	}
}

func TestServiceLinkedRoleBootstrapperPropagationWaitHonorsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0})
	}))
	defer srv.Close()

	b := NewServiceLinkedRoleBootstrapper(NewUAccountClient(srv.URL),
		"compshare", "ucs-service-role/ServiceRoleForCompshare")
	b.PropagateWait = 10 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := b.Bootstrap(ctx, 42)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("propagation wait did not abort on ctx done; elapsed=%v", elapsed)
	}
}

func TestServiceLinkedRoleBootstrapperRejectsNilClient(t *testing.T) {
	b := &ServiceLinkedRoleBootstrapper{
		ServiceName: "compshare", RoleName: "ucs-service-role/ServiceRoleForCompshare",
	}
	if err := b.Bootstrap(context.Background(), 42); err == nil {
		t.Fatal("expected error with nil client")
	}
}

func TestServiceLinkedRoleBootstrapperRejectsEmptyServiceOrRole(t *testing.T) {
	b := NewServiceLinkedRoleBootstrapper(NewUAccountClient("http://localhost:0"), "", "")
	if err := b.Bootstrap(context.Background(), 42); err == nil {
		t.Fatal("expected error with empty ServiceName/RoleName")
	}
}
