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

var _ RateLimiter = (*InMemoryRateLimiter)(nil)

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

func TestInMemoryRateLimiterQPSLimit(t *testing.T) {
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.Local)
	limiter := NewInMemoryRateLimiter(Limits{
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

func TestInMemoryRateLimiterUsesDefaultLimits(t *testing.T) {
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.Local)
	limiter := NewInMemoryRateLimiter(Limits{})

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

func TestInMemoryRateLimiterQPSRefillWithFakeClock(t *testing.T) {
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.Local)
	limiter := NewInMemoryRateLimiter(Limits{
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

func TestInMemoryRateLimiterWithClockUsedWhenRequestNowIsZero(t *testing.T) {
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.Local)
	limiter := NewInMemoryRateLimiter(Limits{
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

func TestInMemoryRateLimiterDailyQuota(t *testing.T) {
	now := time.Date(2026, 5, 9, 23, 30, 0, 0, time.FixedZone("CST", 8*3600))
	limiter := NewInMemoryRateLimiter(Limits{
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

func TestInMemoryRateLimiterSubjectsAndClassesAreIndependent(t *testing.T) {
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.Local)
	limiter := NewInMemoryRateLimiter(Limits{
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

func TestInMemoryRateLimiterReadExpensiveUsesSeparateBucket(t *testing.T) {
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.Local)
	limiter := NewInMemoryRateLimiter(Limits{
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

func TestConcurrentAllowNoRace(t *testing.T) {
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.Local)
	limiter := NewInMemoryRateLimiter(Limits{
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

// TestUserTurn_DisabledByDefault — when an operator hasn't configured
// UserTurnQPS / UserTurnDaily, the limiter MUST short-circuit Allow with
// Allowed=true regardless of how many requests come in. This is the
// migration-safe default: existing deployments that don't know about the
// new class must not see any behavior change. WHY: a "zero means default"
// promotion (the rule for LLM / Mutating / ReadExpensive) would silently
// gate every existing tenant on whatever the built-in default happened to
// be, breaking the migration contract.
func TestUserTurn_DisabledByDefault(t *testing.T) {
	limits := DefaultLimits()
	if limits.UserTurnQPS != 0 || limits.UserTurnDaily != 0 {
		t.Fatalf("DefaultLimits must leave UserTurn zero (opt-in); got qps=%d daily=%d",
			limits.UserTurnQPS, limits.UserTurnDaily)
	}
	limiter := NewInMemoryRateLimiter(limits)
	for i := 0; i < 1000; i++ {
		req := Request{
			SubjectKey: "tenant-a",
			Class:      ClassUserTurn,
			Action:     "chat_turn",
		}
		decision := limiter.Allow(req)
		if !decision.Allowed {
			t.Fatalf("request #%d denied while UserTurn class is disabled: %+v", i, decision)
		}
	}
}

// TestUserTurn_DailyExhaustion — daily=N, qps=0 means "no QPS check, just
// stop at N per day". Encodes the test-phase intent of "30 messages/day":
// QPS=0 must not block, daily must enforce, retry-after points to midnight.
// WHY: an early draft accidentally short-circuited the daily counter when
// QPS=0, defeating the cap entirely.
func TestUserTurn_DailyExhaustion(t *testing.T) {
	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.Local)
	limits := DefaultLimits()
	limits.UserTurnDaily = 3
	// qps stays 0 — daily-only cap
	limiter := NewInMemoryRateLimiter(limits, WithClock(func() time.Time { return now }))

	req := Request{SubjectKey: "tenant-a", Class: ClassUserTurn, Action: "chat_turn", Now: now}
	for i := 0; i < 3; i++ {
		decision := limiter.Allow(req)
		if !decision.Allowed {
			t.Fatalf("request #%d should be allowed (within daily cap), got denied: %+v", i, decision)
		}
	}
	decision := limiter.Allow(req)
	if decision.Allowed {
		t.Fatalf("4th request should be denied (daily cap = 3), got allowed: %+v", decision)
	}
	if decision.Reason != ReasonDailyExceeded {
		t.Fatalf("denial reason should be ReasonDailyExceeded, got %q", decision.Reason)
	}
	if decision.RetryAfter <= 0 || decision.RetryAfter > 24*time.Hour {
		t.Fatalf("retry-after should point to next-day reset (0,24h], got %v", decision.RetryAfter)
	}
}

// TestUserTurn_PerSubjectBucket — confirms each tenant has its own daily
// counter. WHY: a shared bucket would let one noisy tenant exhaust the
// cap for everyone, which is the bug this whole class is meant to prevent.
func TestUserTurn_PerSubjectBucket(t *testing.T) {
	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.Local)
	limits := DefaultLimits()
	limits.UserTurnDaily = 1
	limiter := NewInMemoryRateLimiter(limits, WithClock(func() time.Time { return now }))

	reqA := Request{SubjectKey: "tenant-a", Class: ClassUserTurn, Action: "chat_turn", Now: now}
	reqB := Request{SubjectKey: "tenant-b", Class: ClassUserTurn, Action: "chat_turn", Now: now}

	if !limiter.Allow(reqA).Allowed {
		t.Fatalf("tenant A's first request should be allowed")
	}
	if limiter.Allow(reqA).Allowed {
		t.Fatalf("tenant A's second request should be denied (cap=1)")
	}
	if !limiter.Allow(reqB).Allowed {
		t.Fatalf("tenant B's first request should be allowed despite A being exhausted")
	}
}
