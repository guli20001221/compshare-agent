package engine

import (
	"context"
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
	capacityEnough   bool     // CheckCompShareResourceCapacity ResourceEnough
	instanceStates   []string // DescribeCompShareInstance State sequence (last repeats)
	createID         string   // UHostIds[0] returned by CreateCompShareInstance
	communityImageID string   // when set, the community group carries Data[] with this id; "" = group without Data[] (halt case)
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
			return map[string]any{"ImageSet": []any{
				map[string]any{
					"CompShareImageId": "img-pt",
					"Name":             "PyTorch 2.9.1 cuda128",
					"ImageType":        "App",
					"Softwares":        map[string]any{"Framework": "PyTorch"},
					"Description":      "PyTorch 基础镜像",
				},
			}}, nil
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

func newDeployEngine(matchJSON string, exec *mockExecutorFn, confirm func(string, map[string]any) bool) *Engine {
	client := &mockLLM{responses: []llm.ChatResponse{{Content: matchJSON}}}
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

// TestMatchDeployImage_PrefersAgentClient proves the TierAgent routing split
// (ADR-002): when agentLLMClient is set, the matcher calls IT, not the fast
// llmClient fallback. A regression that called the wrong tier would be caught here.
func TestMatchDeployImage_PrefersAgentClient(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true})
	fast := &mockLLM{responses: []llm.ChatResponse{{Content: deployMatchJSON}}}
	agent := &mockLLM{responses: []llm.ChatResponse{{Content: deployMatchJSON}}}
	eng := NewWithDeps(fast, exec, func(string, map[string]any) bool { return true })
	eng.agentLLMClient = agent

	_, err := eng.matchDeployImage(context.Background(), "部署 Qwen2.5-7B", noopStep)

	require.NoError(t, err)
	assert.Equal(t, 1, len(agent.calls), "TierAgent client must be used for image matching")
	assert.Equal(t, 0, len(fast.calls), "fast client must NOT be used when agentLLMClient is set")
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

// TestTryDeployModel_MutatingDisabled proves the read-only default refuses up
// front: no image query, no matcher LLM call, no saga.
func TestTryDeployModel_MutatingDisabled(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true})
	client := &mockLLM{responses: []llm.ChatResponse{{Content: deployMatchJSON}}}
	eng := NewWithDeps(client, exec, func(string, map[string]any) bool { return true })
	eng.SetMutatingToolsEnabled(false)

	reply, handled := eng.tryDeployModel(context.Background(), deployDispatch(), "部署 Qwen2.5-7B", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "只读模式")
	assert.Empty(t, exec.calls, "no upstream calls when writes are disabled")
	assert.Equal(t, 0, len(client.calls), "no matcher LLM call when writes are disabled")
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
// the reply renders access info (SSH command) but NEVER the base64 password.
func TestBuildDeployReply_NeverLeaksPassword(t *testing.T) {
	host := map[string]any{
		"Name":            "deploy-x",
		"State":           "Running",
		"SshLoginCommand": "ssh root@9.9.9.9 -p 22",
		"Password":        "FAKE-PW-DO-NOT-LEAK",
	}
	reply := buildDeployReply(deployPlan{ImageSource: "platform", ImageName: "PyTorch", GpuType: "A100"}, "u-9", host, "Running")
	assert.Contains(t, reply, "u-9")
	assert.Contains(t, reply, "ssh root@9.9.9.9")
	assert.Contains(t, reply, "A100")
	assert.NotContains(t, reply, "FAKE-PW-DO-NOT-LEAK", "password must never be rendered")
}

func TestBuildDeployReply_TransientStateGuidesUser(t *testing.T) {
	reply := buildDeployReply(deployPlan{GpuType: "A100"}, "u-1", map[string]any{"State": "Starting"}, "Starting")
	assert.Contains(t, reply, "初始化")
	assert.Contains(t, reply, "查询我的实例")
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
