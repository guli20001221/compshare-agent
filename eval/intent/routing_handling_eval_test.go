package intent_eval

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/config"
	intp "github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/require"
)

// Routing-accuracy gate (P0 阶段0 §③). Complements TestOnlineRouterEval (which
// scores the real router over the 68 synthetic fixtures.jsonl): this scores the
// real router over the 173-case production-representative, handling-class-labeled
// set (eval/realism/route_label_637/, derived from 637 real sessions) and reports
// per-class accuracy + an 8×8 confusion matrix + a regression floor.
//
// PRIMARY axis = handling_class (8 classes), the MECE-stable downstream-handling
// taxonomy. Per docs/research/routing_eval_set_637_2026-06-22.md §5: the planned
// taxonomy convergence (image 6→1, tail merges) does NOT change handling_class
// labels, so gating here survives those changes (only fine_intent would remap).
// fine_intent exact-match is reported as a SECONDARY metric.
//
// This is DELIBERATELY a label-coupled classifier eval — the opposite of the
// behavioral gate (cmd/behavioral_gate_test.go), which asserts only observable
// outcomes and must stay taxonomy-agnostic. Here we score intent labels on
// purpose; when the taxonomy is converged this small set is re-labeled (a
// maintenance step, not a conflict). See the two-axis rationale in
// eval/realism/two_axis_reclassify.py.
//
// Gated on -model (shared with TestOnlineRouterEval) so the default `go test ./...`
// suite skips it. Run:
//
//	go test ./eval/intent -run TestOnlineRoutingHandlingEval -model deepseek-v4-flash
//	go test ./eval/intent -run TestOnlineRoutingHandlingEval -model deepseek-v4-flash -min-route-acc 60

var (
	routeMinAcc    = flag.Float64("min-route-acc", 0, "fail if ROUTABLE handling-class accuracy (%) is below this; 0 = report-only")
	routeCasesPath = flag.String("routing-cases", "", "path to routing_eval_cases.jsonl; empty = the committed default")
	routeMaxCases  = flag.Int("routing-max-cases", 0, "cap the number of cases (for a cheap smoke); 0 = all")
)

const defaultRoutingCasesPath = "../realism/route_label_637/routing_eval_cases.jsonl"

type routingCase struct {
	SID           string `json:"sid"`
	HandlingClass string `json:"handling_class"`
	FineIntent    string `json:"fine_intent"`
	MultiHandling bool   `json:"multi_handling"`
	LastUserText  string `json:"last_user_text"`
}

// handlingClassOrder is the canonical row/column order for reporting + the
// confusion matrix. Includes greeting_smalltalk for transparency even though the
// router has no greeting intent (see intentToHandlingClass).
var handlingClassOrder = []string{
	"read_query",
	"knowledge_answer",
	"lifecycle_mutate",
	"diagnosis",
	"refuse_out_of_scope",
	"create_deploy",
	"ambiguous",
	"greeting_smalltalk",
}

// intentToHandlingClass maps a runtime router intent to its INTENDED / should-be
// downstream handling_class — the MECE handling taxonomy the labels use (see
// docs/research/routing_eval_set_637_2026-06-22.md §7: handling_class is labeled
// by "应该怎么处理", the should-be handling), NOT a snapshot of what the current
// engine literally does. For a few intents the engine has since diverged from the
// should-be (e.g. account-billing no longer canned-refuses — the keyword
// hard-block was removed 2026-06-10, see internal/engine/engine.go:64-66, so it
// now dispatches to ReAct; recommendation runs a tool-grounded read; vague_failure
// is a canned clarify). Those divergences are themselves routing/handling signal
// this gate surfaces — they are NOT mapping bugs.
//
//   - read_query: all deterministic read routes (resource/monitor/billing-read/
//     pricing/specs/stock/image-list/disk/expiry/net-accel).
//   - knowledge_answer: knowledge_qa + the kb-dominant advice intents.
//   - lifecycle_mutate: operation_lifecycle (confirm-gated saga).
//   - diagnosis: diagnosis/vague_failure (symptom triage).
//   - refuse_out_of_scope: billing_account_unsupported (should-be: honest refusal;
//     main no longer canned-refuses it → this class scoring ~0% is a real, known
//     routing gap, exactly what the eval exists to surface).
//   - create_deploy: deploy_model (create confirm saga).
//   - ambiguous: unknown (clarify).
//
// NOTE: there is NO router intent for greeting_smalltalk. The router emits
// `unknown` for "你好"-style turns, which maps to `ambiguous`. So greeting rows
// are structurally unrecoverable at the router layer (greeting is separated
// behaviorally and tested in the behavioral gate). For that reason the gate FLOOR
// is computed over ROUTABLE classes (all 8 except greeting_smalltalk); the full
// 8-class accuracy + matrix are still reported for transparency.
//
// mixed_billing_kb / mixed_diagnosis_kb are mapped defensively but are NOT in
// RuntimeIntents() (validIntent rejects them) — the router can never emit them,
// so a session whose fine_intent is mixed_* is predicted via the unknown→ambiguous
// fallback. The arms exist only so the mapping stays correct if they are ever
// promoted to runtime intents.
func intentToHandlingClass(intent intp.Intent) string {
	switch intent {
	case intp.IntentResourceInfo,
		intp.IntentMonitorQuery,
		intp.IntentMonitorHistory,
		intp.IntentBillingInstance,
		intp.IntentPricingQuery,
		intp.IntentGPUSpecsQuery,
		intp.IntentStockAvailability,
		intp.IntentPlatformImageList,
		intp.IntentCustomImageList,
		intp.IntentCommunityImageList,
		intp.IntentSharedImageList,
		intp.IntentImageTagCatalog,
		intp.IntentModelRepositoryBrowse,
		intp.IntentNetAcceleratorStatus,
		intp.IntentDiskInfo,
		intp.IntentExpiryRenewal:
		return "read_query"
	case intp.IntentKnowledgeQA,
		intp.IntentMixedBillingKB,
		intp.IntentRecommendation:
		return "knowledge_answer"
	case intp.IntentOperationLifecycle:
		return "lifecycle_mutate"
	case intp.IntentDiagnosis,
		intp.IntentVagueFailure,
		intp.IntentMixedDiagnosisKB:
		return "diagnosis"
	case intp.IntentBillingAccountUnsupported:
		return "refuse_out_of_scope"
	case intp.IntentDeployModel:
		return "create_deploy"
	case intp.IntentUnknown:
		return "ambiguous"
	default:
		// Any intent not explicitly mapped is treated as ambiguous so a new
		// enum value can never silently masquerade as a correct route.
		return "ambiguous"
	}
}

// routableClass reports whether the router can structurally ever produce this
// handling_class. greeting_smalltalk cannot (no greeting intent) and is excluded
// from the gate floor.
func routableClass(hc string) bool { return hc != "greeting_smalltalk" }

// realTrafficFreq is the handling_class distribution over all 637 labeled
// sessions (docs/research/routing_eval_set_637_2026-06-22.md §2). The 173-case
// eval set is BALANCED (≤25/class), so it deliberately over-weights the rare,
// high-misroute-cost minority classes (create/refuse/ambiguous) — that is the
// right basis for the GATE (a frequency-weighted gate could pass by only nailing
// read_query). This map exists solely to ALSO report a frequency-weighted
// accuracy that estimates production-load routing quality (read_query +
// knowledge_answer dominate real traffic and route near-perfectly), so the
// balanced ~60% is not misread as production accuracy. Report-only, never gated.
var realTrafficFreq = map[string]float64{
	"read_query":          0.416,
	"knowledge_answer":    0.250,
	"lifecycle_mutate":    0.113,
	"diagnosis":           0.075,
	"refuse_out_of_scope": 0.071,
	"greeting_smalltalk":  0.030,
	"create_deploy":       0.025,
	"ambiguous":           0.020,
}

func loadRoutingCases(t *testing.T, path string) []routingCase {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err, "open routing cases %s", path)
	defer f.Close()
	var cases []routingCase
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c routingCase
		require.NoError(t, json.Unmarshal([]byte(line), &c), line)
		cases = append(cases, c)
	}
	require.NoError(t, sc.Err())
	return cases
}

func TestOnlineRoutingHandlingEval(t *testing.T) {
	if *onlineModelFlag == "" {
		t.Skip("use -model to evaluate the real router over the routing set (e.g. -model deepseek-v4-flash)")
	}
	apiKey := os.Getenv("LLM_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("MODELVERSE_API_KEY")
	}
	if apiKey == "" {
		t.Fatal("-model set but no LLM_API_KEY / MODELVERSE_API_KEY in env")
	}
	baseURL := os.Getenv("LLM_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.modelverse.cn/v1"
	}

	path := *routeCasesPath
	if path == "" {
		path = defaultRoutingCasesPath
	}
	cases := loadRoutingCases(t, path)
	require.GreaterOrEqual(t, len(cases), 100, "routing set looks truncated")
	if *routeMaxCases > 0 && *routeMaxCases < len(cases) {
		cases = cases[:*routeMaxCases]
	}

	client := llm.NewClient(config.LLMConfig{BaseURL: baseURL, APIKey: apiKey, Model: *onlineModelFlag})
	router := intp.NewIntentRouter(onlineRouterLLM{client: client}, intp.IntentRouterOptions{
		BaseURL: baseURL,
		Model:   *onlineModelFlag,
	})

	// Tallies. confusion[actual][predicted].
	classTotal := map[string]int{}
	classCorrect := map[string]int{}
	confusion := map[string]map[string]int{}
	for _, a := range handlingClassOrder {
		confusion[a] = map[string]int{}
	}
	var overallTotal, overallCorrect int
	var routableTotal, routableCorrect int
	var fineTotal, fineCorrect int
	var multiTotal, multiCorrect int
	var routeErrors int

	for _, c := range cases {
		// Route the representative (last) user turn standalone. nil Registry is
		// the most permissive entity-validation path (the not-in-account check
		// is skipped — validator.go:128/138 — only source_span provenance is
		// enforced), which is the fair choice when we lack each session's live
		// account registry.
		result, err := router.Plan(context.Background(), intp.IntentRouterInput{
			UserText: c.LastUserText,
		})
		predicted := "ambiguous"
		if err != nil {
			routeErrors++
			t.Logf("ERROR %s (%s): %v", c.SID, c.HandlingClass, err)
		} else {
			predicted = intentToHandlingClass(result.Plan.Intent)
		}

		actual := c.HandlingClass
		classTotal[actual]++
		if _, ok := confusion[actual]; !ok {
			confusion[actual] = map[string]int{}
		}
		confusion[actual][predicted]++

		hit := predicted == actual
		overallTotal++
		if hit {
			overallCorrect++
			classCorrect[actual]++
		}
		if routableClass(actual) {
			routableTotal++
			if hit {
				routableCorrect++
			}
		}
		if c.MultiHandling {
			multiTotal++
			if hit {
				multiCorrect++
			}
		}
		// Secondary axis: fine_intent exact-match, only where the labeled
		// fine_intent is a real runtime router intent (skip label-only values
		// like "none"/"create_deploy" that the router enum doesn't emit).
		if err == nil && isRuntimeIntentLabel(c.FineIntent) {
			fineTotal++
			if string(result.Plan.Intent) == c.FineIntent {
				fineCorrect++
			}
		}
	}

	pct := func(n, d int) float64 {
		if d == 0 {
			return 0
		}
		return float64(n) / float64(d) * 100
	}

	overallAcc := pct(overallCorrect, overallTotal)
	routableAcc := pct(routableCorrect, routableTotal)

	// Frequency-weighted accuracy over the real-traffic class distribution
	// (report-only — see realTrafficFreq). Normalized over classes actually
	// present so it stays correct under -routing-max-cases smokes.
	var wNum, wDen float64
	for _, hc := range handlingClassOrder {
		if classTotal[hc] == 0 {
			continue
		}
		wNum += realTrafficFreq[hc] * (float64(classCorrect[hc]) / float64(classTotal[hc]))
		wDen += realTrafficFreq[hc]
	}
	weightedAcc := 0.0
	if wDen > 0 {
		weightedAcc = wNum / wDen * 100
	}

	t.Logf("routing handling-class eval (%s): cases=%d errors=%d", *onlineModelFlag, overallTotal, routeErrors)
	t.Logf("  overall accuracy (all 8 classes) = %.1f%% (%d/%d)", overallAcc, overallCorrect, overallTotal)
	t.Logf("  ROUTABLE accuracy (excl greeting, GATE BASIS) = %.1f%% (%d/%d)", routableAcc, routableCorrect, routableTotal)
	t.Logf("  real-traffic-weighted accuracy (report-only) = %.1f%% [§2 637-session freq; balanced set over-weights rare classes]", weightedAcc)
	t.Logf("  fine_intent exact-match (secondary) = %.1f%% (%d/%d)", pct(fineCorrect, fineTotal), fineCorrect, fineTotal)
	if multiTotal > 0 {
		t.Logf("  multi_handling subset = %.1f%% (%d/%d) [harder: session spans >1 class]", pct(multiCorrect, multiTotal), multiCorrect, multiTotal)
	}

	// Per-class recall.
	t.Logf("  per-class recall:")
	for _, hc := range handlingClassOrder {
		tot := classTotal[hc]
		if tot == 0 {
			continue
		}
		note := ""
		if !routableClass(hc) {
			note = "  (structurally unroutable: no router intent → excluded from gate)"
		}
		t.Logf("    %-20s %5.1f%% (%d/%d)%s", hc, pct(classCorrect[hc], tot), classCorrect[hc], tot, note)
	}

	// Confusion matrix (rows = actual label, cols = predicted).
	t.Logf("  confusion matrix (row=actual, col=predicted):")
	t.Logf("    %s", confusionHeader())
	for _, actual := range handlingClassOrder {
		if classTotal[actual] == 0 {
			continue
		}
		t.Logf("    %s", confusionRow(actual, confusion[actual]))
	}

	if *routeMinAcc > 0 && routableAcc < *routeMinAcc {
		t.Errorf("routable handling-class accuracy %.1f%% below threshold %.1f%%", routableAcc, *routeMinAcc)
	}
}

// isRuntimeIntentLabel reports whether a fine_intent label string is also an
// emittable runtime router intent (so fine_intent exact-match is well-defined).
func isRuntimeIntentLabel(label string) bool {
	for _, it := range intp.RuntimeIntents() {
		if string(it) == label {
			return true
		}
	}
	return false
}

// short abbreviates a handling_class for the confusion-matrix header/cells.
func shortClass(hc string) string {
	switch hc {
	case "read_query":
		return "read"
	case "knowledge_answer":
		return "know"
	case "lifecycle_mutate":
		return "lifec"
	case "diagnosis":
		return "diag"
	case "refuse_out_of_scope":
		return "refus"
	case "create_deploy":
		return "creat"
	case "ambiguous":
		return "ambig"
	case "greeting_smalltalk":
		return "greet"
	default:
		return hc
	}
}

func confusionHeader() string {
	cols := make([]string, 0, len(handlingClassOrder))
	for _, hc := range handlingClassOrder {
		cols = append(cols, fmt.Sprintf("%5s", shortClass(hc)))
	}
	return fmt.Sprintf("%-12s | %s", "actual\\pred", strings.Join(cols, " "))
}

func confusionRow(actual string, row map[string]int) string {
	cells := make([]string, 0, len(handlingClassOrder))
	for _, hc := range handlingClassOrder {
		cells = append(cells, fmt.Sprintf("%5d", row[hc]))
	}
	// Append any predicted classes outside the canonical order (defensive).
	var extra []string
	for pred, n := range row {
		if !containsClass(handlingClassOrder, pred) {
			extra = append(extra, fmt.Sprintf("%s=%d", pred, n))
		}
	}
	sort.Strings(extra)
	suffix := ""
	if len(extra) > 0 {
		suffix = "  +" + strings.Join(extra, ",")
	}
	return fmt.Sprintf("%-12s | %s%s", shortClass(actual), strings.Join(cells, " "), suffix)
}

func containsClass(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
