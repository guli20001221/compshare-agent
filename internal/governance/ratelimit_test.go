package governance

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var _ RateLimiter = (*MemoryLimiter)(nil)

func TestSubjectKeyFromOrganization(t *testing.T) {
	t.Run("nonzero pair produces sha256 key", func(t *testing.T) {
		key, ok := SubjectKeyFromOrganization(10, 20)
		if !ok {
			t.Fatalf("expected ok=true for valid pair")
		}
		if !strings.HasPrefix(key, "sha256:") {
			t.Fatalf("expected sha256: prefix, got %q", key)
		}
	})
	t.Run("zero topOrg returns anonymous", func(t *testing.T) {
		key, ok := SubjectKeyFromOrganization(0, 20)
		if ok {
			t.Fatalf("expected ok=false for topOrg=0")
		}
		if key != AnonymousSubjectKey {
			t.Fatalf("expected %q, got %q", AnonymousSubjectKey, key)
		}
	})
	t.Run("zero org returns anonymous", func(t *testing.T) {
		key, ok := SubjectKeyFromOrganization(10, 0)
		if ok {
			t.Fatalf("expected ok=false for org=0")
		}
		if key != AnonymousSubjectKey {
			t.Fatalf("expected %q, got %q", AnonymousSubjectKey, key)
		}
	})
	t.Run("deterministic and different pairs differ", func(t *testing.T) {
		k1a, _ := SubjectKeyFromOrganization(1, 2)
		k1b, _ := SubjectKeyFromOrganization(1, 2)
		if k1a != k1b {
			t.Fatalf("hash not deterministic: %q vs %q", k1a, k1b)
		}
		k2, _ := SubjectKeyFromOrganization(3, 4)
		if k1a == k2 {
			t.Fatalf("different pairs must produce different keys")
		}
	})
	t.Run("raw key material not leaked", func(t *testing.T) {
		key, ok := SubjectKeyFromOrganization(99999, 88888)
		if !ok {
			t.Fatalf("expected ok=true")
		}
		rendered := fmt.Sprintf("%v", key)
		if strings.Contains(rendered, "99999") || strings.Contains(rendered, "88888") {
			t.Fatalf("key leaked raw org IDs: %q", key)
		}
	})
}

func TestSubjectKeyFromPublicKey(t *testing.T) {
	subject, ok := SubjectKeyFromPublicKey("public-key-123")
	if !ok {
		t.Fatalf("SubjectKeyFromPublicKey returned ok=false for non-empty public key")
	}
	if !strings.HasPrefix(subject, "sha256:") {
		t.Fatalf("subject key should use sha256 prefix, got %q", subject)
	}
	if strings.Contains(subject, "public-key-123") {
		t.Fatalf("subject key leaked raw public key: %q", subject)
	}

	anonymous, ok := SubjectKeyFromPublicKey("")
	if ok {
		t.Fatalf("empty public key should return ok=false")
	}
	if anonymous != AnonymousSubjectKey {
		t.Fatalf("empty public key should return anonymous subject, got %q", anonymous)
	}
	if strings.HasPrefix(anonymous, "sha256:") {
		t.Fatalf("empty public key must not be hashed")
	}
}

func TestMemoryLimiterQPSLimit(t *testing.T) {
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.Local)
	limiter := NewMemoryLimiter(Limits{
		LLMQPS:        2,
		LLMDaily:      100,
		MutatingQPS:   1,
		MutatingDaily: 50,
	})

	req := Request{SubjectKey: "sha256:subject", Class: ClassLLM, Action: "main_react_chat", Now: now}
	assertAllowed(t, limiter.Allow(req))
	assertAllowed(t, limiter.Allow(req))

	denied := limiter.Allow(req)
	assertDenied(t, denied, ReasonQPSExceeded)
	if denied.RetryAfter <= 0 {
		t.Fatalf("qps denial should include retry_after, got %s", denied.RetryAfter)
	}
}

func TestMemoryLimiterUsesDefaultLimits(t *testing.T) {
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.Local)
	limiter := NewMemoryLimiter(Limits{})

	req := Request{SubjectKey: "sha256:subject", Class: ClassLLM, Action: "main_react_chat", Now: now}
	for i := 0; i < DefaultLLMQPS; i++ {
		assertAllowed(t, limiter.Allow(req))
	}
	assertDenied(t, limiter.Allow(req), ReasonQPSExceeded)

	req.Class = ClassReadExpensiveTool
	req.Action = "GetCompShareInstanceMonitor"
	for i := 0; i < DefaultReadExpensiveQPS; i++ {
		assertAllowed(t, limiter.Allow(req))
	}
	assertDenied(t, limiter.Allow(req), ReasonQPSExceeded)
}

func TestMemoryLimiterQPSRefillWithFakeClock(t *testing.T) {
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.Local)
	limiter := NewMemoryLimiter(Limits{
		LLMQPS:        1,
		LLMDaily:      100,
		MutatingQPS:   1,
		MutatingDaily: 50,
	})

	req := Request{SubjectKey: "sha256:subject", Class: ClassLLM, Action: "main_react_chat", Now: now}
	assertAllowed(t, limiter.Allow(req))
	assertDenied(t, limiter.Allow(req), ReasonQPSExceeded)

	req.Now = now.Add(time.Second)
	assertAllowed(t, limiter.Allow(req))
}

func TestMemoryLimiterWithClockUsedWhenRequestNowIsZero(t *testing.T) {
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.Local)
	limiter := NewMemoryLimiter(Limits{
		LLMQPS:        1,
		LLMDaily:      100,
		MutatingQPS:   1,
		MutatingDaily: 50,
	}, WithClock(func() time.Time {
		return now
	}))

	req := Request{SubjectKey: "sha256:subject", Class: ClassLLM, Action: "main_react_chat"}
	assertAllowed(t, limiter.Allow(req))
	assertDenied(t, limiter.Allow(req), ReasonQPSExceeded)

	now = now.Add(time.Second)
	assertAllowed(t, limiter.Allow(req))
}

func TestMemoryLimiterDailyQuota(t *testing.T) {
	now := time.Date(2026, 5, 9, 23, 30, 0, 0, time.FixedZone("CST", 8*3600))
	limiter := NewMemoryLimiter(Limits{
		LLMQPS:        10,
		LLMDaily:      2,
		MutatingQPS:   1,
		MutatingDaily: 50,
	})

	req := Request{SubjectKey: "sha256:subject", Class: ClassLLM, Action: "main_react_chat", Now: now}
	assertAllowed(t, limiter.Allow(req))
	assertAllowed(t, limiter.Allow(req))

	denied := limiter.Allow(req)
	assertDenied(t, denied, ReasonDailyExceeded)
	if denied.RetryAfter != 30*time.Minute {
		t.Fatalf("daily retry_after should point to next local midnight, got %s", denied.RetryAfter)
	}

	req.Now = now.Add(31 * time.Minute)
	assertAllowed(t, limiter.Allow(req))
}

func TestMemoryLimiterSubjectsAndClassesAreIndependent(t *testing.T) {
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.Local)
	limiter := NewMemoryLimiter(Limits{
		LLMQPS:        1,
		LLMDaily:      1,
		MutatingQPS:   1,
		MutatingDaily: 1,
	})

	req := Request{SubjectKey: "sha256:subject-a", Class: ClassLLM, Action: "main_react_chat", Now: now}
	assertAllowed(t, limiter.Allow(req))
	assertDenied(t, limiter.Allow(req), ReasonDailyExceeded)

	req.SubjectKey = "sha256:subject-b"
	assertAllowed(t, limiter.Allow(req))

	req.SubjectKey = "sha256:subject-a"
	req.Class = ClassMutatingTool
	req.Action = "StartCompShareInstance"
	assertAllowed(t, limiter.Allow(req))

	req.SubjectKey = "sha256:subject-a"
	req.Class = ClassReadExpensiveTool
	req.Action = "GetCompShareInstanceMonitor"
	assertAllowed(t, limiter.Allow(req))
}

func TestMemoryLimiterReadExpensiveUsesSeparateBucket(t *testing.T) {
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.Local)
	limiter := NewMemoryLimiter(Limits{
		LLMQPS:             1,
		LLMDaily:           1,
		MutatingQPS:        1,
		MutatingDaily:      1,
		ReadExpensiveQPS:   2,
		ReadExpensiveDaily: 2,
	})

	req := Request{SubjectKey: "sha256:subject", Class: ClassReadExpensiveTool, Action: "GetCompShareInstanceMonitor", Now: now}
	assertAllowed(t, limiter.Allow(req))
	assertAllowed(t, limiter.Allow(req))
	assertDenied(t, limiter.Allow(req), ReasonDailyExceeded)
}

func TestMemoryLimiterDoesNotLeakRawPublicKey(t *testing.T) {
	rawPublicKey := "public-key-that-must-not-appear"
	subject, ok := SubjectKeyFromPublicKey(rawPublicKey)
	if !ok {
		t.Fatalf("SubjectKeyFromPublicKey returned ok=false")
	}
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.Local)
	limiter := NewMemoryLimiter(Limits{
		LLMQPS:        1,
		LLMDaily:      100,
		MutatingQPS:   1,
		MutatingDaily: 50,
	})

	req := Request{SubjectKey: subject, Class: ClassLLM, Action: "main_react_chat", Now: now}
	assertAllowed(t, limiter.Allow(req))
	denied := limiter.Allow(req)
	assertDenied(t, denied, ReasonQPSExceeded)

	rendered := fmt.Sprintf("%+v %v", denied, denied.Err)
	if strings.Contains(rendered, rawPublicKey) {
		t.Fatalf("decision/error leaked raw public key: %s", rendered)
	}
}

func TestConcurrentAllowNoRace(t *testing.T) {
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.Local)
	limiter := NewMemoryLimiter(Limits{
		LLMQPS:        10,
		LLMDaily:      100,
		MutatingQPS:   1,
		MutatingDaily: 50,
	})

	const goroutines = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	var allowed atomic.Int64

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			decision := limiter.Allow(Request{
				SubjectKey: "sha256:subject",
				Class:      ClassLLM,
				Action:     "main_react_chat",
				Now:        now,
			})
			if decision.Allowed {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := allowed.Load(); got != 10 {
		t.Fatalf("allowed count = %d, want 10", got)
	}
}

func assertAllowed(t *testing.T, decision Decision) {
	t.Helper()
	if !decision.Allowed {
		t.Fatalf("expected allow, got %#v", decision)
	}
	if decision.Err != nil {
		t.Fatalf("allowed decision should not include error, got %v", decision.Err)
	}
}

func assertDenied(t *testing.T, decision Decision, reason Reason) {
	t.Helper()
	if decision.Allowed {
		t.Fatalf("expected denial, got allow: %#v", decision)
	}
	if decision.Reason != reason {
		t.Fatalf("expected reason %q, got %q", reason, decision.Reason)
	}
	if !errors.Is(decision.Err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", decision.Err)
	}
}
