package governance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

const (
	AnonymousSubjectKey = "anonymous"

	DefaultLLMQPS             = 5
	DefaultLLMDaily           = 5000
	DefaultMutatingQPS        = 1
	DefaultMutatingDaily      = 50
	DefaultReadExpensiveQPS   = 6
	DefaultReadExpensiveDaily = 500
)

type Class string

const (
	ClassLLM               Class = "llm"
	ClassMutatingTool      Class = "mutating_tool"
	ClassReadExpensiveTool Class = "read_expensive_tool"
	// ClassUserTurn counts user-initiated chat turns (one
	// ClientMsgUserMessage frame). Confirm responses and pings do not
	// increment this counter. Unlike LLM / mutating / read-expensive, the
	// daily/qps fields default to 0 = disabled (no enforcement) rather
	// than to a built-in default — the operator opts in.
	ClassUserTurn Class = "user_turn"
)

type Reason string

const (
	ReasonNone          Reason = ""
	ReasonQPSExceeded   Reason = "qps_exceeded"
	ReasonDailyExceeded Reason = "daily_exceeded"
)

var ErrRateLimited = errors.New("rate limited")

type RateLimiter interface {
	Allow(Request) Decision
}

type Request struct {
	// SubjectKey must be pre-hashed with SubjectKeyFromPublicKey before it
	// reaches the limiter. Allow does not validate or hash raw key material.
	SubjectKey string
	Class      Class
	Action     string
	Now        time.Time
}

type Decision struct {
	Allowed     bool
	Class       Class
	Action      string
	Reason      Reason
	SubjectHash string
	RetryAfter  time.Duration
	Err         error
}

type Limits struct {
	LLMQPS             int
	LLMDaily           int
	MutatingQPS        int
	MutatingDaily      int
	ReadExpensiveQPS   int
	ReadExpensiveDaily int
	// UserTurnQPS / UserTurnDaily: per-tenant cap on user-initiated chat
	// turns. 0 = disabled (no enforcement for that dimension). Used by
	// the WS server path (test-phase guardrail). NOT subject to the
	// "zero means default" promotion that normalizeLimits applies to
	// the other classes — operator opts in explicitly.
	UserTurnQPS   int
	UserTurnDaily int
}

func DefaultLimits() Limits {
	return Limits{
		LLMQPS:             DefaultLLMQPS,
		LLMDaily:           DefaultLLMDaily,
		MutatingQPS:        DefaultMutatingQPS,
		MutatingDaily:      DefaultMutatingDaily,
		ReadExpensiveQPS:   DefaultReadExpensiveQPS,
		ReadExpensiveDaily: DefaultReadExpensiveDaily,
	}
}

type MemoryLimiter struct {
	mu     sync.Mutex
	limits Limits
	now    func() time.Time
	state  map[limitKey]*counterState
}

type Option func(*MemoryLimiter)

func WithClock(now func() time.Time) Option {
	return func(l *MemoryLimiter) {
		if now != nil {
			l.now = now
		}
	}
}

func NewMemoryLimiter(limits Limits, opts ...Option) *MemoryLimiter {
	l := &MemoryLimiter{
		limits: normalizeLimits(limits),
		now:    time.Now,
		state:  make(map[limitKey]*counterState),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

func (l *MemoryLimiter) Allow(req Request) Decision {
	now := req.Now
	if now.IsZero() {
		now = l.now()
	}
	if req.SubjectKey == "" {
		req.SubjectKey = AnonymousSubjectKey
	}

	qps, daily := l.limits.forClass(req.Class)
	// Disabled class: both dimensions unset → no enforcement. Used for
	// ClassUserTurn when operator hasn't opted in. Short-circuit before
	// touching counterState so we don't create map entries for a noop.
	if qps <= 0 && daily <= 0 {
		return Decision{
			Allowed:     true,
			Class:       req.Class,
			Action:      req.Action,
			SubjectHash: req.SubjectKey,
		}
	}
	key := limitKey{subject: req.SubjectKey, class: req.Class}

	l.mu.Lock()
	defer l.mu.Unlock()

	st := l.state[key]
	if st == nil {
		st = &counterState{
			tokens:       float64(qps),
			lastRefill:   now,
			dailyDateKey: localDateKey(now),
		}
		l.state[key] = st
	}
	st.resetDateIfNeeded(now)
	if qps > 0 {
		st.refill(now, qps)
	}

	if daily > 0 && st.dailyCount >= daily {
		return deniedDecision(req, ReasonDailyExceeded, retryAfterDaily(now))
	}
	if qps > 0 && st.tokens < 1 {
		return deniedDecision(req, ReasonQPSExceeded, retryAfterQPS(st.tokens, qps))
	}

	if qps > 0 {
		st.tokens -= 1
	}
	st.dailyCount++
	return Decision{
		Allowed:     true,
		Class:       req.Class,
		Action:      req.Action,
		SubjectHash: req.SubjectKey,
	}
}

func SubjectKeyFromPublicKey(publicKey string) (string, bool) {
	if publicKey == "" {
		return AnonymousSubjectKey, false
	}
	sum := sha256.Sum256([]byte(publicKey))
	return "sha256:" + hex.EncodeToString(sum[:]), true
}

// SubjectKeyFromTenant returns a hashed subject key derived from the console
// tenant identity (top_organization_id + organization_id). Used by the WS
// server path so each tenant gets its own rate-limit bucket; CLI path keeps
// SubjectKeyFromPublicKey because there is only one process-level identity.
//
// Format: "sha256:" + hex(sha256("top=<topOrgID>;org=<orgID>"))
// When both IDs are 0 returns AnonymousSubjectKey so MemoryLimiter still
// applies a quota bucket (rather than passing an empty SubjectKey which
// MemoryLimiter would normalize to AnonymousSubjectKey anyway).
func SubjectKeyFromTenant(topOrgID, orgID int64) string {
	if topOrgID == 0 && orgID == 0 {
		return AnonymousSubjectKey
	}
	raw := fmt.Sprintf("top=%d;org=%d", topOrgID, orgID)
	sum := sha256.Sum256([]byte(raw))
	return "sha256:" + hex.EncodeToString(sum[:])
}

type limitKey struct {
	subject string
	class   Class
}

type counterState struct {
	tokens       float64
	lastRefill   time.Time
	dailyDateKey string
	dailyCount   int
}

func (s *counterState) refill(now time.Time, qps int) {
	if now.Before(s.lastRefill) {
		s.lastRefill = now
		return
	}
	elapsed := now.Sub(s.lastRefill).Seconds()
	if elapsed <= 0 {
		return
	}
	capacity := float64(qps)
	s.tokens = math.Min(capacity, s.tokens+elapsed*float64(qps))
	s.lastRefill = now
}

func (s *counterState) resetDateIfNeeded(now time.Time) {
	dateKey := localDateKey(now)
	if s.dailyDateKey == dateKey {
		return
	}
	s.dailyDateKey = dateKey
	s.dailyCount = 0
}

func deniedDecision(req Request, reason Reason, retryAfter time.Duration) Decision {
	return Decision{
		Allowed:     false,
		Class:       req.Class,
		Action:      req.Action,
		Reason:      reason,
		SubjectHash: req.SubjectKey,
		RetryAfter:  retryAfter,
		Err:         ErrRateLimited,
	}
}

func (l Limits) forClass(class Class) (qps int, daily int) {
	switch class {
	case ClassMutatingTool:
		return l.MutatingQPS, l.MutatingDaily
	case ClassReadExpensiveTool:
		return l.ReadExpensiveQPS, l.ReadExpensiveDaily
	case ClassLLM:
		return l.LLMQPS, l.LLMDaily
	case ClassUserTurn:
		// Returned raw; both fields default to 0 = disabled. The
		// "qps<=0 && daily<=0 → no enforcement" short-circuit in Allow
		// handles the opt-in semantics. Do NOT fall through to the LLM
		// budget here — that would silently gate every existing tenant
		// on the LLM quota the moment they upgraded to a binary with
		// this class.
		return l.UserTurnQPS, l.UserTurnDaily
	default:
		// Phase 1 hardening defines LLM, mutating-tool, and read-expensive
		// quota classes. Unknown
		// future classes fall back to the LLM budget until config validation
		// grows an explicit class registry.
		return l.LLMQPS, l.LLMDaily
	}
}

func normalizeLimits(l Limits) Limits {
	defaults := DefaultLimits()
	if l.LLMQPS <= 0 {
		l.LLMQPS = defaults.LLMQPS
	}
	if l.LLMDaily <= 0 {
		l.LLMDaily = defaults.LLMDaily
	}
	if l.MutatingQPS <= 0 {
		l.MutatingQPS = defaults.MutatingQPS
	}
	if l.MutatingDaily <= 0 {
		l.MutatingDaily = defaults.MutatingDaily
	}
	if l.ReadExpensiveQPS <= 0 {
		l.ReadExpensiveQPS = defaults.ReadExpensiveQPS
	}
	if l.ReadExpensiveDaily <= 0 {
		l.ReadExpensiveDaily = defaults.ReadExpensiveDaily
	}
	return l
}

func retryAfterQPS(tokens float64, qps int) time.Duration {
	if qps <= 0 {
		return time.Second
	}
	missing := 1 - tokens
	if missing <= 0 {
		return 0
	}
	return time.Duration(math.Ceil((missing/float64(qps))*1000)) * time.Millisecond
}

func retryAfterDaily(now time.Time) time.Duration {
	y, m, d := now.Date()
	next := time.Date(y, m, d+1, 0, 0, 0, 0, now.Location())
	return next.Sub(now)
}

func localDateKey(now time.Time) string {
	return now.Format("2006-01-02")
}
