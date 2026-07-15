package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/httpapi"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/ocr"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/turncoord"
	"github.com/gin-gonic/gin"
	"github.com/spf13/cobra"
)

const serverTraceDrainTimeout = 5 * time.Second

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "启动 HTTP 服务",
	RunE:  runServer,
}

func init() {
	serverCmd.Flags().String("addr", "", "覆盖配置的监听地址")
	rootCmd.AddCommand(serverCmd)
}

func runServer(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if addr, _ := cmd.Flags().GetString("addr"); addr != "" {
		cfg.Agent.HTTP.ListenAddr = addr
	}
	if err := validateServerConfig(cfg); err != nil {
		return err
	}

	db, err := store.OpenMySQL(cfg.Agent.MySQL)
	if err != nil {
		return err
	}
	defer db.Close()

	sessionStore := store.NewSessionStore(db)
	messageStore := store.NewMessageStore(db)
	feedbackStore := store.NewFeedbackStore(db)

	// overlayGetenv makes the YAML runtime-flag sections (agent.features /
	// retrieval / trace / planner) win over the OS env, with env as the
	// fallback for any field omitted in YAML. Every flag the server reads below
	// goes through it so a single config.yaml can configure the whole server.
	overlayGetenv := cfg.RuntimeGetenv(os.Getenv)
	serverGetenv := serverTraceGetenv(overlayGetenv, cfg.Agent.MySQL.DSN)
	if traceMySQLSinkEnabled(serverGetenv) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := store.VerifyTraceSchema(ctx, db); err != nil {
			cancel()
			return fmt.Errorf("%w; run deploy/migrations/0002_create_agent_traces.sql before enabling PostgreSQL trace persistence", err)
		}
		cancel()
	}
	traceWriter, traceEnabled, traceErr := traceWriterFromEnv(serverGetenv)
	if traceErr != nil {
		return fmt.Errorf("trace writer setup: %w", traceErr)
	}
	if traceEnabled {
		if err := cleanupTraceWriter(traceWriter, time.Now()); err != nil {
			log.Printf("warning: trace cleanup failed: %v", err)
		}
		defer closeServerTraceWriter(traceWriter)
	}

	pool, err := buildHTTPServerPool(cfg, messageStore, overlayGetenv)
	if err != nil {
		return err
	}
	defer pool.Close()

	handlers := newServerHandlers(cfg, sessionStore, messageStore, feedbackStore, pool, traceWriter, overlayGetenv)
	switch value := overlayGetenv("COMPSHARE_DURABLE_TURNS"); value {
	case "1":
		// OpenMySQL has already run the full column-level VerifySchema probe,
		// including every durable turn/lease/event/interaction table. Construct
		// the coordinator only after that fail-fast check succeeds.
		coordinatorOptions, err := serverTurnCoordinatorOptions(overlayGetenv, traceWriter)
		if err != nil {
			return err
		}
		coordinator := turncoord.NewCoordinator(
			store.NewPostgresTurnStore(db),
			sessionStore,
			turncoord.EngineFactoryFromPool(pool),
			coordinatorOptions,
		)
		handlers.SetTurnCoordinator(coordinator)
		defer coordinator.Close()
		log.Printf("durable turns enabled: every WebSocket chat uses the globally fenced commit path")
	case "", "0":
		// Compatibility-only mode for local rollback and old tests.
	default:
		return fmt.Errorf("unknown COMPSHARE_DURABLE_TURNS value %q", value)
	}
	if cfg.Agent.OCR.Model != "" {
		handlers.SetOCRClient(ocr.NewClient(cfg.Agent.OCR))
		log.Printf("OCR enabled: model=%s", cfg.Agent.OCR.Model)
	}
	router := gin.New()
	if !cfg.Agent.HTTP.DisableCORS {
		router.Use(corsMiddleware())
	}
	router.Use(gin.CustomRecovery(func(c *gin.Context, recovered any) {
		c.JSON(http.StatusInternalServerError, httpapi.Response{
			RetCode: 226618,
			Message: fmt.Sprint(recovered),
		})
	}))
	router.GET("/healthz", httpapi.Healthz)
	// GET / is the WebSocket upgrade for streaming chat (gateway Action
	// CreateCSAgentWS); POST / serves the non-streaming Actions (meta, session,
	// feedback) the frontend still sends over HTTP. The gateway does not pass
	// through SSE, so chat streaming moved to WS — see
	// docs/plans/2026-06-03-websocket-transport-refactor.md.
	router.GET("/", handlers.HandleWS)
	router.POST("/", handlers.Dispatch)
	router.OPTIONS("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	// WriteTimeout stays 0 (the config default already enforces this): a
	// non-zero server-level write deadline would abort a long-lived streaming
	// connection mid-turn — true for both the former SSE path and the WS path.
	// The per-connection context deadline (maxWSConnLifetime) bounds WS
	// connections. ReadTimeout governs only the pre-upgrade request read; after
	// websocket.Accept hijacks the conn it no longer applies to the read loop,
	// so the configured value is safe to keep.
	srv := &http.Server{
		Addr:         cfg.Agent.HTTP.ListenAddr,
		Handler:      router,
		ReadTimeout:  cfg.Agent.HTTP.ReadTimeout,
		WriteTimeout: cfg.Agent.HTTP.WriteTimeout,
	}
	return serveUntilSignal(srv)
}

func serverTurnCoordinatorOptions(getenv getenvFunc, traceWriter observability.Writer) (turncoord.Options, error) {
	encodedKey := strings.TrimSpace(getenv("COMPSHARE_TURN_SECRET_KEY"))
	secretKey, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil || len(secretKey) != 32 {
		return turncoord.Options{}, fmt.Errorf("COMPSHARE_TURN_SECRET_KEY must be base64 for exactly 32 random bytes")
	}
	return turncoord.Options{
		ReplicaID:            serverReplicaID(),
		MutatingToolsEnabled: getenv("COMPSHARE_ENABLE_MUTATING_TOOLS") == "1",
		TraceWriter:          traceWriter,
		SecretKey:            secretKey,
	}, nil
}

type interactionFeatureSetter interface {
	SetConfirmFormEnabled(bool)
	SetGuidedCreateEnabled(bool)
}

func newServerHandlers(
	cfg *config.Config,
	sessions store.SessionStore,
	messages store.MessageStore,
	feedback store.FeedbackStore,
	pool httpapi.EnginePool,
	traceWriter observability.Writer,
	getenv func(string) string,
) *httpapi.Handlers {
	handlers := httpapi.NewHandlers(cfg, sessions, messages, feedback, pool, traceWriter)
	configureInteractionFeatures(handlers, getenv)
	return handlers
}

// configureInteractionFeatures installs the server half of the feature gate
// for both the durable and compatibility transports. Durable confirmations now
// persist the reviewed form and resolution, so withholding these capabilities
// when COMPSHARE_DURABLE_TURNS=1 would silently disable the production path.
func configureInteractionFeatures(handlers interactionFeatureSetter, getenv func(string) string) {
	confirmForm := serverFeatureEnabled(getenv, "COMPSHARE_CONFIRM_FORM")
	if confirmForm {
		handlers.SetConfirmFormEnabled(true)
		log.Printf("confirm form enabled: opted-in clients may review and edit a persisted confirmation card")
	}
	guidedCreate := serverFeatureEnabled(getenv, "COMPSHARE_GUIDED_CREATE")
	if guidedCreate && !confirmForm {
		log.Printf("warning: COMPSHARE_GUIDED_CREATE requires COMPSHARE_CONFIRM_FORM=1, treating guided create as off")
		return
	}
	if guidedCreate {
		handlers.SetGuidedCreateEnabled(true)
		log.Printf("guided create enabled: opted-in clients use the persisted guided GPU create flow")
	}
}

func serverFeatureEnabled(getenv func(string) string, name string) bool {
	switch value := getenv(name); value {
	case "1":
		return true
	case "", "0":
		return false
	default:
		log.Printf("warning: unknown %s value %q, treating as off", name, value)
		return false
	}
}

func serverReplicaID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

func closeServerTraceWriter(writer observability.Writer) {
	if writer == nil {
		return
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), serverTraceDrainTimeout)
	defer cancel()
	if err := writer.Close(drainCtx); err != nil {
		log.Printf("warning: trace writer drain failed: %v", err)
	}
}

func serverTraceGetenv(getenv getenvFunc, mysqlDSN string) getenvFunc {
	return func(key string) string {
		if key == "MYSQL_DSN" {
			return mysqlDSN
		}
		return getenv(key)
	}
}

func validateServerConfig(cfg *config.Config) error {
	if cfg.Agent.MySQL.DSN == "" {
		return fmt.Errorf("agent.mysql.dsn is required for server")
	}
	if cfg.Agent.Meta.Welcome == "" {
		return fmt.Errorf("agent.meta.welcome is required for server")
	}
	if len(cfg.Agent.Meta.SuggestedPrompts) == 0 {
		return fmt.Errorf("agent.meta.suggested_prompts is required for server")
	}
	if cfg.Agent.HTTP.MaxInputLength != cfg.Agent.Meta.MaxInputLength {
		return fmt.Errorf("agent.http.max_input_length must equal agent.meta.max_input_length")
	}
	stsEnabled := cfg.Agent.STS.ServiceAK != "" || cfg.Agent.STS.ServiceSK != ""
	if !stsEnabled {
		if cfg.Agent.PublicKey == "" {
			return fmt.Errorf("agent.public_key is required for server when agent.sts.service_ak is empty")
		}
		if cfg.Agent.PrivateKey == "" {
			return fmt.Errorf("agent.private_key is required for server when agent.sts.service_sk is empty")
		}
		return nil
	}
	if cfg.Agent.STS.ServiceAK == "" {
		return fmt.Errorf("agent.sts.service_ak is required when agent.sts.service_sk is set")
	}
	if cfg.Agent.STS.ServiceSK == "" {
		return fmt.Errorf("agent.sts.service_sk is required when agent.sts.service_ak is set")
	}
	if cfg.Agent.STS.URL == "" {
		return fmt.Errorf("agent.sts.url is required for server")
	}
	if cfg.Agent.STS.RoleUrnTemplate == "" && cfg.Agent.STS.DefaultRoleUrn == "" {
		return fmt.Errorf("agent.sts.role_urn_template or agent.sts.default_role_urn is required for server")
	}
	return nil
}

func serveUntilSignal(srv *http.Server) error {
	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-sigCh:
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}

// corsMiddleware allows browser clients on any origin to call the agent.
// Permissive by design — the agent sits behind the console gateway in prod,
// so origin enforcement lives there; locally we accept everything to keep
// front-end dev simple. In prod set agent.http.disable_cors: true so the
// gateway is the sole source of Access-Control-Allow-* headers.
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			origin = "*"
		}
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Vary", "Origin")
		c.Header("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Accept, X-Requested-With")
		c.Header("Access-Control-Max-Age", "600")
		c.Next()
	}
}
