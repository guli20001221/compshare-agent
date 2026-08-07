package engine

import (
	"strings"
	"time"
)

func (f ContextFrame) active(now time.Time) bool {
	if strings.TrimSpace(f.Kind) == "" || strings.TrimSpace(f.Status) == "" {
		return false
	}
	if f.Freshness == ContinuityFreshnessExpired {
		return false
	}
	ttl := f.TTLSeconds
	if ttl <= 0 {
		ttl = ContextFrameTTLSeconds
	}
	if f.ProducedAtUnix <= 0 {
		return false
	}
	return now.Unix()-f.ProducedAtUnix <= int64(ttl)
}

func (e *Engine) expireContextFrame(now time.Time) {
	if !e.sessionStateHydrated {
		return
	}
	if e.sessionState.ContextFrame.Kind == "" {
		return
	}
	if !e.sessionState.ContextFrame.active(now) {
		frame := e.sessionState.ContextFrame
		frame.Freshness = ContinuityFreshnessExpired
		// An expired frame remains useful for understanding, but every
		// previously trusted slot loses execution authority.
		frame.SlotSources = nil
		e.sessionState.ContextFrame = frame
		e.sessionState.SchemaVersion = SessionStateSchemaCurrent
		return
	}
	e.sessionState.ContextFrame.Freshness = continuityFreshness(
		e.sessionState.ContextFrame.ProducedAtUnix,
		effectiveContextFrameTTL(e.sessionState.ContextFrame),
		now,
	)
}

func (e *Engine) clearContextFrame() {
	if !e.sessionStateHydrated {
		return
	}
	e.sessionState.ContextFrame = ContextFrame{}
	e.sessionState.SchemaVersion = SessionStateSchemaCurrent
}

func (e *Engine) clearContextFrameForNewDirectWorkflow() {
	e.clearContextFrame()
}

func (e *Engine) clearCreateFamilyCarry() {
	if !e.sessionStateHydrated {
		return
	}
	e.clearContextFrame()
}

func (e *Engine) setContextFrame(frame ContextFrame) {
	if !e.sessionStateHydrated {
		return
	}
	if frame.Version == 0 {
		frame.Version = 1
	}
	if frame.TTLSeconds == 0 {
		frame.TTLSeconds = ContextFrameTTLSeconds
	}
	if frame.ProducedAtUnix == 0 {
		frame.ProducedAtUnix = time.Now().Unix()
	}
	frame.Freshness = ContinuityFreshnessFresh
	e.sessionState.ContextFrame = frame
	e.sessionState.SchemaVersion = SessionStateSchemaCurrent
}

func (e *Engine) activeContextFrame(now time.Time) (ContextFrame, bool) {
	if !e.sessionStateHydrated {
		return ContextFrame{}, false
	}
	frame := e.sessionState.ContextFrame
	if !frame.active(now) {
		e.expireContextFrame(now)
		return ContextFrame{}, false
	}
	return frame, true
}

func contextFrameCreateFamily(kind string) bool {
	return kind == ContextFrameKindCreate || kind == ContextFrameKindDeploy
}
