// Isolation for the context-dependence replay harness.
//
// WHAT THE ISOLATION DIMENSION ACTUALLY IS: the TENANT, not the credentials.
//
// The first version of this stripped the STS service AK/SK and the platform
// AK/SK, on the theory that removing credentials removes risk. That was wrong in
// both directions. It bought no safety — the service key is the service's own,
// and which tenant it reaches is decided per request by the organization IDs in
// tools.UserContext, not by the key — and it destroyed the measurement: with no
// credentials every platform read failed, and the agent wrote replies like
// "实例状态查询暂时失败" that read as product behavior while actually being an
// auth failure wearing a product's clothes. 12 of 12 tool-calling turns in the
// first smoke were exactly that.
//
// So this keeps the credential material that makes a real call possible, and
// isolates on the thing that actually decides what gets touched:
//
//   - The TENANT is mandatory and explicit. The harness will not run against
//     whatever tenant a config happens to imply.
//   - PERSISTENCE is off by construction: the harness passes a nil DB, so no
//     session or message row is written. Clearing the DSN too means nothing can
//     open it by accident.
//   - WRITES keep production's tool surface (see below) but cannot complete,
//     because the harness supplies a ConfirmFunc that always denies.
//
// WHY mutating_tools IS NOT FORCED OFF. Production ships it on, and the flag
// does more than gate execution: internal/prompt/segment_readonly.go skips the
// whole read-only boundary section when writes are enabled, so forcing it off
// changes the SYSTEM PROMPT on every single turn — including the ~64% of turns
// that never touch a write tool. A replay generated under a prompt production
// does not use is not measuring production. Safety comes from the confirm gate
// and the tenant instead, which is also how 联调 runs.
package replayiso

import (
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/config"
)

// Tenant is the account a replay is allowed to touch. It is required, and there
// is deliberately no default: a replay whose tool surface includes writes must
// name its target account out loud.
type Tenant struct {
	TopOrganizationID uint32
	OrganizationID    uint32
	ProjectID         string
	UserEmail         string
}

func (t Tenant) validate() error {
	if t.TopOrganizationID == 0 || t.OrganizationID == 0 {
		return fmt.Errorf("replay tenant is required: pass the test account's top-org and org IDs explicitly")
	}
	return nil
}

// ProductionReach names a way the replay could touch something outside its own
// run, plus how to read and clear it. Deliberately short now: the previous
// version cleared credentials, which is not what this list is for.
type ProductionReach struct {
	Name  string
	Read  func(*config.Config) string
	Clear func(*config.Config)
}

func productionReaches() []ProductionReach {
	return []ProductionReach{
		// The production database. Cleared unconditionally; a replay that wants
		// persistence supplies its own DSN, which is checked against this one.
		{"agent.mysql.dsn", func(c *config.Config) string { return c.Agent.MySQL.DSN },
			func(c *config.Config) { c.Agent.MySQL = config.MySQLConfig{} }},
		// A replay has no business holding a live bot connection.
		{"agent.feishu.app_secret", func(c *config.Config) string { return c.Agent.Feishu.AppSecret },
			func(c *config.Config) { c.Agent.Feishu = config.FeishuConfig{} }},
		// NOT a safety strip: the endpoint is a ClusterIP and is not routable
		// outside the cluster, so leaving it set makes every retrieval time out
		// after 12s and return nothing, and the bundled corpus is never opened.
		// Clearing it is what puts the replay on the local pinned corpus — which
		// is NOT the production retrieval stack, and every report must say so.
		{"agent.retrieval.mcp_url", func(c *config.Config) string { return c.Agent.Retrieval.MCPURL },
			func(c *config.Config) { c.Agent.Retrieval.MCPURL = ""; c.Agent.Retrieval.MCPBearerToken = "" }},
	}
}

// InheritedFlag names a runtime flag a replay must NOT diverge on, because
// diverging silently changes the prompt or the tool window. The harness reports
// their values with the run rather than letting a reader assume production.
type InheritedFlag struct {
	Name string
	Read func(*config.Config) bool
}

func InheritedFlags() []InheritedFlag {
	return []InheritedFlag{
		{"features.mutating_tools", func(c *config.Config) bool {
			return c.Agent.Features.MutatingTools != nil && *c.Agent.Features.MutatingTools
		}},
		{"features.canonical_transcript", func(c *config.Config) bool {
			return c.Agent.Features.CanonicalTranscript != nil && *c.Agent.Features.CanonicalTranscript
		}},
		{"ssh_ops.enabled", func(c *config.Config) bool {
			return c.Agent.SSHOps.Enabled != nil && *c.Agent.SSHOps.Enabled
		}},
		{"ssh_ops.allow_writes", func(c *config.Config) bool { return c.Agent.SSHOps.AllowWrites }},
	}
}

// Isolated is a config a replay may run against, plus what had to change to get
// there. The report is returned rather than logged so the harness can print it
// beside the numbers and a reader can see the divergence.
type Isolated struct {
	Config   *config.Config
	Tenant   Tenant
	Stripped []string
	// Inherited records the production value of each flag the replay keeps, so a
	// report states the runtime it measured instead of implying production.
	Inherited map[string]bool
}

// LoadIsolatedReplayConfig prepares the deploy baseline for replay against an
// explicitly named tenant.
//
// It KEEPS STS and the platform keys: those are what make a real API call
// possible, and 联调 runs the same way. What it does not keep is anything that
// would write outside the replay, and what it will not do is guess a tenant.
// replayDSN, when non-empty, replaces the production DSN so the ssh-ops lane can
// run at its production setting (it needs a database for its fail-closed audit,
// and refuses to start without one). It is checked against the production value
// rather than trusted: "I passed the local one" is the same class of claim as
// the guard flags that were all silently false when a read-only probe created
// three billed instances.
func LoadIsolatedReplayConfig(baselinePath string, tenant Tenant, replayDSN string) (*Isolated, error) {
	if err := tenant.validate(); err != nil {
		return nil, err
	}
	cfg, err := config.Load(baselinePath)
	if err != nil {
		return nil, fmt.Errorf("load baseline %s: %w", baselinePath, err)
	}
	productionDSN := strings.TrimSpace(cfg.Agent.MySQL.DSN)
	if strings.TrimSpace(cfg.Agent.LLM.APIKey) == "" {
		return nil, fmt.Errorf("baseline %s has no agent.llm.api_key; the replay needs a model", baselinePath)
	}
	if strings.TrimSpace(cfg.Agent.STS.ServiceAK) == "" || strings.TrimSpace(cfg.Agent.STS.ServiceSK) == "" {
		return nil, fmt.Errorf("baseline %s has no STS service credentials; without them every platform read "+
			"fails and the agent reports the auth failure as a product outcome", baselinePath)
	}

	inherited := map[string]bool{}
	for _, flag := range InheritedFlags() {
		inherited[flag.Name] = flag.Read(cfg)
	}

	var stripped []string
	for _, reach := range productionReaches() {
		if strings.TrimSpace(reach.Read(cfg)) != "" {
			stripped = append(stripped, reach.Name)
		}
		reach.Clear(cfg)
	}
	// Verify rather than assume: the Clear funcs are hand-written and a
	// struct-valued reset can be undone by a later entry touching the same struct.
	for _, reach := range productionReaches() {
		if remaining := strings.TrimSpace(reach.Read(cfg)); remaining != "" {
			return nil, fmt.Errorf("isolation failed: %s survived the strip", reach.Name)
		}
	}

	if replay := strings.TrimSpace(replayDSN); replay != "" {
		if productionDSN != "" && replay == productionDSN {
			return nil, fmt.Errorf("the replay DSN is the production DSN; a replay must not write into the " +
				"database the product serves from")
		}
		cfg.Agent.MySQL.DSN = replay
	}

	if project := strings.TrimSpace(tenant.ProjectID); project != "" {
		cfg.Agent.ProjectId = project
	}
	return &Isolated{Config: cfg, Tenant: tenant, Stripped: stripped, Inherited: inherited}, nil
}
