package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/httpapi"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/ocr"
	"github.com/compshare-agent/internal/store"
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

	serverGetenv := serverTraceGetenv(os.Getenv, cfg.Agent.MySQL.DSN)
	if traceMySQLSinkEnabled(serverGetenv) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := store.VerifyTraceSchema(ctx, db); err != nil {
			cancel()
			return fmt.Errorf("%w; run deploy/migrations/0002_create_agent_traces.sql before enabling MySQL trace persistence", err)
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

	pool, err := buildHTTPServerPool(cfg, messageStore, os.Getenv)
	if err != nil {
		return err
	}
	defer pool.Close()

	handlers := httpapi.NewHandlers(cfg, sessionStore, messageStore, feedbackStore, pool, traceWriter)
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
	router.POST("/", handlers.Dispatch)
	router.OPTIONS("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	srv := &http.Server{
		Addr:         cfg.Agent.HTTP.ListenAddr,
		Handler:      router,
		ReadTimeout:  cfg.Agent.HTTP.ReadTimeout,
		WriteTimeout: cfg.Agent.HTTP.WriteTimeout,
	}
	return serveUntilSignal(srv)
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
