//go:build live

// Classifies each of the 50 real production queries by WHICH SOURCE its answer
// should come from, not by whether retrieval found evidence. This is the split
// the gap audit could not make: the audit and the partial-classifier both assumed
// every case is a RAG question and only asked "is the content retrievable". The
// JupyterLab case (real-600d35c95c0434ac) broke that assumption — its answer is a
// live instance endpoint (GetSoftwareURL / instance_access), not a corpus chunk —
// so the real question for sizing agent-layer work is: of the 50, how many belong
// to a TOOL, how many to the CORPUS, and how many to an honest "no public doc".
//
// # Why source, not sufficiency
//
// "Is the evidence sufficient" is exactly where the judge disagrees with itself
// (10% verdict flips on byte-identical evidence). "Where does the answer live"
// is a more stable question: it turns on what the platform exposes (a documented
// tool surface) and what kind of question it is (live account data vs product
// rule vs out-of-scope), neither of which depends on a retrieval score.
//
// # Buckets
//
//	tool   — the answer is the user's live account/instance data, or a
//	         platform-specific action/config, that one of the read/workflow/
//	         diagnose tools provides. Answering from the corpus would be stale or
//	         generic. (JupyterLab access, "what's my instance's IP", a refund
//	         estimate, current stock.)
//	corpus — a documentation / knowledge question: product rules, feature scope,
//	         how-to, tech principles, GPU/AI/Linux troubleshooting knowledge.
//	         Does not depend on this user's live data; can be written into a doc.
//	say_no — the platform has NEITHER a public doc NOR a tool for it: a service
//	         the platform does not offer, something outside an compute platform's
//	         scope, or a human-support-only matter. The honest "I have no material
//	         on X" exit — which must speak to evidence, never deny the product.
//	undetermined — the classifier failed (LLM error / unparseable output).
//
// A case may plausibly touch two buckets (JupyterLab is a tool for a user WITH an
// instance, a how-to otherwise). The tie-break is the PRIMARY authoritative
// source: a question about a specific instance's state/access/price is tool; a
// general "how does X work / how do I generally do X" is corpus.
//
// The retrieved top chunks are shown so the classifier can tell corpus from
// say_no (does the corpus actually carry it), but the decision is source-first:
// a weak retrieval does not by itself make a real doc question a say_no.
//
// Real customer questions are never committed. Queries come from
// COMPSHARE_REAL_QUERY_CORPUS; per-case detail (which quotes them) goes only to
// COMPSHARE_PROBE_OUT, outside the repo. Runs on deepseek-v4-pro
// (COMPSHARE_PRO_MODEL), the decided production/verification model.
//
//	go test ./internal/engine -tags live -run TestLiveThreeWaySourceClassification -v -timeout 40m
package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/knowledge"
)

const (
	bucketTool  = "tool"
	bucketCorp  = "corpus"
	bucketSayNo = "say_no"
	bucketUndet = "undetermined"

	// threeWayRounds mirrors the partial classifier: a single pass is not a
	// measurement when the classifier disagrees with itself ~18% of the time.
	threeWayRounds = 5
	// threeWayTopChunks is how much retrieved evidence the classifier sees for the
	// corpus-vs-say_no call. Enough to show whether the corpus carries it.
	threeWayTopChunks = 5
)

// toolSurfaceCatalog is the read/workflow/diagnose surface the central agent can
// reach, curated from internal/tools/registry.go + read_instance_access.go. It is
// what makes a "tool" verdict concrete: the classifier must be able to name the
// tool that would answer, or it is not a tool case. Grouped, not exhaustive on
// every param, because the classifier needs to know WHAT each answers.
const toolSurfaceCatalog = `平台可调用的工具（只列能力，不是全部参数）：
【实例实时数据】
- DescribeCompShareInstance：某台实例的状态、配置、磁盘、SSH 登录命令（SshLoginCommand 是 SSH 权威来源）。
- GetCompShareInstanceMonitor：实例 CPU/内存/GPU/显存 使用率，实时或历史（≤24h 窗口）。
- instance_access：检查某台实例 SSH / Jupyter / 自定义端口的云侧配置，并可在用户明确要求时取该实例的 Jupyter Token（不进实例、不改防火墙）。
- DescribeCompShareSoftwarePort：镜像的应用端口映射目录（JupyterLab/FileBrowser 等）。
【价格 / 退费】
- GetCompShareInstancePrice / GetCompShareInstanceUserPrice：创建实例目录价 / 用户折后价。
- GetCompShareRefundPrice：某台实例现在释放可退金额（只读估算）。
- GetCompShareInstanceUpgradePrice：变配（升降 CPU/GPU/内存）差额。
- CFS 价格族：GetCompShareCFSPrice / UpgradePrice / RefundPrice。
【库存 / 规格】
- DescribeCompShareGpuInventory：GPU 原始张数库存快照。
- CheckCompShareResourceCapacity：某具体配置现在能否创建。
- DescribeAvailableCompShareInstanceTypes / GPU 规格：机型与合法 CPU/内存/GPU 组合、是否售罄。
【镜像 / 模型仓库】
- DescribeCompShareImages（平台）/ CustomImages（自制）/ SharingImages（共享给我）/ CommunityImages（社区）/ 镜像标签目录。
- DescribeModelRepositoryModels / Tags：公共模型仓库有哪些模型 / 标签。
【网络 / 存储】
- CheckCompShareNetOptimizer：网络加速是否已开通。
- DescribeCFS：共享文件存储列表 / 单个 CFS。
【写操作（需确认）】创建/开机/关机/重启/改名/重置密码/变配/重装/挂盘/扩盘/制作镜像/跨区克隆镜像/开网络加速/建 CFS/定时关机。
【诊断链】DiagnoseBilling（计费异常）；实例内 SSH/端口/进程诊断走 instance_access + 实例内只读排查。`

type sourceVerdict struct {
	CaseID   string `json:"case_id"`
	Category string `json:"category"`
	Query    string `json:"query"`
	Bucket   string `json:"bucket"`
	// Tool is the tool the classifier says would answer, empty unless tool.
	// Recorded so a "tool" verdict can be sanity-checked against the real surface
	// rather than trusted blind.
	Tool string `json:"tool,omitempty"`
	// CorpusCovers is the classifier's read of whether the shown chunks already
	// answer it — the corpus/say_no discriminator, recorded separately from the
	// bucket so a "corpus" verdict on empty evidence (a doc that SHOULD exist but
	// doesn't) is distinguishable from one on real evidence.
	CorpusCovers bool   `json:"corpus_covers"`
	Reason       string `json:"reason"`
	VoteSplit    string `json:"vote_split"`
	Note         string `json:"note,omitempty"`
}

func TestLiveThreeWaySourceClassification(t *testing.T) {
	cfg := loadLiveConfig(t)
	cfg.Agent.LLM.Model = proModelName() // decided production/verification model
	t.Logf("classifier model = %s", cfg.Agent.LLM.Model)

	// Reachability smoke first: an unreachable model / bad key returns an error on
	// every one of the 250 classification calls and looks identical to "the model
	// classified everything as undetermined". Fail loud in one call instead.
	if _, err := askJSON(cfg, `只输出 JSON：{"ok":true}`); err != nil {
		t.Fatalf("model %q not reachable on %s: %v", cfg.Agent.LLM.Model, cfg.Agent.LLM.BaseURL, err)
	}
	t.Logf("model reachable")

	cases := loadAllRealCases(t)
	if len(cases) == 0 {
		t.Fatalf("no real cases loaded")
	}
	corpus, sidecar := mergedProductionIndex(t)
	retriever := productionAnswerRetriever(t, cfg, corpus, sidecar)
	t.Logf("cases=%d  merged index=%d chunks", len(cases), len(corpus.Chunks))

	votes := make([][]sourceVerdict, len(cases))
	type job struct{ caseIdx, round int }
	jobs := make(chan job)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				got := classifySource(cfg, retriever, cases[j.caseIdx])
				mu.Lock()
				votes[j.caseIdx] = append(votes[j.caseIdx], got)
				mu.Unlock()
			}
		}()
	}
	for round := 0; round < threeWayRounds; round++ {
		for i := range cases {
			jobs <- job{caseIdx: i, round: round}
		}
	}
	close(jobs)
	wg.Wait()

	results := make([]sourceVerdict, len(cases))
	for i := range cases {
		results[i] = majorityBucket(votes[i])
	}

	reportSourceBuckets(t, results)
	writeSourceDetail(t, results)
}

type realCase struct {
	CaseID   string `json:"case_id"`
	Category string `json:"category"`
	Query    string `json:"query"`
}

// loadAllRealCases reads every case in the uncommitted corpus (all 50, not just
// the graded subset). Category is kept when present for the per-bucket breakdown.
func loadAllRealCases(t *testing.T) []realCase {
	t.Helper()
	path := os.Getenv("COMPSHARE_REAL_QUERY_CORPUS")
	if path == "" {
		t.Skip("COMPSHARE_REAL_QUERY_CORPUS not set; real questions are never committed")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var out []realCase
	for _, line := range splitJSONLines(raw) {
		var one realCase
		if err := json.Unmarshal(line, &one); err != nil {
			t.Fatalf("corpus line: %v", err)
		}
		if strings.TrimSpace(one.Query) == "" {
			continue
		}
		out = append(out, one)
	}
	return out
}

func classifySource(cfg *config.Config, retriever *knowledge.Retriever, c realCase) sourceVerdict {
	out := sourceVerdict{CaseID: c.CaseID, Category: c.Category, Query: c.Query}

	hits := retriever.Retrieve(c.Query, "").HitItems
	var b strings.Builder
	shown := 0
	for _, h := range hits {
		if shown >= threeWayTopChunks {
			break
		}
		fmt.Fprintf(&b, "[%d] %s\n\n", shown+1, renderChunk(h.Chunk))
		shown++
	}
	evidence := strings.TrimSpace(b.String())
	if evidence == "" {
		evidence = "（检索没有返回任何资料）"
	}

	prompt := fmt.Sprintf(`你在给一个算力（GPU）平台的客服 AI 分类一个真实用户问题：这个问题的答案应该主要来自哪一种来源。判来源，不判"证据够不够"。

%s

三选一：
- tool：答案是用户自己账号/某台实例的实时数据，或平台特定的操作/配置，且上面工具清单里有能给出它的工具（答案随账号/实例/时间变化，从文档答会过时或太泛）。
- corpus：这是文档/知识类问题——平台产品规则、功能范围、收费规则、操作方法，或通用 GPU/AI/训练/推理/Linux 技术知识。不依赖这个用户的实时数据，能写进文档。
- say_no：平台既没有公开文档也没有对应工具——平台根本不提供的服务、超出算力平台范围、或只能转人工。这是"我没有关于X的资料"该出现的地方。

判定要点：
- 同一问题若"有实例时该调工具、泛问时是通用做法"，按答案的权威来源判：问具体某台实例的状态/访问/价格/监控 → tool；问"一般怎么用X/X 是什么/X 怎么配" → corpus。
- 只有当上面清单里确实有对应工具时才判 tool，并写出工具名；说不出具体工具就不是 tool。
- corpus 与 say_no 的区别：这是不是一个平台该有文档、或属于通用技术知识的问题。检索资料弱/空不能单独把一个真文档问题判成 say_no；反过来，就算检索捞到点沾边的，只要问题本身平台不提供该服务，也判 say_no。

用户问题：%s

检索到的资料（用于判断 corpus 是否已覆盖，不代表答案在这里）：
%s

只输出 JSON，不要任何其他文字：
{"bucket":"tool|corpus|say_no","tool":"若 bucket=tool 则填工具名，否则空","corpus_covers":true/false,"reason":"一句话，不超过40字"}`,
		toolSurfaceCatalog, c.Query, evidence)

	raw, err := askJSON(cfg, prompt)
	if err != nil {
		out.Bucket = bucketUndet
		out.Note = "classify failed: " + err.Error()
		return out
	}
	var parsed struct {
		Bucket       string `json:"bucket"`
		Tool         string `json:"tool"`
		CorpusCovers bool   `json:"corpus_covers"`
		Reason       string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		out.Bucket = bucketUndet
		out.Note = "unparseable verdict: " + err.Error()
		return out
	}
	switch strings.TrimSpace(strings.ToLower(parsed.Bucket)) {
	case bucketTool:
		out.Bucket = bucketTool
	case bucketCorp:
		out.Bucket = bucketCorp
	case bucketSayNo, "sayno", "say-no", "no":
		out.Bucket = bucketSayNo
	default:
		out.Bucket = bucketUndet
		out.Note = "unknown bucket: " + parsed.Bucket
		return out
	}
	out.Tool = strings.TrimSpace(parsed.Tool)
	out.CorpusCovers = parsed.CorpusCovers
	out.Reason = strings.TrimSpace(parsed.Reason)
	return out
}

// majorityBucket takes the bucket the rounds agreed on most, and carries the
// detail of one winning run plus the split so a 3-2 never reads like a 5-0.
func majorityBucket(votes []sourceVerdict) sourceVerdict {
	if len(votes) == 0 {
		return sourceVerdict{Bucket: bucketUndet, Note: "no votes"}
	}
	counts := map[string]int{}
	for _, v := range votes {
		counts[v.Bucket]++
	}
	best, bestN := votes[0].Bucket, 0
	for _, v := range votes {
		if counts[v.Bucket] > bestN {
			best, bestN = v.Bucket, counts[v.Bucket]
		}
	}
	buckets := make([]string, 0, len(counts))
	for k := range counts {
		buckets = append(buckets, k)
	}
	sort.Strings(buckets)
	parts := make([]string, 0, len(buckets))
	for _, k := range buckets {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	for _, v := range votes {
		if v.Bucket == best {
			v.VoteSplit = strings.Join(parts, ",")
			return v
		}
	}
	return votes[0]
}

func reportSourceBuckets(t *testing.T, results []sourceVerdict) {
	t.Helper()
	counts := map[string]int{}
	contested := 0
	toolNames := map[string]int{}
	for _, r := range results {
		counts[r.Bucket]++
		if r.VoteSplit != fmt.Sprintf("%s=%d", r.Bucket, threeWayRounds) {
			contested++
		}
		if r.Bucket == bucketTool && r.Tool != "" {
			toolNames[r.Tool]++
		}
	}
	n := len(results)
	t.Logf("== 50 条三路来源分类（每条 %d 次多数表决）==", threeWayRounds)
	t.Logf("  (%d/%d 条投票不一致，下面分子含这些摇摆条目)", contested, n)
	for _, b := range []string{bucketTool, bucketCorp, bucketSayNo, bucketUndet} {
		t.Logf("  %-13s : %2d/%d (%.0f%%)", b, counts[b], n, 100*float64(counts[b])/float64(n))
	}
	if len(toolNames) > 0 {
		named := make([]string, 0, len(toolNames))
		for k := range toolNames {
			named = append(named, k)
		}
		sort.Slice(named, func(i, j int) bool { return toolNames[named[i]] > toolNames[named[j]] })
		t.Logf("  tool 命中的工具分布:")
		for _, k := range named {
			t.Logf("    %-32s %d", k, toolNames[k])
		}
	}
	// The say_no rate is the ceiling on the "no public doc" exit's value; the tool
	// rate is the ceiling on the agent-layer routing lever the user prioritized.
	t.Logf("  ⇒ tool=%d 是 agent 层路由杠杆上界；say_no=%d 是「我没有资料」出口上界；corpus=%d 才是真 RAG 面。",
		counts[bucketTool], counts[bucketSayNo], counts[bucketCorp])
}

func writeSourceDetail(t *testing.T, results []sourceVerdict) {
	t.Helper()
	out := os.Getenv("COMPSHARE_PROBE_OUT")
	if out == "" {
		t.Logf("COMPSHARE_PROBE_OUT unset; per-case detail not written")
		return
	}
	sorted := append([]sourceVerdict{}, results...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Bucket != sorted[j].Bucket {
			return sorted[i].Bucket < sorted[j].Bucket
		}
		return sorted[i].CaseID < sorted[j].CaseID
	})
	blob, err := json.MarshalIndent(sorted, "", "  ")
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	if err := os.WriteFile(filepath.Clean(out), blob, 0o600); err != nil {
		t.Fatalf("write detail: %v", err)
	}
	t.Logf("per-case detail -> %s", out)
}
