//go:build live

package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

// End-to-end A/B for gap-driven retrieval, scored on the answer rather than on
// the retrieved chunk set.
//
// Why the answer and not the chunks: an A/A probe showed the retrieved set
// flipping on 50-60% of identical inputs, dominated by cosmetic planner
// rewrites. Swapping one chunk out of ten usually does not change what the user
// reads, so a chunk-set metric measures something the user cannot perceive, at a
// noise level no arm difference could clear.
//
// Two things this harness does deliberately:
//
//   - It runs an A/A pair (arm 1 twice) alongside the A/B pair. The A/A pair is
//     the floor: if the judge prefers one arm-1 answer over the other arm-1
//     answer as often as it prefers arm 2, the A/B result is noise. Reporting a
//     win rate without that floor would be reporting nothing.
//   - It judges only the cases where the arms actually diverged. The arms differ
//     when the planner fans out (measured at 8-14% of multi-turn turns) or when
//     evidence is thin enough to trigger the follow-up affordance. Judging the
//     identical remainder buries a real effect under cases that cannot show one;
//     the divergent subset size is reported so the power is visible, not implied.
//
// Judging is pairwise with a position swap, because there is no ground-truth
// answer to grade against and pairwise is far more sensitive than absolute
// grading at this sample size. A pair counts as a win only if both orderings
// agree; disagreement is recorded as a tie, which is the judge telling us it
// cannot separate them.
//
//	go test ./internal/engine -tags live -run TestLiveGapDrivenAB -v -timeout 60m
type abAnswer struct {
	Text     string
	Searches int
	Queries  int
}

func runTurnForArm(t *testing.T, cfg *abConfig, in probeInput, gapDriven bool) abAnswer {
	previous := gapDrivenRetrievalEnabled
	SetGapDrivenRetrievalEnabled(gapDriven)
	defer SetGapDrivenRetrievalEnabled(previous)

	eng := NewWithDeps(llm.NewClient(cfg.LLM), &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(cfg.Retriever)
	eng.InitWithContext("用户当前没有实例。")
	for _, pair := range in.Prior {
		eng.messages = append(eng.messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: pair.User},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: firstNonEmptyText(pair.Assistant, "（略）")},
		)
	}
	reply, err := eng.Chat(context.Background(), in.Current, noopStep)
	if err != nil {
		t.Logf("case %s arm gapDriven=%v errored: %v", in.CaseID, gapDriven, err)
	}
	return abAnswer{
		Text:     reply,
		Searches: eng.searchKnowledgeCallsThisTurn,
		Queries:  eng.searchKnowledgeQueriesThisTurn,
	}
}

func firstNonEmptyText(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// row holds one case's three end-to-end runs: the control arm, a second run of
// the SAME control arm (the A/A partner that establishes the floor), and the
// experiment arm.
type row struct {
	in    probeInput
	base  abAnswer
	baseB abAnswer
	gap   abAnswer
}

type abConfig struct {
	LLM       config.LLMConfig
	Retriever *knowledge.Retriever
	Judge     config.LLMConfig
}

func TestLiveGapDrivenAB(t *testing.T) {
	cfg := loadLiveConfig(t)
	corpus := loadRealQueries(t)
	inputs := pairWithinCategory(corpus)

	judgeCfg := cfg.Agent.LLM
	judgeCfg.Model = judgeModelName()

	abCfg := &abConfig{
		LLM:       cfg.Agent.LLM,
		Retriever: deterministicCorpusRetriever(t),
		Judge:     judgeCfg,
	}

	rows := make([]row, len(inputs))
	var wg sync.WaitGroup
	ch := make(chan int)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				in := inputs[i]
				rows[i] = row{
					in:    in,
					base:  runTurnForArm(t, abCfg, in, false),
					baseB: runTurnForArm(t, abCfg, in, false),
					gap:   runTurnForArm(t, abCfg, in, true),
				}
			}
		}()
	}
	for i := range inputs {
		ch <- i
	}
	close(ch)
	wg.Wait()

	var divergent []row
	for _, r := range rows {
		if strings.TrimSpace(r.base.Text) == "" || strings.TrimSpace(r.gap.Text) == "" {
			continue
		}
		if r.base.Queries != r.gap.Queries || r.base.Searches != r.gap.Searches {
			divergent = append(divergent, r)
		}
	}
	t.Logf("== 端到端 A/B (gap-driven retrieval) ==")
	t.Logf("  总 case            : %d", len(rows))
	t.Logf("  两臂检索行为不同    : %d  (只判这些；其余两臂输入相同，判了也只是噪声)", len(divergent))
	if len(divergent) == 0 {
		t.Skip("no case diverged between the arms; nothing to judge")
	}

	aaWins, abWins := judgePairs(t, abCfg, divergent, func(r row) (abAnswer, abAnswer) {
		return r.base, r.baseB
	}), judgePairs(t, abCfg, divergent, func(r row) (abAnswer, abAnswer) {
		return r.base, r.gap
	})
	t.Logf("  A/A 噪声地板(同臂互比，'第二个更好'的比例): %.0f%% (%d/%d)",
		100*float64(aaWins)/float64(len(divergent)), aaWins, len(divergent))
	t.Logf("  A/B gap-driven 更好的比例               : %.0f%% (%d/%d)",
		100*float64(abWins)/float64(len(divergent)), abWins, len(divergent))
	t.Logf("  => 只有 A/B 明显高于 A/A 才算有效果")
}

// judgePairs asks the judge twice per pair with the answers swapped, and counts
// a win only when both orderings agree. Position bias in pairwise judging is
// large enough to manufacture a result on its own.
func judgePairs(t *testing.T, cfg *abConfig, rows []row, pick func(row) (abAnswer, abAnswer)) int {
	t.Helper()
	wins := 0
	for _, r := range rows {
		left, right := pick(r)
		forward := judgeOnce(t, cfg, r.in.Current, left.Text, right.Text)
		backward := judgeOnce(t, cfg, r.in.Current, right.Text, left.Text)
		if forward == "B" && backward == "A" {
			wins++
		}
	}
	return wins
}

func judgeOnce(t *testing.T, cfg *abConfig, question, a, b string) string {
	t.Helper()
	prompt := fmt.Sprintf(`你在评估两份对同一问题的回答，判断哪份对提问者更有用。
标准：是否直接回答了问题、事实是否有依据、是否避免了无根据的断言。更长不等于更好。
只输出一个字符：A 或 B。无法区分则输出 T。

问题：%s

回答 A：
%s

回答 B：
%s`, question, a, b)
	resp, err := llm.NewClient(cfg.Judge).Chat(context.Background(), llm.ChatRequest{
		Messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: prompt}},
	})
	if err != nil || resp == nil {
		t.Logf("judge error: %v", err)
		return "T"
	}
	verdict := strings.ToUpper(strings.TrimSpace(resp.Content))
	switch {
	case strings.HasPrefix(verdict, "A"):
		return "A"
	case strings.HasPrefix(verdict, "B"):
		return "B"
	default:
		return "T"
	}
}

func judgeModelName() string {
	if name := strings.TrimSpace(os.Getenv("COMPSHARE_JUDGE_MODEL")); name != "" {
		return name
	}
	return "doubao-seed-2-1-turbo-260628"
}
