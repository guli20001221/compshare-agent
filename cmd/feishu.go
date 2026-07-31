package main

import (
	"context"
	"os/signal"
	"syscall"

	feishuadapter "github.com/compshare-agent/internal/feishu"
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
	service, err := feishuadapter.NewService(ctx, cfg.Agent.Feishu)
	if err != nil {
		return err
	}
	return service.Run(ctx)
}
