package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/observability"
)

type getenvFunc func(string) string

func traceWriterFromEnv(getenv getenvFunc) (observability.Writer, bool, error) {
	if getenv("COMPSHARE_TRACE_ENABLED") != "1" {
		return nil, false, nil
	}
	sink := strings.ToLower(strings.TrimSpace(getenv("COMPSHARE_TRACE_SINK")))
	if sink == "" {
		sink = "file"
	}
	dir := getenv("COMPSHARE_TRACE_DIR")
	dsn := getenv("MYSQL_DSN")

	switch sink {
	case "file":
		writer, err := observability.NewWriter(observability.WriterOptions{Dir: dir})
		if err != nil {
			return nil, false, err
		}
		return writer, true, nil
	case "mysql":
		writer, err := observability.NewMySQLWriter(dsn, observability.MySQLWriterOptions{})
		if err != nil {
			return nil, false, err
		}
		return writer, true, nil
	case "both":
		fileW, err := observability.NewWriter(observability.WriterOptions{Dir: dir})
		if err != nil {
			return nil, false, err
		}
		mysqlW, err := observability.NewMySQLWriter(dsn, observability.MySQLWriterOptions{})
		if err != nil {
			_ = fileW.Close(context.Background())
			return nil, false, err
		}
		return multiTraceWriter{fileW, mysqlW}, true, nil
	default:
		return nil, false, fmt.Errorf("unknown COMPSHARE_TRACE_SINK value %q (want file|mysql|both)", sink)
	}
}

func traceMySQLSinkEnabled(getenv getenvFunc) bool {
	if getenv("COMPSHARE_TRACE_ENABLED") != "1" {
		return false
	}
	sink := strings.ToLower(strings.TrimSpace(getenv("COMPSHARE_TRACE_SINK")))
	return sink == "mysql" || sink == "both"
}

// multiTraceWriter fans out a TraceRecord to multiple sinks. Used when
// COMPSHARE_TRACE_SINK=both during cutover (run file + mysql side-by-side
// to compare). Failures from any individual sink are logged-then-ignored
// so one sink's downtime does not stall the other.
type multiTraceWriter []observability.Writer

func (m multiTraceWriter) Append(rec observability.TraceRecord) error {
	for _, w := range m {
		if err := w.Append(rec); err != nil {
			log.Printf("trace sink append failed (sink dir=%q): %v", w.Dir(), err)
		}
	}
	return nil
}

func (m multiTraceWriter) EmitStep(step observability.StepTrace) error {
	var firstErr error
	for _, w := range m {
		if err := w.EmitStep(step); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m multiTraceWriter) Enqueue(tenant observability.TenantContext, rec observability.TraceRecord) error {
	for _, w := range m {
		if enqueuer, ok := w.(interface {
			Enqueue(observability.TenantContext, observability.TraceRecord) error
		}); ok {
			if err := enqueuer.Enqueue(tenant, rec); err != nil {
				log.Printf("trace sink enqueue failed (sink dir=%q): %v", w.Dir(), err)
			}
			continue
		}
		if err := w.Append(rec); err != nil {
			log.Printf("trace sink append failed (sink dir=%q): %v", w.Dir(), err)
		}
	}
	return nil
}

func (m multiTraceWriter) Dir() string {
	for _, w := range m {
		if d := w.Dir(); d != "" {
			return d
		}
	}
	return ""
}

func (m multiTraceWriter) Close(ctx context.Context) error {
	var firstErr error
	for _, w := range m {
		if err := w.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func cleanupTraceWriter(writer observability.Writer, now time.Time) error {
	if writer == nil {
		return nil
	}
	// MySQLWriter returns "" — Cleanup is a no-op on empty dir, which is
	// correct (nothing to delete on disk for the db-backed sink).
	dir := writer.Dir()
	if dir == "" {
		return nil
	}
	return observability.Cleanup(dir, observability.DefaultTraceRetentionDays, now)
}

func mutatingToolsEnabledFromEnv(getenv getenvFunc) (bool, string) {
	value := strings.TrimSpace(getenv("COMPSHARE_ENABLE_MUTATING_TOOLS"))
	switch value {
	case "":
		return false, ""
	case "1":
		return true, ""
	default:
		return false, value
	}
}

func mutatingToolsRuntimeLine(enabled bool) string {
	if enabled {
		return "mutating=enabled"
	}
	return "mutating=disabled (read-only mode)"
}

func reactResultProjectionEnabledFromEnv(getenv getenvFunc) (bool, string) {
	value := strings.TrimSpace(getenv("USE_REACT_RESULT_PROJECTION"))
	switch value {
	case "", "0":
		return false, ""
	case "1":
		return true, ""
	default:
		return false, value
	}
}

// canonicalTranscriptEnabledFromEnv parses COMPSHARE_CANONICAL_TRANSCRIPT, the
// single gate over the whole transcript pipeline: capture, persistence, and
// whether a prior turn's tool calls and tool results reach the model instead of
// being deleted and paraphrased back through semantic state. Default off, and off
// means none of the three happens — so a rollout starts collecting history the
// moment it is flipped on, rather than finding it already there.
func canonicalTranscriptEnabledFromEnv(getenv getenvFunc) (bool, string) {
	raw := strings.TrimSpace(getenv("COMPSHARE_CANONICAL_TRANSCRIPT"))
	switch strings.ToLower(raw) {
	case "", "0", "off", "no", "false", "disabled", "none":
		return false, ""
	case "1", "true", "yes", "on":
		return true, ""
	default:
		return false, raw
	}
}

func knowledgeRetrievalModeFromEnv(getenv getenvFunc) (bool, string) {
	raw := strings.ToLower(strings.TrimSpace(getenv("USE_KNOWLEDGE_RETRIEVAL")))
	switch raw {
	case "", "curated":
		return true, ""
	case "off", "none", "disabled", "false", "0":
		return false, ""
	default:
		return false, raw
	}
}

func knowledgeMCPTimeoutFromEnv(getenv getenvFunc) time.Duration {
	raw := strings.TrimSpace(getenv("COMPSHARE_KB_MCP_TIMEOUT_MS"))
	if raw == "" {
		return 0
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms <= 0 {
		log.Printf("knowledge MCP: invalid COMPSHARE_KB_MCP_TIMEOUT_MS=%q; using default", raw)
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

func knowledgeRetrieverFromEnv(getenv getenvFunc) (engine.KnowledgeRetriever, bool, error) {
	enabled, unknown := knowledgeRetrievalModeFromEnv(getenv)
	if unknown != "" || !enabled {
		return nil, false, nil
	}
	endpoint := strings.TrimSpace(getenv("COMPSHARE_KB_MCP_URL"))
	if endpoint == "" {
		return nil, false, errors.New("COMPSHARE_KB_MCP_URL is required when knowledge retrieval is enabled")
	}
	retriever, err := knowledge.NewMCPRetriever(knowledge.MCPRetrieverOptions{
		Endpoint:    endpoint,
		BearerToken: strings.TrimSpace(getenv("COMPSHARE_KB_MCP_BEARER_TOKEN")),
		Timeout:     knowledgeMCPTimeoutFromEnv(getenv),
	})
	if err != nil {
		return nil, false, fmt.Errorf("knowledge MCP client: %w", err)
	}
	log.Printf("knowledge: using remote MCP endpoint %s", endpoint)
	return retriever, true, nil
}
