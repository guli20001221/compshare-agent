package server

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/observability"
)

// TestDeriveStatus_Priority asserts the documented priority order in
// plan §7.7: chatErr (other than rate-limit) wins → "error";
// rate-limit (in either chatErr or trace) → "blocked"; engine hard-block
// → "blocked"; else "success".
//
// Encodes WHY: agent_messages and agent_traces both have status enums
// that operators filter on. If priority drifts, the same logical turn
// shows different status in the two tables, breaking join-by-status
// dashboards.
func TestDeriveStatus_Priority(t *testing.T) {
	cases := []struct {
		name    string
		chatErr error
		trace   observability.TraceRecord
		want    string
	}{
		{
			name: "clean success",
			want: "success",
		},
		{
			name:    "chatErr generic → error",
			chatErr: errors.New("upstream timeout"),
			want:    "error",
		},
		{
			name:    "chatErr rate-limited → blocked",
			chatErr: governance.ErrRateLimited,
			want:    "blocked",
		},
		{
			name:  "engine hard-block → blocked",
			trace: observability.TraceRecord{EngineHardBlock: observability.EngineHardBlockTrace{Hit: true, Category: "account_billing"}},
			want:  "blocked",
		},
		{
			name:  "rate-limit denial in trace → blocked",
			trace: observability.TraceRecord{RateLimit: observability.RateLimitTrace{Checked: true, Allowed: false, Reason: "qps_exceeded"}},
			want:  "blocked",
		},
		{
			name:  "rate-limit checked but allowed → success",
			trace: observability.TraceRecord{RateLimit: observability.RateLimitTrace{Checked: true, Allowed: true}},
			want:  "success",
		},
		{
			// chatErr wins over trace status: even if trace says success
			// the chat layer error must surface.
			name:    "chatErr beats clean trace",
			chatErr: errors.New("anything"),
			trace:   observability.TraceRecord{},
			want:    "error",
		},
		{
			// Explicit priority 1 > 3: a chatErr AND an engine hard-block
			// firing in the same turn must surface as "error" (the
			// caller's error wins). Encodes WHY: dashboards filtering
			// status="error" must include all turns where Engine.Chat
			// returned an error even when the engine also tripped a
			// hard-block guard along the way.
			name:    "chatErr beats hard-block",
			chatErr: errors.New("upstream stall"),
			trace:   observability.TraceRecord{EngineHardBlock: observability.EngineHardBlockTrace{Hit: true, Category: "account_billing"}},
			want:    "error",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveStatus(c.chatErr, c.trace); got != c.want {
				t.Fatalf("DeriveStatus(%s) = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

// TestMessageRecorder_RecordNonBlocking asserts the documented non-
// blocking semantics: when the queue is full, Record drops and returns
// without waiting for the worker. Same contract as MySQLWriter — DB
// outage MUST NOT freeze Engine.Chat.
func TestMessageRecorder_RecordNonBlocking(t *testing.T) {
	r := &MessageRecorder{
		queue:  make(chan MessageEntry, 1),
		logger: log.New(io.Discard, "", 0),
	}
	if err := r.Record(MessageEntry{RequestUUID: "first"}); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	done := make(chan struct{})
	go func() {
		if err := r.Record(MessageEntry{RequestUUID: "dropped"}); err != nil {
			t.Errorf("second Record returned error %v; expected silent drop", err)
		}
		close(done)
	}()
	select {
	case <-done:
		// Expected: returned promptly without blocking on the full queue.
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("Record blocked despite full queue; non-blocking contract broken")
	}
}

// TestMessageRecorder_NilReceiver_Noop guards the convenience that
// handler code can call r.Record on a nil recorder without crashing
// (the optional-recorder pattern).
func TestMessageRecorder_NilReceiver_Noop(t *testing.T) {
	var r *MessageRecorder
	if err := r.Record(MessageEntry{}); err != nil {
		t.Fatalf("nil-receiver Record returned err: %v", err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("nil-receiver Close returned err: %v", err)
	}
}

// TestNewMessageRecorder_EmptyDSNErrors mirrors MySQLWriter's empty-DSN
// guard so the two MySQL-backed sinks fail fast in the same shape.
func TestNewMessageRecorder_EmptyDSNErrors(t *testing.T) {
	r, err := NewMessageRecorder("", MessageRecorderOptions{})
	if err == nil {
		t.Fatalf("NewMessageRecorder(\"\") returned err=nil")
	}
	if r != nil {
		t.Fatalf("NewMessageRecorder(\"\") returned non-nil recorder on error")
	}
}

// TestNewMessageRecorder_UnreachableDSNErrors guards the ping-on-startup
// contract.
func TestNewMessageRecorder_UnreachableDSNErrors(t *testing.T) {
	r, err := NewMessageRecorder("root:none@tcp(127.0.0.1:1)/nodb", MessageRecorderOptions{})
	if err == nil {
		if r != nil {
			_ = r.db.Close()
		}
		t.Fatalf("NewMessageRecorder unreachable DSN returned err=nil")
	}
}
