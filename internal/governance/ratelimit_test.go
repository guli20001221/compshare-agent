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

func TestSubjectKeyFromTenant_BothZero_ReturnsAnonymous(t *testing.T) {
	got := SubjectKeyFromTenant(0, 0)
	if got != AnonymousSubjectKey {
		t.Fatalf("expected anonymous subject for zero tenant ids, got %q", got)
	}
	if strings.HasPrefix(got, "sha256:") {
		t.Fatalf("zero tenant ids must not be hashed, got %q", got)
	}
}

func TestSubjectKeyFromTenant_DifferentTenants_DifferentKeys(t *testing.T) {
	keyA := SubjectKeyFromTenant(66391350, 151147646)
	keyB := SubjectKeyFromTenant(66882207, 151640760)
	if keyA == keyB {
		t.Fatalf("different tenants must produce different subject keys, both got %q", keyA)
	}
	if !strings.HasPrefix(keyA, "sha256:") {
		t.Fatalf("subject key should use sha256 prefix, got %q", keyA)
	}
	if strings.Contains(keyA, "66391350") || strings.Contains(keyA, "151147646") {
		t.Fatalf("subject key leaked raw tenant ids: %q", keyA)
	}
}

func TestSubjectKeyFromTenant_SameTenant_DeterministicKey(t *testing.T) {
	keyA := SubjectKeyFromTenant(66391350, 151147646)
	keyB := SubjectKeyFromTenant(66391350, 151147646)
	if keyA != keyB {
		t.Fatalf("same tenant must produce identical subject keys, got %q vs %q", keyA, keyB)
	}
}

func TestSubjectKeyFromTenant_OnlyOrgID_StillHashes(t *testing.T) {
	// 仅给 organization_id（top_organization_id 缺失）不应退化为 anonymous——
	// 限流仍需为这部分流量分桶。
	got := SubjectKeyFromTenant(0, 151147646)
	if got == AnonymousSubjectKey {
		t.Fatalf("partial tenant info must still produce a hashed subject, got anonymous")
	}
	if !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("expected sha256 prefix, got %q", got)
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
	limiter := NewMemoryLimiter(limits)
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
	limiter := NewMemoryLimiter(limits, WithClock(func() time.Time { return now }))

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
	limiter := NewMemoryLimiter(limits, WithClock(func() time.Time { return now }))

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
