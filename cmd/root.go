package main

import "github.com/spf13/cobra"

var configPath string

var rootCmd = &cobra.Command{
	Use:   "compshare-agent",
	Short: "优云算力共享平台 AI 助手",
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "deploy/conf/agent.yaml", "配置文件路径")
}
