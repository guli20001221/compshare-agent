package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	// RateLimitConfig.Limits returns the governance type directly so engine
	// wiring does not duplicate field mapping.
	"github.com/compshare-agent/internal/governance"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Agent AgentConfig `yaml:"agent"`
}

// STSConfig holds settings for UCloud STS (Security Token Service) credential
// generation. All fields are resolved as optional placeholders at Load time;
// the server sub-command validates the required subset before starting.
type STSConfig struct {
	ServiceAK          string        `yaml:"service_ak"`
	ServiceSK          string        `yaml:"service_sk"`
	URL                string        `yaml:"url"`
	RoleUrnTemplate    string        `yaml:"role_urn_template"`
	DefaultRoleUrn     string        `yaml:"default_role_urn"`
	DefaultSessionName string        `yaml:"default_session_name"`
	DurationSeconds    int           `yaml:"duration_seconds"`
	RefreshBefore      time.Duration `yaml:"refresh_before"`
	// IAMURL is the internal UAccount endpoint used to bootstrap per-tenant
	// service-linked roles on demand when AssumeRole returns RoleNotExist
	// (RetCode 11277). Optional: when empty, RoleNotExist errors are returned
	// to the caller unchanged. Reachable only from the UCloud private network.
	IAMURL string `yaml:"iam_url"`
}

type AgentConfig struct {
	LLM       LLMConfig       `yaml:"llm"`
	RateLimit RateLimitConfig `yaml:"rate_limit"`
	Executor  string          `yaml:"executor"` // "external" or "internal"

	CompShareAPIURL string `yaml:"compshare_api_url"`
	PublicKey       string `yaml:"public_key"`
	PrivateKey      string `yaml:"private_key"`
	Region          string `yaml:"region"`
	// ProjectId is the CompShare project ID required by some APIs
	// (e.g. UpdateCompShareStopScheduler). Optional: if empty, the
	// engine will attempt to discover it via GetProjectList at Init.
	ProjectId string `yaml:"project_id"`

	HTTP   HTTPConfig   `yaml:"http"`
	MySQL  MySQLConfig  `yaml:"mysql"`
	Meta   MetaConfig   `yaml:"meta"`
	STS    STSConfig    `yaml:"sts"`
	OCR    OCRConfig    `yaml:"ocr"`
	Feishu FeishuConfig `yaml:"feishu"`

	// Runtime feature flags, previously env-only (read in cmd/). These are
	// overlaid on os.Getenv via RuntimeGetenv with "YAML wins, env fallback"
	// precedence — see runtime.go. Omitting any section preserves the prior
	// env-driven behavior unchanged.
	Features  FeaturesConfig  `yaml:"features"`
	Retrieval RetrievalConfig `yaml:"retrieval"`
	WebSearch WebSearchConfig `yaml:"web_search"`
	Trace     TraceConfig     `yaml:"trace"`
	SSHOps    SSHOpsConfig    `yaml:"ssh_ops"`
}

// FeishuConfig connects a Feishu application bot to the Agent's WebSocket
// endpoint. The adapter is started explicitly with `compshare-agent feishu`.
type FeishuConfig struct {
	AppID          string   `yaml:"app_id"`
	AppSecret      string   `yaml:"app_secret"`
	AgentWSURL     string   `yaml:"agent_ws_url"`
	CompanyID      uint32   `yaml:"company_id"`
	OrganizationID uint32   `yaml:"organization_id"`
	ProjectID      string   `yaml:"project_id"`
	UserEmail      string   `yaml:"user_email"`
	AllowedChatIDs []string `yaml:"allowed_chat_ids"`
	// AutoReplyNewTopics answers a topic_group root message and a direct
	// follow-up by that topic's initiator without requiring an @ mention when
	// AutoReplyAllMessages is disabled. Replies beneath another participant's
	// comment otherwise still require an @ mention.
	AutoReplyNewTopics bool `yaml:"auto_reply_new_topics"`
	// AutoReplyAllMessages answers every user message in an allowlisted group
	// without requiring an @ mention. It supersedes AutoReplyNewTopics while
	// retaining the chat allowlist and the adapter's bot-message loop guard.
	AutoReplyAllMessages bool `yaml:"auto_reply_all_messages"`
	// EnablePlatformReadOnlyQueries switches the Feishu adapter from its legacy
	// RAG-only window to the fail-closed public-platform query window. That
	// window contains only publicly safe catalog/inventory reads; it never
	// exposes account, instance, diagnostic, or mutating tools.
	EnablePlatformReadOnlyQueries bool `yaml:"enable_platform_readonly_queries"`
	// EnableConsoleHandoff lets the public Feishu adapter show approved
	// authenticated diagnosis entry points when the answer needs live instance
	// state, logs, processes, ports, or another per-account diagnostic. It does
	// not carry any Feishu conversation content or identity into either entry
	// point.
	EnableConsoleHandoff bool `yaml:"enable_console_handoff"`
	// ConsoleAssistantURL is the user-facing console entry point shown for the
	// handoff. Keep it configurable because console routes may differ by deploy.
	ConsoleAssistantURL string `yaml:"console_assistant_url"`
	// ClientDownloadURL is the optional official desktop-client download page
	// shown beside the web console. After the user installs and logs into that
	// client, its own Agent may use the user's local CLI and SSH session; the
	// Feishu adapter never forwards a conversation or credential to it.
	ClientDownloadURL string `yaml:"client_download_url"`
	MaxConcurrent     int    `yaml:"max_concurrent"`
	MaxReplyRunes     int    `yaml:"max_reply_runes"`
	MaxImageBytes     int    `yaml:"max_image_bytes"`
	// ExternalImageOAuth lets the Feishu adapter use a consenting internal
	// group member's user_access_token to read an image uploaded in an external
	// group. It is intentionally off unless configured explicitly.
	ExternalImageOAuth ExternalImageOAuthConfig `yaml:"external_image_oauth"`
}

type ExternalImageOAuthConfig struct {
	Enabled bool `yaml:"enabled"`
	// BootstrapRefreshToken is consumed only when PostgreSQL has no stored
	// token yet. The adapter rotates and encrypts the replacement in PostgreSQL.
	BootstrapRefreshToken string `yaml:"bootstrap_refresh_token"`
	// RedirectURL is used only by the local `feishu-authorize` command.
	RedirectURL string `yaml:"redirect_url"`
}

// SSHOpsConfig configures the consent-gated, read-only in-instance SSH-ops lane
// (COMPSHARE_SSH_OPS). Enabled is tri-state like the FeaturesConfig bools: nil = fall through to
// the COMPSHARE_SSH_OPS env, then to the built-in default (off). The remaining fields configure the
// harness subprocess and have no env fallback — deployment sets them in YAML. The lane also requires
// durable turns on the server path and a non-static STS provider (INV-12); see cmd/instance_ops.go.
type SSHOpsConfig struct {
	Enabled     *bool         `yaml:"enabled"`      // COMPSHARE_SSH_OPS (default off)
	HarnessPath string        `yaml:"harness_path"` // absolute path to deploy/ssh_ops_harness/harness.py
	BaseURL     string        `yaml:"base_url"`     // ANTHROPIC_BASE_URL of the ModelVerse Anthropic endpoint
	APIKey      string        `yaml:"api_key"`      // empty = agent.llm.api_key
	Python      string        `yaml:"python"`       // interpreter; empty = "python3"
	Model       string        `yaml:"model"`        // third-party model id; empty = agent.llm.model
	Timeout     time.Duration `yaml:"timeout"`      // hard per-task wall clock; empty = 5m
	// AllowWrites lets the harness EXECUTE the mutating tier instead of refusing it, so the agent
	// can repair the box rather than only describe the repair. Destructive commands (delete, wipe,
	// reboot, account/ssh lockout) stay refused in both modes, as does command substitution — that
	// shape gate is the injection firewall, not part of the read-only policy. Default off, and off
	// is a different PRODUCT: the consent card, the tool description and the audit phase all change
	// with it, because a write executed under a card that said "只读排查" is consent we did not get.
	AllowWrites bool `yaml:"allow_writes"`
	// InternalIPv6 makes the lane dial the instance's internal IPv6 instead of the public
	// EIP its SshLoginCommand advertises. Set it wherever this process runs inside the
	// UCloud private network and therefore has no route to customer EIPs — which is the
	// whole production deployment, and is why the lane could not enter a single box there
	// while the identical code connected in under 1.2s from a normal network.
	//
	// It requires agent.sts.iam_url: the address is derived by asking the internal gateway
	// (UVPCFEGO.TransformIPv4ToIPv6), the same call uhost-compshare-api makes before
	// handing a container to compshare-access, which then reaches the box at [IPv6]:22.
	//
	// Default off, because on a developer machine the opposite is true: the EIP is the
	// only reachable address and the internal gateway is not routable at all.
	InternalIPv6 bool `yaml:"internal_ipv6"`
	// PublicIPv6Prefix, when set, gives the lane a SECOND address to try when the internal
	// one does not answer: the instance's public IPv4 expressed under this translation
	// prefix (both the simple low-32-bit form and the RFC 6052 /48 form are attempted).
	//
	// It exists for one measured gap. cn-wlcb-01 is reached over its internal IPv6 and
	// cn-sh2-02 is not — the deployment runs in the c5 cluster and cn-sh2 sits behind c3,
	// so the mapping resolves and the dial goes nowhere. A translation prefix reaches the
	// box without a per-cluster fabric route.
	//
	// Measured in-cluster 2026-08-16 (two runs, no disagreement): the prefix closes exactly
	// that gap — cn-sh2-02 answers on the simple low-32-bit form in ~30ms with an sshd
	// banner, while its internal address burns the full dial timeout. It is not a guess
	// about the network either; the platform publishes the same prefix and the same
	// encoding itself, since every *.podtcp.compshare.cn name resolves to that prefix with
	// the zone pod ingress's public IPv4 in the low 32 bits.
	//
	// Still a setting, and still empty by default: it is a production network fact like
	// mysql.host_override, a wrong prefix must be an ops edit rather than a code change,
	// and a deployment that never sets it dials exactly what it dialled before. The
	// per-encoding measurements and the reason the RFC 6052 form is kept despite never
	// answering live beside the value in deploy/conf/config.prod.yaml.
	//
	// Ordering is the safety property, not this field: the internal address stays the
	// first candidate and the public IPv4 is never dialled in any position.
	PublicIPv6Prefix string `yaml:"public_ipv6_prefix"`
}

// HTTPConfig holds settings for the HTTP server mode (compshare-agent server).
// All duration fields accept Go duration strings (e.g. "30s", "1m").
type HTTPConfig struct {
	ListenAddr           string        `yaml:"listen_addr"`
	ReadTimeout          time.Duration `yaml:"read_timeout"`
	WriteTimeout         time.Duration `yaml:"write_timeout"`
	SSEKeepaliveInterval time.Duration `yaml:"sse_keepalive_interval"`
	MaxInputLength       int           `yaml:"max_input_length"`
	PoolCapacity         int           `yaml:"pool_capacity"`
	PoolIdleTTL          time.Duration `yaml:"pool_idle_ttl"`
	// MaxSessionTurns caps the compatibility chat path only. Durable WebSocket
	// turns deliberately have no conversation-length wall: committed history is
	// paged and rebuilt per turn instead of forcing a context-breaking session
	// rollover. Zero or unset uses DefaultMaxSessionTurns in compatibility mode.
	MaxSessionTurns int `yaml:"max_session_turns"`
	// DisableCORS turns off the permissive CORS middleware. Default false =
	// CORS headers are added (needed for local front-end debug per the
	// front-back-local-debug skill: dev server on localhost:3000 fetches
	// 127.0.0.1:<port> directly). In production the agent sits behind a
	// gateway that already issues CORS headers, so set this to true to
	// avoid duplicate Access-Control-Allow-* headers (which browsers
	// reject as malformed even when the values match).
	DisableCORS bool `yaml:"disable_cors"`
}

// MaxSessionTurnsCeiling is the largest agent.http.max_session_turns a
// deployment may configure. It is a PRODUCT QUOTA and nothing else.
//
// It used to be MaxReplayedExchanges, and it used to mean something stronger:
// engine.maxAgentContextPairs read it, so it was simultaneously the model's
// entire cross-turn memory, and the validation below existed to stop a
// deployment running 30-turn sessions against a 20-exchange window — turn 1
// forgotten from turn 22 with no error and no log line.
//
// That coupling is gone. The engine no longer caps replayed exchanges by count
// at all; what the model remembers is decided by engine.maxReplayedHistoryRunes
// and then by engine.maxAssembledRequestRunes, both of which are sizes. So this
// number no longer describes memory, and a larger session no longer silently
// truncates history — it just fills the size budget sooner.
//
// The quota is kept at 20 because lifting it is a product decision, not a
// consequence of the engine change.
//
// LIFTING IT PAST ~50 TURNS IS NOT FREE, and an earlier draft of this comment
// claimed it was. agentpool.buildEngine rebuilds a cold session by reading a
// fixed 100 messages with ORDER BY created_at ASC — the OLDEST 100, not the
// newest — so past 50 turns a restart or an LRU eviction would restore the
// beginning of the conversation and drop everything recent. The engine's own
// ceilings no longer stand in the way; that read does. It is left alone here
// because at 20 turns a session is ~40 messages and the page cannot truncate,
// and because a token-aware tail belongs with whatever raises the quota rather
// than with the change that removed the count ceilings.
const MaxSessionTurnsCeiling = 20

// ShippedMaxTokensPerTurn mirrors agent.rate_limit.max_tokens_per_turn in
// deploy/conf/config.yaml, for the code that has to SIZE ITSELF against the
// deployed cap rather than read it from a loaded Config.
//
// It exists because that number had grown two producers that disagreed: the
// history-ceiling test asserted against a hardcoded 200000 that was correct until
// the config was raised on 2026-07-23 and then silently was not. The stale copy
// happened to be the conservative direction, so nothing failed — which is
// precisely why it survived, and why the next drift could as easily go the other
// way.
//
// maxReplayedHistoryRunes NO LONGER DERIVES FROM THIS CAP. It was sized against
// half of it divided by maxReActRounds until 2026-08-05. The request budget now
// derives from the context window instead (engine.maxAssembledRequestRunes),
// because this cap is a per-turn RATE limit enforced at runtime across every
// round, not a bound on any single request — and a single request is what a
// provider rejects.
//
// TestShippedConfigMatchesTheTokenCapConstant reads the yaml and fails if the two
// diverge, so this is a pinned mirror rather than a third copy. Runtime enforcement
// still reads the operator's loaded RateLimitConfig.MaxTokensPerTurn — a deployment
// may set its own value, and this constant does not override it.
const ShippedMaxTokensPerTurn = 400_000

// DefaultMaxSessionTurns is the compatibility-path fallback when
// agent.http.max_session_turns is zero or unset. The durable turn coordinator
// never consults it. Must stay <= MaxSessionTurnsCeiling; validateHTTPConfig
// enforces that for configured values and TestDefaultTurnCapFitsSessionQuota
// for this one.
const DefaultMaxSessionTurns = 20

// MySQLConfig holds connection settings for the MySQL backing store.
// DSN accepts any ${ENV_VAR} placeholder; if the env var is unset the field is
// set to "" so the server sub-command can validate presence before starting.
// A plain literal DSN is passed through unchanged.
// It is optional at Load time so CLI users are not forced to set the env var.
type MySQLConfig struct {
	DSN string `yaml:"dsn"`
	// HostOverride replaces only the host component of a PostgreSQL URL DSN.
	// It lets a deployment choose its reachable database address without
	// duplicating the DSN credentials in another config field.
	HostOverride    string        `yaml:"host_override"`
	MaxOpenConns    int           `yaml:"max_open_conns"`
	MaxIdleConns    int           `yaml:"max_idle_conns"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
}

// MetaConfig provides the static metadata returned by GetMeta.
type MetaConfig struct {
	Welcome          string   `yaml:"welcome"`
	SuggestedPrompts []string `yaml:"suggested_prompts"`
	MaxInputLength   int      `yaml:"max_input_length"`
}

type LLMConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
}

// OCRConfig holds settings for the optional screenshot-understanding feature
// used by the HTTP Chat handler. Despite the name it drives a vision-language
// (Qwen3-VL) call, not plain character OCR. When Model is empty, it falls back
// to MODELVERSE_QWEN_VL_MODEL; if still empty, the feature is disabled. BaseURL
// and APIKey default to the LLM values when empty.
type OCRConfig struct {
	Model   string `yaml:"model"`
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	// Prompt overrides the built-in vision prompt the VL model is asked to
	// follow when reading a screenshot. Empty = use the built-in structured
	// interpretation prompt (ocr.DefaultPrompt). Set it to tune what the model
	// extracts/interprets (e.g. emphasize training-error diagnosis) without a
	// code change. An empty/whitespace value is treated as "use the default";
	// it is never sent as an empty instruction.
	Prompt   string        `yaml:"prompt"`
	Timeout  time.Duration `yaml:"timeout"`
	MaxBytes int           `yaml:"max_bytes"`
}

type RateLimitConfig struct {
	LLMQPS             int `yaml:"llm_qps"`
	LLMDaily           int `yaml:"llm_daily"`
	MutatingQPS        int `yaml:"mutating_qps"`
	MutatingDaily      int `yaml:"mutating_daily"`
	ReadExpensiveQPS   int `yaml:"read_expensive_qps"`
	ReadExpensiveDaily int `yaml:"read_expensive_daily"`

	// SSHExecQPS / SSHExecDaily: per-tenant cap on consent-gated in-instance
	// SSH-ops diagnoses. Same shape as the other classes — zero is promoted to
	// the built-in default (governance.DefaultSSHExec*).
	SSHExecQPS   int `yaml:"ssh_exec_qps"`
	SSHExecDaily int `yaml:"ssh_exec_daily"`

	// UserTurnQPS / UserTurnDaily: per-tenant cap on user-initiated chat
	// turns (one ClientMsgUserMessage frame = 1 turn; confirm responses
	// and pings do NOT count). 0 = disabled. Unlike the other classes
	// these are NOT promoted to a built-in default when zero — operator
	// opts in by setting a positive value.
	//
	// Counts in-memory per process. Single-replica + non-persistent: a
	// pod restart resets every tenant's counter, and N replicas without
	// sticky routing yield an effective cap of N × UserTurnDaily.
	UserTurnQPS   int `yaml:"user_turn_qps"`
	UserTurnDaily int `yaml:"user_turn_daily"`

	// MaxTokensPerTurn caps total LLM tokens (prompt + completion summed
	// across every LLM call) used by a single user turn. 0 = disabled.
	// Engine enforces this at ReAct iteration boundaries — never mid
	// tool_call/tool_result pair — so the WS protocol invariant that
	// every tool_call is followed by a tool_result stays intact.
	MaxTokensPerTurn int `yaml:"max_tokens_per_turn"`
}

func (c RateLimitConfig) Limits() governance.Limits {
	return governance.Limits{
		LLMQPS:             c.LLMQPS,
		LLMDaily:           c.LLMDaily,
		MutatingQPS:        c.MutatingQPS,
		MutatingDaily:      c.MutatingDaily,
		ReadExpensiveQPS:   c.ReadExpensiveQPS,
		ReadExpensiveDaily: c.ReadExpensiveDaily,
		SSHExecQPS:         c.SSHExecQPS,
		SSHExecDaily:       c.SSHExecDaily,
		UserTurnQPS:        c.UserTurnQPS,
		UserTurnDaily:      c.UserTurnDaily,
	}
}

func Load(path string) (*Config, error) {
	raw, err := loadConfigMap(path, map[string]struct{}{})
	if err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("render merged config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := resolveOptionalPlaceholder(&cfg.Agent.PublicKey, "agent.public_key"); err != nil {
		return nil, err
	}
	if err := resolveOptionalPlaceholder(&cfg.Agent.PrivateKey, "agent.private_key"); err != nil {
		return nil, err
	}
	if err := resolveRequiredSecret(&cfg.Agent.LLM.APIKey, "agent.llm.api_key", "LLM_API_KEY"); err != nil {
		return nil, err
	}
	// SSH-ops may use a dedicated ModelVerse Anthropic key. Empty inherits the
	// answer-model key in cmd/instance_ops.go, which is the production Terra setup.
	if err := resolveOptionalCredential(&cfg.Agent.SSHOps.APIKey, "agent.ssh_ops.api_key"); err != nil {
		return nil, err
	}
	if err := resolveOptionalPlaceholder(&cfg.Agent.ProjectId, "agent.project_id"); err != nil {
		return nil, err
	}
	if err := applyRateLimitDefaults(&cfg.Agent.RateLimit); err != nil {
		return nil, err
	}
	// mysql.dsn is optional at Load time: CLI path does not require it.
	// The "server" sub-command must validate DSN presence before starting.
	if err := resolveOptionalDSN(&cfg.Agent.MySQL.DSN); err != nil {
		return nil, err
	}
	if err := validateHTTPConfig(&cfg.Agent.HTTP); err != nil {
		return nil, err
	}
	if err := validateMySQLConfig(&cfg.Agent.MySQL); err != nil {
		return nil, err
	}
	if err := validateMetaConfig(&cfg.Agent.Meta); err != nil {
		return nil, err
	}
	// STS: resolve optional placeholders for service credentials.
	if err := resolveOptionalCredential(&cfg.Agent.STS.ServiceAK, "agent.sts.service_ak"); err != nil {
		return nil, err
	}
	if err := resolveOptionalCredential(&cfg.Agent.STS.ServiceSK, "agent.sts.service_sk"); err != nil {
		return nil, err
	}
	if err := resolveOptionalCredential(&cfg.Agent.STS.DefaultRoleUrn, "agent.sts.default_role_urn"); err != nil {
		return nil, err
	}
	if err := resolveOptionalCredential(&cfg.Agent.STS.IAMURL, "agent.sts.iam_url"); err != nil {
		return nil, err
	}
	if err := validateSTSConfig(&cfg.Agent.STS); err != nil {
		return nil, err
	}
	applySTSDefaults(&cfg.Agent.STS)
	applyHTTPDefaults(&cfg.Agent.HTTP)
	applyMySQLDefaults(&cfg.Agent.MySQL)

	if err := resolveOptionalCredential(&cfg.Agent.OCR.APIKey, "agent.ocr.api_key"); err != nil {
		return nil, err
	}
	if err := validateOCRConfig(&cfg.Agent.OCR); err != nil {
		return nil, err
	}
	applyOCRDefaults(&cfg.Agent.OCR, &cfg.Agent.LLM)

	// Check for explicit mismatch before meta defaults are applied.
	// meta.max_input_length == 0 means "not set"; inheritance happens below.
	if cfg.Agent.Meta.MaxInputLength != 0 && cfg.Agent.HTTP.MaxInputLength != 0 &&
		cfg.Agent.Meta.MaxInputLength != cfg.Agent.HTTP.MaxInputLength {
		return nil, fmt.Errorf(
			"agent.http.max_input_length (%d) and agent.meta.max_input_length (%d) conflict: set only one or make them equal",
			cfg.Agent.HTTP.MaxInputLength, cfg.Agent.Meta.MaxInputLength,
		)
	}

	applyMetaDefaults(&cfg.Agent.Meta, cfg.Agent.HTTP.MaxInputLength)

	return &cfg, nil
}

// loadConfigMap loads a YAML mapping and applies an optional relative
// "extends" file before the current file's values. Environment-specific
// configs therefore need to carry only their deliberate overrides instead of
// duplicating credential-bearing settings from the local base configuration.
func loadConfigMap(path string, stack map[string]struct{}) (map[string]any, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	if _, seen := stack[absPath]; seen {
		return nil, fmt.Errorf("config extends cycle at %s", absPath)
	}
	stack[absPath] = struct{}{}
	defer delete(stack, absPath)

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	raw := make(map[string]any)
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	basePath, hasBase := raw["extends"]
	if !hasBase {
		return raw, nil
	}
	delete(raw, "extends")
	extends, ok := basePath.(string)
	if !ok || strings.TrimSpace(extends) == "" {
		return nil, fmt.Errorf("config %s: extends must be a non-empty relative path", absPath)
	}
	if filepath.IsAbs(extends) {
		return nil, fmt.Errorf("config %s: extends must be relative", absPath)
	}

	base, err := loadConfigMap(filepath.Join(filepath.Dir(absPath), extends), stack)
	if err != nil {
		return nil, err
	}
	return mergeConfigMaps(base, raw), nil
}

func mergeConfigMaps(base, override map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(override))
	for key, value := range base {
		merged[key] = value
	}
	for key, overrideValue := range override {
		baseValue, hasBaseValue := merged[key]
		baseMap, baseIsMap := baseValue.(map[string]any)
		overrideMap, overrideIsMap := overrideValue.(map[string]any)
		if hasBaseValue && baseIsMap && overrideIsMap {
			merged[key] = mergeConfigMaps(baseMap, overrideMap)
			continue
		}
		merged[key] = overrideValue
	}
	return merged
}

func applyRateLimitDefaults(rateLimit *RateLimitConfig) error {
	defaults := governance.DefaultLimits()
	if rateLimit.LLMQPS < 0 {
		return negativeRateLimitError("agent.rate_limit.llm_qps")
	}
	if rateLimit.LLMDaily < 0 {
		return negativeRateLimitError("agent.rate_limit.llm_daily")
	}
	if rateLimit.MutatingQPS < 0 {
		return negativeRateLimitError("agent.rate_limit.mutating_qps")
	}
	if rateLimit.MutatingDaily < 0 {
		return negativeRateLimitError("agent.rate_limit.mutating_daily")
	}
	if rateLimit.ReadExpensiveQPS < 0 {
		return negativeRateLimitError("agent.rate_limit.read_expensive_qps")
	}
	if rateLimit.ReadExpensiveDaily < 0 {
		return negativeRateLimitError("agent.rate_limit.read_expensive_daily")
	}
	if rateLimit.SSHExecQPS < 0 {
		return negativeRateLimitError("agent.rate_limit.ssh_exec_qps")
	}
	if rateLimit.SSHExecDaily < 0 {
		return negativeRateLimitError("agent.rate_limit.ssh_exec_daily")
	}
	if rateLimit.UserTurnQPS < 0 {
		return negativeRateLimitError("agent.rate_limit.user_turn_qps")
	}
	if rateLimit.UserTurnDaily < 0 {
		return negativeRateLimitError("agent.rate_limit.user_turn_daily")
	}
	if rateLimit.MaxTokensPerTurn < 0 {
		return negativeRateLimitError("agent.rate_limit.max_tokens_per_turn")
	}
	if rateLimit.LLMQPS == 0 {
		rateLimit.LLMQPS = defaults.LLMQPS
	}
	if rateLimit.LLMDaily == 0 {
		rateLimit.LLMDaily = defaults.LLMDaily
	}
	if rateLimit.MutatingQPS == 0 {
		rateLimit.MutatingQPS = defaults.MutatingQPS
	}
	if rateLimit.MutatingDaily == 0 {
		rateLimit.MutatingDaily = defaults.MutatingDaily
	}
	if rateLimit.ReadExpensiveQPS == 0 {
		rateLimit.ReadExpensiveQPS = defaults.ReadExpensiveQPS
	}
	if rateLimit.ReadExpensiveDaily == 0 {
		rateLimit.ReadExpensiveDaily = defaults.ReadExpensiveDaily
	}
	if rateLimit.SSHExecQPS == 0 {
		rateLimit.SSHExecQPS = defaults.SSHExecQPS
	}
	if rateLimit.SSHExecDaily == 0 {
		rateLimit.SSHExecDaily = defaults.SSHExecDaily
	}
	return nil
}

func negativeRateLimitError(yamlPath string) error {
	return fmt.Errorf("%s must be non-negative (0 or omit to use default)", yamlPath)
}

func negativeValueError(yamlPath string) error {
	return fmt.Errorf("%s must be non-negative (0 or omit to use default)", yamlPath)
}

// validateHTTPConfig rejects any explicitly-set negative numeric values.
// Zero values are allowed (meaning "use default" or intentional zero for SSE write timeout).
func validateHTTPConfig(h *HTTPConfig) error {
	if h.PoolCapacity < 0 {
		return negativeValueError("agent.http.pool_capacity")
	}
	if h.MaxInputLength < 0 {
		return negativeValueError("agent.http.max_input_length")
	}
	if h.PoolIdleTTL < 0 {
		return negativeValueError("agent.http.pool_idle_ttl")
	}
	if h.ReadTimeout < 0 {
		return negativeValueError("agent.http.read_timeout")
	}
	if h.WriteTimeout < 0 {
		return negativeValueError("agent.http.write_timeout")
	}
	if h.SSEKeepaliveInterval < 0 {
		return negativeValueError("agent.http.sse_keepalive_interval")
	}
	if h.MaxSessionTurns < 0 {
		return negativeValueError("agent.http.max_session_turns")
	}
	// The product quota. This used to guard the model's memory as well — past the
	// replayed-history window the oldest exchanges stopped reaching the model, and
	// the symptom looked like a model defect rather than a config one. The engine
	// no longer has a count window, so this is now only what it says it is.
	if h.MaxSessionTurns > MaxSessionTurnsCeiling {
		return fmt.Errorf(
			"agent.http.max_session_turns=%d exceeds the configured session quota (%d)",
			h.MaxSessionTurns, MaxSessionTurnsCeiling)
	}
	return nil
}

// validateMySQLConfig rejects any explicitly-set negative numeric values.
func validateMySQLConfig(m *MySQLConfig) error {
	if m.MaxOpenConns < 0 {
		return negativeValueError("agent.mysql.max_open_conns")
	}
	if m.MaxIdleConns < 0 {
		return negativeValueError("agent.mysql.max_idle_conns")
	}
	if m.ConnMaxLifetime < 0 {
		return negativeValueError("agent.mysql.conn_max_lifetime")
	}
	return nil
}

// validateMetaConfig rejects any explicitly-set negative numeric values.
func validateMetaConfig(meta *MetaConfig) error {
	if meta.MaxInputLength < 0 {
		return negativeValueError("agent.meta.max_input_length")
	}
	return nil
}

// resolveRequiredSecret resolves a required secret field. It accepts EITHER a
// ${ENV_VAR} placeholder (resolved from the environment; the named var must be
// non-empty) OR an inline literal value. Inline literals are permitted so a
// production config.yaml can be fully self-contained with no environment
// variables (the YAML-first config migration). envKey names the conventional
// env var for the error message only; any ${...} placeholder is honored for
// compatibility.
func resolveRequiredSecret(field *string, yamlPath, envKey string) error {
	raw := strings.TrimSpace(*field)
	if raw == "" {
		return fmt.Errorf("%s is required: use the ${%s} placeholder or an inline value", yamlPath, envKey)
	}
	if strings.HasPrefix(raw, "${") && strings.HasSuffix(raw, "}") {
		key := strings.TrimSuffix(strings.TrimPrefix(raw, "${"), "}")
		if key == "" {
			return fmt.Errorf("%s placeholder must name an environment variable", yamlPath)
		}
		value := os.Getenv(key)
		if value == "" {
			return fmt.Errorf("environment variable %s is required for %s", key, yamlPath)
		}
		*field = value
		return nil
	}
	if strings.HasPrefix(raw, "$") {
		return fmt.Errorf("%s must use ${ENV_VAR} placeholder syntax or an inline value", yamlPath)
	}
	// Inline literal — pass through unchanged.
	*field = raw
	return nil
}

// resolveOptionalPlaceholder resolves an optional credential-ish field
// (public_key / private_key / project_id). Empty is allowed (left unchanged). A
// ${ENV_VAR} placeholder is resolved from the environment (the named var must be
// non-empty, matching the prior contract). An inline literal is now permitted
// too (YAML-first migration), so a self-contained config.yaml needs no env. A
// "$"-prefixed value that is not valid ${...} is rejected as a typo.
func resolveOptionalPlaceholder(field *string, yamlPath string) error {
	raw := strings.TrimSpace(*field)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "${") && strings.HasSuffix(raw, "}") {
		envKey := strings.TrimSuffix(strings.TrimPrefix(raw, "${"), "}")
		if envKey == "" {
			return fmt.Errorf("%s placeholder must name an environment variable", yamlPath)
		}
		value := os.Getenv(envKey)
		if value == "" {
			return fmt.Errorf("environment variable %s is required for %s", envKey, yamlPath)
		}
		*field = value
		return nil
	}
	if strings.HasPrefix(raw, "$") {
		return fmt.Errorf("%s must use ${ENV_VAR} placeholder syntax or be a plain literal", yamlPath)
	}
	// Inline literal — pass through unchanged.
	*field = raw
	return nil
}

// resolveOptionalDSN resolves any ${ENV_VAR} placeholder in the DSN field.
// If the field is empty or a plain literal it is left unchanged.
// If the field looks like a placeholder but the env var is unset, the field is
// set to "" so server-mode validators can simply check dsn == "".
// Returns an error only when the value starts with "$" but is not valid ${...}
// syntax, to catch typos like "$MYSQL_DSN".
func resolveOptionalDSN(field *string) error {
	raw := strings.TrimSpace(*field)
	if raw == "" {
		return nil
	}
	// Detect placeholder-like values that start with "$".
	if strings.HasPrefix(raw, "$") {
		if !strings.HasPrefix(raw, "${") || !strings.HasSuffix(raw, "}") {
			return fmt.Errorf("agent.mysql.dsn must use ${ENV_VAR} placeholder syntax or be a plain literal DSN")
		}
		envKey := strings.TrimSuffix(strings.TrimPrefix(raw, "${"), "}")
		if envKey == "" {
			return fmt.Errorf("agent.mysql.dsn placeholder must name an environment variable")
		}
		// Env var unset → blank the field; server sub-command will reject it.
		*field = os.Getenv(envKey)
		return nil
	}
	// Plain literal DSN — pass through unchanged.
	return nil
}

// applyHTTPDefaults fills zero-value fields with documented defaults.
func applyHTTPDefaults(h *HTTPConfig) {
	if h.ListenAddr == "" {
		h.ListenAddr = "0.0.0.0:7429"
	}
	if h.ReadTimeout == 0 {
		h.ReadTimeout = 30 * time.Second
	}
	// WriteTimeout == 0 is intentional for SSE; keep it.
	if h.SSEKeepaliveInterval == 0 {
		h.SSEKeepaliveInterval = 15 * time.Second
	}
	if h.MaxInputLength == 0 {
		h.MaxInputLength = 4000
	}
	if h.PoolCapacity == 0 {
		h.PoolCapacity = 200
	}
	if h.PoolIdleTTL == 0 {
		h.PoolIdleTTL = 30 * time.Minute
	}
}

// applyMySQLDefaults fills zero-value connection pool fields with documented defaults.
// DSN is not defaulted here; it is optional at Load time.
func applyMySQLDefaults(m *MySQLConfig) {
	if m.MaxOpenConns == 0 {
		m.MaxOpenConns = 20
	}
	if m.MaxIdleConns == 0 {
		m.MaxIdleConns = 5
	}
	if m.ConnMaxLifetime == 0 {
		m.ConnMaxLifetime = time.Hour
	}
}

// applyMetaDefaults fills the meta section. MaxInputLength inherits from the
// http section when omitted so both are always consistent.
func applyMetaDefaults(meta *MetaConfig, httpMaxInputLength int) {
	if meta.MaxInputLength == 0 {
		meta.MaxInputLength = httpMaxInputLength
	}
}

// resolveOptionalCredential resolves any ${ENV_VAR} placeholder in an
// optional credential field (e.g. STS service_ak, service_sk).
// If the field is empty it is left unchanged.
// If the field looks like a ${...} placeholder but the env var is unset, the
// field is cleared to "" so callers (server/cli sub-commands) can validate
// presence at startup time.
// Returns an error only when the value starts with "$" but uses invalid
// syntax (e.g. "$COMPSHARE_SERVICE_PUBLIC_KEY" without braces).
func resolveOptionalCredential(field *string, yamlPath string) error {
	raw := strings.TrimSpace(*field)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "$") {
		if !strings.HasPrefix(raw, "${") || !strings.HasSuffix(raw, "}") {
			return fmt.Errorf("%s must use ${ENV_VAR} placeholder syntax or be a plain literal", yamlPath)
		}
		envKey := strings.TrimSuffix(strings.TrimPrefix(raw, "${"), "}")
		if envKey == "" {
			return fmt.Errorf("%s placeholder must name an environment variable", yamlPath)
		}
		// Env var unset → blank the field; sub-command validates before use.
		*field = os.Getenv(envKey)
		return nil
	}
	// Plain literal — pass through unchanged.
	return nil
}

// validateSTSConfig rejects explicitly negative numeric or duration values.
func validateSTSConfig(s *STSConfig) error {
	if s.DurationSeconds < 0 {
		return negativeValueError("agent.sts.duration_seconds")
	}
	if s.RefreshBefore < 0 {
		return negativeValueError("agent.sts.refresh_before")
	}
	return nil
}

// applySTSDefaults fills zero-value STS fields with documented defaults.
func applySTSDefaults(s *STSConfig) {
	if s.DurationSeconds == 0 {
		s.DurationSeconds = 3600
	}
	if s.RefreshBefore == 0 {
		s.RefreshBefore = 5 * time.Minute
	}
	if s.DefaultSessionName == "" {
		s.DefaultSessionName = "agent-default"
	}
	if s.URL == "" {
		s.URL = "https://api.ucloud.cn/"
	}
}

func validateOCRConfig(ocr *OCRConfig) error {
	if ocr.Timeout < 0 {
		return negativeValueError("agent.ocr.timeout")
	}
	if ocr.MaxBytes < 0 {
		return negativeValueError("agent.ocr.max_bytes")
	}
	return nil
}

func applyOCRDefaults(ocr *OCRConfig, llmCfg *LLMConfig) {
	if ocr.Model == "" {
		ocr.Model = strings.TrimSpace(os.Getenv("MODELVERSE_QWEN_VL_MODEL"))
	}
	if ocr.Timeout == 0 {
		ocr.Timeout = 15 * time.Second
	}
	if ocr.MaxBytes == 0 {
		ocr.MaxBytes = 5 * 1024 * 1024
	}
	if ocr.BaseURL == "" && llmCfg != nil {
		ocr.BaseURL = llmCfg.BaseURL
	}
	if ocr.APIKey == "" && llmCfg != nil {
		ocr.APIKey = llmCfg.APIKey
	}
}
