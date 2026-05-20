package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/renderer"
	"github.com/compshare-agent/internal/server"

	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "WebSocket server for console-deployment",
	RunE:  runServe,
}

func init() {
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Assemble shared engine deps once at startup — see plan §3.1 for
	// which fields are safe to share across sessions.
	deps, err := engine.NewSharedDeps(cfg)
	if err != nil {
		return fmt.Errorf("shared deps: %w", err)
	}
	if err := applySharedDepsFromEnv(deps, cfg, os.Getenv); err != nil {
		return fmt.Errorf("apply shared deps from env: %w", err)
	}

	// Trace sink + optional MySQL handle for /readyz ping. We share the
	// MySQL connection between the trace writer and readyz so a single
	// outage signal flips both signals consistently.
	traceSink, mysqlDB, err := assembleTraceSink(os.Getenv)
	if err != nil {
		return fmt.Errorf("trace sink: %w", err)
	}
	defer func() {
		if traceSink != nil {
			_ = traceSink.Close(context.Background())
		}
		if mysqlDB != nil {
			_ = mysqlDB.Close()
		}
	}()

	addr := os.Getenv("COMPSHARE_SERVE_ADDR")
	if addr == "" {
		addr = ":7777"
	}

	srv, err := server.New(server.Options{
		Addr:           addr,
		Deps:           deps,
		TraceSink:      traceSink,
		Model:          cfg.Agent.LLM.Model,
		TenantSource:   server.TenantSource(os.Getenv("COMPSHARE_TENANT_SOURCE")),
		AllowedOrigins: splitCSV(os.Getenv("COMPSHARE_WS_ORIGINS")),
		DB:             mysqlDB,
	})
	if err != nil {
		return err
	}
	log.Printf("compshare-agent serve listening on %s (tenant_source=%s)", addr, os.Getenv("COMPSHARE_TENANT_SOURCE"))
	return srv.Run(ctx)
}

// applySharedDepsFromEnv unifies CLI + server env-driven SharedDeps setup
// (plan §5.6). For PR4 it covers the planner / knowledge retriever /
// grounded renderer slots; the CLI in cmd/agent.go continues to call its
// existing setters for back-compat. A future PR can flip CLI to also call
// this helper so both code paths sit behind one env-reading function.
func applySharedDepsFromEnv(deps *engine.SharedDeps, cfg *config.Config, getenv func(string) string) error {
	cutoverIntents, unknownCutover := intentPlannerCutoverIntentsFromEnv(getenv)
	for _, value := range unknownCutover {
		log.Printf("warning: ignoring unknown USE_INTENT_PLANNER_FOR value %q", value)
	}

	knowledgeRetrievalRequested, unknownKnowledge := knowledgeRetrievalModeFromEnv(getenv)
	if unknownKnowledge != "" {
		log.Printf("warning: ignoring unknown USE_KNOWLEDGE_RETRIEVAL value %q", unknownKnowledge)
	}
	retriever, knowledgeEnabled, knowledgeErr := knowledgeRetrieverFromEnv(getenv)
	if knowledgeRetrievalRequested && knowledgeErr != nil {
		// RAG-enabled-but-failed is a hard startup gate per the CLI
		// applyKnowledgeRetrieverStartup helper. Server should fail the
		// same way: refusing to start beats silently disabling RAG.
		return fmt.Errorf("RAG enabled but corpus digest mismatch: %w", knowledgeErr)
	}
	if knowledgeEnabled {
		deps.KnowledgeRetriever = retriever
	}

	groundedMode, unknownGrounded := groundedRendererModeFromEnv(getenv)
	if unknownGrounded != "" {
		log.Printf("warning: ignoring unknown USE_GROUNDED_RENDERER value %q", unknownGrounded)
	}
	if groundedMode == "llm" {
		deps.GroundedRenderer = renderer.NewGroundedRenderer(llm.NewClient(cfg.Agent.LLM))
		deps.GroundedRendererModel = cfg.Agent.LLM.Model
	}

	cutoverEnabled := len(cutoverIntents) > 0
	if cutoverEnabled || knowledgeEnabled {
		deps.IntentPlanner = newCLIPlanner(cfg)
		deps.IntentPlannerModel = cfg.Agent.LLM.Model
		enabled, cutover := engine.BuildIntentPlannerMaps(cutoverIntents)
		deps.IntentPlannerEnabledIntents = enabled
		deps.IntentCutoverIntents = cutover
	}
	return nil
}

// assembleTraceSink mirrors traceWriterFromEnv but also returns the MySQL
// *sql.DB so /readyz can ping it. When the sink is file-only we return
// (writer, nil, nil).
func assembleTraceSink(getenv func(string) string) (observability.Writer, *sql.DB, error) {
	if getenv("COMPSHARE_TRACE_ENABLED") != "1" {
		return nil, nil, nil
	}
	sink := strings.ToLower(strings.TrimSpace(getenv("COMPSHARE_TRACE_SINK")))
	if sink == "" {
		sink = "file"
	}
	switch sink {
	case "file":
		w, err := observability.NewWriter(observability.WriterOptions{Dir: getenv("COMPSHARE_TRACE_DIR")})
		return w, nil, err
	case "mysql":
		dsn := getenv("MYSQL_DSN")
		w, err := observability.NewMySQLWriter(dsn, observability.MySQLWriterOptions{})
		if err != nil {
			return nil, nil, err
		}
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			_ = w.Close(context.Background())
			return nil, nil, fmt.Errorf("readyz mysql handle: %w", err)
		}
		return w, db, nil
	case "both":
		// Not exposed for server-side serve until ops asks for it; serve
		// currently picks file OR mysql, not both, to keep startup simple.
		return nil, nil, fmt.Errorf("COMPSHARE_TRACE_SINK=both is not supported in serve mode yet")
	default:
		return nil, nil, fmt.Errorf("unknown COMPSHARE_TRACE_SINK %q", sink)
	}
}

func splitCSV(v string) []string {
	if v == "" {
		return nil
	}
	out := []string{}
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// intent.Intent is referenced indirectly through engine.BuildIntentPlannerMaps's
// argument type. Keep this sentinel so the import stays valid even if a
// future refactor narrows the cmd surface.
var _ = intent.IntentResourceInfo
