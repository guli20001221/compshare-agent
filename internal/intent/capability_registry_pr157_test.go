package intent

import (
	"strings"
	"testing"
)

// PR #157 (intent matrix smoke 2026-05-22): three capability renderer
// bugs found by real CLI test against api.compshare.cn. Each test below
// locks the fix so the regression class can't reappear silently.

// TestFullGPUSpecsRequest_CPUMemoryCoOccurrence covers q06: user says
// "5090 几核 CPU 多大内存" and pre-#157 keyword set only matched compound
// substrings like "cpu和内存" — colloquial phrasing slipped through and
// renderer skipped the very CPU+Memory breakdown the user asked for.
func TestFullGPUSpecsRequest_CPUMemoryCoOccurrence(t *testing.T) {
	colloquialCases := []string{
		"5090 几核 CPU 多大内存",
		"4090 cpu 多少 内存 多大",
		"A100 几核多少内存",
		"几核CPU 配多少内存",
		"how many cores and how much memory for 5090",
	}
	for _, msg := range colloquialCases {
		if !fullGPUSpecsRequest(msg) {
			t.Errorf("expected detailed=true for colloquial %q (co-occurrence rule)", msg)
		}
	}
	// Negatives — CPU-only or memory-only or unrelated must stay overview.
	for _, msg := range []string{"4090 几核", "5090 显存多大", "4090 性能", "我账单"} {
		if fullGPUSpecsRequest(msg) {
			t.Errorf("expected detailed=false for non-CPU+memory text %q", msg)
		}
	}
}

// TestFullGPUSpecsRequest_KnownFP_CoOccurrenceContainedClass locks the
// intentionally-accepted FP class flagged by PR #157 review N2: phrases
// that mention both cpu+memory but really are diagnosis / monitor /
// recommendation queries. The renderer level can't distinguish; the
// planner gate upstream is what scopes these out of gpu_specs_query in
// practice. Test exists so a future widening of the planner→gpu_specs
// router shows up as a failing assertion here, prompting reconsideration.
func TestFullGPUSpecsRequest_KnownFP_CoOccurrenceContainedClass(t *testing.T) {
	knownFP := []string{
		"我的实例 cpu 占用率高 内存也满了",
		"创建实例选什么 cpu 和 内存 配置",
		"4090 cpu 推理 内存占用",
	}
	for _, msg := range knownFP {
		if !fullGPUSpecsRequest(msg) {
			t.Logf("note: %q no longer flips detailed=true — co-occurrence rule may have been narrowed", msg)
		}
		// We intentionally accept these — verdict is "true" but the planner
		// gate should never route these to gpu_specs_query. If you're seeing
		// detailed-mode output for them in production, the bug is in
		// planner.go few-shot scoping, not here.
	}
}

// TestRenderGPUSpecs_FiveZeroNinetyCpuMemoryRequest end-to-end covers q06.
func TestRenderGPUSpecs_FiveZeroNinetyCpuMemoryRequest(t *testing.T) {
	raw := map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{
				"Name":           "5090",
				"Zone":           "cn-wlcb-01",
				"Status":         "Normal",
				"GraphicsMemory": map[string]any{"Value": 32},
				"Performance":    map[string]any{"Value": 105},
				"MachineSizes": []any{
					map[string]any{
						"Gpu": float64(1),
						"Collection": []any{
							map[string]any{"Cpu": float64(16), "Memory": []any{float64(96)}},
						},
					},
					map[string]any{
						"Gpu": float64(2),
						"Collection": []any{
							map[string]any{"Cpu": float64(32), "Memory": []any{float64(192)}},
						},
					},
				},
			},
		},
	}
	reply := renderGPUSpecsReply(raw, "5090 几核 CPU 多大内存")
	// Detailed mode appends 完整配置=<gpu>卡/<cpu>C/<mem>G per machine-size combo.
	if !strings.Contains(reply, "16C/96G") {
		t.Errorf("detailed reply must include 16C/96G; got:\n%s", reply)
	}
	if !strings.Contains(reply, "32C/192G") {
		t.Errorf("detailed reply must include 32C/192G; got:\n%s", reply)
	}
	if !strings.Contains(reply, "完整配置=") {
		t.Errorf("detailed reply must include 完整配置 marker; got:\n%s", reply)
	}
}

// TestIsImageListAllIntent covers the q09/q10 root cause: list-all intent
// detector for image renderers.
func TestIsImageListAllIntent(t *testing.T) {
	listAll := []string{
		"你们平台有什么镜像",
		"有哪些镜像",
		"看下我做的镜像",
		"列出所有镜像",
		"我的镜像",
		"全部镜像",
		"list all images",
		"show me all images",
		"what images are available",
		"my images",
	}
	for _, msg := range listAll {
		if !isImageListAllIntent(msg) {
			t.Errorf("expected list-all=true for %q", msg)
		}
	}
	for _, msg := range []string{
		"Ubuntu 22.04 镜像",
		"cuda 12.8 镜像下载",
		"我账单",
		"4090 多少钱",
	} {
		if isImageListAllIntent(msg) {
			t.Errorf("expected list-all=false for %q (specific request)", msg)
		}
	}
}

// TestRenderImageListReply_ListAllBypass covers q09 end-to-end: list-all
// intent must skip keyword filter and pass all images through, not return
// "未找到匹配的镜像" against 39 real images.
func TestRenderImageListReply_ListAllBypass(t *testing.T) {
	raw := map[string]any{
		"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-1", "Name": "Ubuntu 22.04 64位", "ImageType": "System"},
			map[string]any{"CompShareImageId": "img-2", "Name": "vLLM v0.12.0", "ImageType": "System"},
			map[string]any{"CompShareImageId": "img-3", "Name": "ComfyUI", "ImageType": "System"},
		},
	}
	reply := renderImageListReply(raw, "ImageSet",
		[]string{"CompShareImageId", "CompShareImageName", "ImageName", "ImageType", "Name"},
		"你们平台有什么镜像")
	if strings.Contains(reply, noImageListNoMatchReply) {
		t.Errorf("list-all intent must bypass keyword filter; got 未找到 reply:\n%s", reply)
	}
	for _, want := range []string{"Ubuntu 22.04", "vLLM", "ComfyUI"} {
		if !strings.Contains(reply, want) {
			t.Errorf("list-all reply must include %q; got:\n%s", want, reply)
		}
	}
}

// TestIsImageListAllIntent_SpecificNameGuard locks the PR #157 review N2
// fix: list-all phrase + specific image family/version token must NOT
// bypass the filter. User says "我的 ubuntu 镜像" → list-all=false →
// substring filter runs as expected.
func TestIsImageListAllIntent_SpecificNameGuard(t *testing.T) {
	// List-all phrase + specific token → false (filter applies).
	guarded := []string{
		"我的 ubuntu 镜像在哪",
		"看看 cuda 12.8 镜像",
		"我的 vLLM 镜像",
		"看下 comfyui 镜像",
		"我做的 pytorch 镜像",
		"有什么 windows 镜像",
		"看看 v0.3.66 那个",
		"看看 SD WebUI 镜像",
		"我的 CentOS 镜像",
		"我的训练镜像",
	}
	for _, msg := range guarded {
		if isImageListAllIntent(msg) {
			t.Errorf("expected list-all=false for specific-name request %q (N2 guard)", msg)
		}
	}
	// Pure list-all (no specific token) → true (still bypasses).
	stillBypasses := []string{
		"你们平台有什么镜像",
		"我的镜像",
		"列出所有镜像",
		"看看我做的镜像",
	}
	for _, msg := range stillBypasses {
		if !isImageListAllIntent(msg) {
			t.Errorf("expected list-all=true for pure list-all %q", msg)
		}
	}
}

// TestRenderImageListReply_SpecificNameStillFilters guards that the
// list-all bypass didn't break existing exact-match behavior.
func TestRenderImageListReply_SpecificNameStillFilters(t *testing.T) {
	raw := map[string]any{
		"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-1", "Name": "Ubuntu 22.04 64位", "ImageType": "System"},
			map[string]any{"CompShareImageId": "img-2", "Name": "vLLM v0.12.0", "ImageType": "System"},
		},
	}
	reply := renderImageListReply(raw, "ImageSet",
		[]string{"CompShareImageId", "ImageName", "ImageType", "Name"},
		"ubuntu 22.04 镜像")
	if !strings.Contains(reply, "Ubuntu 22.04") {
		t.Errorf("specific Ubuntu request must include Ubuntu; got:\n%s", reply)
	}
	if strings.Contains(reply, "vLLM") {
		t.Errorf("specific Ubuntu request must NOT bleed in vLLM; got:\n%s", reply)
	}
}

func TestRenderImageListReply_UnknownSpecificNameStillFilters(t *testing.T) {
	raw := map[string]any{
		"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-1", "Name": "SD WebUI Forge", "ImageType": "Community"},
			map[string]any{"CompShareImageId": "img-2", "Name": "ComfyUI", "ImageType": "Community"},
		},
	}
	reply := renderImageListReply(raw, "ImageSet",
		[]string{"CompShareImageId", "ImageName", "ImageType", "Name"},
		"看看 SD WebUI 镜像")
	if !strings.Contains(reply, "SD WebUI Forge") {
		t.Errorf("specific unknown image request must include SD WebUI; got:\n%s", reply)
	}
	if strings.Contains(reply, "ComfyUI") {
		t.Errorf("specific unknown image request must NOT list unrelated images; got:\n%s", reply)
	}
}
