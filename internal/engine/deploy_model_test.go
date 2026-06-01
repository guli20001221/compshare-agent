package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
)

// deployDispatch builds the minimal plannerDispatchResult the deploy arm needs:
// an IntentDeployModel plan. The arm reads only the intent + (for the trace) the
// plan; everything else it derives from the user message + live queries.
func deployDispatch() plannerDispatchResult {
	return plannerDispatchResult{
		result: intent.PlannerResult{Plan: intent.Plan{Intent: intent.IntentDeployModel}},
	}
}

// deployMockConfig parameterizes the fake upstream for a deploy run.
type deployMockConfig struct {
	capacityEnough        bool     // CheckCompShareResourceCapacity ResourceEnough
	instanceStates        []string // DescribeCompShareInstance State sequence (last repeats)
	createID              string   // UHostIds[0] returned by CreateCompShareInstance
	communityImageID      string   // when set, the community group carries Data[] with this id; "" = group without Data[] (halt case)
	platformSupportedGPUs []string // when set, the platform image declares these SupportedGpuTypes (M2 intersection)
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
			gt := firstMachineType(args)
			return map[string]any{"AvailableInstanceTypes": []any{
				map[string]any{"Name": gt, "MachineSizes": []any{
					map[string]any{"Gpu": float64(1), "Collection": []any{
						map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
					}},
				}},
			}}, nil
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

func firstMachineType(args map[string]any) string {
	if mt, ok := args["MachineTypes"].([]any); ok && len(mt) > 0 {
		if s, ok := mt[0].(string); ok {
			return s
		}
	}
	return "A100"
}

const deployMatchJSON = `{"image_source":"platform","image_name":"PyTorch","model_name":"Qwen2.5-7B","quantization":""}`

// deploySearchJSON is the extractDeploySearch (call 1) response the matcher makes
// BEFORE the image pick (call 2). Tests seed it as the first mock LLM response.
const deploySearchJSON = `{"search":"Qwen"}`

func newDeployEngine(matchJSON string, exec *mockExecutorFn, confirm func(string, map[string]any) bool) *Engine {
	// matchDeployImage makes TWO TierAgent calls: (1) keyword extraction for the
	// community FuzzySearch, then (2) the image pick. Seed both in order so the
	// single-arg helper still drives the whole arm.
	client := &mockLLM{responses: []llm.ChatResponse{{Content: deploySearchJSON}, {Content: matchJSON}}}
	return NewWithDeps(client, exec, confirm) // mutatingToolsEnabled=true; agentLLMClient=nil → falls back to client
}

// withFastPoll shrinks the poll loop so tests don't sleep. Restored on cleanup.
func withFastPoll(t *testing.T, rounds int) {
	t.Helper()
	origRounds, origInterval := deployPollMaxRounds, deployPollInterval
	deployPollMaxRounds = rounds
	deployPollInterval = 0
	t.Cleanup(func() { deployPollMaxRounds = origRounds; deployPollInterval = origInterval })
}

// TestTryDeployModel_HappyPath_AlreadyRunning proves the end-to-end arm: TierAgent
// match → CreateInstanceDef saga (incl. the L1 create) → poll sees Running →
// deterministic reply with the new instance id + access info. WHY it matters:
// this is B8's first agent-tier skill exercising the orchestrator on a real
// mutating create; the reply must carry the created resource so the user owns it.
func TestTryDeployModel_HappyPath_AlreadyRunning(t *testing.T) {
	withFastPoll(t, 5)
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

// TestTryDeployModel_SurfacesUsageGuidance proves the arm fetches the deployed
// image's usage detail post-create and renders an access endpoint, so the user
// learns HOW to use the instance (here: JupyterLab on :8888 built from the image
// SoftwarePorts + the instance public IP) — closing the "deployed but no guidance"
// gap. The DescribeCompShareImages re-read (by id) must actually happen.
func TestTryDeployModel_SurfacesUsageGuidance(t *testing.T) {
	withFastPoll(t, 5)
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

// TestTryDeployModel_PollUntilRunning proves the handler-side poll loop advances
// across states. The saga's own describe (step 7) consumes the first state; the
// poll loop then re-reads until Running. WHY: a freshly created instance is not
// Running yet; the arm must wait and report the live state, never fabricate it.
func TestTryDeployModel_PollUntilRunning(t *testing.T) {
	withFastPoll(t, 10)
	// [saga step-7]=Starting, [poll1]=Starting, [poll2]=Running.
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Starting", "Starting", "Running"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "运行状态", "poll should observe the Running transition")
	// saga step-7 (1) + at least 2 poll reads to reach Running (index 2).
	assert.GreaterOrEqual(t, countCalls(exec.calls, "DescribeCompShareInstance"), 3)
}

// TestTryDeployModel_PollExhausted proves "poll exhausted ≠ failure": the instance
// never reaches Running within the bound, but the reply still returns the id and
// frames it as still-provisioning (not an error). WHY: slow provisioning must not
// read as a failure — the instance exists and is billable/owned.
func TestTryDeployModel_PollExhausted(t *testing.T) {
	withFastPoll(t, 2)
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Starting"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "uhost-deploy-1")
	assert.Contains(t, reply, "初始化", "exhausted poll frames as still-initializing, not failed")
	assert.NotContains(t, reply, "运行状态")
	// saga step-7 (1) + 2 poll rounds (withFastPoll 2) = 3; pins that the loop
	// actually ran rather than the host==nil fallback masking a no-op poll.
	assert.Equal(t, 3, countCalls(exec.calls, "DescribeCompShareInstance"),
		"saga describe (1) + 2 poll rounds = 3")
}

// TestTryDeployModel_CommunityHappyPath proves the community image path end-to-end:
// the matcher picks a community app, grounding matches it against the live catalog,
// and the saga resolves its CompShareImageId from Data[] and creates.
func TestTryDeployModel_CommunityHappyPath(t *testing.T) {
	withFastPoll(t, 3)
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
	withFastPoll(t, 3)
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	matchJSON := `{"image_source":"community","image_name":"TotallyMadeUpApp","model_name":"","quantization":""}`
	eng := newDeployEngine(matchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "随便跑点啥", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "回退到平台框架镜像", "fallback note should explain the platform fallback")
	assert.Equal(t, 1, countCalls(exec.calls, "CreateCompShareInstance"), "fallback still creates (on a platform base)")
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
// a card the static gpuSpecs table has never heard of ("B200") is selectable. A
// 16B model (~39GB) sizes to the only fitting live card, B200 (48GB) — impossible
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
				map[string]any{"Name": "B200", "Zone": z, "Status": "Normal", "GraphicsMemory": gmem(48), "Performance": perf(130), "MachineSizes": sizes},
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
	assert.Equal(t, "B200", plan.GpuType, "16B (~39GB) must size to the live 48GB card B200, which the static table does not model")
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
	withFastPoll(t, 3)
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
			// Saga's FuzzySearch=ImageName query → a DIFFERENT id at index 0.
			return map[string]any{"CompshareImageGroup": []any{
				map[string]any{"ImageName": "数字人 LiveTalking",
					"Data": []any{map[string]any{"CompShareImageId": "saga-index0-WRONG"}}},
			}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			gt := firstMachineType(args)
			return map[string]any{"AvailableInstanceTypes": []any{
				map[string]any{"Name": gt, "MachineSizes": []any{
					map[string]any{"Gpu": float64(1), "Collection": []any{
						map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
					}},
				}},
			}}, nil
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

// TestTryDeployModel_TerminalFailState proves a terminal init-failure stops the
// poll early and reports the failure (instance created but init failed).
func TestTryDeployModel_TerminalFailState(t *testing.T) {
	withFastPoll(t, 10)
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Install Fail"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "初始化未成功")
	// Poll must stop at the first terminal-fail read, not loop to exhaustion.
	assert.Equal(t, 2, countCalls(exec.calls, "DescribeCompShareInstance"), "saga step-7 + one poll read, then stop")
}

// TestTryDeployModel_CapacitySoldOut proves the saga stops at the capacity check
// (sold out) BEFORE create, and the arm reports it without creating anything.
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
	assert.Contains(t, reply, "已取消")
	assert.Equal(t, 0, countCalls(exec.calls, "CreateCompShareInstance"))
}

// TestTryDeployModel_MutatingDisabled proves the deploy v2 read-only behavior:
// instead of a blank refusal, the arm ADVISES — it runs the matcher (read-only
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
		"MachineSizes":   []any{map[string]any{"Gpu": float64(1)}, map[string]any{"Gpu": float64(8)}},
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
	r := buildAdviseReply(deployPlan{ImageSource: "platform", ImageName: "ComfyUI", GpuType: "A100", ChosenZone: "cn-wlcb-01", MatchNote: "按显存推荐"})
	assert.Contains(t, r, "推荐 GPU：A100")
	assert.Contains(t, r, "ComfyUI")
	assert.Contains(t, r, "cn-wlcb-01")
	assert.Contains(t, r, "只读模式", "advice tells the user how to actually deploy")
	assert.NotContains(t, r, "实例 ID", "advice never reports a created instance")
}

// newZoneDeployMock is a full-arm mock with per-zone availability + stock so the
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
				// Saga spec-resolution query (Zone + MachineTypes) → Collection shape.
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
// primary zone (cn-wlcb-01) is sold out for the chosen card, so the arm creates in
// cn-sh2-02 instead, threads that zone to the saga's create, and tells the user.
func TestTryDeployModel_FallbackZoneInReply(t *testing.T) {
	withFastPoll(t, 3)
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

func TestExtractDeployZone(t *testing.T) {
	assert.Equal(t, "cn-sh2-02", extractDeployZone("在上海部署 Qwen2.5-7B"))
	assert.Equal(t, "cn-sh2-02", extractDeployZone("deploy in cn-sh2-02 please"))
	assert.Equal(t, "cn-wlcb-01", extractDeployZone("用乌兰察布的卡跑 ComfyUI"))
	assert.Equal(t, "cn-wlcb-01", extractDeployZone("create in CN-WLCB-01"))
	assert.Equal(t, "", extractDeployZone("帮我部署 Qwen2.5-7B"), "no zone mentioned → empty")
}

// TestTryDeployModel_UserZoneFromMessage proves the deterministic zone extraction
// reaches selectDeployZoneAndGPU end-to-end: a request naming 上海 (cn-sh2-02)
// creates in that zone even though cn-wlcb-01 is the default preference.
func TestTryDeployModel_UserZoneFromMessage(t *testing.T) {
	withFastPoll(t, 3)
	var createArgs map[string]any
	// Both zones in stock; without a user zone the arm would pick cn-wlcb-01.
	exec := newZoneDeployMock(map[string]bool{"cn-wlcb-01": true, "cn-sh2-02": true}, &createArgs)
	eng := newDeployEngine(`{"image_source":"platform","image_name":"PyTorch","model_name":"Qwen2.5-7B","quantization":""}`, exec, okConfirm)

	_, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "在上海部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	require.NotNil(t, createArgs, "create must run")
	assert.Equal(t, "cn-sh2-02", createArgs["Zone"], "user-named zone (上海) overrides the cn-wlcb-01 preference")
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

func countCalls(calls []string, action string) int {
	n := 0
	for _, c := range calls {
		if c == action {
			n++
		}
	}
	return n
}
