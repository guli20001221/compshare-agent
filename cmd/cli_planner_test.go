package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/require"
)

func TestPlannerStructuredOutputModeFromEnv(t *testing.T) {
	mode, unknown := plannerStructuredOutputModeFromEnv(func(string) string { return "" })
	require.Equal(t, plannerStructuredOutputOff, mode)
	require.Empty(t, unknown)

	mode, unknown = plannerStructuredOutputModeFromEnv(func(key string) string {
		if key == "COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT" {
			return " json_object "
		}
		return ""
	})
	require.Equal(t, plannerStructuredOutputJSONObject, mode)
	require.Empty(t, unknown)

	mode, unknown = plannerStructuredOutputModeFromEnv(func(key string) string {
		if key == "COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT" {
			return "off"
		}
		return ""
	})
	require.Equal(t, plannerStructuredOutputOff, mode)
	require.Empty(t, unknown)

	mode, unknown = plannerStructuredOutputModeFromEnv(func(key string) string {
		if key == "COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT" {
			return "json_schema"
		}
		return ""
	})
	require.Equal(t, plannerStructuredOutputOff, mode)
	require.Equal(t, "json_schema", unknown)
}

func TestPlannerResponseFormatForMode(t *testing.T) {
	format := plannerResponseFormatForMode(intent.OutputModeJSONObject, plannerStructuredOutputJSONObject)
	require.NotNil(t, format)
	require.Equal(t, "json_object", string(format.Type))

	require.Nil(t, plannerResponseFormatForMode(intent.OutputModeStrictPromptJSON, plannerStructuredOutputJSONObject))
	require.Nil(t, plannerResponseFormatForMode(intent.OutputModeJSONSchema, plannerStructuredOutputJSONObject))
	require.Nil(t, plannerResponseFormatForMode(intent.OutputModeJSONObject, plannerStructuredOutputOff))
}

func TestCLIPlannerLLMSendsJSONObjectResponseFormatWhenFlagOn(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &captured))

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"{}\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	plannerLLM := cliPlannerLLM{
		client: llm.NewClient(config.LLMConfig{
			BaseURL: srv.URL + "/v1",
			APIKey:  "test-key",
			Model:   "test-model",
		}),
		structuredOutputMode: plannerStructuredOutputJSONObject,
	}

	_, err := plannerLLM.CompleteIntentPlanWithUsage(context.Background(), intent.IntentRouterLLMRequest{
		Mode:         intent.OutputModeJSONObject,
		SystemPrompt: "system",
		UserPrompt:   "user",
	})
	require.NoError(t, err)

	responseFormat, ok := captured["response_format"].(map[string]any)
	require.True(t, ok, "response_format should be sent when flag is on")
	require.Equal(t, "json_object", responseFormat["type"])
	require.NotContains(t, captured, "tools")
	require.NotContains(t, captured, "tool_choice")
}

func TestCLIPlannerLLMOmitsResponseFormatWhenFlagOff(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &captured))

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"{}\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	plannerLLM := cliPlannerLLM{
		client: llm.NewClient(config.LLMConfig{
			BaseURL: srv.URL + "/v1",
			APIKey:  "test-key",
			Model:   "test-model",
		}),
		structuredOutputMode: plannerStructuredOutputOff,
	}

	_, err := plannerLLM.CompleteIntentPlanWithUsage(context.Background(), intent.IntentRouterLLMRequest{
		Mode:         intent.OutputModeJSONObject,
		SystemPrompt: "system",
		UserPrompt:   "user",
	})
	require.NoError(t, err)

	require.NotContains(t, captured, "response_format")
	require.NotContains(t, captured, "tools")
	require.NotContains(t, captured, "tool_choice")
}
