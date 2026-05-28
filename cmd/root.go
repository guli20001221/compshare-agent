package main

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/compshare-agent/internal/config"
	"github.com/spf13/cobra"
)

var configPath string

const (
	defaultConfigPath        = "deploy/conf/agent.yaml"
	defaultConfigExamplePath = "deploy/conf/agent.yaml.example"
)

var rootCmd = &cobra.Command{
	Use:   "compshare-agent",
	Short: "Compshare Copilot — 服务于优云算力共享平台",
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", defaultConfigPath, "配置文件路径")
}

func loadConfig() (*config.Config, error) {
	if configPath != defaultConfigPath {
		return config.Load(configPath)
	}
	candidates := []string{
		defaultConfigPath,
		filepath.Join("..", defaultConfigPath),
		defaultConfigExamplePath,
		filepath.Join("..", defaultConfigExamplePath),
	}
	var firstErr error
	for _, candidate := range candidates {
		cfg, err := config.Load(candidate)
		if err == nil {
			return cfg, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return nil, firstErr
}
