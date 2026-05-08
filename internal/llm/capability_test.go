package llm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupCapability_BuiltinMatrix(t *testing.T) {
	t.Setenv(CapabilityConfigEnv, "")

	tests := []struct {
		name                      string
		baseURL                   string
		model                     string
		wantJSONSchema            bool
		wantJSONObject            bool
		wantThinking              bool
		wantObjectToolChoice      bool
		wantRequiredToolChoice    bool
		wantRequiresExtraBodyKeys int
	}{
		{
			name:                   "modelverse deepseek v4 flash object tool_choice broken in thinking mode",
			baseURL:                "https://api.modelverse.cn/v1",
			model:                  "deepseek-v4-flash",
			wantJSONObject:         true,
			wantObjectToolChoice:   false,
			wantRequiredToolChoice: true,
		},
		{
			name:                   "modelverse qwen3 max example model supports json object and tool choice",
			baseURL:                "https://api.modelverse.cn/v1",
			model:                  "Qwen/Qwen3-Max",
			wantJSONObject:         true,
			wantObjectToolChoice:   true,
			wantRequiredToolChoice: true,
		},
		{
			name:                   "modelverse qwen3.6 plus is conservative thinking-mode",
			baseURL:                "https://api.modelverse.cn/v1/",
			model:                  "qwen3.6-plus",
			wantJSONObject:         true,
			wantThinking:           true,
			wantObjectToolChoice:   false,
			wantRequiredToolChoice: false,
		},
		{
			name:                   "modelverse glm-5 turbo is thinking-mode and does not reliably return tool calls",
			baseURL:                "https://api.modelverse.cn/v1",
			model:                  "glm-5-turbo",
			wantJSONObject:         true,
			wantThinking:           true,
			wantObjectToolChoice:   false,
			wantRequiredToolChoice: false,
		},
		{
			name:                   "modelverse doubao lite supports json object and tool choice",
			baseURL:                "https://api.modelverse.cn/v1",
			model:                  "doubao-seed-2-0-lite-260215",
			wantJSONObject:         true,
			wantThinking:           true,
			wantObjectToolChoice:   true,
			wantRequiredToolChoice: true,
		},
		{
			name:                   "volcengine ark doubao lite non thinking supports object tool choice",
			baseURL:                "https://ark.cn-beijing.volces.com/api/v3",
			model:                  "doubao-lite",
			wantJSONObject:         true,
			wantObjectToolChoice:   true,
			wantRequiredToolChoice: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LookupCapability(tt.baseURL, tt.model)

			assert.True(t, got.Known)
			assert.Equal(t, tt.wantJSONSchema, got.SupportsJSONSchema)
			assert.Equal(t, tt.wantJSONObject, got.SupportsJSONObject)
			assert.Equal(t, tt.wantThinking, got.IsThinkingMode)
			assert.Equal(t, tt.wantObjectToolChoice, got.SupportsObjectToolChoice)
			assert.Equal(t, tt.wantRequiredToolChoice, got.SupportsRequiredToolChoice)
			assert.Len(t, got.RequiresExtraBody, tt.wantRequiresExtraBodyKeys)
		})
	}
}

func TestLookupCapability_UnknownUsesSafeDefaults(t *testing.T) {
	t.Setenv(CapabilityConfigEnv, "")

	got := LookupCapability("https://unknown.example/v1", "unknown-model")

	assert.False(t, got.Known)
	assert.False(t, got.SupportsJSONSchema)
	assert.False(t, got.SupportsJSONObject)
	assert.False(t, got.IsThinkingMode)
	assert.False(t, got.SupportsObjectToolChoice)
	assert.False(t, got.SupportsRequiredToolChoice)
	assert.Nil(t, got.RequiresExtraBody)
}

func TestLookupCapability_EnvYAMLOverrideAndHotUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capabilities.yaml")
	t.Setenv(CapabilityConfigEnv, path)

	writeCapabilityFile(t, path, `
capabilities:
  - base_url: "https://api.modelverse.cn/v1"
    model: "deepseek-v4-flash"
    supports_json_schema: true
    supports_json_object: true
    is_thinking_mode: true
    supports_object_tool_choice: false
    supports_required_tool_choice: false
    requires_extra_body:
      thinking:
        type: disabled
`)

	first := LookupCapability("https://api.modelverse.cn/v1", "deepseek-v4-flash")
	require.True(t, first.Known)
	assert.True(t, first.SupportsJSONSchema)
	assert.True(t, first.IsThinkingMode)
	assert.False(t, first.SupportsObjectToolChoice)
	require.Contains(t, first.RequiresExtraBody, "thinking")

	writeCapabilityFile(t, path, `
capabilities:
  - base_url: "https://api.modelverse.cn/v1"
    model: "deepseek-v4-flash"
    supports_json_schema: false
    supports_json_object: true
    is_thinking_mode: false
    supports_object_tool_choice: true
    supports_required_tool_choice: true
`)

	second := LookupCapability("https://api.modelverse.cn/v1/", "deepseek-v4-flash")
	require.True(t, second.Known)
	assert.False(t, second.SupportsJSONSchema)
	assert.False(t, second.IsThinkingMode)
	assert.True(t, second.SupportsObjectToolChoice)
	assert.True(t, second.SupportsRequiredToolChoice)
	assert.Nil(t, second.RequiresExtraBody)
}

func TestLookupCapability_EnvYAMLAddsNewModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capabilities.yaml")
	t.Setenv(CapabilityConfigEnv, path)

	writeCapabilityFile(t, path, `
capabilities:
  - base_url: "https://local-router.example/v1"
    model: "custom-json-model"
    supports_json_schema: false
    supports_json_object: true
    is_thinking_mode: false
    supports_object_tool_choice: true
    supports_required_tool_choice: false
`)

	got := LookupCapability("https://local-router.example/v1", "custom-json-model")

	assert.True(t, got.Known)
	assert.False(t, got.SupportsJSONSchema)
	assert.True(t, got.SupportsJSONObject)
	assert.True(t, got.SupportsObjectToolChoice)
	assert.False(t, got.SupportsRequiredToolChoice)
}

func TestLookupCapability_TestdataFixtureProbe(t *testing.T) {
	t.Setenv(CapabilityConfigEnv, filepath.Join("testdata", "capabilities.override.yaml"))

	got := LookupCapability("https://fixture-router.example/v1", "fixture-json-model")

	assert.True(t, got.Known)
	assert.False(t, got.SupportsJSONSchema)
	assert.True(t, got.SupportsJSONObject)
	assert.True(t, got.SupportsObjectToolChoice)
	assert.False(t, got.SupportsRequiredToolChoice)
	require.Contains(t, got.RequiresExtraBody, "vendor_option")
}

func writeCapabilityFile(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}
