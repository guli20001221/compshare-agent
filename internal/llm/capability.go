package llm

import (
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// CapabilityConfigEnv points to an optional YAML file that extends or overrides
// the built-in capability matrix. LookupCapability reads it on each call so
// developers can hot-update the table while iterating on planner behavior.
const CapabilityConfigEnv = "COMPSHARE_LLM_CAPABILITY_FILE"

// Capability describes model/provider behavior that affects planner and tool
// routing. Unknown models intentionally default to conservative false values.
type Capability struct {
	Known                      bool           `yaml:"-"`
	SupportsJSONSchema         bool           `yaml:"supports_json_schema"`
	SupportsJSONObject         bool           `yaml:"supports_json_object"`
	IsThinkingMode             bool           `yaml:"is_thinking_mode"`
	SupportsObjectToolChoice   bool           `yaml:"supports_object_tool_choice"`
	SupportsRequiredToolChoice bool           `yaml:"supports_required_tool_choice"`
	RequiresExtraBody          map[string]any `yaml:"requires_extra_body,omitempty"`
}

type capabilityEntry struct {
	BaseURL    string `yaml:"base_url"`
	Model      string `yaml:"model"`
	Capability `yaml:",inline"`
}

type capabilityFile struct {
	Capabilities []capabilityEntry `yaml:"capabilities"`
}

var builtinCapabilities = []capabilityEntry{
	// Object tool_choice: re-probed 2026-06-08 at true production shape (full
	// VisibleRegistry(true) = 37 tools, real ReAct system prompt, system->user->
	// assistant->user multi-turn) and it now returns HTTP 200 + the forced
	// tool_call — 12/12 forced across the four tools that uniformly 400'd on
	// 2026-05-08 (GetCompShareInstanceMonitor, DescribeCompShareInstance,
	// typed instance-access read and DiagnoseBilling). The upstream rejection was lifted, so the
	// flag is flipped true. See eval/capability/2026-06-08-ds-v4-flash-tool-
	// choice-reprobe.md (supersedes the 2026-05-08 probe). Hot-rollback: a
	// COMPSHARE_LLM_CAPABILITY_FILE override with supports_object_tool_choice:false.
	{
		BaseURL: "https://api.modelverse.cn/v1",
		Model:   "deepseek-v4-flash",
		Capability: Capability{
			// json_schema re-probed 2026-06-23 (supersedes the untested
			// conservative default): a STRICT json_schema request returned HTTP 200
			// with grammar-constrained output — the model was forced into an enum
			// value that excluded the natural answer — EVEN in thinking mode; and a
			// NON-strict json_schema whose intent enum omitted the natural intent
			// likewise produced an in-enum value. So flip true. The intent router
			// uses NON-strict json_schema (SelectOutputMode + IntentRouteResponseSchema)
			// because IntentRoute is polymorphic/omitempty and strict mode's
			// all-required rule would fight it; modelverse enforces enum/const even
			// without strict. Hot-rollback: COMPSHARE_LLM_CAPABILITY_FILE override
			// with supports_json_schema:false.
			SupportsJSONSchema:         true,
			SupportsJSONObject:         true,
			IsThinkingMode:             true,
			SupportsObjectToolChoice:   true,
			SupportsRequiredToolChoice: true,
		},
	},
	{
		BaseURL: "https://api.modelverse.cn/v1",
		Model:   "Qwen/Qwen3-Max",
		Capability: Capability{
			SupportsJSONObject:         true,
			SupportsObjectToolChoice:   true,
			SupportsRequiredToolChoice: true,
		},
	},
	{
		BaseURL: "https://api.modelverse.cn/v1",
		Model:   "qwen3.6-plus",
		Capability: Capability{
			SupportsJSONObject:         true,
			IsThinkingMode:             true,
			SupportsObjectToolChoice:   false,
			SupportsRequiredToolChoice: false,
		},
	},
	{
		BaseURL: "https://api.modelverse.cn/v1",
		Model:   "glm-5-turbo",
		Capability: Capability{
			SupportsJSONObject:         true,
			IsThinkingMode:             true,
			SupportsObjectToolChoice:   false,
			SupportsRequiredToolChoice: false,
		},
	},
	// Probed 2026-05-07 against Modelverse with no extra_body/thinking override:
	// both object tool_choice {"type":"function","function":{"name":"ping_tool"}}
	// and string tool_choice "required" returned tool_calls for a minimal
	// ping_tool request. Earlier 2026-05-01 real CLI flow for the same tuple hit
	// "tool_choice parameter does not support being set to required or object in
	// thinking mode"; T-007a planner shadow must re-probe with production request
	// shape before relying on forced tool_choice routing for this entry.
	{
		BaseURL: "https://api.modelverse.cn/v1",
		Model:   "doubao-seed-2-0-lite-260215",
		Capability: Capability{
			SupportsJSONObject:         true,
			IsThinkingMode:             true,
			SupportsObjectToolChoice:   true,
			SupportsRequiredToolChoice: true,
		},
	},
	{
		BaseURL: "https://ark.cn-beijing.volces.com/api/v3",
		Model:   "doubao-lite",
		Capability: Capability{
			SupportsJSONObject:         true,
			SupportsObjectToolChoice:   true,
			SupportsRequiredToolChoice: true,
		},
	},
}

// LookupCapability returns the best-known capability for (baseURL, model).
// Entries from COMPSHARE_LLM_CAPABILITY_FILE override built-ins and can add new
// tuples without recompilation.
func LookupCapability(baseURL, model string) Capability {
	key := capabilityKey(baseURL, model)
	table := make(map[string]Capability, len(builtinCapabilities))
	for _, entry := range builtinCapabilities {
		table[capabilityKey(entry.BaseURL, entry.Model)] = entry.withKnown()
	}

	if path := strings.TrimSpace(os.Getenv(CapabilityConfigEnv)); path != "" {
		for _, entry := range loadCapabilityFile(path) {
			table[capabilityKey(entry.BaseURL, entry.Model)] = entry.withKnown()
		}
	}

	if cap, ok := table[key]; ok {
		if len(cap.RequiresExtraBody) == 0 {
			cap.RequiresExtraBody = nil
		}
		return cap
	}
	return Capability{}
}

func loadCapabilityFile(path string) []capabilityEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var file capabilityFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil
	}
	return file.Capabilities
}

func (e capabilityEntry) withKnown() Capability {
	cap := e.Capability
	cap.Known = true
	if len(cap.RequiresExtraBody) == 0 {
		cap.RequiresExtraBody = nil
	}
	return cap
}

func capabilityKey(baseURL, model string) string {
	return normalizeBaseURL(baseURL) + "\x00" + normalizeModel(model)
}

func normalizeBaseURL(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	raw = strings.TrimRight(raw, "/")
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return raw
	}
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	return parsed.Host + path
}

func normalizeModel(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}
