package main

import (
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
	sink := strings.ToLower(strings.TrimSpace(getenv("COMPSHARE_TRACE_SINK")))
	if sink == "" {
		return nil, false, nil
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
	default:
		return nil, false, fmt.Errorf("unknown COMPSHARE_TRACE_SINK value %q (want file|mysql)", sink)
	}
}

func traceMySQLSinkEnabled(getenv getenvFunc) bool {
	sink := strings.ToLower(strings.TrimSpace(getenv("COMPSHARE_TRACE_SINK")))
	return sink == "mysql"
}

func cleanupTraceWriter(writer observability.Writer, now time.Time) error {
	if writer == nil {
		return nil
	}
	// The database writer returns ""; Cleanup is a no-op for it.
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

func knowledgeRetrieverFromEnv(getenv getenvFunc) (engine.KnowledgeRetriever, error) {
	endpoint := strings.TrimSpace(getenv("COMPSHARE_KB_MCP_URL"))
	if endpoint == "" {
		return nil, errors.New("COMPSHARE_KB_MCP_URL is required")
	}
	retriever, err := knowledge.NewMCPRetriever(knowledge.MCPRetrieverOptions{
		Endpoint:    endpoint,
		BearerToken: strings.TrimSpace(getenv("COMPSHARE_KB_MCP_BEARER_TOKEN")),
		Timeout:     knowledgeMCPTimeoutFromEnv(getenv),
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge MCP client: %w", err)
	}
	log.Printf("knowledge: using remote MCP endpoint %s", endpoint)
	return retriever, nil
}
