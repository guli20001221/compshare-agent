//go:build live

// Live probe for the knowledge query planner's fan-out.
//
// Why this exists: maxSearchKnowledgeCallsPerTurn bounds the per-turn retrieval
// budget, but executeSearchKnowledge increments that counter once per PLANNED
// QUERY rather than once per SearchKnowledge call. So the planner's fan-out
// decides whether a second retrieval hop is reachable at all. Before changing
// the counter semantics we need the real fan-out distribution, measured on real
// production questions rather than invented ones.
//
// The corpus is NOT committed: real customer questions stay in ignored local
// evaluation output. Point COMPSHARE_REAL_QUERY_CORPUS at that JSONL.
//
//	go test ./internal/engine -tags live -run TestLivePlannerFanout -v \
//	  -timeout 30m
//
// Env:
//
//	COMPSHARE_LIVE_CONFIG        config.yaml path (default ../../deploy/conf/config.yaml)
//	COMPSHARE_REAL_QUERY_CORPUS  JSONL with {case_id,date,category,query}
//	COMPSHARE_PROBE_OUT          optional path for the JSON summary
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/llm"
)

type realQueryCase struct {
	CaseID   string `json:"case_id"`
	Date     string `json:"date"`
	Category string `json:"category"`
	Query    string `json:"query"`
}

type fanoutObservation struct {
	Arm      string   `json:"arm"`
	CaseID   string   `json:"case_id"`
	Category string   `json:"category"`
	Planned  int      `json:"planned_queries"`
	Executed int      `json:"executed_queries"`
	Dropped  int      `json:"dropped_queries"`
	Queries  []string `json:"-"` // real text: never serialized to the summary
}

// executedUnderCurrentBudget mirrors executeSearchKnowledge's loop: the counter
// is incremented per planned query and the loop breaks at the cap, so a plan of
// N queries executes min(N, maxSearchKnowledgeCallsPerTurn) of them in the very
// first SearchKnowledge call.
func executedUnderCurrentBudget(planned int) int {
	if planned > maxSearchKnowledgeCallsPerTurn {
		return maxSearchKnowledgeCallsPerTurn
	}
	return planned
}

func loadLiveConfig(t *testing.T) *config.Config {
	t.Helper()
	path := os.Getenv("COMPSHARE_LIVE_CONFIG")
	if path == "" {
		path = filepath.Join("..", "..", "deploy", "conf", "config.yaml")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config %s: %v", path, err)
	}
	if cfg.Agent.LLM.APIKey == "" || cfg.Agent.LLM.Model == "" {
		t.Fatalf("config %s has no usable agent.llm (model=%q)", path, cfg.Agent.LLM.Model)
	}
	return cfg
}

func loadRealQueries(t *testing.T) []realQueryCase {
	t.Helper()
	path := os.Getenv("COMPSHARE_REAL_QUERY_CORPUS")
	if path == "" {
		t.Skip("COMPSHARE_REAL_QUERY_CORPUS not set; real questions are never committed")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var out []realQueryCase
	for _, line := range splitJSONLines(raw) {
		var one realQueryCase
		if err := json.Unmarshal(line, &one); err != nil {
			t.Fatalf("corpus line: %v", err)
		}
		if one.Query != "" {
			out = append(out, one)
		}
	}
	if len(out) == 0 {
		t.Fatalf("corpus %s produced no queries", path)
	}
	return out
}

func splitJSONLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i <= len(raw); i++ {
		if i == len(raw) || raw[i] == '\n' {
			line := raw[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if len(line) > 0 {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	return out
}

// planOnce builds a throwaway Engine so concurrent probes never share per-turn
// state, then asks the production planner for this turn's retrieval queries.
func planOnce(cfg *config.Config, prior []ConversationPair, current string) knowledgeQueryPlan {
	eng := NewWithDeps(llm.NewClient(cfg.Agent.LLM), &mockExecutor{}, nil)
	eng.turnContextViewThisTurn = TurnContextView{
		CurrentQuestion:    current,
		RecentConversation: prior,
	}
	eng.turnContextViewReady = true
	return eng.planKnowledgeQuery(context.Background(), current)
}

// loadContextReplayPairs is arm A: the production-derived multi-turn cases that
// already ship in the repo. Only the follow-up turn is probed, because that is
// the only position where planKnowledgeQuery runs at all (it returns the
// one-query fallback when RecentConversation is empty).
//
// The replay fixtures record the user turns and the expectations, not the
// assistant prose, so the prior pair carries the real user utterance with an
// empty assistant side. That is weaker context than production, which biases
// this arm toward FEWER planned queries — i.e. it under-reports the defect.
func loadContextReplayPairs(t *testing.T) []probeInput {
	t.Helper()
	path := filepath.Join("..", "..", "eval", "contextreplay", "cases.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var file struct {
		Cases []struct {
			Name  string `json:"name"`
			Turns []struct {
				Message string `json:"message"`
			} `json:"turns"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []probeInput
	for _, c := range file.Cases {
		if len(c.Turns) < 2 {
			continue // single-turn case: the planner never fires
		}
		out = append(out, probeInput{
			Arm:      "A-real-multiturn",
			CaseID:   c.Name,
			Category: "contextreplay",
			Prior:    []ConversationPair{{User: c.Turns[0].Message}},
			Current:  c.Turns[1].Message,
		})
	}
	if len(out) == 0 {
		t.Fatalf("%s has no multi-turn case", path)
	}
	return out
}

// pairWithinCategory is arm B: every utterance is a real production question
// from the 6.26-7.9 window, but the adjacency is constructed — each question is
// probed as the follow-up of a different real question in the same category.
// Read it as a conditional fan-out distribution, never as a traffic rate.
func pairWithinCategory(corpus []realQueryCase) []probeInput {
	byCategory := map[string][]realQueryCase{}
	for _, c := range corpus {
		byCategory[c.Category] = append(byCategory[c.Category], c)
	}
	var out []probeInput
	for _, c := range corpus {
		peers := byCategory[c.Category]
		if len(peers) < 2 {
			continue
		}
		var prior realQueryCase
		for i, p := range peers {
			if p.CaseID == c.CaseID {
				prior = peers[(i+1)%len(peers)]
				break
			}
		}
		if prior.Query == "" {
			continue
		}
		out = append(out, probeInput{
			Arm:      "B-real-question-synthetic-adjacency",
			CaseID:   c.CaseID,
			Category: c.Category,
			Prior:    []ConversationPair{{User: prior.Query}},
			Current:  c.Query,
		})
	}
	return out
}

func TestLivePlannerFanoutOnRealTraffic(t *testing.T) {
	cfg := loadLiveConfig(t)
	corpus := loadRealQueries(t)

	// Arm A — real multi-turn shape. Every utterance and both positions come
	// from the committed production-derived replay cases.
	armA := loadContextReplayPairs(t)

	// Arm B — real questions, synthetic pairing. Each production question is
	// placed as the follow-up of a DIFFERENT production question from the same
	// category, so both utterances are real but the adjacency is constructed.
	// This measures the conditional fan-out ("if this question arrives with
	// history, how many queries does the planner emit"), not a traffic rate.
	armB := pairWithinCategory(corpus)

	observations := runFanoutProbes(t, cfg, armA, armB)
	reportFanout(t, observations)
}

type probeInput struct {
	Arm      string
	CaseID   string
	Category string
	Prior    []ConversationPair
	Current  string
}

func runFanoutProbes(t *testing.T, cfg *config.Config, arms ...[]probeInput) []fanoutObservation {
	t.Helper()
	var inputs []probeInput
	for _, arm := range arms {
		inputs = append(inputs, arm...)
	}

	results := make([]fanoutObservation, len(inputs))
	const workers = 6
	var wg sync.WaitGroup
	ch := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				in := inputs[i]
				plan := planOnce(cfg, in.Prior, in.Current)
				planned := len(plan.SearchQueries)
				executed := executedUnderCurrentBudget(planned)
				results[i] = fanoutObservation{
					Arm:      in.Arm,
					CaseID:   in.CaseID,
					Category: in.Category,
					Planned:  planned,
					Executed: executed,
					Dropped:  planned - executed,
					Queries:  plan.SearchQueries,
				}
			}
		}()
	}
	for i := range inputs {
		ch <- i
	}
	close(ch)
	wg.Wait()
	return results
}

func reportFanout(t *testing.T, observations []fanoutObservation) {
	t.Helper()
	byArm := map[string][]fanoutObservation{}
	for _, o := range observations {
		byArm[o.Arm] = append(byArm[o.Arm], o)
	}
	arms := make([]string, 0, len(byArm))
	for arm := range byArm {
		arms = append(arms, arm)
	}
	sort.Strings(arms)

	for _, arm := range arms {
		rows := byArm[arm]
		hist := map[int]int{}
		var multiHopDead, silentDrop int
		for _, o := range rows {
			hist[o.Planned]++
			// A plan that consumes the whole budget in one call leaves no
			// second SearchKnowledge hop: engine.go withdraws the tool.
			if o.Executed >= maxSearchKnowledgeCallsPerTurn {
				multiHopDead++
			}
			if o.Dropped > 0 {
				silentDrop++
			}
		}
		t.Logf("== arm %s (n=%d) ==", arm, len(rows))
		for n := 1; n <= maxKnowledgePlanQueries; n++ {
			if hist[n] > 0 {
				t.Logf("  planned=%d : %d (%.0f%%)", n, hist[n], 100*float64(hist[n])/float64(len(rows)))
			}
		}
		t.Logf("  第二跳不可用(单次调用即耗尽预算): %d/%d (%.0f%%)",
			multiHopDead, len(rows), 100*float64(multiHopDead)/float64(len(rows)))
		t.Logf("  计划了但被静默丢弃: %d/%d (%.0f%%)",
			silentDrop, len(rows), 100*float64(silentDrop)/float64(len(rows)))
	}

	if out := os.Getenv("COMPSHARE_PROBE_OUT"); out != "" {
		blob, err := json.MarshalIndent(observations, "", "  ")
		if err != nil {
			t.Fatalf("marshal summary: %v", err)
		}
		if err := os.WriteFile(out, blob, 0o600); err != nil {
			t.Fatalf("write summary: %v", err)
		}
		t.Logf("summary -> %s (query text omitted)", out)
	}
	fmt.Fprintln(os.Stderr, "fanout probe complete")
}
