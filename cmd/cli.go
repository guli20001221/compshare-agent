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
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/prompt"
	"github.com/compshare-agent/internal/tools"

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
		fmt.Printf("    配置：%.0f 核 / %.0f GB\n", cpu, mem/1024) // Memory is MB; show GB
	}
	if z := str("Zone"); z != "" {
		fmt.Printf("    可用区：%s\n", z)
	}
	if ct := str("ChargeType"); ct != "" {
		fmt.Printf("    计费：%s\n", ct)
	}
	if p := str("price"); p != "" {
		fmt.Printf("    价格：%s\n", p)
		// The value already says 预估; this is the sentence that explains why.
		// Upstream quotes no locked price, so a number shown without this reads as
		// a commitment the platform has not made.
		if note := str("PriceNote"); note != "" {
			fmt.Printf("          %s\n", note)
		}
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

	// overlayGetenv makes the YAML runtime-flag sections (agent.features /
	// retrieval / trace / planner) win over the OS env, env being the fallback
	// for any field omitted in YAML. Every flag the CLI reads below goes through
	// it so a single config.yaml configures the CLI the same way it does the server.
	getenv := cfg.RuntimeGetenv(os.Getenv)

	ctx := context.Background()

	// Inject a CLI UserContext when needed. COMPSHARE_USER_EMAIL is a local
	// smoke-test stand-in for the production gateway's user_email field.
	if cliUser, ok := cliUserContextFromConfig(cfg, getenv); ok {
		ctx = tools.WithUser(ctx, cliUser)
	}

	scanner := bufio.NewScanner(os.Stdin)
	eng := engine.New(cfg, cliConfirm(scanner))
	mutatingToolsEnabled, unknownMutatingTools := mutatingToolsEnabledFromEnv(getenv)
	if unknownMutatingTools != "" {
		fmt.Fprintf(os.Stderr, "warning: ignoring unknown COMPSHARE_ENABLE_MUTATING_TOOLS value %q\n", unknownMutatingTools)
	}
	eng.SetMutatingToolsEnabled(mutatingToolsEnabled)
	// Consent-gated read-only in-instance SSH-ops lane (CLI path, F13: no durable dependency). Off
	// unless COMPSHARE_SSH_OPS=1; nil (logged) when off or misconfigured. The CLI injects it via the
	// per-session setter because engine.New builds SharedDeps internally and does not return it (B1).
	if r := cliInstanceOpsRunner(cfg, getenv); r != nil {
		eng.SetInstanceOps(r)
	}
	reactResultProjection, unknownReactResultProjection := reactResultProjectionEnabledFromEnv(getenv)
	if unknownReactResultProjection != "" {
		fmt.Fprintf(os.Stderr, "warning: ignoring unknown USE_REACT_RESULT_PROJECTION value %q\n", unknownReactResultProjection)
	}
	eng.SetReactResultProjectionEnabled(reactResultProjection)
	reactHistoryCompaction, unknownReactHistoryCompaction := reactHistoryCompactionEnabledFromEnv(getenv)
	if unknownReactHistoryCompaction != "" {
		fmt.Fprintf(os.Stderr, "warning: ignoring unknown USE_REACT_HISTORY_COMPACTION value %q\n", unknownReactHistoryCompaction)
	}
	eng.SetReactHistoryCompactionEnabled(reactHistoryCompaction)
	domainMatchGuard, unknownDomainMatchGuard := domainMatchGuardEnabledFromEnv(getenv)
	if unknownDomainMatchGuard != "" {
		fmt.Fprintf(os.Stderr, "warning: ignoring unknown COMPSHARE_RAG_DOMAIN_MATCH_GUARD value %q\n", unknownDomainMatchGuard)
	}
	engine.SetDomainMatchGuardEnabled(domainMatchGuard)
	forcedKnowledgeHop, unknownForcedKnowledgeHop := forcedKnowledgeHopEnabledFromEnv(getenv)
	if unknownForcedKnowledgeHop != "" {
		fmt.Fprintf(os.Stderr, "warning: ignoring unknown COMPSHARE_FORCED_KNOWLEDGE_HOP value %q\n", unknownForcedKnowledgeHop)
	}
	engine.SetForcedKnowledgeHopEnabled(forcedKnowledgeHop)
	canonicalTranscript, unknownCanonicalTranscript := canonicalTranscriptEnabledFromEnv(getenv)
	if unknownCanonicalTranscript != "" {
		fmt.Fprintf(os.Stderr, "warning: ignoring unknown COMPSHARE_CANONICAL_TRANSCRIPT value %q\n", unknownCanonicalTranscript)
	}
	engine.SetCanonicalTranscriptEnabled(canonicalTranscript)
	knowledgeRetrievalRequested, unknownKnowledgeRetrieval := knowledgeRetrievalModeFromEnv(getenv)
	if unknownKnowledgeRetrieval != "" {
		fmt.Fprintf(os.Stderr, "warning: ignoring unknown USE_KNOWLEDGE_RETRIEVAL value %q\n", unknownKnowledgeRetrieval)
	}
	knowledgeRetriever, knowledgeRetrievalEnabled, knowledgeErr := knowledgeRetrieverFromEnv(getenv)
	applyKnowledgeRetrieverStartup(eng, knowledgeRetrievalRequested, knowledgeRetriever, knowledgeRetrievalEnabled, knowledgeErr)
	traceWriter, traceEnabled, traceErr := traceWriterFromEnv(getenv)
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
	fmt.Println("╭──────────────────────────────────────╮")
	fmt.Println("│     Compshare Copilot v0.1           │")
	fmt.Println("╰──────────────────────────────────────╯")
	fmt.Println("runtime: agent_runtime=central")
	fmt.Printf("tools: %s\n", mutatingToolsRuntimeLine(mutatingToolsEnabled))
	fmt.Println()
	fmt.Println("正在初始化，获取您的实例信息...")

	var initTraceRecorder *cliTraceRecorder
	initStart := time.Now()
	if traceEnabled {
		initTraceRecorder = newCLITraceRecorder(traceWriter, "", 0, "init_context", initStart)
		initTraceRecorder.SetRuntimeTrace(observability.RuntimeTrace{RouterMode: "central_agent"})
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
		eng.SetRetrievalTraceObserver(nil)
		eng.SetFreshnessTraceObserver(nil)
		eng.SetDiagnosisTraceObserver(nil)
		eng.SetOutcomeTraceObserver(nil)
		eng.SetRendererTraceObserver(nil)
		eng.SetAuthorizationTraceObserver(nil)
		eng.SetTokenUsageObserver(nil)
		if traceEnabled {
			traceRecorder = newCLITraceRecorder(traceWriter, "", turnIndex, input, turnStart)
			traceRecorder.SetRuntimeTrace(observability.RuntimeTrace{RouterMode: "central_agent"})
			traceRecorder.SetRegistryTraceSupplier(eng.RegistryTraceState)
			eng.SetRateLimitObserver(traceRecorder.SetRateLimitDecision)
			eng.SetHardBlockObserver(traceRecorder.SetEngineHardBlock)
			eng.SetTokenUsageObserver(traceRecorder.AddTokenUsage)
			eng.SetFreshnessTraceObserver(traceRecorder.SetFreshnessTrace)
			eng.SetDiagnosisTraceObserver(traceRecorder.SetDiagnosisTrace)
			if knowledgeRetrievalEnabled {
				eng.SetRetrievalTraceObserver(traceRecorder.SetRetrievalTrace)
				eng.SetOutcomeTraceObserver(traceRecorder.SetOutcomeTrace)
			}
			eng.SetRendererTraceObserver(traceRecorder.SetRendererTrace)
			eng.SetAuthorizationTraceObserver(traceRecorder.AddAuthorizationTrace)
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
			traceRecorder.SetTerminalSignals(observability.FinishSignals{
				ReplyEmpty:                strings.TrimSpace(reply) == "",
				ReactRounds:               eng.ReactRoundsThisTurn(),
				RoundCeilingHit:           eng.ReactCeilingHitThisTurn(),
				ActionProposalDisposition: eng.ActionProposalDispositionThisTurn(),
			})
			sessState, _, hydrated := eng.SessionStateSnapshot()
			traceRecorder.SetStateTrace(observability.StateTrace{
				SessionStateHydrated:          hydrated,
				ResolutionSource:              eng.InstanceResolutionSource(),
				SelectedInstanceID:            sessState.SelectedInstanceID,
				SelectedInstanceIDAtTurnStart: eng.SelectedInstanceIDAtTurnStart(),
				FactCacheOldestAgeBucket:      observability.BucketFactCacheAge(eng.FactCacheOldestAgeSeconds()),
			})
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

func cliUserContextFromConfig(cfg *config.Config, getenv getenvFunc) (tools.UserContext, bool) {
	userEmail := strings.TrimSpace(getenv("COMPSHARE_USER_EMAIL"))
	if cfg.Agent.STS.DefaultRoleUrn == "" && userEmail == "" {
		return tools.UserContext{}, false
	}
	return tools.UserContext{
		RoleUrn:     cfg.Agent.STS.DefaultRoleUrn,
		SessionName: cfg.Agent.STS.DefaultSessionName,
		ProjectId:   cfg.Agent.ProjectId,
		Region:      cfg.Agent.Region,
		UserEmail:   userEmail,
	}, true
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
