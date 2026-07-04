package engine

import (
	"strings"
	"time"

	"github.com/compshare-agent/internal/intent"
)

func newContextFrame(kind string, plan intent.IntentRoute, userMsg string, turn int, now time.Time) ContextFrame {
	return ContextFrame{
		Version:         1,
		Kind:            kind,
		Status:          ContextFrameStatusPending,
		Intent:          string(plan.Intent),
		OriginalUserMsg: strings.TrimSpace(userMsg),
		CreatedTurn:     turn,
		ProducedAtUnix:  now.Unix(),
		TTLSeconds:      ContextFrameTTLSeconds,
	}
}

func (f ContextFrame) active(now time.Time) bool {
	if strings.TrimSpace(f.Kind) == "" || strings.TrimSpace(f.Status) == "" {
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
		e.sessionState.ContextFrame = ContextFrame{}
	}
}

func (e *Engine) clearContextFrame() {
	if !e.sessionStateHydrated {
		return
	}
	e.sessionState.ContextFrame = ContextFrame{}
}

func (e *Engine) clearContextFrameForNewDirectWorkflow() {
	if !ContextContinuationEnabled() {
		return
	}
	e.clearContextFrame()
}

func (e *Engine) clearCreateFamilyCarry() {
	if !e.sessionStateHydrated {
		return
	}
	e.sessionState.ContextFrame = ContextFrame{}
	e.sessionState.PendingDeployModel = ""
	e.sessionState.LastDeployWorkload = ""
	e.sessionState.LastDeployZone = ""
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
	e.sessionState.ContextFrame = frame
	e.sessionState.SchemaVersion = SessionStateSchemaCurrent
}

func (e *Engine) activeContextFrame(now time.Time) (ContextFrame, bool) {
	if !e.sessionStateHydrated {
		return ContextFrame{}, false
	}
	frame := e.sessionState.ContextFrame
	if !frame.active(now) {
		if frame.Kind != "" {
			e.sessionState.ContextFrame = ContextFrame{}
		}
		return ContextFrame{}, false
	}
	return frame, true
}

func contextFrameCreateFamily(kind string) bool {
	return kind == ContextFrameKindCreate || kind == ContextFrameKindDeploy
}
