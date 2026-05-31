package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/prompt"
	"github.com/compshare-agent/internal/renderer"
	"github.com/compshare-agent/internal/tools"

	openai "github.com/sashabaranov/go-openai"
	"github.com/spf13/cobra"
)

var startupFatalf = log.Fatalf

// cliTraceDrainTimeout bounds how long runCLI blocks at exit waiting for
// async trace sinks (e.g. MySQLWriter) to drain their queues. Long enough
// to flush a normal-sized batch on a healthy database; short enough that
// a hung connection cannot wedge CLI shutdown.
const cliTraceDrainTimeout = 5 * time.Second

var cliCmd = &cobra.Command{
	Use:   "cli",
	Short: "CLI 交互模式",
	RunE:  runCLI,
}

func init() {
	rootCmd.AddCommand(cliCmd)
}

// cliConfirm prompts the user to confirm an L1 operation in the terminal.
// Instance-create confirms (the deploy_model + ReAct create path) render as a
// field-by-field card; other confirms keep the raw-args JSON dump. The HTTP path
// delivers the same structured fields to the frontend via the confirmation event's
// Summary, so both surfaces show the user GPU/image/zone/price before approving.
func cliConfirm(scanner *bufio.Scanner) engine.ConfirmFunc {
	return func(action string, args map[string]any) bool {
		if wf, _ := args["workflow"].(string); wf == "CreateInstanceWorkflow" {
			printCreateConfirmCard(args)
		} else {
			argsJSON, _ := json.MarshalIndent(args, "    ", "  ")
			fmt.Printf("  ⚠️  即将执行变更操作: %s\n", action)
			fmt.Printf("    参数: %s\n", string(argsJSON))
		}
		fmt.Print("  确认执行？(y/N) ")
		if !scanner.Scan() {
			return false
		}
		answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
		return answer == "y" || answer == "yes"
	}
}

// printCreateConfirmCard renders the instance-create confirm as readable fields
// (the CLI degradation of the structured confirm card) instead of a JSON blob.
func printCreateConfirmCard(args map[string]any) {
	str := func(k string) string {
		if s, ok := args[k].(string); ok {
			return s
		}
		return ""
	}
	num := func(k string) (float64, bool) {
		switch n := args[k].(type) {
		case float64:
			return n, true
		case int:
			return float64(n), true
		}
		return 0, false
	}
	fmt.Println("  ⚠️  即将创建实例，请确认：")
	if gt := str("GpuType"); gt != "" {
		if n, ok := num("Gpu"); ok {
			fmt.Printf("    GPU：%s × %.0f\n", gt, n)
		} else {
			fmt.Printf("    GPU：%s\n", gt)
		}
	}
	if img := str("image"); img != "" {
		fmt.Printf("    镜像：%s\n", img)
	}
	cpu, hasCPU := num("CPU")
	mem, hasMem := num("Memory")
	if hasCPU && hasMem {
		fmt.Printf("    配置：%.0f 核 / %.0f MB\n", cpu, mem/1024) // Memory is MB; show GB
	}
	if z := str("Zone"); z != "" {
		fmt.Printf("    可用区：%s\n", z)
	}
	if ct := str("ChargeType"); ct != "" {
		fmt.Printf("    计费：%s\n", ct)
	}
	if fb := str("FallbackNote"); fb != "" {
		fmt.Printf("    ℹ️  %s\n", fb)
	}
}

func runCLI(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx := context.Background()

	// Inject a CLI UserContext when DefaultRoleUrn is configured; otherwise
	// leave ctx as-is so the engine runs with an anonymous subject.
	if cfg.Agent.STS.DefaultRoleUrn != "" {
		cliUser := tools.UserContext{
			RoleUrn:     cfg.Agent.STS.DefaultRoleUrn,
			SessionName: cfg.Agent.STS.DefaultSessionName,
			ProjectId:   cfg.Agent.ProjectId,
			Region:      cfg.Agent.Region,
		}
		ctx = tools.WithUser(ctx, cliUser)
	}

	scanner := bufio.NewScanner(os.Stdin)
	eng := engine.New(cfg, cliConfirm(scanner))
	mutatingToolsEnabled, unknownMutatingTools := mutatingToolsEnabledFromEnv(os.Getenv)
	if unknownMutatingTools != "" {
		fmt.Fprintf(os.Stderr, "warning: ignoring unknown COMPSHARE_ENABLE_MUTATING_TOOLS value %q\n", unknownMutatingTools)
	}
	eng.SetMutatingToolsEnabled(mutatingToolsEnabled)
	useSkillRegistry, unknownSkillRegistry := useSkillRegistryFromEnv(os.Getenv)
	if unknownSkillRegistry != "" {
		fmt.Fprintf(os.Stderr, "warning: ignoring unknown USE_SKILL_REGISTRY value %q\n", unknownSkillRegistry)
	}
	intent.SetCapabilitySource(useSkillRegistry)
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
	applyKnowledgeRetrieverStartup(eng, knowledgeRetrievalRequested, knowledgeRetriever, knowledgeRetrievalEnabled, knowledgeErr)
	groundedRendererMode, unknownGroundedRendererMode := groundedRendererModeFromEnv(os.Getenv)
	if unknownGroundedRendererMode != "" {
		fmt.Fprintf(os.Stderr, "warning: ignoring unknown USE_GROUNDED_RENDERER value %q\n", unknownGroundedRendererMode)
	}
	if groundedRendererMode == "llm" || groundedRendererMode == "fast_template" {
		router, err := buildLLMRouter(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: build LLM router for grounded renderer: %v\n", err)
			os.Exit(1)
		}
		// LLM renderer serves knowledge/agent tiers in both modes; B3
		// fast_template additionally diverts fast-tier catalog envelopes to
		// the deterministic template.
		eng.SetGroundedRenderer(renderer.NewGroundedRenderer(router.For(llm.TierKnowledge)), router.Model(llm.TierKnowledge))
		if groundedRendererMode == "fast_template" {
			eng.SetFastTemplate(true)
		}
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
	if traceEnabled {
		if err := cleanupTraceWriter(traceWriter, time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "warning: trace cleanup failed: %v\n", err)
		}
		// F1 (PR #90, 2026-05-21): drain async sinks before CLI exit.
		// Without this, MySQL's bounded queue + background flush goroutine
		// (see internal/observability/mysql_writer.go:136 Close) loses any
		// record not yet committed when the subprocess returns. Symptom
		// observed during the C2 smoke run: 8 in-process traces visible
		// in the file sink (sync flush-on-close) but 0 reaching MySQL.
		// FileWriter.Close is a no-op so the file-only path is unaffected;
		// drain timeout is bounded (cliTraceDrainTimeout) so a hung MySQL
		// connection cannot wedge CLI shutdown.
		defer func() {
			drainCtx, cancel := context.WithTimeout(context.Background(), cliTraceDrainTimeout)
			defer cancel()
			if err := traceWriter.Close(drainCtx); err != nil {
				fmt.Fprintf(os.Stderr, "warning: trace writer drain failed: %v\n", err)
			}
		}()
	}
	var shadowRunner *intent.ShadowRunner
	if useSeparateShadowRunner(traceEnabled, shadowEnabled, plannerDispatchEnabled) {
		shadowRunner = newCLIShadowRunner(cfg, eng)
	}

	fmt.Println("╭──────────────────────────────────────╮")
	fmt.Println("│     Compshare Copilot v0.1           │")
	fmt.Println("╰──────────────────────────────────────╯")
	fmt.Printf("runtime: %s\n", plannerRuntimeModeLine(shadowEnabled, plannerDispatchEnabled, cutoverIntents))
	fmt.Printf("renderer: %s\n", groundedRendererRuntimeLine(groundedRendererMode))
	fmt.Printf("tools: %s\n", mutatingToolsRuntimeLine(mutatingToolsEnabled))
	fmt.Printf("capability: %s\n", skillRegistryRuntimeLine(useSkillRegistry))
	fmt.Println()
	fmt.Println("正在初始化，获取您的实例信息...")

	var initTraceRecorder *cliTraceRecorder
	initStart := time.Now()
	if traceEnabled {
		initTraceRecorder = newCLITraceRecorder(traceWriter, "", 0, "init_context", initStart)
		initTraceRecorder.SetRuntimeTrace(plannerRuntimeTrace(shadowEnabled, plannerDispatchEnabled, cutoverIntents))
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

		// Check if user typed a startup suggestion number. This is intentionally
		// limited to the first user turn so later resource-selection replies such
		// as "2" are passed to the engine unchanged.
		if rewritten, ok := applyStartupSuggestion(input, suggestions, turnIndex); ok {
			input = rewritten
			fmt.Printf("→ %s\n", input)
		}

		turnIndex++
		turnStart := time.Now()
		var traceRecorder *cliTraceRecorder
		// Reset each turn so a previous trace recorder is never retained
		// when the next turn creates a fresh recorder.
		eng.SetPlannerTraceObserver(nil)
		eng.SetRetrievalTraceObserver(nil)
		eng.SetOutcomeTraceObserver(nil)
		eng.SetRendererTraceObserver(nil)
		eng.SetTokenUsageObserver(nil)
		if traceEnabled {
			traceRecorder = newCLITraceRecorder(traceWriter, "", turnIndex, input, turnStart)
			traceRecorder.SetRuntimeTrace(plannerRuntimeTrace(shadowEnabled, plannerDispatchEnabled, cutoverIntents))
			traceRecorder.SetRegistryTraceSupplier(eng.RegistryTraceState)
			eng.SetRateLimitObserver(traceRecorder.SetRateLimitDecision)
			eng.SetHardBlockObserver(traceRecorder.SetEngineHardBlock)
			eng.SetTokenUsageObserver(traceRecorder.AddTokenUsage)
			if plannerDispatchEnabled {
				// When Phase 1 cutover or Stage 2B retrieval is enabled, Engine
				// owns the single planner call for this turn and writes that same
				// result into trace.planner.
				traceRecorder.SetPlannerTraceSupplier(nil)
				eng.SetPlannerTraceObserver(traceRecorder.SetPlannerTrace)
				if knowledgeRetrievalEnabled {
					eng.SetRetrievalTraceObserver(traceRecorder.SetRetrievalTrace)
					eng.SetOutcomeTraceObserver(traceRecorder.SetOutcomeTrace)
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

func applyKnowledgeRetrieverStartup(eng *engine.Engine, requested bool, retriever *knowledge.Retriever, enabled bool, err error) {
	if requested && err != nil {
		startupFatalf("RAG enabled but retrieval setup failed (refusing to start): %v", err)
		return
	}
	if enabled && eng != nil {
		eng.SetKnowledgeRetriever(retriever)
	}
}

func applyStartupSuggestion(input string, suggestions []prompt.Suggestion, turnIndex int) (string, bool) {
	if turnIndex != 0 {
		return input, false
	}
	n, err := strconv.Atoi(input)
	if err != nil || n < 1 || n > len(suggestions) {
		return input, false
	}
	return suggestions[n-1].Text, true
}

func cliShadowPlannerInput(eng *engine.Engine, userText string) intent.PlannerInput {
	input := intent.PlannerInput{UserText: userText}
	if eng == nil {
		return input
	}
	input.PriorText = eng.PlannerPriorTextSnapshot()
	input.Resolver = eng.RegistrySnapshot()
	// PR1 hotfix Bug 2 (2026-05-28): structured prior-turn signals for the
	// planner USER prompt. PriorText is retained above for the validator's
	// source:prior_turn span check; buildUserPrompt no longer emits it.
	if state, _, hydrated := eng.SessionStateSnapshot(); hydrated {
		input.LastSelectedInstanceID = state.SelectedInstanceID
		input.LastIntent = state.LastIntent
	}
	input.LastAssistantSnippet = eng.PlannerLastAssistantSnippet()
	return input
}

type cliPlannerLLM struct {
	client *llm.Client
}

func (c cliPlannerLLM) CompleteIntentPlan(ctx context.Context, req intent.PlannerLLMRequest) (string, error) {
	resp, err := c.CompleteIntentPlanWithUsage(ctx, req)
	return resp.Content, err
}

func (c cliPlannerLLM) CompleteIntentPlanWithUsage(ctx context.Context, req intent.PlannerLLMRequest) (intent.PlannerLLMResponse, error) {
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
		return intent.PlannerLLMResponse{}, err
	}
	return intent.PlannerLLMResponse{Content: resp.Content, Usage: resp.Usage}, nil
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
