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

func (w *captureAppendWriter) EmitStep(observability.StepTrace) error { return nil }

func (w *captureAppendWriter) Dir() string { return "" }

func (w *captureAppendWriter) Close(context.Context) error { return nil }

type captureEnqueueWriter struct {
	records []observability.TraceRecord
	tenants []observability.TenantContext
}

func (w *captureEnqueueWriter) Append(record observability.TraceRecord) error {
	w.records = append(w.records, record)
	w.tenants = append(w.tenants, observability.TenantContext{})
	return nil
}

func (w *captureEnqueueWriter) Enqueue(tenant observability.TenantContext, record observability.TraceRecord) error {
	w.records = append(w.records, record)
	w.tenants = append(w.tenants, tenant)
	return nil
}

func (w *captureEnqueueWriter) EmitStep(observability.StepTrace) error { return nil }

func (w *captureEnqueueWriter) Dir() string { return "" }

func (w *captureEnqueueWriter) Close(context.Context) error { return nil }

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
		case "COMPSHARE_TRACE_ENABLED":
			return "1"
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
		t.Fatal("trace should be enabled when COMPSHARE_TRACE_ENABLED=1")
	}
	if writer == nil || writer.Dir() != traceDir {
		t.Fatalf("writer dir = %#v, want %q", writer, traceDir)
	}
}

func TestDomainMatchGuardEnabledFromEnv_DefaultOff(t *testing.T) {
	// COMPSHARE_RAG_DOMAIN_MATCH_GUARD is DEFAULT-OFF (#5): the wrong-domain verdict
	// is always traced, but the refuse arm stays off until a flag-on eval proves
	// 0 over-refusal. unset/empty/explicit-negative => off; affirmative => on;
	// unknown => off + non-empty warn string per CLAUDE.md (never silently coerce).
	off := []string{"", "  ", "0", "off", "OFF", "false", "no", "disabled", "none"}
	for _, v := range off {
		got, unknown := domainMatchGuardEnabledFromEnv(func(string) string { return v })
		require.Falsef(t, got, "value %q should be off (default-off)", v)
		require.Emptyf(t, unknown, "value %q should not warn", v)
	}
	on := []string{"1", "on", "ON", "true", "TRUE", "yes", " On "}
	for _, v := range on {
		got, unknown := domainMatchGuardEnabledFromEnv(func(string) string { return v })
		require.Truef(t, got, "value %q should enable", v)
		require.Emptyf(t, unknown, "value %q should not warn", v)
	}
	got, unknown := domainMatchGuardEnabledFromEnv(func(string) string { return "maybe" })
	require.False(t, got, "unknown value treated as off")
	require.Equal(t, "maybe", unknown, "unknown value surfaced for caller warning")
}

func TestForcedKnowledgeHopEnabledFromEnv(t *testing.T) {
	// COMPSHARE_FORCED_KNOWLEDGE_HOP is OFF everywhere since 2026-08-01: Go-default
	// off, and the deploy config omits the key so this env var is what decides. unset/empty/explicit-negative => off; affirmative => on; unknown => off +
	// non-empty warn string per CLAUDE.md (never silently coerce).
	off := []string{"", "  ", "0", "off", "OFF", "false", "no", "disabled", "none"}
	for _, v := range off {
		got, unknown := forcedKnowledgeHopEnabledFromEnv(func(string) string { return v })
		require.Falsef(t, got, "value %q should be off (default-off)", v)
		require.Emptyf(t, unknown, "value %q should not warn", v)
	}
	on := []string{"1", "on", "ON", "true", "TRUE", "yes", " On "}
	for _, v := range on {
		got, unknown := forcedKnowledgeHopEnabledFromEnv(func(string) string { return v })
		require.Truef(t, got, "value %q should enable", v)
		require.Emptyf(t, unknown, "value %q should not warn", v)
	}
	got, unknown := forcedKnowledgeHopEnabledFromEnv(func(string) string { return "maybe" })
	require.False(t, got, "unknown value treated as off")
	require.Equal(t, "maybe", unknown, "unknown value surfaced for caller warning")
}

func TestMultiTraceWriterEnqueuePreservesTenantForMySQLLikeSink(t *testing.T) {
	fileSink := &captureAppendWriter{}
	mysqlSink := &captureEnqueueWriter{}
	writer := multiTraceWriter{fileSink, mysqlSink}
	tenant := observability.TenantContext{
		TopOrgID:     7,
		OrgID:        8,
		ConnectionID: "sess-1",
	}
	record := observability.TraceRecord{TraceID: "req-1"}

	require.NoError(t, writer.Enqueue(tenant, record))

	require.Len(t, fileSink.records, 1)
	require.Len(t, mysqlSink.records, 1)
	require.Len(t, mysqlSink.tenants, 1)
	require.Equal(t, tenant, mysqlSink.tenants[0])
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

func TestSessionFactContextEnabledFromEnv(t *testing.T) {
	enabled, unknown := sessionFactContextEnabledFromEnv(func(string) string { return "" })
	require.False(t, enabled)
	require.Empty(t, unknown)

	enabled, unknown = sessionFactContextEnabledFromEnv(func(key string) string {
		if key == "USE_SESSION_FACT_CONTEXT" {
			return "0"
		}
		return ""
	})
	require.False(t, enabled)
	require.Empty(t, unknown)

	enabled, unknown = sessionFactContextEnabledFromEnv(func(key string) string {
		if key == "USE_SESSION_FACT_CONTEXT" {
			return "1"
		}
		return ""
	})
	require.True(t, enabled)
	require.Empty(t, unknown)

	enabled, unknown = sessionFactContextEnabledFromEnv(func(key string) string {
		if key == "USE_SESSION_FACT_CONTEXT" {
			return "yes"
		}
		return ""
	})
	require.False(t, enabled)
	require.Equal(t, "yes", unknown)
}

func TestReactResultProjectionEnabledFromEnv(t *testing.T) {
	enabled, unknown := reactResultProjectionEnabledFromEnv(func(string) string { return "" })
	require.False(t, enabled)
	require.Empty(t, unknown)

	enabled, unknown = reactResultProjectionEnabledFromEnv(func(key string) string {
		if key == "USE_REACT_RESULT_PROJECTION" {
			return "0"
		}
		return ""
	})
	require.False(t, enabled)
	require.Empty(t, unknown)

	enabled, unknown = reactResultProjectionEnabledFromEnv(func(key string) string {
		if key == "USE_REACT_RESULT_PROJECTION" {
			return "1"
		}
		return ""
	})
	require.True(t, enabled)
	require.Empty(t, unknown)

	enabled, unknown = reactResultProjectionEnabledFromEnv(func(key string) string {
		if key == "USE_REACT_RESULT_PROJECTION" {
			return "yes"
		}
		return ""
	})
	require.False(t, enabled)
	require.Equal(t, "yes", unknown)
}

func TestReactHistoryCompactionEnabledFromEnv(t *testing.T) {
	enabled, unknown := reactHistoryCompactionEnabledFromEnv(func(string) string { return "" })
	require.False(t, enabled)
	require.Empty(t, unknown)

	enabled, unknown = reactHistoryCompactionEnabledFromEnv(func(key string) string {
		if key == "USE_REACT_HISTORY_COMPACTION" {
			return "0"
		}
		return ""
	})
	require.False(t, enabled)
	require.Empty(t, unknown)

	enabled, unknown = reactHistoryCompactionEnabledFromEnv(func(key string) string {
		if key == "USE_REACT_HISTORY_COMPACTION" {
			return "1"
		}
		return ""
	})
	require.True(t, enabled)
	require.Empty(t, unknown)

	enabled, unknown = reactHistoryCompactionEnabledFromEnv(func(key string) string {
		if key == "USE_REACT_HISTORY_COMPACTION" {
			return "yes"
		}
		return ""
	})
	require.False(t, enabled)
	require.Equal(t, "yes", unknown)
}

func TestKnowledgeRetrievalModeFromEnv(t *testing.T) {
	enabled, unknown := knowledgeRetrievalModeFromEnv(func(string) string { return "" })
	if !enabled || unknown != "" {
		t.Fatalf("unset knowledge retrieval = %v/%q, want curated default", enabled, unknown)
	}
	enabled, unknown = knowledgeRetrievalModeFromEnv(func(key string) string {
		if key == "USE_KNOWLEDGE_RETRIEVAL" {
			return " curated "
		}
		return ""
	})
	if !enabled || unknown != "" {
		t.Fatalf("curated mode = %v/%q, want enabled", enabled, unknown)
	}
	enabled, unknown = knowledgeRetrievalModeFromEnv(func(key string) string {
		if key == "USE_KNOWLEDGE_RETRIEVAL" {
			return "raw-chat"
		}
		return ""
	})
	if enabled || unknown != "raw-chat" {
		t.Fatalf("unknown mode = %v/%q, want disabled raw-chat", enabled, unknown)
	}
}

func TestKnowledgeRetrieverFromEnvRequiresMCP(t *testing.T) {
	retriever, enabled, err := knowledgeRetrieverFromEnv(func(key string) string {
		if key == "USE_KNOWLEDGE_RETRIEVAL" {
			return "curated"
		}
		return ""
	})

	require.ErrorContains(t, err, "COMPSHARE_KB_MCP_URL")
	require.False(t, enabled)
	require.Nil(t, retriever)
}

func TestKnowledgeRetrieverFromEnvBuildsMCP(t *testing.T) {
	retriever, enabled, err := knowledgeRetrieverFromEnv(func(key string) string {
		switch key {
		case "USE_KNOWLEDGE_RETRIEVAL":
			return "curated"
		case "COMPSHARE_KB_MCP_URL":
			return "http://compshare-kb.prj-ucompshare-prod.svc.c5.u4/mcp"
		case "COMPSHARE_KB_MCP_TIMEOUT_MS":
			return "9000"
		default:
			return ""
		}
	})

	require.NoError(t, err)
	require.True(t, enabled)
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
