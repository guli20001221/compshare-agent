package eval

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/prompt"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
)

var modelFlag = flag.String("model", "", "model name/ID to evaluate, or 'all'")

// EvalCase is one evaluation case from cases.json.
type EvalCase struct {
	ID               string   `json:"id"`
	Category         string   `json:"category"`
	Input            string   `json:"input"`
	Description      string   `json:"description"`
	ExpectedIntent   string   `json:"expected_intent"`
	ExpectedTools    []string `json:"expected_tools"`
	ExpectedKeywords []string `json:"expected_keywords,omitempty"` // for knowledge_qa content check
	UserContext      string   `json:"user_context"`
	Tags             []string `json:"tags"`
}

func loadCases(t *testing.T) []EvalCase {
	t.Helper()
	data, err := os.ReadFile("cases.json")
	if err != nil {
		t.Fatalf("failed to read cases.json: %v", err)
	}
	var cases []EvalCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("failed to parse cases.json: %v", err)
	}
	return cases
}

// toolToIntent maps a tool name to the expected intent category.
func toolToIntent(toolName string) string {
	switch toolName {
	case "GetGPUSpecs", "GetGPURecommendation":
		return "recommendation"
	case "CreateInstanceWorkflow", "StopInstanceWorkflow", "StartInstanceWorkflow",
		"RebootInstanceWorkflow", "RenameInstanceWorkflow", "ResetPasswordWorkflow":
		return "complex_task"
	case "DiagnoseSSH", "DiagnoseInitFailure", "DiagnoseGPU", "DiagnoseBilling",
		"DiagnosePortOrFirewall", "DiagnoseImageIssue":
		return "diagnosis"
	default:
		// All other registered API tools are simple queries
		return "simple_query"
	}
}

// evaluateCase sends one case to the LLM and checks intent + tool selection.
func evaluateCase(ctx context.Context, client *llm.Client, c EvalCase) CaseResult {
	result := CaseResult{
		CaseID:         c.ID,
		Category:       c.Category,
		Input:          c.Input,
		ExpectedIntent: c.ExpectedIntent,
		ExpectedTools:  c.ExpectedTools,
		ToolApplicable: c.ExpectedIntent != "knowledge_qa" && c.ExpectedIntent != "clarification",
	}

	userCtx := c.UserContext
	if userCtx == "" {
		userCtx = "暂无用户信息"
	}
	systemPrompt := prompt.BuildSystem(userCtx)

	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
		{Role: openai.ChatMessageRoleUser, Content: c.Input},
	}

	resp, err := client.Chat(ctx, llm.ChatRequest{
		Messages: messages,
		Tools:    tools.Registry,
	})
	if err != nil {
		result.Error = err.Error()
		return result
	}

	// Determine what the LLM did
	if len(resp.ToolCalls) == 0 {
		// No tool call → knowledge_qa or clarification (both produce text-only replies)
		if c.ExpectedIntent == "clarification" {
			result.GotIntent = "clarification"
		} else {
			result.GotIntent = "knowledge_qa"
		}
		result.GotTool = "(none)"
	} else {
		firstTool := resp.ToolCalls[0].Function.Name
		result.GotTool = firstTool
		result.GotIntent = toolToIntent(firstTool)
	}

	// Check intent
	result.IntentCorrect = result.GotIntent == c.ExpectedIntent

	// Check tool (only if applicable)
	if result.ToolApplicable && len(c.ExpectedTools) > 0 {
		for _, expected := range c.ExpectedTools {
			if result.GotTool == expected {
				result.ToolCorrect = true
				break
			}
		}
	} else if !result.ToolApplicable {
		// knowledge_qa: tool correctness is N/A, mark as correct for reporting
		result.ToolCorrect = true
	}

	// Content quality check for knowledge_qa: keyword hits in reply
	if len(c.ExpectedKeywords) > 0 {
		result.KeywordTotal = len(c.ExpectedKeywords)
		if resp.Content == "" {
			// Empty reply cannot satisfy any keyword — fail deterministically
			result.ContentCorrect = false
		} else {
			for _, kw := range c.ExpectedKeywords {
				if strings.Contains(resp.Content, kw) {
					result.KeywordHits++
				}
			}
			minHits := 2
			if result.KeywordTotal < minHits {
				minHits = result.KeywordTotal
			}
			result.ContentCorrect = result.KeywordHits >= minHits
		}
	}

	return result
}

func TestEval(t *testing.T) {
	if *modelFlag == "" {
		t.Skip("use -model flag to specify model (e.g., -model 'Qwen/Qwen3-Max' or -model all)")
	}

	cases := loadCases(t)
	if len(cases) == 0 {
		t.Fatal("no evaluation cases found in cases.json")
	}

	var modelsToTest []ModelConfig
	if *modelFlag == "all" {
		modelsToTest = Models()
	} else {
		m := FindModel(*modelFlag)
		if m == nil {
			t.Fatalf("unknown model: %s", *modelFlag)
		}
		modelsToTest = []ModelConfig{*m}
	}

	var reports []ModelReport

	for _, model := range modelsToTest {
		if model.APIKey == "" {
			t.Logf("SKIP %s: no API key (set MODELVERSE_API_KEY or LOCAL_PROXY_API_KEY)", model.Name)
			continue
		}

		t.Run(model.Name, func(t *testing.T) {
			client := llm.NewClient(llmConfigFromModel(model))
			report := ModelReport{ModelName: model.Name}
			start := time.Now()

			for _, c := range cases {
				t.Run(c.ID, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer cancel()

					result := evaluateCase(ctx, client, c)
					report.Results = append(report.Results, result)

					if result.Error != "" {
						t.Logf("ERROR %s: %s", c.ID, result.Error)
						return
					}

					if !result.IntentCorrect {
						t.Logf("FAIL intent: %s — expected %s, got %s (tool: %s)",
							c.ID, c.ExpectedIntent, result.GotIntent, result.GotTool)
					}
					if result.ToolApplicable && !result.ToolCorrect {
						t.Logf("FAIL tool: %s — expected %v, got %s",
							c.ID, c.ExpectedTools, result.GotTool)
					}
					if result.KeywordTotal > 0 && !result.ContentCorrect {
						t.Logf("FAIL content: %s — keywords %d/%d hit",
							c.ID, result.KeywordHits, result.KeywordTotal)
					}
				})
			}

			report.Duration = time.Since(start)
			report.Tally()
			reports = append(reports, report)

			t.Logf("\n%s: intent=%.1f%% (%d/%d) tool=%.1f%% (%d/%d) content=%.1f%% (%d/%d) time=%s",
				model.Name,
				report.IntentAccuracy(), report.IntentCorrect, report.IntentTotal,
				report.ToolAccuracy(), report.ToolCorrect, report.ToolTotal,
				report.ContentAccuracy(), report.ContentCorrect, report.ContentTotal,
				report.Duration.Round(time.Second))
		})
	}

	// Print comparison report
	if len(reports) > 0 {
		fmt.Println("\n" + FormatReport(reports))
	}
}

func llmConfigFromModel(m ModelConfig) config.LLMConfig {
	return config.LLMConfig{
		BaseURL: m.BaseURL,
		APIKey:  m.APIKey,
		Model:   m.ModelID,
	}
}
