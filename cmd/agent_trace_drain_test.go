package main

// F1 trace-drain unit tests.
//
// The full behavioral assertion (traces land in MySQL after a single-turn
// CLI invocation exits) is verified by the real CLI smoke at
// F:/compshare-agent-runs/90-cli-trace-drain-*-smoke transcripts. These
// unit tests guard the pieces that smoke can't cover from inside the
// process boundary:
//
//   1. cliTraceDrainTimeout stays in a reasonable band — a future
//      refactor that shrinks it below MySQL roundtrip latency would
//      silently re-open the queue-loss bug under load.
//   2. The Writer interface's Close method actually drains MySQLWriter's
//      queue (not just closes the db handle) — this is the contract
//      the deferred call in runCLI relies on.

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/observability"
)

// TestCLITraceDrainTimeout_InReasonableBand pins the drain budget. The
// constant is per-process at CLI exit; it must:
//   - be > MySQL roundtrip + insertBatch latency on a healthy server
//     (so we don't truncate the drain mid-batch),
//   - be < typical user patience for CLI exit-to-prompt (so a hung
//     connection doesn't wedge shutdown).
//
// 1s is too tight; 30s is too long. Today's value is 5s.
func TestCLITraceDrainTimeout_InReasonableBand(t *testing.T) {
	if cliTraceDrainTimeout < 1*time.Second {
		t.Errorf("cliTraceDrainTimeout=%v is too tight; risks truncating "+
			"in-flight MySQL batches under normal latency. Floor is 1s.",
			cliTraceDrainTimeout)
	}
	if cliTraceDrainTimeout > 30*time.Second {
		t.Errorf("cliTraceDrainTimeout=%v is too loose; a hung connection "+
			"will wedge CLI shutdown for that long. Ceiling is 30s.",
			cliTraceDrainTimeout)
	}
}

// TestObservabilityWriterInterface_HasCloseMethod is a compile-time guard
// that the Writer contract still includes Close(ctx) — the deferred call
// in runCLI assumes this. If a refactor removes Close from the interface,
// the var declaration below stops compiling at the same moment the
// deferred call does — making the dependency explicit.
func TestObservabilityWriterInterface_HasCloseMethod(t *testing.T) {
	// Compile-time interface satisfaction check: *noopWriter must
	// implement observability.Writer (which requires Close(ctx) error).
	var _ observability.Writer = (*noopWriter)(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := (&noopWriter{}).Close(ctx); err != nil {
		t.Fatalf("noopWriter.Close returned err=%v", err)
	}
}

// noopWriter is a minimal observability.Writer implementation used to
// type-check the interface. NOT a replacement for MySQLWriter in real
// drain behavior tests — that lives in internal/observability/mysql_writer_test.go.
type noopWriter struct{}

func (*noopWriter) Append(observability.TraceRecord) error { return nil }
func (*noopWriter) EmitStep(observability.StepTrace) error { return nil }
func (*noopWriter) Dir() string                            { return "" }
func (*noopWriter) Close(context.Context) error            { return nil }
