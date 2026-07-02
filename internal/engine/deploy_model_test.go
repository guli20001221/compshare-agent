package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
	"github.com/compshare-agent/internal/zones"
)

// deployDispatch builds the minimal routerDispatchResult the deploy handler needs:
// an IntentDeployModel plan. The handler reads only the intent + (for the trace) the
// plan; everything else it derives from the user message + live queries.
func deployDispatch() routerDispatchResult {
	return routerDispatchResult{
		result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentDeployModel}},
	}
}

func TestDeployStopReplyClassifiesAdaptiveImageFailure(t *testing.T) {
	reply := deployStopReply(&workflow.Result{
		Message: "步骤「创建实例」执行失败: ActionError: adaptive uhost image id is empty",
	})

	assert.Contains(t, reply, "当前可用区")
	assert.Contains(t, reply, "镜像")
	assert.NotContains(t, reply, "adaptive uhost image id is empty")
}

// deployMockConfig parameterizes the fake upstream for a deploy run.
type deployMockConfig struct {
	capacityEnough        bool     // CheckCompShareResourceCapacity ResourceEnough
	instanceStates        []string // DescribeCompShareInstance State sequence (last repeats)
	createID              string   // UHostIds[0] returned by CreateCompShareInstance
	communityImageID      string   // when set, the community group carries Data[] with this id; "" = group without Data[] (halt case)
	platformSupportedGPUs []string // when set, the platform image declares these SupportedGpuTypes (M2 intersection)
	platformImages        []any    // when set, overrides the platform ImageSet wholesale (e.g. an arch variant + its base)
}

// newDeployMock returns a function-based executor covering every action the
// matcher + CreateInstanceDef saga + poll loop invoke. DescribeAvailableCompShareInstanceTypes
// echoes the requested GpuType so the test is decoupled from RecommendGPUType's
// exact pick. DescribeCompShareInstance walks instanceStates by call index.
func newDeployMock(cfg deployMockConfig) *mockExecutorFn {
	if cfg.createID == "" {
		cfg.createID = "uhost-deploy-1"
	}
	if len(cfg.instanceStates) == 0 {
		cfg.instanceStates = []string{"Running"}
	}
	describeIdx := 0
	return &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareImages":
			if len(cfg.platformImages) > 0 {
				return map[string]any{"ImageSet": cfg.platformImages}, nil
			}
			img := map[string]any{
				"CompShareImageId": "img-pt",
				"Name":             "PyTorch 2.9.1 cuda128",
				"ImageType":        "App",
				"Softwares":        map[string]any{"Framework": "PyTorch"},
				"Description":      "PyTorch 基础镜像",
				"SoftwarePorts":    []any{map[string]any{"Software": "JupyterLab", "Port": float64(8888)}},
			}
			if len(cfg.platformSupportedGPUs) > 0 {
				arr := make([]any, len(cfg.platformSupportedGPUs))
				for i, g := range cfg.platformSupportedGPUs {
					arr[i] = g
				}
				img["SupportedGpuTypes"] = arr
			}
			return map[string]any{"ImageSet": []any{img}}, nil
		case "DescribeCommunityImages":
			group := map[string]any{"ImageName": "LiveTalking 数字人", "ImageDesc": "开箱即用数字人"}
			if cfg.communityImageID != "" {
				group["Data"] = []any{map[string]any{"CompShareImageId": cfg.communityImageID, "Name": "LiveTalking v1"}}
			}
			return map[string]any{"CompshareImageGroup": []any{group}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			// The create workflow no longer filters by MachineTypes (it queries the
			// full catalog and selects the type/zone in-code), so the mock mirrors
			// that: every GPU type the handler might pick, each at 1×/16C/64GB so the
			// capacity Specs below match the resolved spec regardless of choice.
			var types []any
			for _, name := range []string{"4090", "4090_48G", "5090", "A100", "A800", "V100S", "H20", "3090", "3080Ti", "P40", "2080Ti", "2080"} {
				types = append(types, map[string]any{"Name": name, "MachineSizes": []any{
					map[string]any{"Gpu": float64(1), "Collection": []any{
						map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
					}},
				}})
			}
			return map[string]any{"AvailableInstanceTypes": types}, nil
		case "CheckCompShareResourceCapacity":
			return map[string]any{"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": cfg.capacityEnough},
			}}, nil
		case "GetCompShareInstanceUserPrice":
			return map[string]any{"PriceDetails": []any{
				map[string]any{"ChargeType": "Postpay", "Price": 1.58},
			}}, nil
		case "CreateCompShareInstance":
			return map[string]any{"UHostIds": []any{cfg.createID}}, nil
		case "DescribeCompShareInstance":
			state := cfg.instanceStates[len(cfg.instanceStates)-1]
			if describeIdx < len(cfg.instanceStates) {
				state = cfg.instanceStates[describeIdx]
			}
			describeIdx++
			return map[string]any{"UHostSet": []any{
				map[string]any{
					"UHostId":         cfg.createID,
					"Name":            "deploy-test",
					"State":           state,
					"GpuType":         "A100",
					"SshLoginCommand": "ssh root@1.2.3.4 -p 22",
					"Password":        "FAKE-PW-DO-NOT-LEAK", // stands in for the (base64) instance password — must NOT leak into reply
					"IPSet":           []any{map[string]any{"IP": "1.2.3.4", "Type": "Bgp", "Weight": float64(10)}},
				},
			}}, nil
		default:
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
}

const deployMatchJSON = `{"image_source":"platform","image_name":"PyTorch","model_name":"Qwen2.5-7B","quantization":""}`

// deploySearchJSON is the extractDeploySearch (call 1) response the matcher makes
// BEFORE the image pick (call 2). Tests seed it as the first mock LLM response.
const deploySearchJSON = `{"search":"Qwen"}`

func newDeployEngine(matchJSON string, exec *mockExecutorFn, confirm func(string, map[string]any) bool) *Engine {
	// matchDeployImage makes TWO TierAgent calls: (1) keyword extraction for the
	// community FuzzySearch, then (2) the image pick. Seed both in order so the
	// single-arg helper still drives the whole handler.
	client := &mockLLM{responses: []llm.ChatResponse{{Content: deploySearchJSON}, {Content: matchJSON}}}
	eng := NewWithDeps(client, exec, confirm) // mutatingToolsEnabled=true; agentLLMClient=nil → falls back to client
	eng.zoneCatalog = zones.NewCatalog(0)
	return eng
}

type fakeCreatePreferenceExtractor struct {
	result *CreatePreferenceExtractionResult
	err    error
	calls  []CreatePreferenceExtractionInput
}

func (f *fakeCreatePreferenceExtractor) ExtractCreatePreferences(_ context.Context, in CreatePreferenceExtractionInput) (*CreatePreferenceExtractionResult, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

// TestTryDeployModel_HappyPath_AlreadyRunning proves the end-to-end handler: TierAgent
// match → CreateInstanceDef saga (incl. the L1 create) → poll sees Running →
// deterministic reply with the new instance id + access info. WHY it matters:
// this is B8's first agent-tier skill exercising the orchestrator on a real
// mutating create; the reply must carry the created resource so the user owns it.
func TestTryDeployModel_HappyPath_AlreadyRunning(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	confirmCalls := 0
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { confirmCalls++; return true })
	onStep, events := collectSteps()

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "帮我部署 Qwen2.5-7B", onStep)

	require.True(t, handled)
	assert.Contains(t, reply, "uhost-deploy-1", "reply must surface the created instance id")
	assert.Contains(t, reply, "运行状态", "Running state should be reported")
	assert.Contains(t, reply, "ssh root@1.2.3.4", "SSH access info should be surfaced")
	assert.NotContains(t, reply, "FAKE-PW-DO-NOT-LEAK", "the instance password must NEVER appear in the reply")

	// The saga actually ran the create (not a bypass), and exactly once.
	assert.Equal(t, 1, countCalls(exec.calls, "CreateCompShareInstance"), "create must run through the saga exactly once")
	// No double-confirm: the saga's StepConfirm is the sole HITL gate; the L1
	// create step (OriginWorkflowInternal) must NOT re-prompt.
	assert.Equal(t, 1, confirmCalls, "exactly one confirm (the StepConfirm)")
	assert.NotEmpty(t, *events, "user-facing progress steps should be emitted")
}

func TestTryDeployModel_ConfirmCancelKeepsCreateFamilyFrameForFollowup(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return false })
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindDeploy,
			Status:         ContextFrameStatusFailedRecoverable,
			GPU:            "4090",
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
		LastDeployWorkload: "Qwen2.5-7B",
		LastDeployZone:     "cn-bj2-03",
		PendingDeployModel: "DeepSeek R1",
	}, 1)

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "帮我部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "本次创建未执行")
	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, ContextFrameKindDeploy, state.ContextFrame.Kind)
	assert.Equal(t, ContextFrameStatusFailedRecoverable, state.ContextFrame.Status)
	assert.Equal(t, "4090", state.ContextFrame.GPU)
	assert.Equal(t, "Qwen2.5-7B", state.LastDeployWorkload)
	assert.NotEmpty(t, state.LastDeployZone)
	assert.Empty(t, state.PendingDeployModel)
}

func TestRecordDeployContextFrame_FlagOffDoesNotPersistFrame(t *testing.T) {
	SetContextContinuationEnabled(false)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

	eng.recordDeployContextFrameFromError("部署 Qwen", "部署 Qwen", "cn-wlcb-01", "暂无容量")
	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.ContextFrame.Kind)

	eng.recordDeployContextFrameFromPlan("部署 Qwen", deployPlan{
		GpuType:    "4090",
		ModelName:  "Qwen",
		ChosenZone: "cn-wlcb-01",
	}, "暂无容量")
	state, _, _ = eng.SessionStateSnapshot()
	assert.Empty(t, state.ContextFrame.Kind)
}

// TestTryDeployModel_SurfacesUsageGuidance proves the handler fetches the deployed
// image's usage detail post-create and renders an access endpoint, so the user
// learns HOW to use the instance (here: JupyterLab on :8888 built from the image
// SoftwarePorts + the instance public IP) — closing the "deployed but no guidance"
// gap. The DescribeCompShareImages re-read (by id) must actually happen.
func TestTryDeployModel_SurfacesUsageGuidance(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "帮我部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "访问地址", "usage block surfaced")
	assert.Contains(t, reply, "http://1.2.3.4:8888", "endpoint built from image SoftwarePorts + instance public IP")
	// fetchImageUsage re-reads the image by id AFTER the matcher's catalog read:
	// matcher (1 list) + post-create detail (1 by-id) ≥ 2.
	assert.GreaterOrEqual(t, countCalls(exec.calls, "DescribeCompShareImages"), 2,
		"image is re-read by id post-create for usage guidance")
}

// TestTryDeployModel_StillInitializing proves the handler replies immediately on the
// saga's own post-create describe instead of blocking until Running: a freshly
// created instance is still initializing, and the reply frames it as such (not an
// error) while still returning the id. WHY: a GPU instance can take minutes to
// reach Running; holding the turn open that long stalls the SSE stream and the
// frontend's post-create jump — login/status is surfaced on the console page.
func TestTryDeployModel_StillInitializing(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Starting"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "uhost-deploy-1")
	assert.Contains(t, reply, "初始化", "a still-initializing instance frames as initializing, not failed")
	assert.NotContains(t, reply, "运行状态")
	// Exactly ONE describe — the saga's own post-create "查看状态" step. The handler
	// no longer polls, so the turn returns without waiting for Running.
	assert.Equal(t, 1, countCalls(exec.calls, "DescribeCompShareInstance"),
		"only the saga's post-create describe runs; no handler-side poll")
}

// TestTryDeployModel_CommunityHappyPath proves the community image path end-to-end:
// the matcher picks a community app, grounding matches it against the live catalog,
// and the saga resolves its CompShareImageId from Data[] and creates.
func TestTryDeployModel_CommunityHappyPath(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, communityImageID: "comm-img-9", instanceStates: []string{"Running"}})
	matchJSON := `{"image_source":"community","image_name":"LiveTalking","model_name":"","quantization":""}`
	eng := newDeployEngine(matchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "帮我跑一个数字人", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "运行状态")
	assert.Contains(t, reply, "社区镜像", "community source should be reflected in the reply")
	assert.Equal(t, 1, countCalls(exec.calls, "CreateCompShareInstance"))
}

// TestTryDeployModel_CommunityEmptyDataHalts proves the create guard: when the
// community group has no Data[] (no resolvable CompShareImageId), the saga halts
// at the create step rather than POSTing an empty image id.
func TestTryDeployModel_CommunityEmptyDataHalts(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, communityImageID: ""}) // group without Data[]
	matchJSON := `{"image_source":"community","image_name":"LiveTalking","model_name":"","quantization":""}`
	eng := newDeployEngine(matchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "帮我跑一个数字人", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "创建未完成")
	assert.Equal(t, 0, countCalls(exec.calls, "CreateCompShareInstance"), "no create when image id cannot be resolved")
}

// TestTryDeployModel_CommunityGroundingFallback proves a hallucinated community
// image name (absent from the live catalog) falls back to a platform base rather
// than reaching the saga with an unresolvable name.
func TestTryDeployModel_CommunityGroundingFallback(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	matchJSON := `{"image_source":"community","image_name":"TotallyMadeUpApp","model_name":"","quantization":""}`
	eng := newDeployEngine(matchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "随便跑点啥", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "回退到平台框架镜像", "fallback note should explain the platform fallback")
	assert.Equal(t, 1, countCalls(exec.calls, "CreateCompShareInstance"), "fallback still creates (on a platform base)")
}

func TestMatchDeployImage_ExactCommunityModelOverridesBaseChoice(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-ollama", "Name": "Ollama v0.13.1", "ImageType": "App"},
			}}, nil
		case "DescribeCommunityImages":
			search, _ := args["FuzzySearch"].(string)
			if strings.Contains(strings.ToLower(search), "deepseek") || strings.Contains(strings.ToLower(search), "r1") {
				return map[string]any{"CompshareImageGroup": []any{
					map[string]any{"ImageName": "DeepSeek-R1:32b", "ImageDesc": "DeepSeek R1 32B 开箱即用镜像", "Data": []any{
						map[string]any{"CompShareImageId": "comm-deepseek-32b", "Name": "DeepSeek-R1:32b", "SupportedGpuTypes": []any{"4090_48G", "4090"}},
					}},
				}}, nil
			}
			return map[string]any{"CompshareImageGroup": []any{}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{
				availCardZ("4090_48G", "cn-bj2-03", 48),
				availCardZ("4090", "cn-bj2-03", 24),
			}}, nil
		case "CheckCompShareResourceCapacity":
			return map[string]any{"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
			}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	client := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"search":"DeepSeek R1"}`},
		{Content: `{"image_source":"platform","image_name":"Ollama v0.13.1","model_name":"DeepSeek-R1-32B","match_kind":"base","size_ambiguous":false,"quantization":""}`},
	}}
	eng := NewWithDeps(client, exec, okConfirm)

	plan, err := eng.matchDeployImage(context.Background(), "为我部署 DeepSeek R1 32B", "", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "community", plan.ImageSource)
	assert.Equal(t, "DeepSeek-R1:32b", plan.ImageName)
	assert.Equal(t, "comm-deepseek-32b", plan.ImageID)
	assert.Equal(t, "exact", plan.MatchKind)
	assert.NotEqual(t, "A100", plan.GpuType, "exact community image should not default to fp16 A100 sizing")
	assert.Contains(t, plan.MatchNote, "现成 DeepSeek-R1:32b 镜像")
	assert.NotContains(t, plan.CandidateGPUs, "A100", "guided deploy card should show the feasible recommendation set, not unrelated sellable cards")
	assert.Contains(t, plan.GPUReasons[plan.GpuType], "现成 DeepSeek-R1:32b 镜像")
}

func TestExactCommunityModelImageName_DoesNotMatchSameSizeSibling(t *testing.T) {
	community := map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "QwQ-32B", "ImageDesc": "32B reasoning model"},
		map[string]any{"ImageName": "Janus-Pro-32B", "ImageDesc": "DeepSeek family visual model"},
	}}

	_, ok := exactCommunityModelImageName("DeepSeek-R1-32B", "为我部署 DeepSeek R1 32B", community)

	assert.False(t, ok, "same parameter scale or same brand is not the same model")
}

// TestTryDeployModel_MatcherJSONParseFailure proves a non-JSON matcher response
// yields a clarification reply (not a crash or a garbage create).
func TestTryDeployModel_MatcherJSONParseFailure(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true})
	eng := newDeployEngine("抱歉，我无法判断该用哪个镜像。", exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署点东西", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "告诉我你想部署的模型", "unparseable match → clarification")
	assert.Equal(t, 0, countCalls(exec.calls, "CreateCompShareInstance"))
}

// TestMatchDeployImage_UsesLiveGPUSet proves GPU sizing is API-driven: the matcher
// queries DescribeAvailableCompShareInstanceTypes and sizes against the LIVE set, so
// a card the static gpuSpecs table has never heard of ("TEST_GPU_X") is selectable. A
// 16B model (~39GB) sizes to the only fitting live card, TEST_GPU_X (48GB) — impossible
// via the static table, proving the live path (not the static fallback) was taken.
func TestMatchDeployImage_UsesLiveGPUSet(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-pt", "Name": "PyTorch 2.9.1 cuda128",
					"Softwares": map[string]any{"Framework": "PyTorch"}},
			}}, nil
		case "DescribeCommunityImages":
			return map[string]any{"CompshareImageGroup": []any{}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			gmem := func(v int) map[string]any { return map[string]any{"Value": float64(v)} }
			perf := func(v int) map[string]any { return map[string]any{"Value": float64(v)} }
			sizes := []any{map[string]any{"Gpu": float64(1)}, map[string]any{"Gpu": float64(8)}}
			z := "cn-wlcb-01" // matcher filters availability to the create-zone
			return map[string]any{"AvailableInstanceTypes": []any{
				map[string]any{"Name": "4090", "Zone": z, "Status": "Normal", "GraphicsMemory": gmem(24), "Performance": perf(83), "MachineSizes": sizes},
				map[string]any{"Name": "TEST_GPU_X", "Zone": z, "Status": "Normal", "GraphicsMemory": gmem(48), "Performance": perf(130), "MachineSizes": sizes},
				map[string]any{"Name": "A100", "Zone": z, "Status": "Normal", "GraphicsMemory": gmem(80), "Performance": perf(100), "MachineSizes": sizes},
			}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	client := &mockLLM{responses: []llm.ChatResponse{
		{Content: deploySearchJSON},
		{Content: `{"image_source":"platform","image_name":"PyTorch","model_name":"Qwen2.5-16B","quantization":"fp16"}`},
	}}
	eng := NewWithDeps(client, exec, func(string, map[string]any) bool { return true })

	plan, err := eng.matchDeployImage(context.Background(), "部署 Qwen2.5-16B", "", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "TEST_GPU_X", plan.GpuType, "16B (~39GB) must size to the live 48GB card TEST_GPU_X, which the static table does not model")
}

// TestMatchDeployImage_PrefersAgentClient proves the TierAgent routing split
// (ADR-002): when agentLLMClient is set, the matcher calls IT, not the fast
// llmClient fallback. A regression that called the wrong tier would be caught here.
func TestMatchDeployImage_PrefersAgentClient(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true})
	fast := &mockLLM{responses: []llm.ChatResponse{{Content: deployMatchJSON}}}
	// Both matcher calls (extract + pick) must go to the TierAgent client.
	agent := &mockLLM{responses: []llm.ChatResponse{{Content: deploySearchJSON}, {Content: deployMatchJSON}}}
	eng := NewWithDeps(fast, exec, func(string, map[string]any) bool { return true })
	eng.agentLLMClient = agent

	_, err := eng.matchDeployImage(context.Background(), "部署 Qwen2.5-7B", "", noopStep)

	require.NoError(t, err)
	assert.Equal(t, 2, len(agent.calls), "TierAgent client must serve both matcher calls (extract + pick)")
	assert.Equal(t, 0, len(fast.calls), "fast client must NOT be used when agentLLMClient is set")
}

// TestMatchDeployImage_GPUConstrainedByImageSupport proves M2: when the chosen
// image declares a SupportedGpuTypes that excludes the VRAM-ideal card, the GPU
// pick is constrained to a supported card that still fits. A 7B model sizes to
// 4090 (24GB) unconstrained, but an image that only supports 5090 must yield 5090.
func TestMatchDeployImage_GPUConstrainedByImageSupport(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, platformSupportedGPUs: []string{"5090"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	plan, err := eng.matchDeployImage(context.Background(), "部署 Qwen2.5-7B", "", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "5090", plan.GpuType, "GPU must be constrained to the image's SupportedGpuTypes")
	assert.Contains(t, plan.MatchNote, "5090", "the note should explain the image-supported pick")
}

func TestGpuImageCompatible(t *testing.T) {
	assert.True(t, gpuImageCompatible("4090", nil), "empty supported list = no constraint")
	assert.True(t, gpuImageCompatible("4090", []string{"5090", "4090"}))
	assert.True(t, gpuImageCompatible("a100", []string{"A100"}), "case-insensitive")
	assert.False(t, gpuImageCompatible("V100S", []string{"5090"}), "card not in the image's supported set")
	assert.True(t, gpuImageCompatible("", []string{"5090"}), "empty gpu = no claim")
}

func TestPreferDeployPlatformImageForPinnedGPUUsesSupportedTypes(t *testing.T) {
	platform := map[string]any{"ImageSet": []any{
		map[string]any{"Name": "vLLM v0.12.0", "CompShareImageId": "img-base",
			"SupportedGpuTypes": []any{"4090", "A100", "H20"}},
		map[string]any{"Name": "vLLM v0.12.0-newgpu", "CompShareImageId": "img-newgpu",
			"SupportedGpuTypes": []any{"TEST_GPU_X", "TEST_GPU_XL"}},
		map[string]any{"Name": "ComfyUI基础镜像0.3.66", "CompShareImageId": "img-cf",
			"SupportedGpuTypes": []any{"4090"}},
		map[string]any{"Name": "ComfyUI基础镜像0.3.66 新架构版", "CompShareImageId": "img-cf-newgpu",
			"SupportedGpuTypes": []any{"TEST_GPU_X"}},
	}}

	t.Run("pinned catalog GPU swaps base to supported sibling without card-specific suffixes", func(t *testing.T) {
		plan := deployPlan{ImageSource: "platform", ImageName: "vLLM v0.12.0", ImageID: "img-base"}
		got, supp, note := preferDeployPlatformImageForGPU(plan, platform, "TEST_GPU_X", "", false)
		require.NotEmpty(t, note, "swap must be noted so the user sees why the image changed")
		assert.Equal(t, "vLLM v0.12.0-newgpu", got.ImageName)
		assert.Equal(t, "img-newgpu", got.ImageID)
		assert.Equal(t, []string{"TEST_GPU_X", "TEST_GPU_XL"}, supp, "supported set follows the swapped image")
	})

	t.Run("non-suffix sibling also works when SupportedGpuTypes matches", func(t *testing.T) {
		plan := deployPlan{ImageSource: "platform", ImageName: "ComfyUI基础镜像0.3.66", ImageID: "img-cf"}
		got, _, note := preferDeployPlatformImageForGPU(plan, platform, "TEST_GPU_X", "", false)
		require.NotEmpty(t, note)
		assert.Equal(t, "ComfyUI基础镜像0.3.66 新架构版", got.ImageName)
	})

	t.Run("no swap when image already supports requested GPU", func(t *testing.T) {
		plan := deployPlan{ImageSource: "platform", ImageName: "vLLM v0.12.0", ImageID: "img-base"}
		got, _, note := preferDeployPlatformImageForGPU(plan, platform, "4090", "", false)
		assert.Empty(t, note)
		assert.Equal(t, "vLLM v0.12.0", got.ImageName)
	})

	t.Run("no swap when no compatible sibling exists", func(t *testing.T) {
		plan := deployPlan{ImageSource: "platform", ImageName: "vLLM v0.12.0", ImageID: "img-base"}
		_, _, note := preferDeployPlatformImageForGPU(plan, platform, "MI300X", "", false)
		assert.Empty(t, note)
	})
}

func TestPreferDeployPlatformImageForPinnedGPU(t *testing.T) {
	platform := map[string]any{"ImageSet": []any{
		map[string]any{"Name": "PyTorch 2.4", "CompShareImageId": "img-a800",
			"ImageType": "App", "Status": "Available", "SupportedGpuTypes": []any{"A800"}},
		map[string]any{"Name": "PyTorch 2.4-4090", "CompShareImageId": "img-4090",
			"ImageType": "App", "Status": "Available", "SupportedGpuTypes": []any{"4090"}},
		map[string]any{"Name": "Ubuntu 22.04", "CompShareImageId": "img-ubuntu",
			"ImageType": "System", "Status": "Available"},
	}}
	plan := deployPlan{ImageSource: "platform", ImageName: "PyTorch 2.4", ImageID: "img-a800"}

	got, supported, note := preferDeployPlatformImageForGPU(plan, platform, "4090", "", false)

	require.NotEmpty(t, note)
	assert.Equal(t, "PyTorch 2.4-4090", got.ImageName)
	assert.Equal(t, "img-4090", got.ImageID)
	assert.Equal(t, []string{"4090"}, supported)
}

func TestPreferDeployPlatformImageForPinnedGPURejectsVMImageInPodZone(t *testing.T) {
	platform := map[string]any{"ImageSet": []any{
		map[string]any{"Name": "PyTorch 2.4", "CompShareImageId": "img-vm",
			"ImageType": "System", "Status": "Available", "SupportedGpuTypes": []any{"4090"}},
		map[string]any{"Name": "PyTorch 2.4 Container", "CompShareImageId": "img-container",
			"ImageType": "App", "Status": "Available", "Container": true, "SupportedGpuTypes": []any{"4090"}},
	}}
	plan := deployPlan{ImageSource: "platform", ImageName: "PyTorch 2.4", ImageID: "img-vm"}

	got, supported, note := preferDeployPlatformImageForGPU(plan, platform, "4090", "cn-pod-01", true)

	require.NotEmpty(t, note)
	assert.Equal(t, "PyTorch 2.4 Container", got.ImageName)
	assert.Equal(t, "img-container", got.ImageID)
	assert.Equal(t, []string{"4090"}, supported)
}

func TestAlignDeployPlatformImageForGPU(t *testing.T) {
	platform := map[string]any{"ImageSet": []any{
		map[string]any{"Name": "vLLM v0.12.0-5090", "CompShareImageId": "img-5090", "SupportedGpuTypes": []any{"5090"}},
		map[string]any{"Name": "vLLM v0.12.0", "CompShareImageId": "img-base", "SupportedGpuTypes": []any{"4090", "A100", "A800"}},
	}}

	t.Run("variant + selected GPU → align to compatible sibling", func(t *testing.T) {
		plan, supp, note := alignDeployPlatformImageForGPU(deployPlan{ImageSource: "platform", ImageName: "vLLM v0.12.0-5090", ImageID: "img-5090"}, platform, "A100", "", false)
		require.NotEmpty(t, note)
		assert.Equal(t, "img-base", plan.ImageID)
		assert.Equal(t, "vLLM v0.12.0", plan.ImageName)
		assert.ElementsMatch(t, []string{"4090", "A100", "A800"}, supp)
	})
	t.Run("no compatible sibling", func(t *testing.T) {
		_, _, note := alignDeployPlatformImageForGPU(deployPlan{ImageSource: "platform", ImageName: "vLLM v0.12.0-5090", ImageID: "img-5090"}, platform, "MI300X", "", false)
		assert.Empty(t, note)
	})
}

// TestMatchDeployImage_ArchVariantDowngradesToBase proves the gate end-to-end: the
// matcher picks "vLLM v0.12.0-5090" (supp=[5090]) for a 32B model the 5090's 32GB
// can't hold; instead of erroring, the handler downgrades to the generic
// "vLLM v0.12.0" (supports A100) and proceeds. WHY: ~1-in-5 real runs the matcher
// grabs the arch variant for a large model and the user sees a confusing compat error.
func TestMatchDeployImage_ArchVariantDowngradesToBase(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, platformImages: []any{
		map[string]any{"Name": "vLLM v0.12.0-5090", "CompShareImageId": "img-5090", "Softwares": map[string]any{"Framework": "vLLM"}, "SupportedGpuTypes": []any{"5090"}},
		map[string]any{"Name": "vLLM v0.12.0", "CompShareImageId": "img-base", "Softwares": map[string]any{"Framework": "vLLM"}, "SupportedGpuTypes": []any{"4090", "A100", "A800"}},
	}})
	matchJSON := `{"image_source":"platform","image_name":"vLLM v0.12.0-5090","model_name":"DeepSeek-R1-32B","match_kind":"base","size_ambiguous":false,"quantization":"fp16"}`
	eng := newDeployEngine(matchJSON, exec, okConfirm)

	plan, err := eng.matchDeployImage(context.Background(), "部署 DeepSeek-R1-32B", "", noopStep)

	require.NoError(t, err, "the arch variant must downgrade to the base instead of erroring")
	assert.Equal(t, "vLLM v0.12.0", plan.ImageName, "swapped to the generic base")
	assert.Contains(t, plan.MatchNote, "已切换到支持该 GPU 的同类镜像", "the downgrade is noted for the user")
}

// TestBuildDeployReply_IncludesConsoleManageLink proves the post-create reply hands
// the user a jump to the console instance-list page (manage state / login / billing).
func TestBuildDeployReply_IncludesConsoleManageLink(t *testing.T) {
	plan := deployPlan{ImageName: "vLLM v0.12.0-5090", GpuType: "5090", ImageSource: "platform"}
	host := map[string]any{"State": "Running", "Name": "n"}
	reply := buildDeployReply(plan, "uhost-x", host, "Running", imageUsage{})
	assert.Contains(t, reply, deployConsoleInstancesURL)
	assert.Contains(t, reply, "管理实例")
}

func TestBuildDeployReply_UsesZoneLabelForDisplay(t *testing.T) {
	plan := deployPlan{ImageName: "PyTorch", GpuType: "4090", ImageSource: "platform", ChosenZone: "cn-bj2-03", ZoneLabel: "华北一C"}
	host := map[string]any{"State": "Running", "Name": "n"}
	reply := buildDeployReply(plan, "uhost-x", host, "Running", imageUsage{})
	assert.Contains(t, reply, "可用区：华北一C")
	assert.NotContains(t, reply, "可用区：cn-bj2-03")
}

func TestBuildAdviseReply_UsesZoneLabelForDisplay(t *testing.T) {
	plan := deployPlan{ImageName: "PyTorch", GpuType: "4090", ImageSource: "platform", ChosenZone: "cn-bj2-03", ZoneLabel: "华北一C"}
	reply := buildAdviseReply(plan)
	assert.Contains(t, reply, "可用区：华北一C")
	assert.NotContains(t, reply, "可用区：cn-bj2-03")
}

// TestMatchDeployImage_IncompatibleGPUImage proves the compatibility gate: when no
// image-supported card has enough VRAM, the sizer keeps a VRAM-correct card that the
// image does NOT support (e.g. a 72B model on a 4090-only image), which would error
// at create. The matcher must refuse with an actionable message instead — because
// CheckCompShareResourceCapacity won't catch it (returns ResourceEnough=true).
func TestMatchDeployImage_IncompatibleGPUImage(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, platformSupportedGPUs: []string{"4090"}})
	matchJSON := `{"image_source":"platform","image_name":"PyTorch","model_name":"Qwen2.5-72B","quantization":"fp16"}`
	eng := newDeployEngine(matchJSON, exec, okConfirm)

	_, err := eng.matchDeployImage(context.Background(), "部署 Qwen2.5-72B", "", noopStep)

	require.Error(t, err, "an incompatible GPU↔image combo must be refused, not sent to a failing create")
	var ue deployUserError
	require.ErrorAs(t, err, &ue, "the refusal is a user-facing deployUserError")
	assert.Contains(t, ue.Error(), "4090", "message names the image's supported cards")
	assert.Contains(t, ue.Error(), "显存不足")
	assert.Equal(t, 0, countCalls(exec.calls, "CreateCompShareInstance"), "no create attempt for an incompatible combo")
}

// TestMatchDeployImage_CommunityUsesExtractedKeyword proves M3: the matcher runs a
// keyword-extraction call first and feeds that keyword to the community FuzzySearch
// (rather than an unfiltered sample of the ~743-group catalog).
func TestMatchDeployImage_CommunityUsesExtractedKeyword(t *testing.T) {
	var communityArgs []map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{}}, nil // platform empty → force community pick
		case "DescribeCommunityImages":
			communityArgs = append(communityArgs, args)
			return map[string]any{"CompshareImageGroup": []any{
				map[string]any{"ImageName": "数字人 LiveTalking", "ImageDesc": "开箱即用数字人",
					"Data": []any{map[string]any{"CompShareImageId": "c-1"}}},
			}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	client := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"search":"数字人"}`},
		{Content: `{"image_source":"community","image_name":"数字人 LiveTalking","model_name":"","quantization":""}`},
	}}
	eng := NewWithDeps(client, exec, func(string, map[string]any) bool { return true })

	plan, err := eng.matchDeployImage(context.Background(), "我想跑一个数字人", "", noopStep)

	require.NoError(t, err)
	require.NotEmpty(t, communityArgs, "community must be queried")
	assert.Equal(t, "数字人", communityArgs[0]["FuzzySearch"], "community query must use the extracted keyword")
	assert.Equal(t, "community", plan.ImageSource)
	assert.Equal(t, "数字人 LiveTalking", plan.ImageName)
}

// TestTryDeployModel_ThreadsCommunityImageIDToCreate proves the review fix: the
// matcher and the saga resolve the community image through INDEPENDENT queries
// (matcher: FuzzySearch=keyword; saga: FuzzySearch=ImageName), and the saga's
// index-0 pick can differ from the matcher's name-matched group. The matcher's
// resolved CompShareImageId must be THREADED to the create so the instance built is
// the same image the GPU was sized against — not the saga's index-0 of its own query.
func TestTryDeployModel_ThreadsCommunityImageIDToCreate(t *testing.T) {
	var createArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{}}, nil // platform empty → force community
		case "DescribeCommunityImages":
			if fs, _ := args["FuzzySearch"].(string); fs == "数字人" {
				// Matcher's keyword query → the group the matcher picks + sizes GPU against.
				return map[string]any{"CompshareImageGroup": []any{
					map[string]any{"ImageName": "数字人 LiveTalking",
						"Data": []any{map[string]any{"CompShareImageId": "matcher-pick", "SupportedGpuTypes": []any{"4090"}}}},
				}}, nil
			}
			// StepRunner's FuzzySearch=ImageName query → a DIFFERENT id at index 0.
			return map[string]any{"CompshareImageGroup": []any{
				map[string]any{"ImageName": "数字人 LiveTalking",
					"Data": []any{map[string]any{"CompShareImageId": "saga-index0-WRONG"}}},
			}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			// The create workflow no longer filters by MachineTypes (it queries the
			// full catalog and selects the type/zone in-code), so the mock mirrors
			// that: every GPU type the handler might pick, each at 1×/16C/64GB so the
			// capacity Specs below match the resolved spec regardless of choice.
			var types []any
			for _, name := range []string{"4090", "4090_48G", "5090", "A100", "A800", "V100S", "H20", "3090", "3080Ti", "P40", "2080Ti", "2080"} {
				types = append(types, map[string]any{"Name": name, "MachineSizes": []any{
					map[string]any{"Gpu": float64(1), "Collection": []any{
						map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
					}},
				}})
			}
			return map[string]any{"AvailableInstanceTypes": types}, nil
		case "CheckCompShareResourceCapacity":
			return map[string]any{"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
			}}, nil
		case "GetCompShareInstanceUserPrice":
			return map[string]any{"PriceDetails": []any{}}, nil
		case "CreateCompShareInstance":
			createArgs = args
			return map[string]any{"UHostIds": []any{"u-1"}}, nil
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{"UHostId": "u-1", "State": "Running"}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	client := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"search":"数字人"}`},
		{Content: `{"image_source":"community","image_name":"数字人 LiveTalking","model_name":"","quantization":""}`},
	}}
	eng := NewWithDeps(client, exec, func(string, map[string]any) bool { return true })

	_, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "我想跑一个数字人", noopStep)

	require.True(t, handled)
	require.NotNil(t, createArgs, "create must run")
	assert.Equal(t, "matcher-pick", createArgs["CompShareImageId"],
		"create must use the matcher-resolved image id (threaded), not the saga's index-0 of its own query")
}

// TestTryDeployModel_TerminalFailState proves a terminal init-failure observed on
// the saga's post-create describe is reported as a failure (instance created but
// init failed), not silently framed as still-initializing.
func TestTryDeployModel_TerminalFailState(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Install Fail"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "初始化未成功")
	// Exactly ONE describe — the saga's own post-create "查看状态"; no handler poll.
	assert.Equal(t, 1, countCalls(exec.calls, "DescribeCompShareInstance"), "only the saga's post-create describe runs")
}

// TestTryDeployModel_CapacitySoldOut proves the saga stops at the capacity check
// (sold out) BEFORE create, and the handler reports it without creating anything.
func TestTryDeployModel_CapacitySoldOut(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: false})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "创建未完成")
	assert.Equal(t, 0, countCalls(exec.calls, "CreateCompShareInstance"), "create must NOT run when capacity check fails")
}

// TestTryDeployModel_ConfirmDenied proves the HITL gate works: declining the
// StepConfirm cancels the create.
func TestTryDeployModel_ConfirmDenied(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return false })

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	// A not-granted confirm must honestly report not-executed, never falsely
	// claim the user cancelled.
	assert.Contains(t, reply, "未执行")
	assert.NotContains(t, reply, "已取消")
	assert.Equal(t, 0, countCalls(exec.calls, "CreateCompShareInstance"))
}

// TestTryDeployModel_MutatingDisabled proves the deploy v2 read-only behavior:
// instead of a blank refusal, the handler ADVISES — it runs the matcher (read-only
// queries + sizing) and returns the GPU/image recommendation — but NEVER creates.
// This is the intentional behavior change that makes "跑X用哪个卡 / 帮我搭个能跑Y的
// 环境" useful in read-only mode while keeping create strictly write-gated.
func TestTryDeployModel_MutatingDisabled(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })
	eng.SetMutatingToolsEnabled(false)

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "建议", "read-only mode advises instead of refusing")
	assert.Contains(t, reply, "只读模式", "advice notes write mode is needed to actually deploy")
	assert.Contains(t, reply, "推荐 GPU", "advice surfaces the recommended GPU")
	// The matcher DID run (advice is real), but NO instance was created.
	assert.Equal(t, 0, countCalls(exec.calls, "CreateCompShareInstance"), "read-only must never create")
}

// ── deploy v2: zone selection + advise ──

func okConfirm(string, map[string]any) bool { return true }

// availCardZ builds one DescribeAvailableCompShareInstanceTypes entry tagged with
// a zone + VRAM (the matcher's per-zone ParseAvailableGPUs reads these).
func availCardZ(name, zone string, vram int) map[string]any {
	return map[string]any{
		"Name": name, "Zone": zone, "Status": "Normal",
		"GraphicsMemory": map[string]any{"Value": float64(vram)},
		"Performance":    map[string]any{"Value": float64(vram)},
		// The 1-card size carries a Collection so the saga's resolveTargetSpec can
		// pick a CPU/Memory from the zone-tagged catalog (the create query is no
		// longer MachineTypes-filtered, so it reads these same entries). The matcher
		// only looks at Zone/GraphicsMemory/Gpu, so the extra Collection is inert there.
		"MachineSizes": []any{
			map[string]any{"Gpu": float64(1), "Collection": []any{
				map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
			}},
			map[string]any{"Gpu": float64(8)},
		},
	}
}

// stockExec answers CheckCompShareResourceCapacity per-zone (the pre-create stock
// gate selectDeployZoneAndGPU uses); everything else is an empty result.
func stockExec(byZone map[string]bool) *mockExecutorFn {
	return &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "CheckCompShareResourceCapacity" {
			z, _ := args["Zone"].(string)
			return map[string]any{"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": byZone[z]},
			}}, nil
		}
		return map[string]any{}, nil
	}}
}

func TestSelectDeployZoneAndGPU_PreferredFirst(t *testing.T) {
	exec := stockExec(map[string]bool{"cn-wlcb-01": true, "cn-sh2-02": true})
	eng := NewWithDeps(nil, exec, okConfirm)
	avail := map[string]any{"AvailableInstanceTypes": []any{
		availCardZ("4090", "cn-wlcb-01", 24), availCardZ("4090", "cn-sh2-02", 24),
	}}
	zone, gpu, _, fb, err := eng.selectDeployZoneAndGPU(context.Background(), avail, deployPlan{ModelName: "Qwen2.5-7B", ImageID: "img-1"}, nil, "fp16", "部署", "")
	require.NoError(t, err)
	assert.Equal(t, "cn-wlcb-01", zone, "primary zone wins when it has stock")
	assert.Equal(t, "4090", gpu)
	assert.Empty(t, fb, "no fallback note when the primary zone is used")
}

func TestSelectDeployZoneAndGPU_FallbackOnSoldOut(t *testing.T) {
	exec := stockExec(map[string]bool{"cn-wlcb-01": false, "cn-sh2-02": true})
	eng := NewWithDeps(nil, exec, okConfirm)
	avail := map[string]any{"AvailableInstanceTypes": []any{
		availCardZ("4090", "cn-wlcb-01", 24), availCardZ("4090", "cn-sh2-02", 24),
	}}
	zone, gpu, _, fb, err := eng.selectDeployZoneAndGPU(context.Background(), avail, deployPlan{ModelName: "Qwen2.5-7B", ImageID: "img-1"}, nil, "fp16", "部署", "")
	require.NoError(t, err)
	assert.Equal(t, "cn-sh2-02", zone, "sold-out primary falls back to the next zone")
	assert.Equal(t, "4090", gpu)
	assert.Contains(t, fb, "cn-wlcb-01", "fallback note names the sold-out primary")
	assert.Contains(t, fb, "cn-sh2-02", "fallback note names the chosen zone")
}

func TestSelectDeployZoneAndGPU_UsesLiveZonesWhenNoUserZone(t *testing.T) {
	exec := stockExec(map[string]bool{"cn-bj2-03": true})
	eng := NewWithDeps(nil, exec, okConfirm)
	avail := map[string]any{"AvailableInstanceTypes": []any{
		availCardZ("4090", "cn-bj2-03", 24),
	}}
	zone, gpu, _, fb, err := eng.selectDeployZoneAndGPU(context.Background(), avail, deployPlan{ModelName: "Qwen2.5-7B", ImageID: "img-1"}, nil, "fp16", "部署", "")
	require.NoError(t, err)
	assert.Equal(t, "cn-bj2-03", zone, "new live zones must participate without a hardcoded preference list")
	assert.Equal(t, "4090", gpu)
	assert.Empty(t, fb)
}

func TestSelectDeployZoneAndGPU_UserZoneHonored(t *testing.T) {
	exec := stockExec(map[string]bool{"cn-wlcb-01": true, "cn-sh2-02": true})
	eng := NewWithDeps(nil, exec, okConfirm)
	avail := map[string]any{"AvailableInstanceTypes": []any{
		availCardZ("4090", "cn-wlcb-01", 24), availCardZ("4090", "cn-sh2-02", 24),
	}}
	zone, _, _, fb, err := eng.selectDeployZoneAndGPU(context.Background(), avail, deployPlan{ModelName: "Qwen2.5-7B", ImageID: "img-1"}, nil, "fp16", "部署", "cn-sh2-02")
	require.NoError(t, err)
	assert.Equal(t, "cn-sh2-02", zone, "user-specified zone is honored over the preference order")
	assert.Empty(t, fb, "no fallback note — the user got the zone they asked for")
}

func TestSelectDeployZoneAndGPU_UserZoneUnavailable(t *testing.T) {
	// User pins cn-sh2-02 but it is sold out → strict honor → error, never silently move.
	exec := stockExec(map[string]bool{"cn-wlcb-01": true, "cn-sh2-02": false})
	eng := NewWithDeps(nil, exec, okConfirm)
	avail := map[string]any{"AvailableInstanceTypes": []any{
		availCardZ("4090", "cn-wlcb-01", 24), availCardZ("4090", "cn-sh2-02", 24),
	}}
	_, _, _, _, err := eng.selectDeployZoneAndGPU(context.Background(), avail, deployPlan{ModelName: "Qwen2.5-7B", ImageID: "img-1"}, nil, "fp16", "部署", "cn-sh2-02")
	require.Error(t, err, "a sold-out user-specified zone surfaces an error, not a silent fallback")
	assert.Contains(t, err.Error(), "cn-sh2-02")
}

func TestSelectDeployZoneAndGPU_EmptyAvailFallsBackStatic(t *testing.T) {
	// Availability query failed/empty → static-table sizing on the primary zone.
	eng := NewWithDeps(nil, stockExec(nil), okConfirm)
	zone, gpu, _, fb, err := eng.selectDeployZoneAndGPU(context.Background(), nil, deployPlan{ModelName: "Qwen2.5-7B", ImageID: "img-1"}, nil, "fp16", "部署", "")
	require.NoError(t, err)
	assert.Equal(t, "cn-wlcb-01", zone, "empty live set degrades to the primary zone")
	assert.NotEmpty(t, gpu, "static table still sizes a GPU")
	assert.Empty(t, fb)
}

func TestZoneStockState(t *testing.T) {
	ctx := context.Background()
	in := NewWithDeps(nil, stockExec(map[string]bool{"z": true}), okConfirm)
	assert.Equal(t, zoneInStock, in.zoneStockState(ctx, "z", "4090", "img-1"))

	out := NewWithDeps(nil, stockExec(map[string]bool{"z": false}), okConfirm)
	assert.Equal(t, zoneSoldOut, out.zoneStockState(ctx, "z", "4090", "img-1"))

	// No image id → can't check (capacity is image-scoped) → unknown.
	assert.Equal(t, zoneUnknown, in.zoneStockState(ctx, "z", "4090", ""))

	// Empty Specs → unknown.
	empty := NewWithDeps(nil, &mockExecutorFn{fn: func(string, map[string]any) (map[string]any, error) { return map[string]any{}, nil }}, okConfirm)
	assert.Equal(t, zoneUnknown, empty.zoneStockState(ctx, "z", "4090", "img-1"))
}

func TestBuildAdviseReply(t *testing.T) {
	plan := deployPlan{ImageSource: "platform", ImageName: "ComfyUI", GpuType: "A100", ChosenZone: "cn-wlcb-01", MatchNote: "按显存推荐"}

	reply := buildAdviseReply(plan)
	assert.Contains(t, reply, "推荐 GPU：A100")
	assert.Contains(t, reply, "ComfyUI")
	assert.Contains(t, reply, "cn-wlcb-01")
	assert.Contains(t, reply, "只读模式", "advice tells the user why no instance was created")
	assert.NotContains(t, reply, "实例 ID", "advice never reports a created instance")
}

func TestTryDeployModel_MutatingDeployIntentCreates(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	_, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署 DeepSeek R1 32B", noopStep)

	require.True(t, handled)
	assert.Equal(t, 1, countCalls(exec.calls, "CreateCompShareInstance"), "deploy_model handles execution requests and creates through the confirmation flow")
}

// TestTryDeployModel_ExplicitCreateStillCreates guards the other side: an explicit
// "帮我部署X" with writes on still runs the create saga.
func TestTryDeployModel_ExplicitCreateStillCreates(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	_, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "帮我部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	assert.Equal(t, 1, countCalls(exec.calls, "CreateCompShareInstance"), "an explicit create command still creates")
}

func TestTryDeployModel_GuidedCreateFiltersExplicitGPUIntent(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	eng := newDeployEngine(deployMatchJSON, exec, okConfirm)
	eng.guidedCreate = true
	var gpuOptions []string
	eng.confirmEditsFn = func(_ string, _ map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		require.NotNil(t, form)
		require.NotNil(t, form.Step)
		if form.Step.Index == 1 {
			gpu := form.Field("GpuType")
			require.NotNil(t, gpu)
			for _, opt := range gpu.Options {
				gpuOptions = append(gpuOptions, opt.Value)
			}
		}
		return workflow.ConfirmResolution{Confirmed: false}
	}

	_, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "用4090部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	assert.Equal(t, []string{"4090", "4090_48G"}, gpuOptions)
}

func TestTryDeployModel_CreatePreferenceFlagOffDoesNotCallExtractor(t *testing.T) {
	SetCreatePreferenceExtractionEnabled(false)
	t.Cleanup(func() { SetCreatePreferenceExtractionEnabled(true) })
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	eng := newDeployEngine(deployMatchJSON, exec, okConfirm)
	extractor := &fakeCreatePreferenceExtractor{result: &CreatePreferenceExtractionResult{WorkloadPref: "DeepSeek R1 32B"}}
	eng.SetCreatePreferenceExtractor(extractor)

	_, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署 DeepSeek R1 32B", noopStep)

	require.True(t, handled)
	assert.Empty(t, extractor.calls)
	assert.Nil(t, eng.createPreferenceThisTurn)
}

func TestTryDeployModel_CreatePreferenceFeedsOnlyImageMatcher(t *testing.T) {
	SetCreatePreferenceExtractionEnabled(true)
	t.Cleanup(func() { SetCreatePreferenceExtractionEnabled(true) })
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	client := &mockLLM{responses: []llm.ChatResponse{{Content: deploySearchJSON}, {Content: deployMatchJSON}}}
	eng := NewWithDeps(client, exec, okConfirm)
	extractor := &fakeCreatePreferenceExtractor{result: &CreatePreferenceExtractionResult{
		WorkloadPref: "DeepSeek R1 32B",
		ImagePref:    "PyTorch",
		GPUPref:      "4090",
	}}
	eng.SetCreatePreferenceExtractor(extractor)

	_, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署 DeepSeek R1 32B", noopStep)

	require.True(t, handled)
	require.Len(t, extractor.calls, 1)
	assert.Equal(t, intent.IntentDeployModel, extractor.calls[0].Intent)
	require.NotNil(t, eng.createPreferenceThisTurn)
	assert.Equal(t, "DeepSeek R1 32B", eng.createPreferenceThisTurn.WorkloadPref)
	require.GreaterOrEqual(t, len(client.calls), 2)
	llmInput := joinMessageContent(client.calls[0]) + "\n" + joinMessageContent(client.calls[1])
	assert.Contains(t, llmInput, "workload_pref: DeepSeek R1 32B")
	assert.Contains(t, llmInput, "image_pref: PyTorch")
	assert.Contains(t, llmInput, "gpu_pref: 4090")
}

func TestTryDeployModel_CreatePreferenceFailureFallsBack(t *testing.T) {
	SetCreatePreferenceExtractionEnabled(true)
	t.Cleanup(func() { SetCreatePreferenceExtractionEnabled(true) })
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	eng := newDeployEngine(deployMatchJSON, exec, okConfirm)
	extractor := &fakeCreatePreferenceExtractor{err: errors.New("extractor unavailable")}
	eng.SetCreatePreferenceExtractor(extractor)

	_, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	assert.Len(t, extractor.calls, 1)
	assert.Nil(t, eng.createPreferenceThisTurn, "extractor failures must not poison deploy state")
	assert.Equal(t, 1, countCalls(exec.calls, "CreateCompShareInstance"), "deploy falls back to the existing behavior")
}

func TestTryDeployModel_CreatePreferenceStillUsesDeployCommandPath(t *testing.T) {
	SetCreatePreferenceExtractionEnabled(true)
	t.Cleanup(func() { SetCreatePreferenceExtractionEnabled(true) })
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	confirmCalls := 0
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { confirmCalls++; return true })
	eng.SetCreatePreferenceExtractor(&fakeCreatePreferenceExtractor{result: &CreatePreferenceExtractionResult{
		WorkloadPref: "部署 Qwen2.5-7B",
		ImagePref:    "PyTorch",
	}})

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署一个 PyTorch 环境", noopStep)

	require.True(t, handled)
	assert.Equal(t, 1, countCalls(exec.calls, "CreateCompShareInstance"), "create preference extraction should not bypass the deploy command path")
	assert.Equal(t, 1, confirmCalls)
	assert.Contains(t, reply, "uhost-deploy-1")
}

// newZoneDeployMock is a full-handler mock with per-zone availability + stock so the
// fallback path can be exercised end-to-end. The matcher's unfiltered availability
// query returns zone-tagged cards; the saga's MachineTypes-filtered query returns
// the spec-shaped response resolveTargetSpec needs; capacity answers per zone.
func newZoneDeployMock(stockByZone map[string]bool, createArgs *map[string]any) *mockExecutorFn {
	return &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{map[string]any{
				"CompShareImageId": "img-pt", "Name": "PyTorch", "ImageType": "App",
				"Softwares": map[string]any{"Framework": "PyTorch"},
			}}}, nil
		case "DescribeCommunityImages":
			return map[string]any{"CompshareImageGroup": []any{}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			if _, filtered := args["MachineTypes"]; filtered {
				// StepRunner spec-resolution query (Zone + MachineTypes) → Collection shape.
				return map[string]any{"AvailableInstanceTypes": []any{
					map[string]any{"Name": "4090", "MachineSizes": []any{
						map[string]any{"Gpu": float64(1), "Collection": []any{
							map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
						}},
					}},
				}}, nil
			}
			// Matcher query (no filter) → zone-tagged cards.
			return map[string]any{"AvailableInstanceTypes": []any{
				availCardZ("4090", "cn-wlcb-01", 24), availCardZ("4090", "cn-sh2-02", 24),
			}}, nil
		case "CheckCompShareResourceCapacity":
			z, _ := args["Zone"].(string)
			return map[string]any{"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": stockByZone[z]},
			}}, nil
		case "GetCompShareInstanceUserPrice":
			return map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": 1.5}}}, nil
		case "CreateCompShareInstance":
			if createArgs != nil {
				*createArgs = args
			}
			return map[string]any{"UHostIds": []any{"u-fb"}}, nil
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "u-fb", "State": "Running",
				"IPSet": []any{map[string]any{"IP": "5.6.7.8", "Type": "Bgp", "Weight": float64(10)}},
			}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
}

// TestTryDeployModel_FallbackZoneInReply proves the end-to-end zone fallback: the
// primary zone (cn-wlcb-01) is sold out for the chosen card, so the handler creates in
// cn-sh2-02 instead, threads that zone to the saga's create, and tells the user.
func TestTryDeployModel_FallbackZoneInReply(t *testing.T) {
	var createArgs map[string]any
	exec := newZoneDeployMock(map[string]bool{"cn-wlcb-01": false, "cn-sh2-02": true}, &createArgs)
	eng := newDeployEngine(`{"image_source":"platform","image_name":"PyTorch","model_name":"Qwen2.5-7B","quantization":""}`, exec, okConfirm)

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	require.NotNil(t, createArgs, "create must run (in the fallback zone)")
	assert.Equal(t, "cn-sh2-02", createArgs["Zone"], "create must use the fallback zone, not the sold-out primary")
	assert.Contains(t, reply, "cn-sh2-02", "reply names the zone used")
	assert.Contains(t, reply, "售罄", "reply tells the user the primary zone was sold out")
}

func TestDeployCandidateZonesSortsBySupportZoneCatalog(t *testing.T) {
	avail := map[string]any{"AvailableInstanceTypes": []any{
		availCardZ("4090", "cn-sh2-02", 24),
		availCardZ("4090", "cn-bj2-03", 24),
		availCardZ("4090", "cn-wlcb-01", 24),
	}}
	candidates := deployCandidateZones(avail)
	sorted := sortDeployCandidateZonesBySupportZones(candidates, []zones.ZoneInfo{
		{Zone: "cn-bj2-03", Describe: "华北一C"},
		{Zone: "cn-wlcb-01", Describe: "华北二A"},
		{Zone: "cn-sh2-02", Describe: "上海二B"},
	})
	assert.Equal(t, []string{"cn-bj2-03", "cn-wlcb-01", "cn-sh2-02"}, sorted)
}

func TestExtractDeployZone(t *testing.T) {
	assert.Equal(t, "cn-sh2-02", extractDeployZone("在上海部署 Qwen2.5-7B"))
	assert.Equal(t, "cn-sh2-02", extractDeployZone("deploy in cn-sh2-02 please"))
	assert.Equal(t, "cn-wlcb-01", extractDeployZone("用乌兰察布的卡跑 ComfyUI"))
	assert.Equal(t, "cn-wlcb-01", extractDeployZone("create in CN-WLCB-01"))
	assert.Equal(t, "", extractDeployZone("帮我部署 Qwen2.5-7B"), "no zone mentioned → empty")
}

// TestExtractDeployGPU pins the deterministic user-named-GPU signal that R4 fixed:
// a card the user explicitly names must be recognized (and canonicalized) so the handler
// honors it instead of auto-sizing a different one — WHILE NOT false-matching a
// digit-run inside a model name (the regression that would make this worse than the
// bug). WHY each case matters is named inline.
func TestExtractDeployGPU(t *testing.T) {
	// Named card is recognized (the R4 trigger: scene sizing would pick 4090).
	assert.Equal(t, "A100", extractDeployGPU("用A100部署SD"))
	assert.Equal(t, "A100", extractDeployGPU("在A100上跑Qwen"))
	assert.Equal(t, "4090", extractDeployGPU("帮我用4090部署 Qwen2.5-7B"))
	assert.Equal(t, "H20", extractDeployGPU("H20 跑大模型"))
	assert.Equal(t, "A800", extractDeployGPU("用a800训练"))

	// Canonicalization: the platform sells V100S, never a bare V100.
	assert.Equal(t, "V100S", extractDeployGPU("部署一个v100的实例"))
	assert.Equal(t, "V100S", extractDeployGPU("用 V100S 跑推理"))

	// Specific variants win over the bare token (more specific = correct card).
	assert.Equal(t, "5090D", extractDeployGPU("用5090D部署"))
	assert.Equal(t, "5090", extractDeployGPU("5090 包月"))
	assert.Equal(t, "4090Pro", extractDeployGPU("帮我开个4090Pro"))
	assert.Equal(t, "4090_48G", extractDeployGPU("用4090 48G的卡"))
	assert.Equal(t, "2080Ti", extractDeployGPU("2080Ti 够吗"))

	// Two cards named → the FIRST in the text wins (here A100 precedes 4090).
	assert.Equal(t, "A100", extractDeployGPU("A100 或 4090 都行"))

	// No false positive inside a model name / unrelated digits (the key guard).
	assert.Equal(t, "", extractDeployGPU("帮我部署 Qwen2.5-7B"), "model name digits must not match")
	assert.Equal(t, "", extractDeployGPU("部署 Qwen2.5-72B"), "72B must not match")
	assert.Equal(t, "", extractDeployGPU("跑一下 Llama100 这个模型"), "a100 inside Llama100 must NOT match (boundary)")
	assert.Equal(t, "", extractDeployGPU("帮我跑一个数字人"), "no GPU mentioned → empty")
	assert.Equal(t, "", extractDeployGPU("部署"), "bare verb → empty")
}

func TestExtractDeployGPUFromCatalogSeesNewUpstreamGPU(t *testing.T) {
	avail := map[string]any{"AvailableInstanceTypes": []any{
		availCardZ("4090", "cn-wlcb-01", 24),
		availCardZ("A100", "cn-wlcb-01", 80),
		availCardZ("TEST_GPU_X", "cn-bj2-03", 192),
		availCardZ("TEST_GPU_XL", "cn-bj2-03", 192),
	}}

	assert.Equal(t, "TEST_GPU_X", extractDeployGPUFromCatalog("用 TEST_GPU_X 部署 Qwen", avail))
	assert.Equal(t, "TEST_GPU_XL", extractDeployGPUFromCatalog("用 TEST_GPU_XL 部署 Qwen", avail))
	assert.Equal(t, "4090", extractDeployGPUFromCatalog("用4090部署", avail))
	assert.Equal(t, "", extractDeployGPUFromCatalog("跑一下 Llama100 这个模型", avail), "catalog matching must keep token boundaries")
}

// TestTryDeployModel_UserZoneFromMessage proves the deterministic zone extraction
// reaches selectDeployZoneAndGPU end-to-end: a request naming 上海 (cn-sh2-02)
// creates in that zone even though cn-wlcb-01 is the default preference.
func TestTryDeployModel_UserZoneFromMessage(t *testing.T) {
	var createArgs map[string]any
	// Both zones in stock; without a user zone the handler would pick cn-wlcb-01.
	exec := newZoneDeployMock(map[string]bool{"cn-wlcb-01": true, "cn-sh2-02": true}, &createArgs)
	eng := newDeployEngine(`{"image_source":"platform","image_name":"PyTorch","model_name":"Qwen2.5-7B","quantization":""}`, exec, okConfirm)

	_, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "在上海部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	require.NotNil(t, createArgs, "create must run")
	assert.Equal(t, "cn-sh2-02", createArgs["Zone"], "user-named zone (上海) overrides the cn-wlcb-01 preference")
}

func TestTryDeployModel_CreatePreferenceDoesNotRewriteZoneResolution(t *testing.T) {
	SetCreatePreferenceExtractionEnabled(true)
	t.Cleanup(func() { SetCreatePreferenceExtractionEnabled(true) })
	var createArgs map[string]any
	exec := newZoneDeployMock(map[string]bool{"cn-wlcb-01": true, "cn-sh2-02": true}, &createArgs)
	eng := newDeployEngine(`{"image_source":"platform","image_name":"PyTorch","model_name":"Qwen2.5-7B","quantization":""}`, exec, okConfirm)
	eng.SetCreatePreferenceExtractor(&fakeCreatePreferenceExtractor{result: &CreatePreferenceExtractionResult{
		WorkloadPref: "Qwen2.5-7B",
		ImagePref:    "PyTorch",
		ZonePref:     "华北二A",
	}})

	_, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "在上海部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	require.NotNil(t, createArgs)
	assert.Equal(t, "cn-sh2-02", createArgs["Zone"], "zone resolution must use the literal user request, not extracted preference text")
}

func TestTryDeployModel_ThreadsInventoryContextToGuidedCard(t *testing.T) {
	var inventoryArgs map[string]any
	var gpuForm *workflow.ConfirmForm
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCommunityImages":
			return map[string]any{"CompshareImageGroup": []any{}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{map[string]any{
				"CompShareImageId":  "img-pt",
				"Name":              "PyTorch 2.9.1 cuda128",
				"ImageType":         "App",
				"SupportedGpuTypes": []any{"4090"},
			}}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{availCardZ("4090", "cn-bj2-03", 24)}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{map[string]any{
				"Zone": "cn-bj2-03", "Region": "cn-bj2", "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": false,
			}}}, nil
		case "DescribeCompShareGpuInventory":
			inventoryArgs = args
			return map[string]any{"GpuInventory": map[string]any{"Exclusive": map[string]any{
				"5001": map[string]any{"4090": float64(5)},
			}}}, nil
		case "CheckCompShareResourceCapacity":
			return map[string]any{"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
			}}, nil
		case "GetCompShareInstanceUserPrice":
			return map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": 1.23}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := newDeployEngine(`{"image_source":"platform","image_name":"PyTorch","model_name":"Qwen2.5-7B","quantization":""}`, exec, okConfirm)
	eng.guidedCreate = true
	eng.confirmEditsFn = func(_ string, _ map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		if form != nil && form.Step != nil && form.Step.Index == 1 {
			gpuForm = form
		}
		return workflow.ConfirmResolution{Confirmed: false}
	}
	ctx := tools.WithUser(context.Background(), tools.UserContext{TopOrganizationID: 101, OrganizationID: 202})

	_, handled := eng.tryDeployModel(ctx, deployDispatch(), "部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	require.NotNil(t, inventoryArgs, "deploy saga must query live GPU inventory before showing the guided card")
	assert.Equal(t, uint32(101), inventoryArgs["top_organization_id"])
	assert.Equal(t, uint32(202), inventoryArgs["organization_id"])
	require.NotNil(t, gpuForm, "first guided form should be captured")
	gpuField := gpuForm.Field("GpuType")
	require.NotNil(t, gpuField)
	require.Len(t, gpuField.Options, 1)
	assert.Contains(t, gpuField.Options[0].Note, "库存约 5 张 GPU", "ZoneIds must be threaded so inventory maps back to the live zone")
}

func TestTryDeployModel_UserPinnedCatalogGPULocksGuidedCard(t *testing.T) {
	var forms []*workflow.ConfirmForm
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCommunityImages":
			return map[string]any{"CompshareImageGroup": []any{}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{map[string]any{
				"CompShareImageId": "img-pt",
				"Name":             "PyTorch 2.9.1 cuda128",
				"ImageType":        "App",
			}}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{availCardZ("TEST_GPU_X", "cn-bj2-03", 192), availCardZ("4090", "cn-bj2-03", 24)}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{map[string]any{
				"Zone": "cn-bj2-03", "Region": "cn-bj2", "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": false,
			}}}, nil
		case "DescribeCompShareGpuInventory":
			return map[string]any{"GpuInventory": map[string]any{"Exclusive": map[string]any{
				"5001": map[string]any{"TEST_GPU_X": float64(2), "4090": float64(9)},
			}}}, nil
		case "CheckCompShareResourceCapacity":
			return map[string]any{"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
			}}, nil
		case "GetCompShareInstanceUserPrice":
			return map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": 1.23}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := newDeployEngine(`{"image_source":"platform","image_name":"PyTorch 2.9.1 cuda128","model_name":"Qwen2.5-7B","quantization":""}`, exec, okConfirm)
	eng.guidedCreate = true
	eng.confirmEditsFn = func(_ string, _ map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		if form != nil {
			forms = append(forms, form)
		}
		return workflow.ConfirmResolution{Confirmed: false}
	}

	_, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "用 TEST_GPU_X 部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	require.NotEmpty(t, forms, "a guided flow should still present a later selection/confirm form")
	for _, form := range forms {
		gpuField := form.Field("GpuType")
		if gpuField == nil {
			continue
		}
		require.Len(t, gpuField.Options, 1, "user-pinned catalog GPU must be locked, not mixed with recommendation alternatives")
		assert.Equal(t, "TEST_GPU_X", gpuField.Value)
		assert.Equal(t, "TEST_GPU_X", gpuField.Options[0].Value)
	}
}

func TestTryDeployModel_CatalogGPUBypassesAmbiguousModelSizeClarify(t *testing.T) {
	var forms []*workflow.ConfirmForm
	var confirmArgs []map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCommunityImages":
			return map[string]any{"CompshareImageGroup": []any{}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{map[string]any{
				"CompShareImageId": "img-pt",
				"Name":             "PyTorch 2.9.1 cuda128",
				"ImageType":        "App",
			}}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{availCardZ("TEST_GPU_X", "cn-bj2-03", 192)}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{map[string]any{
				"Zone": "cn-bj2-03", "Region": "cn-bj2", "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": false,
			}}}, nil
		case "DescribeCompShareGpuInventory":
			return map[string]any{"GpuInventory": map[string]any{"Exclusive": map[string]any{
				"5001": map[string]any{"TEST_GPU_X": float64(2)},
			}}}, nil
		case "CheckCompShareResourceCapacity":
			return map[string]any{"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
			}}, nil
		case "GetCompShareInstanceUserPrice":
			return map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": 1.23}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := newDeployEngine(`{"image_source":"platform","image_name":"PyTorch 2.9.1 cuda128","model_name":"DeepSeek R1","size_ambiguous":true,"quantization":""}`, exec, okConfirm)
	eng.guidedCreate = true
	eng.confirmEditsFn = func(_ string, args map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		confirmArgs = append(confirmArgs, args)
		if form != nil {
			forms = append(forms, form)
		}
		return workflow.ConfirmResolution{Confirmed: false}
	}

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "用 TEST_GPU_X 部署 DeepSeek R1", noopStep)

	require.True(t, handled)
	assert.NotContains(t, reply, "多个参数规模", "a user-pinned live catalog GPU should not be blocked by model-size clarification")
	require.NotEmpty(t, forms)
	var seenPinnedGPU bool
	for _, form := range forms {
		if f := form.Field("GpuType"); f != nil {
			assert.Equal(t, "TEST_GPU_X", f.Value)
			seenPinnedGPU = true
		}
	}
	for _, args := range confirmArgs {
		if args["GpuType"] == "TEST_GPU_X" {
			seenPinnedGPU = true
		}
	}
	assert.True(t, seenPinnedGPU, "confirm flow should carry the user-pinned catalog GPU even if the GPU card is skipped")
}

func TestTryDeployModel_RealignsPlatformImageAfterAutoSelectingPodZone(t *testing.T) {
	var createArgs map[string]any
	var capacityArgs map[string]any
	var priceArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCommunityImages":
			return map[string]any{"CompshareImageGroup": []any{}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-vm", "Name": "PyTorch", "ImageType": "System", "Status": "Available", "Container": false, "SupportedGpuTypes": []any{"4090"}},
				map[string]any{"CompShareImageId": "img-container", "Name": "PyTorch-container", "ImageType": "App", "Status": "Available", "Container": true, "SupportedGpuTypes": []any{"4090"}},
			}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{availCardZ("4090", "cn-pod-01", 24)}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{map[string]any{
				"Zone": "cn-pod-01", "Region": "cn-pod", "ZoneId": float64(9001), "Describe": "测试 Pod 区", "IsPod": true,
			}}}, nil
		case "CheckCompShareResourceCapacity":
			capacityArgs = args
			return map[string]any{"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
			}}, nil
		case "GetCompShareInstanceUserPrice":
			priceArgs = args
			return map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": 1.23}}}, nil
		case "CreateCompShareInstance":
			createArgs = args
			return map[string]any{"UHostIds": []any{"uhost-pod"}}, nil
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{"UHostId": "uhost-pod", "State": "Initializing", "GpuType": "4090"}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := newDeployEngine(`{"image_source":"platform","image_name":"PyTorch","model_name":"Qwen2.5-7B","quantization":""}`, exec, okConfirm)
	eng.zoneCatalog = zones.NewCatalog(0)

	_, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	require.NotNil(t, createArgs, "create must run")
	assert.Equal(t, "cn-pod-01", createArgs["Zone"])
	assert.Equal(t, uint32(9001), createArgs["zone_id"], "Pod create must route upstream by zone_id derived from support-zone catalog")
	assert.Equal(t, uint32(9001), capacityArgs["zone_id"], "Pod capacity preflight must use the same zone_id")
	assert.Equal(t, uint32(9001), priceArgs["zone_id"], "Pod price preflight must use the same zone_id")
	assert.Equal(t, "img-container", createArgs["CompShareImageId"], "auto-selected Pod zones must create with a container image")
}

func TestDeployGuidedGPUCandidates_OnlyClaimsStockWhenConfirmed(t *testing.T) {
	_, reasons := deployGuidedGPUCandidates(deployPlan{
		ImageSource:    "community",
		MatchKind:      "exact",
		ImageName:      "DeepSeek-R1:32b",
		GpuType:        "4090",
		StockConfirmed: false,
	}, nil, nil, "", "", "部署 DeepSeek R1 32B")

	reason := reasons["4090"]
	assert.Contains(t, reason, "现成 DeepSeek-R1:32b 镜像")
	assert.NotContains(t, reason, "当前库存可满足")
	assert.Contains(t, reason, "将继续校验库存")
}

// ── R4: user-named GPU is honored strictly (never silently auto-sized away) ──

// TestSelectDeployZoneAndGPU_PinnedGPUHonored proves the core R4 fix: a card the
// user names in the request is deployed AS-IS, overriding the scene/VRAM auto-sizer
// (which for "用A100部署SD" would otherwise pick 4090). The pin still threads through
// the per-zone stock gate.
func TestSelectDeployZoneAndGPU_PinnedGPUHonored(t *testing.T) {
	exec := stockExec(map[string]bool{"cn-wlcb-01": true})
	eng := NewWithDeps(nil, exec, okConfirm)
	avail := map[string]any{"AvailableInstanceTypes": []any{
		availCardZ("A100", "cn-wlcb-01", 80), availCardZ("4090", "cn-wlcb-01", 24),
	}}
	zone, gpu, note, fb, err := eng.selectDeployZoneAndGPU(context.Background(), avail, deployPlan{ImageID: "img-1"}, nil, "", "用A100部署SD", "")
	require.NoError(t, err)
	assert.Equal(t, "A100", gpu, "the user-named A100 must win over the scene-default 4090")
	assert.Equal(t, "cn-wlcb-01", zone)
	assert.Empty(t, fb)
	assert.Contains(t, note, "A100", "note explains the pin was honored")
}

func TestSelectDeployZoneAndGPU_PinnedCatalogGPUHonored(t *testing.T) {
	exec := stockExec(map[string]bool{"cn-bj2-03": true})
	eng := NewWithDeps(nil, exec, okConfirm)
	avail := map[string]any{"AvailableInstanceTypes": []any{
		availCardZ("TEST_GPU_X", "cn-bj2-03", 192),
		availCardZ("4090", "cn-wlcb-01", 24),
	}}

	zone, gpu, note, fb, err := eng.selectDeployZoneAndGPU(context.Background(), avail, deployPlan{ImageID: "img-1"}, []string{"TEST_GPU_X"}, "", "用 TEST_GPU_X 部署 Qwen", "")

	require.NoError(t, err)
	assert.Equal(t, "TEST_GPU_X", gpu)
	assert.Equal(t, "cn-bj2-03", zone)
	assert.Empty(t, fb)
	assert.Contains(t, note, "TEST_GPU_X")
}

// TestSelectDeployZoneAndGPU_PinnedGPUFallbackZone proves the pin reuses the same
// sold-out-primary fallback as the auto path: A100 sold out in the primary but in
// stock in the secondary → create in the secondary, with a note.
func TestSelectDeployZoneAndGPU_PinnedGPUFallbackZone(t *testing.T) {
	exec := stockExec(map[string]bool{"cn-wlcb-01": false, "cn-sh2-02": true})
	eng := NewWithDeps(nil, exec, okConfirm)
	avail := map[string]any{"AvailableInstanceTypes": []any{
		availCardZ("A100", "cn-wlcb-01", 80), availCardZ("A100", "cn-sh2-02", 80),
	}}
	zone, gpu, _, fb, err := eng.selectDeployZoneAndGPU(context.Background(), avail, deployPlan{ImageID: "img-1"}, nil, "", "用A100部署", "")
	require.NoError(t, err)
	assert.Equal(t, "A100", gpu)
	assert.Equal(t, "cn-sh2-02", zone, "sold-out primary falls back to the next zone, still on the pinned card")
	assert.Contains(t, fb, "cn-wlcb-01")
	assert.Contains(t, fb, "cn-sh2-02")
}

// TestSelectDeployZoneAndGPU_PinnedGPUNotOffered proves strict honoring: when the
// named card is not in the deployable set (only 4090 is), the handler surfaces a grounded
// error listing what IS available — it must NEVER silently substitute 4090.
func TestSelectDeployZoneAndGPU_PinnedGPUNotOffered(t *testing.T) {
	exec := stockExec(map[string]bool{"cn-wlcb-01": true, "cn-sh2-02": true})
	eng := NewWithDeps(nil, exec, okConfirm)
	avail := map[string]any{"AvailableInstanceTypes": []any{
		availCardZ("4090", "cn-wlcb-01", 24), availCardZ("4090", "cn-sh2-02", 24),
	}}
	zone, gpu, _, _, err := eng.selectDeployZoneAndGPU(context.Background(), avail, deployPlan{ImageID: "img-1"}, nil, "", "用A100部署", "")
	require.Error(t, err, "an unavailable pinned card surfaces an error, never a silent substitution")
	assert.Empty(t, gpu, "no GPU returned on the error path")
	assert.Empty(t, zone)
	var ue deployUserError
	require.ErrorAs(t, err, &ue)
	assert.Contains(t, ue.Error(), "A100", "error names the card the user asked for")
	assert.Contains(t, ue.Error(), "4090", "error lists the cards that ARE available")
}

// TestSelectDeployZoneAndGPU_PinnedGPUSoldOut proves the sold-out branch: the named
// card IS offered but has no real stock in any candidate zone → a sold-out error
// (distinct from "not offered"), not a silent move to another card.
func TestSelectDeployZoneAndGPU_PinnedGPUSoldOut(t *testing.T) {
	exec := stockExec(map[string]bool{"cn-wlcb-01": false, "cn-sh2-02": false})
	eng := NewWithDeps(nil, exec, okConfirm)
	avail := map[string]any{"AvailableInstanceTypes": []any{
		availCardZ("4090", "cn-wlcb-01", 24), availCardZ("4090", "cn-sh2-02", 24),
	}}
	_, _, _, _, err := eng.selectDeployZoneAndGPU(context.Background(), avail, deployPlan{ImageID: "img-1"}, nil, "", "用4090部署", "")
	require.Error(t, err)
	var ue deployUserError
	require.ErrorAs(t, err, &ue)
	assert.Contains(t, ue.Error(), "4090")
	assert.Contains(t, ue.Error(), "售罄")
}

// TestSelectDeployZoneAndGPU_PinnedGPUImageIncompatible proves a named card the chosen
// image cannot run is a hard stop with a pin-specific message (names the card + the
// image's supported set), not the auto path's "VRAM too small" explanation.
func TestSelectDeployZoneAndGPU_PinnedGPUImageIncompatible(t *testing.T) {
	eng := NewWithDeps(nil, stockExec(nil), okConfirm)
	avail := map[string]any{"AvailableInstanceTypes": []any{availCardZ("V100S", "cn-wlcb-01", 32)}}
	_, _, _, _, err := eng.selectDeployZoneAndGPU(context.Background(), avail, deployPlan{ImageID: "img-1", ImageName: "PyTorch"}, []string{"4090"}, "", "用V100S部署", "")
	require.Error(t, err)
	var ue deployUserError
	require.ErrorAs(t, err, &ue)
	assert.Contains(t, ue.Error(), "V100S", "names the user's card")
	assert.Contains(t, ue.Error(), "4090", "names the image's supported set")
	assert.Contains(t, ue.Error(), "PyTorch", "names the image")
}

// TestSelectDeployZoneAndGPU_PinnedGPUEmptyAvailDegrades proves graceful degradation:
// when the live catalog is empty/unusable we cannot disprove the pin, so we honor it
// on the primary zone and let the saga's capacity gate decide (auto-path parity).
func TestSelectDeployZoneAndGPU_PinnedGPUEmptyAvailDegrades(t *testing.T) {
	eng := NewWithDeps(nil, stockExec(nil), okConfirm)
	zone, gpu, _, fb, err := eng.selectDeployZoneAndGPU(context.Background(), nil, deployPlan{ImageID: "img-1"}, nil, "", "用A100部署", "")
	require.NoError(t, err, "empty live set must not spuriously reject a pinned card")
	assert.Equal(t, "A100", gpu)
	assert.Equal(t, "cn-wlcb-01", zone, "degrades to the primary zone")
	assert.Empty(t, fb)
}

// TestTryDeployModel_HonorsUserNamedGPU is the end-to-end proof of R4: a request that
// names A100 while describing an SD workload (whose scene-sizer picks 4090) must
// create on A100. This exercises the full handler: matcher → pin extraction → zone/stock
// selection → saga create, asserting the create call carries the user's card.
func TestTryDeployModel_HonorsUserNamedGPU(t *testing.T) {
	var createArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{map[string]any{
				"CompShareImageId": "img-pt", "Name": "PyTorch", "ImageType": "App",
				"Softwares": map[string]any{"Framework": "PyTorch"},
			}}}, nil
		case "DescribeCommunityImages":
			return map[string]any{"CompshareImageGroup": []any{}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			// Both the matcher (no filter) and the saga (Zone, no MachineTypes) read
			// these zone-tagged cards. A100 AND the scene-default 4090 are offered, so
			// the test proves the pin (A100) wins over scene sizing (which → 4090).
			return map[string]any{"AvailableInstanceTypes": []any{
				availCardZ("A100", "cn-wlcb-01", 80), availCardZ("4090", "cn-wlcb-01", 24),
			}}, nil
		case "CheckCompShareResourceCapacity":
			return map[string]any{"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
			}}, nil
		case "GetCompShareInstanceUserPrice":
			return map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": 2.5}}}, nil
		case "CreateCompShareInstance":
			createArgs = args
			return map[string]any{"UHostIds": []any{"u-a100"}}, nil
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "u-a100", "State": "Running",
				"IPSet": []any{map[string]any{"IP": "9.9.9.9", "Type": "Bgp", "Weight": float64(10)}},
			}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	// model_name empty → the auto path would size by scene ("SD" → 4090); the user's
	// explicit A100 must override that.
	eng := newDeployEngine(`{"image_source":"platform","image_name":"PyTorch","model_name":"","quantization":""}`, exec, okConfirm)

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "用A100部署SD", noopStep)

	require.True(t, handled)
	require.NotNil(t, createArgs, "create must run")
	assert.Equal(t, "A100", createArgs["GpuType"], "the user-named A100 must be honored, NOT the scene-default 4090")
	assert.Contains(t, reply, "A100")
}

// ── deploy follow-up: a short refinement inherits the previous deploy target ──

// TestExtractDeploySizedModelName pins the structured sized-model token used to
// carry a deploy target across a follow-up: it must recognize the model SIZE
// (normalizing spacing/suffix) and must NOT fire on a bare GPU follow-up.
func TestExtractDeploySizedModelName(t *testing.T) {
	assert.Equal(t, "Qwen2.5-32B", extractDeploySizedModelName("我想部署Qwen2.5 32B"))
	assert.Equal(t, "Qwen2.5-32B", extractDeploySizedModelName("部署 Qwen2.5-32B-Instruct"))
	assert.Equal(t, "32B", extractDeploySizedModelName("部署 32B 模型"))
	assert.Equal(t, "", extractDeploySizedModelName("A800可以吗"), "a bare GPU follow-up has no sized model")
	assert.Equal(t, "", extractDeploySizedModelName("帮我跑个数字人"), "no size → empty")
}

// TestTryDeployModel_FollowupGPUCarriesPreviousDeployTarget proves the multi-turn
// fix: after "部署 Qwen2.5-32B", the short follow-up "A800可以吗" must be matched as
// "Qwen2.5-32B on A800" — the matcher prompt inherits the earlier model AND still
// sees the newly named GPU, so the (R4) strict-GPU path honors A800 for that model.
func TestTryDeployModel_FollowupGPUCarriesPreviousDeployTarget(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: false})
	client := &mockLLM{responses: []llm.ChatResponse{
		{Content: deploySearchJSON},
		{Content: `{"image_source":"platform","image_name":"PyTorch","model_name":"Qwen2.5-32B","quantization":"fp16"}`},
	}}
	eng := NewWithDeps(client, exec, okConfirm)
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "我想部署Qwen2.5 32B"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "A100 1 卡当前库存不足，请换一个规格或稍后再试。"},
	)

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "A800可以吗", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "A800", "the reply addresses the newly named card, not a silent fallback")
	require.GreaterOrEqual(t, len(client.calls), 2)
	prompt := joinedChatMessages(client.calls[0].Messages) + "\n" + joinedChatMessages(client.calls[1].Messages)
	assert.Contains(t, prompt, "Qwen2.5-32B", "the short follow-up must inherit the previous deploy model")
	assert.Contains(t, prompt, "A800", "the user-named GPU must still be visible to the matcher")
}

// TestTryDeployModel_BareSizeFollowupInheritsClarifiedModel proves the size-clarify
// follow-up combine: after the handler asks "「DeepSeek R1」有多个参数规模…", a bare
// "32B" answer must be matched as "DeepSeek R1 32B" — the matcher prompt inherits the
// model family from our own clarify message, so the model is sized & picked as a 32B
// DeepSeek-R1 instead of a family-less "32B". WHY: real session s_c05ecbeccce4 — the
// "32B" answer lost the DeepSeek-R1 identity and deployed a same-size sibling (QwQ-32B).
func TestTryDeployModel_BareSizeFollowupInheritsClarifiedModel(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	client := &mockLLM{responses: []llm.ChatResponse{
		{Content: deploySearchJSON},
		{Content: `{"image_source":"platform","image_name":"vLLM v0.12.0","model_name":"DeepSeek-R1-32B","match_kind":"base","quantization":""}`},
	}}
	eng := NewWithDeps(client, exec, okConfirm)
	// History as it stands when the user answers the size-clarify: their unsized
	// request + our verbatim clarify message (the family carrier).
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "我想要部署 DeepSeek R1"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: deployClarifyModelSizeMsg("DeepSeek R1")},
	)

	_, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "32B", noopStep)

	require.True(t, handled)
	require.GreaterOrEqual(t, len(client.calls), 2)
	prompt := joinedChatMessages(client.calls[0].Messages) + "\n" + joinedChatMessages(client.calls[1].Messages)
	assert.Contains(t, prompt, "DeepSeek R1", "the bare-size answer must inherit the clarified model family")
	assert.Contains(t, prompt, "32B", "the answered size must reach the matcher")
}

// TestEffectiveDeployUserMsg_BareSizeWithoutClarifyUnchanged proves the guard: a bare
// "32B" with NO preceding size-clarify is left untouched (no spurious family is
// invented), so the combine only fires as an answer to our own clarify.
func TestEffectiveDeployUserMsg_BareSizeWithoutClarifyUnchanged(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, newDeployMock(deployMockConfig{}), okConfirm)
	// Last assistant turn is NOT a size-clarify → no model to inherit.
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "你好"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "你好，有什么可以帮你？"},
	)
	assert.Equal(t, "32B", eng.effectiveDeployUserMsg("32B"), "bare size without a clarify stays unchanged")
}

func TestEffectiveDeployUserMsg_UsesExplicitPendingDeployState(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, newDeployMock(deployMockConfig{}), okConfirm)
	eng.SetSessionState(SessionState{
		SchemaVersion:      SessionStateSchemaV1,
		LastIntent:         string(intent.IntentDeployModel),
		PendingDeployModel: "DeepSeek R1",
	}, 1)

	assert.Equal(t, "继续部署 DeepSeek R1 32B", eng.effectiveDeployUserMsg("32B"))
}

func TestEffectiveDeployUserMsg_DoesNotUsePendingDeployStateAfterUnrelatedIntent(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, newDeployMock(deployMockConfig{}), okConfirm)
	eng.SetSessionState(SessionState{
		SchemaVersion:      SessionStateSchemaV1,
		LastIntent:         string(intent.IntentPricingQuery),
		PendingDeployModel: "DeepSeek R1",
	}, 1)

	assert.Equal(t, "32B", eng.effectiveDeployUserMsg("32B"))
}

func joinedChatMessages(messages []openai.ChatCompletionMessage) string {
	var b strings.Builder
	for _, m := range messages {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

// ── pure-helper unit tests ──

func TestMatchCandidateName(t *testing.T) {
	cands := []string{"PyTorch 2.9.1 cuda128", "ComfyUI", "Ubuntu 22.04"}
	got, ok := matchCandidateName("ComfyUI", cands)
	assert.True(t, ok)
	assert.Equal(t, "ComfyUI", got)

	got, ok = matchCandidateName("pytorch", cands) // case-insensitive contains
	assert.True(t, ok)
	assert.Equal(t, "PyTorch 2.9.1 cuda128", got)

	_, ok = matchCandidateName("TensorFlow", cands)
	assert.False(t, ok)

	_, ok = matchCandidateName("", cands)
	assert.False(t, ok)
}

func TestFirstUHostIDAndHost(t *testing.T) {
	assert.Equal(t, "u-1", firstUHostID(map[string]any{"UHostIds": []any{"u-1"}}))
	assert.Equal(t, "", firstUHostID(map[string]any{"UHostIds": []any{}}))
	assert.Equal(t, "", firstUHostID(nil))

	host := firstHost(map[string]any{"UHostSet": []any{map[string]any{"State": "Running"}}})
	assert.Equal(t, "Running", stringFromHost(host, "State"))
	assert.Nil(t, firstHost(map[string]any{"UHostSet": []any{}}))
	assert.Nil(t, firstHost(nil))
}

func TestIsTerminalFailState(t *testing.T) {
	assert.True(t, isTerminalFailState("Install Fail"))
	assert.True(t, isTerminalFailState("install fail"))
	assert.False(t, isTerminalFailState("Running"))
	assert.False(t, isTerminalFailState("Starting"))
	assert.False(t, isTerminalFailState(""))
}

func TestExtractJSONObject(t *testing.T) {
	assert.JSONEq(t, `{"a":1}`, extractJSONObject("```json\n{\"a\":1}\n```"))
	assert.JSONEq(t, `{"a":1}`, extractJSONObject("好的，结果是 {\"a\":1} 仅此而已"))
	assert.JSONEq(t, `{"a":1}`, extractJSONObject(`{"a":1}`))
}

// TestBuildDeployReply_NeverLeaksPassword guards the secret-boundary invariant:
// the reply renders access info (SSH command) but NEVER the base64 password /
// FileBrowserPassword.
func TestBuildDeployReply_NeverLeaksPassword(t *testing.T) {
	host := map[string]any{
		"Name":                "deploy-x",
		"State":               "Running",
		"SshLoginCommand":     "ssh root@9.9.9.9 -p 22",
		"Password":            "FAKE-PW-DO-NOT-LEAK",
		"FileBrowserPassword": "FAKE-FB-PW-DO-NOT-LEAK",
	}
	reply := buildDeployReply(deployPlan{ImageSource: "platform", ImageName: "PyTorch", GpuType: "A100"}, "u-9", host, "Running", imageUsage{})
	assert.Contains(t, reply, "u-9")
	assert.Contains(t, reply, "ssh root@9.9.9.9")
	assert.Contains(t, reply, "A100")
	assert.NotContains(t, reply, "FAKE-PW-DO-NOT-LEAK", "password must never be rendered")
	assert.NotContains(t, reply, "FAKE-FB-PW-DO-NOT-LEAK", "FileBrowser password must never be rendered")
}

func TestBuildDeployReply_TransientStateGuidesUser(t *testing.T) {
	reply := buildDeployReply(deployPlan{GpuType: "A100"}, "u-1", map[string]any{"State": "Starting"}, "Starting", imageUsage{})
	assert.Contains(t, reply, "初始化")
	assert.Contains(t, reply, "查询我的实例")
}

// TestBuildDeployReply_SurfacesAccessEndpoints proves the usage block turns the
// image's SoftwarePorts + the instance public IP into http endpoints, so the user
// learns WHERE the deployed app lives (e.g. ComfyUI on :8188) — not just an SSH
// command. The endpoint is constructed from ports+IP, never echoed from the
// instance Softwares URLs (which can embed a Jupyter token).
func TestBuildDeployReply_SurfacesAccessEndpoints(t *testing.T) {
	host := map[string]any{
		"State":           "Running",
		"SshLoginCommand": "ssh root@9.9.9.9 -p 22",
		"IPSet": []any{
			map[string]any{"IP": "10.0.0.1", "Type": "Private", "Weight": float64(0)},
			map[string]any{"IP": "9.9.9.9", "Type": "Bgp", "Weight": float64(10)},
		},
	}
	usage := imageUsage{
		ports:    []softwarePort{{name: "ComfyUI", port: 8188}, {name: "JupyterLab", port: 8888}},
		firewall: []int{8000, 8188}, // 8188 dup of an app port → must be deduped out
	}
	reply := buildDeployReply(deployPlan{ImageSource: "platform", ImageName: "ComfyUI", GpuType: "A100"}, "u-9", host, "Running", usage)

	assert.Contains(t, reply, "http://9.9.9.9:8188", "ComfyUI endpoint built from port + public (BGP) IP")
	assert.Contains(t, reply, "http://9.9.9.9:8888", "JupyterLab endpoint")
	assert.NotContains(t, reply, "10.0.0.1", "the private IP must not be used for the public endpoint")
	assert.Contains(t, reply, "8000", "extra firewall port (vLLM-style API) surfaced")
	assert.Contains(t, reply, "令牌", "Jupyter token caution shown when JupyterLab is present")
}

// TestBuildDeployReply_CommunityReadmeExcerpt proves the community author's Readme
// is read and surfaced as a plain-text excerpt (HTML/iframe/image-markdown
// stripped), with the auto-start hint — directly answering "after deploy, can the
// skill guide usage". The excerpt is attributed + capped.
func TestBuildDeployReply_CommunityReadmeExcerpt(t *testing.T) {
	readme := "<iframe src=\"//player.bilibili.com/x\"></iframe>\n# 数字人镜像\n![cover](https://x/y.png)\n## 使用指南\n启动后访问 WebUI 即可生成视频。"
	usage := imageUsage{
		ports:     []softwarePort{{name: "打开WebUI", port: 7860}},
		autoStart: true,
		readme:    readme,
	}
	host := map[string]any{"State": "Running", "IPSet": []any{map[string]any{"IP": "9.9.9.9", "Type": "Bgp", "Weight": float64(1)}}}
	reply := buildDeployReply(deployPlan{ImageSource: "community", ImageName: "数字人合集", GpuType: "5090"}, "u-9", host, "Running", usage)

	assert.Contains(t, reply, "使用说明", "README excerpt section header present")
	assert.Contains(t, reply, "使用指南", "README body text surfaced")
	assert.Contains(t, reply, "启动后访问 WebUI", "README body text surfaced")
	assert.NotContains(t, reply, "<iframe", "HTML tags stripped from the excerpt")
	assert.NotContains(t, reply, "player.bilibili.com/x", "iframe src stripped")
	assert.NotContains(t, reply, "![cover]", "markdown image stripped")
	assert.Contains(t, reply, "自启动", "AutoStart hint shown for community auto-start images")
}

func TestPlainTextExcerpt(t *testing.T) {
	assert.Equal(t, "", plainTextExcerpt("", 100))
	assert.Equal(t, "", plainTextExcerpt("   \n\n  ", 100))

	in := "<iframe src=\"//x\"></iframe>\n# 标题\n![img](http://a/b.png)\n正文内容"
	got := plainTextExcerpt(in, 100)
	assert.NotContains(t, got, "<iframe")
	assert.NotContains(t, got, "![img]")
	assert.Contains(t, got, "# 标题")
	assert.Contains(t, got, "正文内容")

	// Truncation adds an ellipsis past the cap.
	long := strings.Repeat("字", 50)
	assert.Equal(t, strings.Repeat("字", 10)+"…", plainTextExcerpt(long, 10))
}

// TestPlainTextExcerpt_SanitizesUntrustedRunes pins the review hardening: the
// Readme is untrusted community content rendered in a terminal, so ANSI escape
// sequences, bell/VT/FF/CR, Unicode bidi overrides (link-spoofing) and zero-width
// chars must be stripped, and exotic Unicode whitespace folded to a plain space.
func TestPlainTextExcerpt_SanitizesUntrustedRunes(t *testing.T) {
	// ANSI escape (ESC=\x1b) + bell + CR + form-feed must all be removed.
	got := plainTextExcerpt("a\x1b[31mRED\x1b[0m\x07b\rc\x0cd", 100)
	assert.NotContains(t, got, "\x1b", "ESC (ANSI escape) must be stripped")
	assert.NotContains(t, got, "\x07", "bell must be stripped")
	assert.NotContains(t, got, "\r", "carriage return must be stripped")
	assert.NotContains(t, got, "\x0c", "form-feed must be stripped")
	assert.Contains(t, got, "RED", "visible text between escapes is preserved")

	// Bidi/zero-width/BOM/NBSP expressed as Go \u escapes (ASCII source, no literal invisibles).
	got = plainTextExcerpt("Visit \u202egro.elgoog\u202c \u200blink\ufeff", 100)
	assert.NotContains(t, got, "\u202e", "RTL override (U+202E) must be stripped")
	assert.NotContains(t, got, "\u202c", "pop-directional (U+202C) must be stripped")
	assert.NotContains(t, got, "\u200b", "zero-width space (U+200B) must be stripped")
	assert.NotContains(t, got, "\ufeff", "BOM (U+FEFF) must be stripped")
	assert.Contains(t, got, "gro.elgoog", "visible text preserved sans override")

	// Non-breaking space (U+00A0) folds to a plain space and collapses.
	got = plainTextExcerpt("Visit \u00a0\u00a0\u00a0site", 100)
	assert.Equal(t, "Visit site", got, "NBSP folded to space + collapsed")

	// Newlines are preserved as structure.
	assert.Equal(t, "a\nb", plainTextExcerpt("a\nb", 100))
}

func TestHostPublicIP(t *testing.T) {
	// Prefers the non-Private highest-Weight IP.
	host := map[string]any{"IPSet": []any{
		map[string]any{"IP": "10.0.0.1", "Type": "Private", "Weight": float64(99)},
		map[string]any{"IP": "1.1.1.1", "Type": "Bgp", "Weight": float64(1)},
		map[string]any{"IP": "2.2.2.2", "Type": "Internation", "Weight": float64(5)},
	}}
	assert.Equal(t, "2.2.2.2", hostPublicIP(host), "highest-weight non-private IP wins")

	// All private → falls back to a non-empty IP rather than returning empty.
	onlyPriv := map[string]any{"IPSet": []any{map[string]any{"IP": "10.0.0.9", "Type": "Private", "Weight": float64(0)}}}
	assert.Equal(t, "10.0.0.9", hostPublicIP(onlyPriv))

	assert.Equal(t, "", hostPublicIP(nil))
	assert.Equal(t, "", hostPublicIP(map[string]any{}))
}

func TestParseSoftwarePortsAndFirewall(t *testing.T) {
	ports := parseSoftwarePorts([]any{
		map[string]any{"Software": "ComfyUI", "Port": float64(8188)},
		map[string]any{"Software": "Bad", "Port": float64(0)}, // skipped: no port
		map[string]any{"Port": float64(8888)},                 // name defaulted
	})
	require.Len(t, ports, 2)
	assert.Equal(t, softwarePort{name: "ComfyUI", port: 8188}, ports[0])
	assert.Equal(t, "服务", ports[1].name)

	assert.Equal(t, []int{8000, 30000}, parseFirewallPorts([]any{float64(8000), float64(30000), float64(0)}))
	assert.Nil(t, parseFirewallPorts(nil))
}

// TestImageUsageFromResponses pins the parse of the two real response shapes
// (platform ImageSet[] and community CompshareImageGroup[].Data[]) keyed by id.
func TestImageUsageFromResponses(t *testing.T) {
	platform := map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "other", "SoftwarePorts": []any{map[string]any{"Software": "X", "Port": float64(1)}}},
		map[string]any{"CompShareImageId": "img-pt", "Readme": "", "AutoStart": false,
			"SoftwarePorts": []any{map[string]any{"Software": "ComfyUI", "Port": float64(8188)}},
			"FirewallPorts": []any{float64(8000)}},
	}}
	u := platformImageUsage(platform, "img-pt")
	require.Len(t, u.ports, 1)
	assert.Equal(t, 8188, u.ports[0].port, "matched by CompShareImageId, not the first entry")
	assert.Equal(t, []int{8000}, u.firewall)
	assert.False(t, u.autoStart)

	community := map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "数字人", "Data": []any{
			map[string]any{"CompShareImageId": "c-1", "Readme": "# 用法\n直接访问 WebUI", "AutoStart": true,
				"SoftwarePorts": []any{map[string]any{"Software": "WebUI", "Port": float64(7860)}}},
		}},
	}}
	cu := communityImageUsage(community, "c-1")
	assert.True(t, cu.autoStart)
	assert.Contains(t, cu.readme, "直接访问 WebUI")
	require.Len(t, cu.ports, 1)
	assert.Equal(t, 7860, cu.ports[0].port)

	assert.Equal(t, imageUsage{}, platformImageUsage(nil, "x"))
	assert.Equal(t, imageUsage{}, communityImageUsage(nil, "x"))
}

// TestCommunityImageUsage_NeverSubstitutesUnrelatedImage is the regression guard for
// the 2026-06-05 bug: a "deploy DeepSeek-R1-32B" reply showed an LTX-2.3 video
// image's Readme under 「使用说明」. Root cause (confirmed by live probe): production
// DescribeCommunityImages IGNORES the CompShareImageId request filter and returns the
// default popular list (LTX-2.3 at the top), so the keyed post-create read never
// contained the created image's id — and the old first-of-group / first-entry
// fallback then surfaced the unrelated top image's Readme. WHY it matters: a
// post-create usage block must describe EXACTLY the created image; surfacing an
// arbitrary image's guide actively misleads. The fix is strict exact-id matching (no
// fallback), so an absent id yields NO usage block rather than a wrong one.
func TestCommunityImageUsage_NeverSubstitutesUnrelatedImage(t *testing.T) {
	// The production default-list shape: unrelated groups, none is the created image.
	defaultList := map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "LTX-2.3视频生成合集", "Data": []any{
			map[string]any{"CompShareImageId": "img-ltx", "Readme": "# LTX-2.3视频生成合集"},
		}},
		map[string]any{"ImageName": "LiveTalking", "Data": []any{
			map[string]any{"CompShareImageId": "img-lt", "Readme": "# LiveTalking 数字人"},
		}},
	}}
	assert.Equal(t, imageUsage{}, communityImageUsage(defaultList, "want-deepseek"),
		"absent id must yield NO usage block — never the first group's unrelated Readme (the LTX bug)")

	// When the FuzzySearch result DOES contain the exact id (possibly among others),
	// it returns THAT image's usage — the correct Readme, not the first group's.
	withTarget := map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "LTX-2.3视频生成合集", "Data": []any{
			map[string]any{"CompShareImageId": "img-ltx", "Readme": "# LTX-2.3视频生成合集"},
		}},
		map[string]any{"ImageName": "DeepSeek-R1:32b", "Data": []any{
			map[string]any{"CompShareImageId": "want-deepseek", "Readme": "# DeepSeek-R1:32b 镜像", "AutoStart": true,
				"SoftwarePorts": []any{map[string]any{"Software": "WebUI", "Port": float64(7860)}}},
		}},
	}}
	got := communityImageUsage(withTarget, "want-deepseek")
	assert.Contains(t, got.readme, "DeepSeek-R1:32b 镜像", "exact-id match returns the created image's Readme")
	assert.NotContains(t, got.readme, "LTX", "must not surface the first group's unrelated Readme")
	assert.True(t, got.autoStart)
	require.Len(t, got.ports, 1)
	assert.Equal(t, 7860, got.ports[0].port)

	// pickByImageID is strict: an absent/empty id returns nil, never the first entry.
	items := []any{map[string]any{"CompShareImageId": "a"}, map[string]any{"CompShareImageId": "b"}}
	assert.Nil(t, pickByImageID(items, "missing"), "absent id → nil (no first-entry fallback)")
	assert.Nil(t, pickByImageID(items, ""), "empty id → nil")
	assert.Equal(t, "b", pickByImageID(items, "b")["CompShareImageId"], "exact id → that entry")
}

// TestFetchImageUsage_CommunityQueriesByName proves the community post-create read
// queries DescribeCommunityImages by FuzzySearch on the image NAME — not by the
// CompShareImageId filter, which production ignores (see the regression test above).
func TestFetchImageUsage_CommunityQueriesByName(t *testing.T) {
	var gotArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCommunityImages" {
			gotArgs = args
			return map[string]any{"CompshareImageGroup": []any{
				map[string]any{"ImageName": "DeepSeek-R1:32b", "Data": []any{
					map[string]any{"CompShareImageId": "ds-id", "Readme": "# DeepSeek-R1:32b 镜像"},
				}},
			}}, nil
		}
		return map[string]any{}, nil
	}}
	eng := NewWithDeps(nil, exec, okConfirm)
	usage := eng.fetchImageUsage(context.Background(),
		deployPlan{ImageSource: "community", ImageName: "DeepSeek-R1:32b", ImageID: "ds-id"})

	require.NotNil(t, gotArgs, "DescribeCommunityImages must be called")
	assert.Equal(t, "DeepSeek-R1:32b", gotArgs["FuzzySearch"], "community usage is queried by image name")
	_, keyed := gotArgs["CompShareImageId"]
	assert.False(t, keyed, "must NOT use the CompShareImageId filter (production ignores it)")
	assert.Contains(t, usage.readme, "DeepSeek-R1:32b 镜像")
}

func countCalls(calls []string, action string) int {
	n := 0
	for _, c := range calls {
		if c == action {
			n++
		}
	}
	return n
}

// TestShouldClarifyDeployModelSize pins the deploy size-clarify gate: a named
// model with no resolvable size and no pinned GPU asks; an explicit size, a
// pinned GPU, or an app deploy (empty model name) proceeds.
func TestShouldClarifyDeployModelSize(t *testing.T) {
	cases := []struct {
		name      string
		modelName string
		userMsg   string
		want      bool
	}{
		{"ambiguous family, no GPU → clarify", "DeepSeek R1", "我想部署 DeepSeek R1", true},
		{"ambiguous family v3 → clarify", "DeepSeek-V3", "部署 DeepSeek V3", true},
		{"explicit size → proceed", "Qwen2.5-32B", "部署 Qwen2.5-32B", false},
		{"distill with size → proceed", "DeepSeek-R1-Distill-Qwen-7B", "部署 DeepSeek-R1-Distill-Qwen-7B", false},
		{"pinned GPU → proceed", "DeepSeek R1", "用 5090 部署 DeepSeek R1", false},
		{"app deploy (empty model) → proceed", "", "部署 ComfyUI", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shouldClarifyDeployModelSize(tc.modelName, tc.userMsg, ""))
		})
	}
}

func TestShouldClarifyDeployModelSize_HonorsCatalogGPU(t *testing.T) {
	assert.False(t, shouldClarifyDeployModelSize("DeepSeek R1", "用 TEST_GPU_X 部署 DeepSeek R1", "TEST_GPU_X"))
}

// TestMatchDeployImage_SizeAmbiguousGatesClarify proves the #16 fix: the size-clarify
// is ANDed with the matcher's size_ambiguous judgment, so a single specific model the
// table can't size ("Fish Audio S2-Pro", size_ambiguous=false) deploys instead of
// being asked a confusing size question, while a genuine multi-size family
// ("DeepSeek R1", size_ambiguous=true) still gets the clarify.
func TestMatchDeployImage_SizeAmbiguousGatesClarify(t *testing.T) {
	// size_ambiguous=false for an unresolvable single model → NO clarify (proceeds).
	exec := newDeployMock(deployMockConfig{capacityEnough: true})
	eng := newDeployEngine(`{"image_source":"platform","image_name":"PyTorch","model_name":"Fish Audio S2-Pro","match_kind":"base","size_ambiguous":false}`, exec, okConfirm)
	_, err := eng.matchDeployImage(context.Background(), "我想要部署 Fish Audio S2-Pro", "", noopStep)
	assert.NoError(t, err, "a single model (size_ambiguous=false) must NOT trigger the size clarify")

	// size_ambiguous=true for a multi-size family with no size → clarify.
	exec2 := newDeployMock(deployMockConfig{capacityEnough: true})
	eng2 := newDeployEngine(`{"image_source":"platform","image_name":"PyTorch","model_name":"DeepSeek R1","match_kind":"base","size_ambiguous":true}`, exec2, okConfirm)
	_, err2 := eng2.matchDeployImage(context.Background(), "我想部署 DeepSeek R1", "", noopStep)
	var ue deployUserError
	require.ErrorAs(t, err2, &ue, "a multi-size family with no size (size_ambiguous=true) must trigger the clarify")
	assert.Contains(t, ue.Error(), "有多个参数规模")
}

// TestDeployClarifyModelSizeMsg checks the clarification names the model and asks
// which size directly. It now fires ONLY for a genuine multi-size family (the call
// site ANDs size_ambiguous), so the message can state "有多个参数规模" plainly — the
// #16 over-fire for a single model (Fish Audio S2-Pro) is handled upstream, not by
// hedging this message.
func TestDeployClarifyModelSizeMsg(t *testing.T) {
	msg := deployClarifyModelSizeMsg("DeepSeek R1")
	assert.Contains(t, msg, "DeepSeek R1", "names the model")
	assert.Contains(t, msg, "有多个参数规模", "asks which size of the multi-size family")
	assert.Contains(t, msg, "32B", "shows a concrete size the user can reply with")

	// The deployClarifyModelRE anchor must still extract the model (the bare-size
	// follow-up combine depends on it).
	mm := deployClarifyModelRE.FindStringSubmatch(msg)
	require.NotNil(t, mm)
	assert.Equal(t, "DeepSeek R1", mm[1])
}

// TestBuildImageMatchPrompt_AntiConfusionContract pins the load-bearing fix: the
// match prompt MUST tell the model that same-brand / same-size ≠ same model and MUST
// ask for a match_kind judgment. WHY it matters: a real session deployed QwQ-32B for
// a "DeepSeek R1 32B" request — a same-size sibling silently shipped as the wrong
// model. The decision to ship a base instead of a wrong model is an LLM judgment
// (no maintained model→image table); this test guards the prompt that carries it, so
// a future prompt edit that drops the rule fails loudly instead of regressing the bug.
func TestBuildImageMatchPrompt_AntiConfusionContract(t *testing.T) {
	msgs := buildImageMatchPrompt("我想部署 DeepSeek R1 32B", nil, nil)
	require.NotEmpty(t, msgs)
	sys := msgs[0].Content
	assert.Contains(t, sys, "同品牌或同参数量都不等于同一个模型", "anti-confusion rule must be present")
	assert.Contains(t, sys, "match_kind", "the exact|base judgment field must be requested")
	assert.Contains(t, sys, "base", "the base-fallback path (self-host) must be described")
	assert.Contains(t, sys, "绝不要硬塞一个名字相近的别的模型", "must forbid substituting a name-similar different model")
}

// TestDeploySelfDeployHint covers the base-match self-host note. The hint fires ONLY
// for match_kind="base" with a named model (no ready-made image → user pulls it
// themselves); an exact match (ready-made model/app image) and a model-less app
// deploy get no hint. The framework line is rendered from the deployed image's own
// name — a render of the already-made decision, never a second matching decision —
// and the hint never fabricates an exact model tag (no model→tag table).
func TestDeploySelfDeployHint(t *testing.T) {
	cases := []struct {
		name         string
		plan         deployPlan
		wantContains []string
		wantEmpty    bool
	}{
		{
			name:         "base + ollama image → ollama pull guidance, names the model",
			plan:         deployPlan{MatchKind: "base", ModelName: "DeepSeek-R1-32B", ImageName: "Ollama v0.13.1"},
			wantContains: []string{"DeepSeek-R1-32B", "Ollama", "ollama pull", "ollama.com/library", "暂无"},
		},
		{
			name:         "base + vllm image → HF download + framework load",
			plan:         deployPlan{MatchKind: "base", ModelName: "Qwen2.5-32B", ImageName: "vLLM v0.12.0"},
			wantContains: []string{"Qwen2.5-32B", "vLLM", "HuggingFace"},
		},
		{
			name:         "base + generic pytorch base → generic self-pull",
			plan:         deployPlan{MatchKind: "base", ModelName: "Llama-3-70B", ImageName: "PyTorch 2.9.1 cuda128"},
			wantContains: []string{"Llama-3-70B", "PyTorch 2.9.1 cuda128"},
		},
		{
			name:      "exact match → no hint (ready-made image needs no self-pull)",
			plan:      deployPlan{MatchKind: "exact", ModelName: "DeepSeek-R1-32B", ImageName: "Ollama-DeepSeek-R1-32B"},
			wantEmpty: true,
		},
		{
			name:      "base but no model named (app deploy) → no hint",
			plan:      deployPlan{MatchKind: "base", ModelName: "", ImageName: "PyTorch"},
			wantEmpty: true,
		},
		{
			name:      "empty match_kind (legacy) → treated as exact, no hint",
			plan:      deployPlan{MatchKind: "", ModelName: "Qwen2.5-7B", ImageName: "PyTorch"},
			wantEmpty: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deploySelfDeployHint(c.plan)
			if c.wantEmpty {
				assert.Empty(t, got)
				return
			}
			for _, sub := range c.wantContains {
				assert.Contains(t, got, sub)
			}
		})
	}
}

// TestTryDeployModel_BaseMatchShipsHintNotWrongModel proves the fix end-to-end: when
// the matcher judges match_kind="base" for a named model (no ready-made image), the
// deploy still succeeds on a framework base AND the reply tells the user the base was
// shipped + how to self-host — instead of silently presenting a wrong-model image as
// if it were the model. This is the real-session bug (DeepSeek-R1-32B → QwQ-32B) seen
// from the correct side: a base honestly framed, not a sibling smuggled in.
func TestTryDeployModel_BaseMatchShipsHintNotWrongModel(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	matchJSON := `{"image_source":"platform","image_name":"vLLM v0.12.0","model_name":"DeepSeek-R1-32B","match_kind":"base","quantization":""}`
	eng := newDeployEngine(matchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "我想部署 DeepSeek R1 32B", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "uhost-deploy-1", "base match still creates the instance")
	assert.Contains(t, reply, "DeepSeek-R1-32B", "the reply names the model the user actually asked for")
	assert.Contains(t, reply, "暂无", "the reply states there is no ready-made image for that model")
	assert.Equal(t, 1, countCalls(exec.calls, "CreateCompShareInstance"), "still creates once, on the base")
}

// TestTryDeployModel_ExactMatchNoBaseHint proves the symmetric case: an exact match
// (the default match JSON omits match_kind → treated as exact) ships NO self-host
// hint, so a ready-made deploy is not cluttered with "no image" framing.
func TestTryDeployModel_ExactMatchNoBaseHint(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "帮我部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	assert.NotContains(t, reply, "平台暂无", "an exact match must not show the no-ready-made-image note")
}
