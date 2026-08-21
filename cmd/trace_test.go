package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/observability"
	"github.com/stretchr/testify/require"
)

type captureAppendWriter struct {
	records []observability.TraceRecord
}

func (w *captureAppendWriter) Append(record observability.TraceRecord) error {
	w.records = append(w.records, record)
	return nil
}

func (w *captureAppendWriter) Dir() string { return "" }

func (w *captureAppendWriter) Close(context.Context) error { return nil }

func TestTraceWriterFromEnvDisabledByDefault(t *testing.T) {
	writer, enabled, err := traceWriterFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatalf("traceWriterFromEnv: %v", err)
	}
	if enabled {
		t.Fatal("trace should be disabled by default")
	}
	if writer != nil {
		t.Fatalf("writer = %#v, want nil when disabled", writer)
	}
}

func TestTraceWriterFromEnvEnabled(t *testing.T) {
	traceDir := t.TempDir()
	writer, enabled, err := traceWriterFromEnv(func(key string) string {
		switch key {
		case "COMPSHARE_TRACE_SINK":
			return "file"
		case "COMPSHARE_TRACE_DIR":
			return traceDir
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("traceWriterFromEnv: %v", err)
	}
	if !enabled {
		t.Fatal("trace should be enabled when a sink is configured")
	}
	if writer == nil || writer.Dir() != traceDir {
		t.Fatalf("writer dir = %#v, want %q", writer, traceDir)
	}
}

func TestCleanupTraceWriterDeletesExpiredFiles(t *testing.T) {
	dir := t.TempDir()
	writer, err := observability.NewWriter(observability.WriterOptions{Dir: dir})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent-trace-2026-04-07.jsonl"), []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent-trace-2026-04-08.jsonl"), []byte("{}\n"), 0o600))

	err = cleanupTraceWriter(writer, time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(dir, "agent-trace-2026-04-07.jsonl"))
	require.FileExists(t, filepath.Join(dir, "agent-trace-2026-04-08.jsonl"))
}

func TestMutatingToolsFromEnvAndRuntimeLine(t *testing.T) {
	enabled, unknown := mutatingToolsEnabledFromEnv(func(string) string { return "" })
	require.False(t, enabled)
	require.Empty(t, unknown)
	require.Equal(t, "mutating=disabled (read-only mode)", mutatingToolsRuntimeLine(enabled))

	enabled, unknown = mutatingToolsEnabledFromEnv(func(key string) string {
		if key == "COMPSHARE_ENABLE_MUTATING_TOOLS" {
			return "1"
		}
		return ""
	})
	require.True(t, enabled)
	require.Empty(t, unknown)
	require.Equal(t, "mutating=enabled", mutatingToolsRuntimeLine(enabled))

	enabled, unknown = mutatingToolsEnabledFromEnv(func(key string) string {
		if key == "COMPSHARE_ENABLE_MUTATING_TOOLS" {
			return "yes"
		}
		return ""
	})
	require.False(t, enabled)
	require.Equal(t, "yes", unknown)
}

func TestKnowledgeRetrieverFromEnvRequiresMCP(t *testing.T) {
	retriever, err := knowledgeRetrieverFromEnv(func(string) string { return "" })

	require.ErrorContains(t, err, "COMPSHARE_KB_MCP_URL")
	require.Nil(t, retriever)
}

func TestKnowledgeRetrieverFromEnvBuildsMCP(t *testing.T) {
	retriever, err := knowledgeRetrieverFromEnv(func(key string) string {
		switch key {
		case "COMPSHARE_KB_MCP_URL":
			return "http://compshare-kb.prj-ucompshare-prod.svc.c5.u4/mcp"
		case "COMPSHARE_KB_MCP_TIMEOUT_MS":
			return "9000"
		default:
			return ""
		}
	})

	require.NoError(t, err)
	require.IsType(t, &knowledge.MCPRetriever{}, retriever)
}

func TestKnowledgeMCPTimeoutFromEnv(t *testing.T) {
	assertDuration := func(raw string, want time.Duration) {
		t.Helper()
		got := knowledgeMCPTimeoutFromEnv(func(key string) string {
			if key == "COMPSHARE_KB_MCP_TIMEOUT_MS" {
				return raw
			}
			return ""
		})
		if got != want {
			t.Fatalf("knowledgeMCPTimeoutFromEnv(%q) = %s, want %s", raw, got, want)
		}
	}
	assertDuration("", 0)
	assertDuration("12000", 12*time.Second)
	assertDuration("bad", 0)
}
