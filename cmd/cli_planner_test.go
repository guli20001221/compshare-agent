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
	require.Equal(t, plannerStructuredOutputJSONSchema, mode)
	require.Empty(t, unknown)

	mode, unknown = plannerStructuredOutputModeFromEnv(func(key string) string {
		if key == "COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT" {
			return "nonsense"
		}
		return ""
	})
	require.Equal(t, plannerStructuredOutputOff, mode)
	require.Equal(t, "nonsense", unknown)
}

func TestPlannerResponseFormatForMode(t *testing.T) {
	// off → never any response_format, regardless of capability.
	require.Nil(t, plannerResponseFormatForMode(intent.OutputModeJSONSchema, plannerStructuredOutputOff))
	require.Nil(t, plannerResponseFormatForMode(intent.OutputModeJSONObject, plannerStructuredOutputOff))

	// json_object opt-in → json_object whenever the model supports object-level
	// structured output (json_object OR the richer json_schema), nil otherwise.
	for _, mode := range []intent.OutputMode{intent.OutputModeJSONObject, intent.OutputModeJSONSchema} {
		format := plannerResponseFormatForMode(mode, plannerStructuredOutputJSONObject)
		require.NotNil(t, format, "json_object opt-in, mode=%s", mode)
		require.Equal(t, "json_object", string(format.Type))
		require.Nil(t, format.JSONSchema)
	}
	require.Nil(t, plannerResponseFormatForMode(intent.OutputModeStrictPromptJSON, plannerStructuredOutputJSONObject))

	// json_schema opt-in → json_schema when the model supports it, carrying the
	// IntentRoute schema (non-strict); degrades to json_object on an
	// object-only model; nil on a model without structured output.
	schemaFormat := plannerResponseFormatForMode(intent.OutputModeJSONSchema, plannerStructuredOutputJSONSchema)
	require.NotNil(t, schemaFormat)
	require.Equal(t, "json_schema", string(schemaFormat.Type))
	require.NotNil(t, schemaFormat.JSONSchema)
	require.Equal(t, "intent_route", schemaFormat.JSONSchema.Name)
	require.False(t, schemaFormat.JSONSchema.Strict)
	require.NotNil(t, schemaFormat.JSONSchema.Schema)

	degraded := plannerResponseFormatForMode(intent.OutputModeJSONObject, plannerStructuredOutputJSONSchema)
	require.NotNil(t, degraded)
	require.Equal(t, "json_object", string(degraded.Type))

	require.Nil(t, plannerResponseFormatForMode(intent.OutputModeStrictPromptJSON, plannerStructuredOutputJSONSchema))
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

func TestCLIPlannerLLMSendsJSONSchemaResponseFormatWhenSchemaMode(t *testing.T) {
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
		structuredOutputMode: plannerStructuredOutputJSONSchema,
	}

	_, err := plannerLLM.CompleteIntentPlanWithUsage(context.Background(), intent.IntentRouterLLMRequest{
		Mode:         intent.OutputModeJSONSchema,
		SystemPrompt: "system",
		UserPrompt:   "user",
	})
	require.NoError(t, err)

	responseFormat, ok := captured["response_format"].(map[string]any)
	require.True(t, ok, "response_format should be sent in json_schema mode")
	require.Equal(t, "json_schema", responseFormat["type"])
	jsonSchema, ok := responseFormat["json_schema"].(map[string]any)
	require.True(t, ok, "json_schema payload must be present")
	require.Equal(t, "intent_route", jsonSchema["name"])
	require.Equal(t, false, jsonSchema["strict"])
	// The schema must carry the intent enum so the provider can constrain output.
	schema, ok := jsonSchema["schema"].(map[string]any)
	require.True(t, ok, "schema object must be present")
	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	intentProp, ok := props["intent"].(map[string]any)
	require.True(t, ok)
	require.NotEmpty(t, intentProp["enum"])
	require.NotContains(t, captured, "tools")
	require.NotContains(t, captured, "tool_choice")
}
