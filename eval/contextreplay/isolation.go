// Isolation for the context-dependence replay harness.
//
// The harness needs exactly one thing from the deploy config — the model
// credentials — and must not inherit anything else from it. deploy/conf/
// config.local.yaml carries a live PostgreSQL DSN, live STS service AK/SK, live
// platform AK/SK, a Feishu app secret, mutating_tools: true, and an in-cluster
// mcp_url. Booting a replay against that file would transact against the
// production database and the production account.
//
// This is written as CODE rather than as a second YAML file for two reasons.
// A copied file goes stale: the day a new production-reach field is added to the
// baseline, a copy silently keeps whatever default it has, while this strip has
// a test that enumerates the fields and fails. And a copy would be a THIRD place
// holding live keys.
//
// The guard deliberately does not depend on the author having been careful.
// A read-only probe in this repo once created three billed 4090 instances
// because three separate guard flags were all silently false, so the rule here
// is that isolation must be asserted structurally, by
// TestReplayIsolationStripsEveryProductionReach, not established by review.
package main

import (
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/config"
)

// ProductionReach names a way the loaded config could touch production, plus how
// to read and clear it. The test walks this table, so adding a reach here is what
// makes it covered — and a reach that exists but is missing from the table is
// what the baseline-scan half of the test is for.
type ProductionReach struct {
	Name  string
	Read  func(*config.Config) string
	Clear func(*config.Config)
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return ""
}

// productionReaches is the full set this harness refuses to inherit.
func productionReaches() []ProductionReach {
	return []ProductionReach{
		{"agent.mysql.dsn", func(c *config.Config) string { return c.Agent.MySQL.DSN },
			func(c *config.Config) { c.Agent.MySQL = config.MySQLConfig{} }},
		{"agent.sts.service_ak", func(c *config.Config) string { return c.Agent.STS.ServiceAK },
			func(c *config.Config) { c.Agent.STS = config.STSConfig{} }},
		{"agent.sts.service_sk", func(c *config.Config) string { return c.Agent.STS.ServiceSK },
			func(c *config.Config) { c.Agent.STS = config.STSConfig{} }},
		{"agent.public_key", func(c *config.Config) string { return c.Agent.PublicKey },
			func(c *config.Config) { c.Agent.PublicKey = "" }},
		{"agent.private_key", func(c *config.Config) string { return c.Agent.PrivateKey },
			func(c *config.Config) { c.Agent.PrivateKey = "" }},
		{"agent.project_id", func(c *config.Config) string { return c.Agent.ProjectId },
			func(c *config.Config) { c.Agent.ProjectId = "" }},
		{"agent.features.mutating_tools", func(c *config.Config) string {
			return boolText(c.Agent.Features.MutatingTools != nil && *c.Agent.Features.MutatingTools)
		}, func(c *config.Config) { off := false; c.Agent.Features.MutatingTools = &off }},
		{"agent.retrieval.mcp_url", func(c *config.Config) string { return c.Agent.Retrieval.MCPURL },
			func(c *config.Config) { c.Agent.Retrieval.MCPURL = ""; c.Agent.Retrieval.MCPBearerToken = "" }},
		{"agent.feishu.app_secret", func(c *config.Config) string { return c.Agent.Feishu.AppSecret },
			func(c *config.Config) { c.Agent.Feishu = config.FeishuConfig{} }},
		{"agent.ssh_ops.enabled", func(c *config.Config) string {
			return boolText(c.Agent.SSHOps.Enabled != nil && *c.Agent.SSHOps.Enabled)
		}, func(c *config.Config) { off := false; c.Agent.SSHOps.Enabled = &off; c.Agent.SSHOps.AllowWrites = false }},
	}
}

// LoadIsolatedReplayConfig reads the deploy baseline for its model credentials
// and returns a config that cannot reach production.
//
// mcp_url is cleared for a reason beyond safety: the endpoint is a ClusterIP,
// which by design is only routable from inside the cluster, so leaving it set
// makes every retrieval time out after 12s and return nothing — and the bundled
// corpus is never opened. Clearing it is what puts the harness on the local
// pinned corpus. That corpus is NOT the production retrieval stack, and any
// report from this harness has to say so.
func LoadIsolatedReplayConfig(baselinePath string) (*config.Config, []string, error) {
	cfg, err := config.Load(baselinePath)
	if err != nil {
		return nil, nil, fmt.Errorf("load baseline %s: %w", baselinePath, err)
	}
	if strings.TrimSpace(cfg.Agent.LLM.APIKey) == "" {
		return nil, nil, fmt.Errorf("baseline %s has no agent.llm.api_key; the replay needs a model", baselinePath)
	}

	var stripped []string
	for _, reach := range productionReaches() {
		if strings.TrimSpace(reach.Read(cfg)) != "" {
			stripped = append(stripped, reach.Name)
		}
		reach.Clear(cfg)
	}

	// Verify rather than assume. Clear() above is hand-written per field and a
	// struct-valued reset can be undone by a later entry that resets the same
	// struct, so re-read every reach and fail loudly instead of returning a
	// config that only looks isolated.
	for _, reach := range productionReaches() {
		if remaining := strings.TrimSpace(reach.Read(cfg)); remaining != "" {
			return nil, nil, fmt.Errorf("isolation failed: %s survived the strip", reach.Name)
		}
	}
	return cfg, stripped, nil
}
