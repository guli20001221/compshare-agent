package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/renderer"

	openai "github.com/sashabaranov/go-openai"
	"github.com/spf13/cobra"
)

var configPath string

var rootCmd = &cobra.Command{
	Use:   "compshare-agent",
	Short: "优云算力共享平台 AI 助手",
}

var cliCmd = &cobra.Command{
	Use:   "cli",
	Short: "CLI 交互模式",
	RunE:  runCLI,
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "deploy/conf/agent.yaml", "配置文件路径")
	rootCmd.AddCommand(cliCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// cliConfirm prompts the user to confirm an L1 operation in the terminal.
func cliConfirm(scanner *bufio.Scanner) engine.ConfirmFunc {
	return func(action string, args map[string]any) bool {
		argsJSON, _ := json.MarshalIndent(args, "    ", "  ")
		fmt.Printf("  ⚠️  即将执行变更操作: %s\n", action)
		fmt.Printf("    参数: %s\n", string(argsJSON))
		fmt.Print("  确认执行？(y/N) ")
		if !scanner.Scan() {
			return false
		}
		answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
		return answer == "y" || answer == "yes"
	}
}

func runCLI(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx := context.Background()
	scanner := bufio.NewScanner(os.Stdin)
	eng := engine.New(cfg, cliConfirm(scanner))
	cutoverIntents, unknownCutoverValues := intentPlannerCutoverIntentsFromEnv(os.Getenv)
	for _, value := range unknownCutoverValues {
		fmt.Fprintf(os.Stderr, "warning: ignoring unknown USE_INTENT_PLANNER_FOR value %q\n", value)
	}
	cutoverEnabled := len(cutoverIntents) > 0
	shadowEnabled := intentPlannerShadowEnabled(os.Getenv)
	knowledgeRetrievalRequested, unknownKnowledgeRetrieval := knowledgeRetrievalModeFromEnv(os.Getenv)
	if unknownKnowledgeRetrieval != "" {
		fmt.Fprintf(os.Stderr, "warning: ignoring unknown USE_KNOWLEDGE_RETRIEVAL value %q\n", unknownKnowledgeRetrieval)
	}
	knowledgeRetriever, knowledgeRetrievalEnabled, knowledgeErr := knowledgeRetrieverFromEnv(os.Getenv)
	if knowledgeRetrievalRequested && knowledgeErr != nil {
		fmt.Fprintf(os.Stderr, "warning: knowledge retrieval disabled: %v\n", knowledgeErr)
	}
	if knowledgeRetrievalEnabled {
		eng.SetKnowledgeRetriever(knowledgeRetriever)
	}
	groundedRendererMode, unknownGroundedRendererMode := groundedRendererModeFromEnv(os.Getenv)
	if unknownGroundedRendererMode != "" {
		fmt.Fprintf(os.Stderr, "warning: ignoring unknown USE_GROUNDED_RENDERER value %q\n", unknownGroundedRendererMode)
	}
	if groundedRendererMode == "llm" {
		eng.SetGroundedRenderer(renderer.NewGroundedRenderer(llm.NewClient(cfg.Agent.LLM)), cfg.Agent.LLM.Model)
	}
	plannerDispatchEnabled := cutoverEnabled || knowledgeRetrievalEnabled
	if plannerDispatchEnabled {
		eng.SetIntentPlanner(newCLIPlanner(cfg), engine.IntentPlannerOptions{
			EnabledIntents: cutoverIntents,
			Model:          cfg.Agent.LLM.Model,
		})
	}
	traceWriter, traceEnabled, traceErr := traceWriterFromEnv(os.Getenv)
	if traceErr != nil {
		fmt.Fprintf(os.Stderr, "warning: trace disabled: %v\n", traceErr)
		traceEnabled = false
	}
	var shadowRunner *intent.ShadowRunner
	if useSeparateShadowRunner(traceEnabled, shadowEnabled, plannerDispatchEnabled) {
		shadowRunner = newCLIShadowRunner(cfg, eng)
	}

	fmt.Println("╭──────────────────────────────────────╮")
	fmt.Println("│     优云算力共享 AI 助手 v0.1        │")
	fmt.Println("╰──────────────────────────────────────╯")
	fmt.Printf("runtime: %s\n", plannerRuntimeModeLine(shadowEnabled, cutoverIntents))
	fmt.Printf("renderer: %s\n", groundedRendererRuntimeLine(groundedRendererMode))
	fmt.Println()
	fmt.Println("正在初始化，获取您的实例信息...")

	var initTraceRecorder *cliTraceRecorder
	initStart := time.Now()
	if traceEnabled {
		initTraceRecorder = newCLITraceRecorder(traceWriter, 0, "init_context", initStart)
		initTraceRecorder.SetRuntimeTrace(plannerRuntimeTrace(shadowEnabled, cutoverIntents))
		initTraceRecorder.SetRegistryTraceSupplier(eng.RegistryTraceState)
		eng.SetRateLimitObserver(initTraceRecorder.SetRateLimitDecision)
	}
	suggestions, err := eng.Init(ctx)
	if initTraceRecorder != nil && initTraceRecorder.HasRateLimitDenial() {
		if traceErr := initTraceRecorder.Finish(err, time.Now()); traceErr != nil {
			fmt.Fprintf(os.Stderr, "warning: init trace write failed: %v\n", traceErr)
		}
	}
	if err != nil {
		fmt.Printf("⚠ 初始化警告: %v\n", err)
	}

	if len(suggestions) > 0 {
		fmt.Println("\n您可以试试：")
		for i, s := range suggestions {
			fmt.Printf("  [%d] %s\n", i+1, s.Text)
		}
	}
	fmt.Println("\n输入 'quit' 或 'exit' 退出。")
	fmt.Println()

	turnIndex := 0
	for {
		fmt.Print("You> ")
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if input == "quit" || input == "exit" {
			fmt.Println("再见！")
			break
		}

		// Check if user typed a suggestion number
		if n, err := strconv.Atoi(input); err == nil && n >= 1 && n <= len(suggestions) {
			input = suggestions[n-1].Text
			fmt.Printf("→ %s\n", input)
		}

		turnIndex++
		turnStart := time.Now()
		var traceRecorder *cliTraceRecorder
		// Reset each turn so a previous trace recorder is never retained
		// when the next turn creates a fresh recorder.
		eng.SetPlannerTraceObserver(nil)
		eng.SetRetrievalTraceObserver(nil)
		eng.SetRendererTraceObserver(nil)
		if traceEnabled {
			traceRecorder = newCLITraceRecorder(traceWriter, turnIndex, input, turnStart)
			traceRecorder.SetRuntimeTrace(plannerRuntimeTrace(shadowEnabled, cutoverIntents))
			traceRecorder.SetRegistryTraceSupplier(eng.RegistryTraceState)
			eng.SetRateLimitObserver(traceRecorder.SetRateLimitDecision)
			eng.SetHardBlockObserver(traceRecorder.SetEngineHardBlock)
			if plannerDispatchEnabled {
				// When Phase 1 cutover or Stage 2B retrieval is enabled, Engine
				// owns the single planner call for this turn and writes that same
				// result into trace.planner.
				traceRecorder.SetPlannerTraceSupplier(nil)
				eng.SetPlannerTraceObserver(traceRecorder.SetPlannerTrace)
				if knowledgeRetrievalEnabled {
					eng.SetRetrievalTraceObserver(traceRecorder.SetRetrievalTrace)
				}
				eng.SetRendererTraceObserver(traceRecorder.SetRendererTrace)
			} else if shadowRunner != nil {
				// By construction, shadowRunner is only created for the
				// trace+shadow+no-cutover case.
				plannerInput := cliShadowPlannerInput(eng, input)
				traceRecorder.SetPlannerTraceSupplier(func() observability.PlannerTrace {
					return shadowRunner.Run(ctx, plannerInput)
				})
			}
		}

		onStep := func(ev engine.StepEvent) {
			if traceRecorder != nil {
				traceRecorder.OnStep(ev)
			}
			switch ev.Type {
			case engine.StepToolCall:
				fmt.Printf("  🔧 调用 %s ...\n", ev.Action)
			case engine.StepToolResult:
				fmt.Printf("  ✅ %s %s\n", ev.Action, ev.Message)
				if ev.Display != "" {
					fmt.Printf("  🔑 %s\n", ev.Display)
				}
			case engine.StepConfirmNeeded:
				// Confirmation prompt is handled by cliConfirm
			case engine.StepBlocked:
				fmt.Printf("  🚫 %s\n", ev.Message)
			case engine.StepError:
				fmt.Printf("  ❌ %s: %s\n", ev.Action, ev.Message)
			}
		}

		reply, err := eng.Chat(ctx, input, onStep)
		if traceRecorder != nil {
			if traceErr := traceRecorder.Finish(err, time.Now()); traceErr != nil {
				fmt.Fprintf(os.Stderr, "warning: trace write failed: %v\n", traceErr)
			}
		}
		if err != nil {
			fmt.Printf("错误: %v\n\n", err)
			continue
		}

		fmt.Printf("\nAssistant> %s\n\n", reply)
	}
	return nil
}

func cliShadowPlannerInput(eng *engine.Engine, userText string) intent.PlannerInput {
	input := intent.PlannerInput{UserText: userText}
	if eng == nil {
		return input
	}
	input.PriorText = eng.PlannerPriorTextSnapshot()
	input.Resolver = eng.RegistrySnapshot()
	return input
}

type cliPlannerLLM struct {
	client *llm.Client
}

func (c cliPlannerLLM) CompleteIntentPlan(ctx context.Context, req intent.PlannerLLMRequest) (string, error) {
	// Planner requests intentionally provide no tools. Omitting ToolChoice here
	// avoids provider-specific validation for tool_choice without tools while
	// still preventing planner-side tool calls.
	resp, err := c.client.Chat(ctx, llm.ChatRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: req.SystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: req.UserPrompt},
		},
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

func newCLIPlanner(cfg *config.Config) *intent.Planner {
	plannerClient := cliPlannerLLM{client: llm.NewClient(cfg.Agent.LLM)}
	return intent.NewPlanner(plannerClient, intent.PlannerOptions{
		BaseURL: cfg.Agent.LLM.BaseURL,
		Model:   cfg.Agent.LLM.Model,
	})
}

func newCLIShadowRunner(cfg *config.Config, eng *engine.Engine) *intent.ShadowRunner {
	planner := newCLIPlanner(cfg)
	return intent.NewShadowRunner(planner, intent.ShadowRunnerOptions{
		Enabled:      true,
		Model:        cfg.Agent.LLM.Model,
		QuotaSubject: eng.RateLimitSubjectKey(),
		QuotaHook:    eng.RateLimitDecision,
	})
}
