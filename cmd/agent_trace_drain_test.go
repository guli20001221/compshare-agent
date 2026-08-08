package main

// Trace-writer shutdown contract.
//
// This file used to also pin cliTraceDrainTimeout, the per-process drain budget the CLI applied at
// exit. The CLI is gone, and with it that constant; what survives is the half that was never
// CLI-specific — the server closes its trace writer on shutdown too (closeServerTraceWriter in
// server.go), and that call is only correct while Close is part of the Writer contract.

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/observability"
)

// TestObservabilityWriterInterface_HasCloseMethod is a compile-time guard that the Writer contract
// still includes Close(ctx). closeServerTraceWriter depends on it to drain MySQLWriter's queue on
// shutdown rather than merely dropping the db handle; if a refactor removes Close from the
// interface, the var declaration below stops compiling at the same moment that call does — making
// the dependency explicit instead of leaving queued traces to be lost silently.
func TestObservabilityWriterInterface_HasCloseMethod(t *testing.T) {
	var _ observability.Writer = (*noopWriter)(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := (&noopWriter{}).Close(ctx); err != nil {
		t.Fatalf("noopWriter.Close returned err=%v", err)
	}
}

// noopWriter is a minimal observability.Writer implementation used to type-check the interface. NOT
// a replacement for MySQLWriter in real drain behavior tests — that lives in
// internal/observability/mysql_writer_test.go.
type noopWriter struct{}

func (*noopWriter) Append(observability.TraceRecord) error { return nil }
func (*noopWriter) Dir() string                            { return "" }
func (*noopWriter) Close(context.Context) error            { return nil }
