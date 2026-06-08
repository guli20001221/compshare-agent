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
		// inference_serving / gpu_troubleshooting (external corpus, RAG Phase 1).
		// These labels live in deploy/kb/external_w0.jsonl, not stage2b_w0.jsonl;
		// the +2 boost fires once that corpus is merged into the live index. The
		// external sets are checked AFTER every platform set, so a platform
		// message keeps its mapping.
		{"vllm 怎么启动推理服务", "inference_serving"},
		{"sglang 部署", "inference_serving"},
		{"ollama 怎么用", "inference_serving"},
		{"out of memory 报错", "gpu_troubleshooting"},
		{"显存不足怎么解决", "gpu_troubleshooting"},
		// Deliberate ordering: a message with "cuda" still maps to the platform
		// driver_cuda group (checked first) even when paired with an OOM phrase.
		{"cuda out of memory", "driver_cuda"},
		// ComfyUI (external corpus, RAG Phase 5). "comfyui" is deliberately NOT a
		// keyword, so most serving/setup/connectivity queries fall through to ""
		// (no +2 boost — retrieval relies on the distinctive "comfyui" token via
		// BM25 + qwen3). Exceptions, both faithfully reflected in the golden:
		//   - the OOM query reaches gpu_troubleshooting via "爆显存" (and only because
		//     it carries no "cuda"/"驱动" token, which would shadow it to driver_cuda);
		//   - the model-directory query says "模型", which is a modelverse keyword
		//     (checked before inference_serving), so it infers "modelverse". On the
		//     external-only retrieval eval that boost is a no-op (no external chunk
		//     has product_area=modelverse); the merged-index mis-boost toward
		//     platform modelverse chunks is covered by CLI smoke, not this unit.
		// These pin the golden areas in scripts/rag_ext/build_external_golden.py.
		{"我在实例里把 comfyui 跑起来了,想让别的电脑也能打开它的页面", ""},
		{"comfyui 出图的时候老是爆显存,有什么办法能降一点", "gpu_troubleshooting"},
		{"下载的大模型放进去了,comfyui 里却选不到,该放哪个文件夹", "modelverse"},
		{"comfyui 想用一个第三方的节点,怎么把它装上", ""},
		{"在新开的实例上从头装 comfyui 该怎么弄", ""},
		{"comfyui 启动了日志也没报错,可浏览器就是打不开", ""},
		{"不想每次手点界面,能不能用脚本自动让 comfyui 跑一个工作流", ""},
		// linux_ops (external corpus, Linux-ops + env-management vertical). Checked
		// LAST (after every platform set + the other external sets), so these only
		// fire for messages no platform keyword claimed.
		{"想让训练在 tmux 里后台一直跑", "linux_ops"},
		{"conda 怎么新建一个虚拟环境", "linux_ops"},
		{"pip 安装太慢,怎么换国内源", "linux_ops"},
		{"磁盘满了怎么清理出空间", "linux_ops"},
		{"scp 怎么把本地文件传到实例上", "linux_ops"},
		{"怎么用 top 命令看哪个进程占内存", "linux_ops"},
		// Deliberate overlap: any SSH-免密 phrasing carries "ssh"/"登录", which the
		// login group (checked first) claims. The SSH-key external chunk therefore
		// gets no +2 affinity boost; the CLI smoke verifies it still retrieves on the
		// merged index. The bare "免密" keyword only catches phrasings without ssh/登录.
		{"ssh 免密登录怎么配置", "login"},
		// pytorch_basics (external corpus, PyTorch/CUDA-basics vertical).
		{"pytorch 单机多卡训练 ddp 怎么写", "pytorch_basics"},
		{"dataloader 的 num_workers 设多少合适", "pytorch_basics"},
		{"想用混合精度加速训练", "pytorch_basics"},
		{"怎么在实例上装 gpu 版的 pytorch", "pytorch_basics"},
		{"conda 里怎么装 pytorch", "pytorch_basics"}, // pytorch_basics checked before linux_ops
		// Precedence: a PyTorch OOM message still maps to gpu_troubleshooting
		// (checked before pytorch_basics) — troubleshooting wins over usage-basics.
		{"pytorch 训练爆显存了怎么办", "gpu_troubleshooting"},
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
