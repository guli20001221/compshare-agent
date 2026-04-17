package eval

import "os"

// ModelConfig defines an LLM endpoint for evaluation.
type ModelConfig struct {
	Name    string // display name
	ModelID string // API model ID
	BaseURL string // API base URL
	APIKey  string // resolved at runtime
}

// Models returns the configured evaluation models with API keys resolved from env.
func Models() []ModelConfig {
	mvKey := os.Getenv("MODELVERSE_API_KEY")
	localKey := os.Getenv("LOCAL_PROXY_API_KEY")
	if localKey == "" {
		localKey = "sk-local"
	}
	aliyunKey := os.Getenv("ALIYUN_API_KEY")
	doubaoKey := os.Getenv("DOUBAO_API_KEY")

	gptGodKey := os.Getenv("GPTGOD_API_KEY")
	geminiGodKey := os.Getenv("GEMINI_GPTGOD_API_KEY")

	return []ModelConfig{
		{Name: "Qwen3-Max", ModelID: "Qwen/Qwen3-Max", BaseURL: "https://api.modelverse.cn/v1", APIKey: mvKey},
		{Name: "GLM-5", ModelID: "zai-org/glm-5", BaseURL: "https://api.modelverse.cn/v1", APIKey: mvKey},
		{Name: "Kimi-K2", ModelID: "moonshotai/Kimi-K2-Instruct", BaseURL: "https://api.modelverse.cn/v1", APIKey: mvKey},
		{Name: "GPT-5.4", ModelID: "gpt-5.4", BaseURL: "https://api.gptgod.online/v1", APIKey: gptGodKey},
		{Name: "GPT-5.4-Mini", ModelID: "gpt-5.4-mini", BaseURL: "https://api.gptgod.online/v1", APIKey: gptGodKey},
		{Name: "GPT-5.3-Codex", ModelID: "gpt-5.3-codex", BaseURL: "https://api.gptgod.online/v1", APIKey: gptGodKey},
		{Name: "Gemini-3.1-Flash-Lite", ModelID: "gemini-3.1-flash-lite-preview", BaseURL: "https://api.gptgod.online/v1", APIKey: geminiGodKey},
		{Name: "Gemini-3-Pro-Think", ModelID: "gemini-3-pro-preview-thinking", BaseURL: "https://api.gptgod.online/v1", APIKey: geminiGodKey},
		{Name: "Qwen-Plus", ModelID: "Qwen/Qwen-Plus", BaseURL: "https://api.modelverse.cn/v1", APIKey: mvKey},
		{Name: "Qwen3-32B", ModelID: "Qwen/Qwen3-32B", BaseURL: "https://api.modelverse.cn/v1", APIKey: mvKey},
		{Name: "Qwen3.6-Plus", ModelID: "Qwen/Qwen3.6-Plus", BaseURL: "https://api.modelverse.cn/v1", APIKey: mvKey},
		{Name: "Qwen3-32B-Aliyun", ModelID: "qwen3-32b", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", APIKey: aliyunKey},
		{Name: "Qwen3.6-Plus-Aliyun", ModelID: "qwen3.6-plus", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", APIKey: aliyunKey},
		{Name: "Doubao-Seed-Lite", ModelID: "doubao-seed-2-0-lite-260215", BaseURL: "https://ark.cn-beijing.volces.com/api/v3", APIKey: doubaoKey},
		{Name: "Doubao-Seed-Pro", ModelID: "doubao-seed-2-0-pro-260215", BaseURL: "https://ark.cn-beijing.volces.com/api/v3", APIKey: doubaoKey},
		{Name: "Doubao-Seed-Mini", ModelID: "doubao-seed-2-0-mini-260215", BaseURL: "https://ark.cn-beijing.volces.com/api/v3", APIKey: doubaoKey},
		{Name: "Doubao-Seed-Code", ModelID: "doubao-seed-2-0-code-preview-260215", BaseURL: "https://ark.cn-beijing.volces.com/api/v3", APIKey: doubaoKey},
	}
}

func localProxyKey(envKey string) string {
	return envKey
}

// FindModel returns the ModelConfig matching the given model ID or name, or nil.
func FindModel(nameOrID string) *ModelConfig {
	for _, m := range Models() {
		if m.Name == nameOrID || m.ModelID == nameOrID {
			return &m
		}
	}
	return nil
}
