package knowledge

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBM25SensitivityToCosmeticQueryRewrites measures how much of the observed
// retrieval instability is ours rather than the model's.
//
// The A/A probe found the planner emitting different query TEXT on 80% of real
// questions run twice. Inspecting those differences, they are almost entirely
// cosmetic: casing ("ollama" vs "Ollama"), spacing ("监听 端口" vs "监听端口"),
// and a trailing intent phrase. The semantic target never moved.
//
// A lexical matcher does not care that a difference is cosmetic — different
// tokens are a different query. So the question is how far a purely cosmetic
// rewrite moves the retrieved set. Whatever it moves is a brittleness we own and
// can normalise away; it also means two users asking the same thing with
// different spacing get different evidence, which is a production concern and
// not only an eval one.
//
// This test reports rather than asserts a threshold: the number is the point,
// and picking a pass/fail line before knowing it would be inventing a contract.
func TestBM25SensitivityToCosmeticQueryRewrites(t *testing.T) {
	corpus, err := LoadPinnedCorpus(filepath.Join("..", "..", "deploy", "kb", "stage2b_w0.jsonl"))
	require.NoError(t, err)
	retriever := NewRetriever(corpus, RetrieverOptions{
		TopK: 10,
		Mode: RetrievalModeBM25Only,
		Now:  determinismProbeNow,
	})

	// Each group is one meaning. Variants are the exact shapes the live planner
	// produced for the same input on two consecutive runs.
	groups := [][]string{
		{"ollama 远程访问 监听 端口 外部连接", "Ollama 远程访问 监听端口 外部连接"},
		{"comfyui 导入工作流 workflow json 方法", "ComfyUI 导入工作流 加载 workflow json 方法"},
		{"优云智算 API base URL HTTPS 地址", "优云智算 API base URL https地址"},
		{"预付费包月退款退订退费政策", "预付费包月产品的退款退订退费政策"},
		{"226604 资源不足 创建实例报错", "错误码226604 资源不足 创建实例报错 原因及解决方案"},
	}

	var moved, top3Moved int
	for _, variants := range groups {
		base := chunkIDsOf(retriever.Retrieve(variants[0], ""))
		require.NotEmpty(t, base, "variant %q retrieved nothing", variants[0])
		for _, variant := range variants[1:] {
			got := chunkIDsOf(retriever.Retrieve(variant, ""))
			if strings.Join(base, ",") != strings.Join(got, ",") {
				moved++
			}
			if !sameTopN(base, got, 3) {
				top3Moved++
			}
			t.Logf("meaning=%q\n  variant : %q\n  top10 同? %v   top3 同? %v",
				variants[0], variant,
				strings.Join(base, ",") == strings.Join(got, ","),
				sameTopN(base, got, 3))
		}
	}
	t.Logf("== 纯形式改写对 BM25 的影响 (n=%d 组) ==", len(groups))
	t.Logf("  top10 结果改变 : %d/%d", moved, len(groups))
	t.Logf("  top3  结果改变 : %d/%d", top3Moved, len(groups))
}

func sameTopN(a, b []string, n int) bool {
	if len(a) > n {
		a = a[:n]
	}
	if len(b) > n {
		b = b[:n]
	}
	return strings.Join(a, ",") == strings.Join(b, ",")
}
