//go:build live

package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/compshare-agent/internal/config"
)

// loadLiveConfig centralizes the deliberately opt-in configuration check for
// live probes. It contains no production behavior and keeps probes independent
// of any one evaluation harness.
func loadLiveConfig(t *testing.T) *config.Config {
	t.Helper()
	path := os.Getenv("COMPSHARE_LIVE_CONFIG")
	if path == "" {
		path = filepath.Join("..", "..", "deploy", "conf", "config.local.yaml")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config %s: %v", path, err)
	}
	if cfg.Agent.LLM.APIKey == "" || cfg.Agent.LLM.Model == "" {
		t.Fatalf("config %s has no usable agent.llm (model=%q)", path, cfg.Agent.LLM.Model)
	}
	return cfg
}

func splitJSONLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i <= len(raw); i++ {
		if i == len(raw) || raw[i] == '\n' {
			line := raw[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if len(line) > 0 {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	return out
}
