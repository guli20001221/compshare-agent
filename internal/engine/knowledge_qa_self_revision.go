package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

// kqaSelfRevisionOn gates the over-conservatism self-revision pass on an
// agent-loop knowledge_qa answer. The Go-package default stays false (so engine
// unit tests are unaffected and flag-off is byte-identical — the
// disciplined-synthesis draft is delivered as-is); the DEPLOY default is ON,
// resolved at boot from COMPSHARE_KQA_SELF_REVISION in cmd (2026-07-08, gated on
// the beval2 A/B: target over_conservative 25->10, 0 fabrication regression).
//
// MOTIVATION (beval over_conservative characterization, 2026-07-08): 78% of the
// over_conservative failures had STRONG evidence that survived the relevance
// floor — the model retrieved the right chunk, answered the substantive part
// correctly, then reflexively (a) appended an unwarranted disclaimer tail about a
// trivial missing console last-mile ("控制台具体入口/步骤资料未写明，建议联系客服"),
// (b) listed the correct methods then disclaimed "资料未覆盖", or (c) declined to
// state a conclusion the evidence supports (billing list lacks "流量" → won't say
// "不单独收费"; rules don't mention "审批" → won't say "无需审批"). The same question
// on a DIFFERENT run often answers well, so the capability is present — the hedge
// is a reflexive, nondeterministic tic, not a knowledge gap. This is a SYNTHESIS
// problem fusion/retrieval cannot touch. The fix leverages agent capability: a
// second pass that re-reads (question, draft, evidence) and commits to the answer
// the draft already supports, adding NO fact absent from the evidence. It is
// bounded by re-validation in synthesizeKnowledgeQAFromLedger (a revision that
// breaks grounding / refuses is discarded), so it is never worse than the draft.
// Set once at boot from COMPSHARE_KQA_SELF_REVISION (cmd); the Go-package default
// stays false so engine unit tests are unaffected.
var kqaSelfRevisionOn bool

// SetKQASelfRevisionEnabled toggles the over-conservatism self-revision pass.
// Boot-only (reversible by restart), mirroring SetDisciplinedKnowledgeQASynthesisEnabled.
func SetKQASelfRevisionEnabled(v bool) { kqaSelfRevisionOn = v }

// KQASelfRevisionEnabled reports whether the self-revision pass is on.
func KQASelfRevisionEnabled() bool { return kqaSelfRevisionOn }

const kqaSelfRevisionSystem = `你是回答质量复核器。你会拿到：用户问题、检索到的证据（每条以 [n] 编号）、以及一份"基于证据"的回答草稿。你的唯一任务是修正草稿的"过度保守"——让它把证据已经支持的内容正面、完整地回答出来。

需要修正的情形：
1. 草稿已给出实质答案，却在结尾追加了关于"控制台具体入口/按钮/步骤资料未写明""建议联系客服/自行查看文档"之类**琐碎、且不影响主答案**的免责尾——删除或弱化这条尾巴，让回答正面收尾。
2. 草稿自己已经列出了做法（如 SCP、下载数据盘、制作镜像），却又说"资料未覆盖具体方法"——这是自相矛盾，改为直接、展开地讲这些做法。
3. 证据支持一个明确结论（例如：计费项列表里没有"流量"→ 说明流量不单独收费；规则里没有"审批"环节 → 说明无需审批），却用"无法确认/不确定"回避——把这个结论明确说出来。

铁律（防止编造，最高优先级）：
- 只能使用【证据】中出现的事实。**绝对不得**新增证据里没有的步骤、按钮名称、菜单路径、页面名、API、参数、数字或命令。缺的就是缺，不要编造来填补。
- 证据里带 [n] 标记的事实，修正后请把对应的 [n] 标记原样保留在该事实后面。
- 如果草稿的保守是**正当的**——证据里确实没有这个答案，或问题涉及实时账户数据（余额/发票/提现到账）、或需要用户本人凭证才能执行的动作——则原样返回草稿，不要强行编出答案。
- 不确定时，倾向于少改、保留草稿。

只输出修正后的回答正文（含保留的 [n] 标记），不要输出任何解释、前缀或元话语。`

// reviseOverConservativeAnswer re-reads a grounded disciplined-synthesis draft and
// rewrites it to commit to the answer the evidence already supports when the draft
// reflexively over-hedges — using ONLY the provided evidence and preserving the [n]
// citation markers (so the caller's grounding re-validation still holds). It shows
// the evidence with the SAME [n] numbering as answerWithRetrievedEvidence
// (ragReferencesFromEvidence) so the markers line up. Best-effort: returns
// (draft, false) on any error / empty response so the caller keeps the original.
func (e *Engine) reviseOverConservativeAnswer(ctx context.Context, userMsg, draft string, evidences []envelope.Evidence) (string, bool) {
	draft = strings.TrimSpace(draft)
	if draft == "" || e.llmClient == nil {
		return draft, false
	}
	var b strings.Builder
	b.WriteString("用户问题：\n")
	b.WriteString(strings.TrimSpace(userMsg))
	b.WriteString("\n\n证据（只能使用这里出现的事实）：\n")
	for _, ref := range ragReferencesFromEvidence(evidences) {
		number := ref.Number
		if number <= 0 {
			number = 1
		}
		b.WriteString(fmt.Sprintf("[%d] %s\n", number, strings.TrimSpace(ref.Title)))
		b.WriteString(strings.TrimSpace(ref.Content))
		b.WriteString("\n\n")
	}
	b.WriteString("回答草稿：\n")
	b.WriteString(draft)

	resp, err := e.llmClient.Chat(ctx, llm.ChatRequest{Messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: kqaSelfRevisionSystem},
		{Role: openai.ChatMessageRoleUser, Content: b.String()},
	}})
	if err != nil || resp == nil {
		return draft, false
	}
	e.emitTokenUsage(resp.Usage)
	revised := strings.TrimSpace(resp.Content)
	if revised == "" {
		return draft, false
	}
	return revised, true
}
