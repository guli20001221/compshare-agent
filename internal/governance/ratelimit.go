package governance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"sync"
	"time"
)

const (
	AnonymousSubjectKey = "anonymous"

	DefaultLLMQPS        = 5
	DefaultLLMDaily      = 5000
	DefaultMutatingQPS   = 1
	DefaultMutatingDaily = 50
)

type Class string

const (
	ClassLLM          Class = "llm"
	ClassMutatingTool Class = "mutating_tool"
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
	LLMQPS        int
	LLMDaily      int
	MutatingQPS   int
	MutatingDaily int
}

func DefaultLimits() Limits {
	return Limits{
		LLMQPS:        DefaultLLMQPS,
		LLMDaily:      DefaultLLMDaily,
		MutatingQPS:   DefaultMutatingQPS,
		MutatingDaily: DefaultMutatingDaily,
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
	st.refill(now, qps)

	if st.dailyCount >= daily {
		return deniedDecision(req, ReasonDailyExceeded, retryAfterDaily(now))
	}
	if st.tokens < 1 {
		return deniedDecision(req, ReasonQPSExceeded, retryAfterQPS(st.tokens, qps))
	}

	st.tokens -= 1
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
	case ClassLLM:
		return l.LLMQPS, l.LLMDaily
	default:
		// Phase 0 defines only LLM and mutating-tool quota classes. Unknown
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
