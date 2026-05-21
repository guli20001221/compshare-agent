package engine

import "testing"

// TestInferKnowledgeProductArea_LabelsMatchCorpus pins each keyword group
// to a product_area string that exists in deploy/kb/stage2b_w0.jsonl, so the
// +2 BM25 boost in Retriever.scoreChunk actually fires. Update both the
// keyword set and the corpus label together — never drift one without the
// other.
func TestInferKnowledgeProductArea_LabelsMatchCorpus(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		// billing_rule
		{"包月规则是怎样的", "billing_rule"},
		{"退款流程", "billing_rule"},
		{"账单怎么看", "billing_rule"},
		// modelverse
		{"Dify 怎么接入 ModelVerse", "modelverse"},
		{"claude 怎么调", "modelverse"},
		{"我的积分怎么用", "modelverse"},
		// image
		{"自定义镜像怎么导出", "image"},
		// login
		{"jupyter 打不开怎么办", "login"},
		{"SSH 连不上", "login"},
		// resource_purchase (new group — was 28 chunks unboosted)
		{"抢占式实例怎么购买", "resource_purchase"},
		{"独占式和共享有什么区别", "resource_purchase"},
		{"规格怎么选", "resource_purchase"},
		// driver_cuda (new group)
		{"nvidia-smi 报错", "driver_cuda"},
		{"驱动版本是什么", "driver_cuda"},
		{"cuda 不能用", "driver_cuda"},
		// init_failure (new group)
		{"实例一直 Initializing 卡住", "init_failure"},
		{"启动失败怎么办", "init_failure"},
		// windows (new group)
		{"Windows 远程桌面连不上", "windows"},
		{"RDP 配置", "windows"},
		// monitor (new group). Both spaced and no-space forms must hit —
		// textutil.Normalize collapses whitespace but does NOT insert a space
		// between adjacent CJK and ASCII, so "CPU占用率" stays joined.
		{"显存占用怎么查", "monitor"},
		{"CPU占用率怎么看", "monitor"},
		{"GPU 占用率高", "monitor"},
		// out-of-scope
		{"今天天气怎么样", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := inferKnowledgeProductArea(c.text)
		if got != c.want {
			t.Errorf("inferKnowledgeProductArea(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}
