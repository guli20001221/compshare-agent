package main

import (
	"context"
	"fmt"
	"log"
	"os/signal"
	"syscall"

	feishuadapter "github.com/compshare-agent/internal/feishu"
	"github.com/compshare-agent/internal/store"
	"github.com/spf13/cobra"
)

var feishuCmd = &cobra.Command{
	Use:   "feishu",
	Short: "启动飞书话题群知识问答机器人",
	RunE:  runFeishu,
}

func init() {
	rootCmd.AddCommand(feishuCmd)
}

func runFeishu(_ *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var options []feishuadapter.ServiceOption
	if cfg.Agent.Feishu.ExternalImageOAuth.Enabled {
		db, err := store.OpenMySQL(cfg.Agent.MySQL)
		if err != nil {
			return fmt.Errorf("open OAuth token database: %w", err)
		}
		defer db.Close()
		if err := feishuadapter.VerifyExternalImageOAuthSchema(ctx, db); err != nil {
			return fmt.Errorf("%w; run deploy/migrations/0012_create_feishu_oauth_tokens.sql before enabling agent.feishu.external_image_oauth", err)
		}
		provider, err := feishuadapter.NewExternalImageTokenProvider(ctx, cfg.Agent.Feishu, db)
		if err != nil {
			return fmt.Errorf("initialize external-group image authorization: %w", err)
		}
		options = append(options, feishuadapter.WithExternalImageUserToken(provider))
		log.Printf("Feishu external-group image fallback enabled with an encrypted delegated token")
	}
	service, err := feishuadapter.NewService(ctx, cfg.Agent.Feishu, options...)
	if err != nil {
		return err
	}
	return service.Run(ctx)
}
