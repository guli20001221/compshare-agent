package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/prompt"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// TestProposalToolExposureAndMapping asserts ONLY that the catalog exposes the
// per-operation Request<Operation> tool and maps it back to its operation, and
// that the retired ProposeAction_<Operation> alias is gone. It does NOT — and its
// old name "FirstHopIsRequest" misleadingly implied it did — assert that the real
// model actually calls RequestCreateInstance first on a create turn: under the free
// ReAct loop that is a probabilistic model-behavior property, to be measured from
// real-model traces for acceptance, not something a tool-list test can prove.
func TestProposalToolExposureAndMapping(t *testing.T) {
	names := centralAgentToolNames(true, false)
	require.Contains(t, names, "RequestCreateInstance",
		"the create proposal tool must be advertised as RequestCreateInstance")
	require.NotContains(t, names, "ProposeAction_CreateInstanceWorkflow",
		"the retired ProposeAction_* alias must not be advertised to the model")

	op, ok := proposalOperationForTool("RequestCreateInstance")
	require.True(t, ok)
	require.Equal(t, "CreateInstanceWorkflow", op)

	_, ok = proposalOperationForTool("ProposeAction_CreateInstanceWorkflow")
	require.False(t, ok, "the retired alias must no longer resolve to an operation")
}

// Request tools carry an operation-specific boundary plus the P2 interaction
// template. The template is derived at catalog construction from the capability
// registry, never from a workflow's internal execution-step description.
func TestRequestToolDescriptionUsesCapabilityBoundaryAndP2Template(t *testing.T) {
	var desc string
	for _, tool := range centralAgentToolWindow(true, false) {
		if tool.Function != nil && tool.Function.Name == "RequestCreateInstance" {
			desc = tool.Function.Description
			break
		}
	}
	require.NotEmpty(t, desc, "RequestCreateInstance must be advertised when mutating is enabled")
	capability, ok := tools.DefaultCapabilityRegistry().Lookup("CreateInstanceWorkflow")
	require.True(t, ok)
	require.Equal(t, tools.WorkflowAgentDescription(capability.AgentInstruction), desc)
	require.Contains(t, desc, "调用/边界：")
	require.NotContains(t, desc, "接续：")
	require.NotContains(t, desc, "输入示例")
	for _, internalAPI := range []string{"DescribeCompShare", "GetCompShare", "CreateCompShare", "SyncCompShare"} {
		require.NotContains(t, desc, internalAPI)
	}
}

func TestRequestToolDescriptionsDoNotRepeatSharedPromptOrExecutionChains(t *testing.T) {
	for _, tool := range centralAgentToolWindow(true, false) {
		if tool.Function == nil || !strings.HasPrefix(tool.Function.Name, "Request") {
			continue
		}
		desc := tool.Function.Description
		require.NotEmpty(t, desc, tool.Function.Name)
		require.Contains(t, desc, "调用/边界：", tool.Function.Name)
		require.NotContains(t, desc, "接续：", tool.Function.Name)
		require.NotContains(t, desc, "失败：", tool.Function.Name)
		require.NotContains(t, desc, "输入示例", tool.Function.Name)
		require.NotContains(t, desc, "服务端负责缺失字段", tool.Function.Name)
		require.NotContains(t, desc, "->", tool.Function.Name)
		require.NotContains(t, desc, "自动执行", tool.Function.Name)
		require.NotContains(t, desc, "确认式工作流", tool.Function.Name)
		for _, internalAPI := range []string{"DescribeCompShare", "GetCompShare", "CreateCompShare", "SyncCompShare"} {
			require.NotContains(t, desc, internalAPI, tool.Function.Name)
		}
	}
}

func TestKnowledgeToolExcludesCurrentPlatformFacts(t *testing.T) {
	var description string
	for _, tool := range centralAgentToolWindow(false, false) {
		if tool.Function != nil && tool.Function.Name == "SearchKnowledge" {
			description = tool.Function.Description
			break
		}
	}
	require.Contains(t, description, "平台当前目录")
	require.Contains(t, description, "对应只读能力")
}

func TestCentralAgentStaticPromptAndToolWindowStayWithinBudget(t *testing.T) {
	shapes := []struct {
		name        string
		mutating    bool
		instanceOps bool
	}{
		{name: "read_only"},
		{name: "production", mutating: true, instanceOps: true},
	}
	for _, shape := range shapes {
		system := prompt.BuildSystemWithOptions("context", prompt.BuildOptions{MutatingToolsEnabled: shape.mutating})
		toolJSON, err := json.Marshal(centralAgentToolWindow(shape.mutating, shape.instanceOps))
		require.NoError(t, err)
		t.Logf("mutating=%t instance_ops=%t system_bytes=%d system_runes=%d tool_bytes=%d tool_runes=%d total_bytes=%d total_runes=%d",
			shape.mutating, shape.instanceOps, len(system), len([]rune(system)), len(toolJSON), len([]rune(string(toolJSON))),
			len(system)+len(toolJSON), len([]rune(system))+len([]rune(string(toolJSON))))
		// P2 adds one compact, shared observation contract. Keep B4's measured
		// write-authorization wording verbatim instead of recovering this budget
		// by weakening its anti-over-questioning safeguards.
		//
		// 4800 -> 4900 (2026-08-03): +128 bytes for the correct_tool_call
		// exception inside the needs_input clause. Bought deliberately: without
		// it the generic "补问缺字段" rule makes the model ask the user to restate
		// a question they already stated correctly, on the ~4% of SearchKnowledge
		// calls whose arguments the MODEL malformed (rate recorded in
		// engine.go's parse-error comment and tool_arg_parse_test's fixtures).
		// That is the over-questioning this budget exists to protect against, so
		// squeezing the exception into ambiguity would defeat its own purpose.
		//
		// 4900 -> 5250 (2026-08-14): +294 bytes for the reclaimed-resource
		// terminal rule. Bought deliberately: without it a confirmed-reclaimed
		// instance gets a manufactured recovery path — retention window, manual
		// support escalation, "related resources may still be recoverable" — for
		// data the platform has already permanently deleted, which is a worse
		// failure than any number of bytes. Measured max after the change is 5183
		// (read-only shape), so the number is the measurement plus a small margin,
		// not a round number chosen to be safe.
		//
		// 5250 -> 5600 (2026-08-20): +348 bytes for the operation-substitution rule
		// and for widening 「可选筛选条件」 to 「可选参数」. The occasion was a customer
		// who asked to start their 3090, hit a failed capacity check, and got the
		// instance started in no-GPU mode — the Agent kept the verb and changed the
		// object until it succeeded. The rule that should have covered it was scoped
		// to QUERY facets ("筛选条件"), and nothing said a failed operation is reported
		// rather than substituted.
		//
		// Be honest about what these bytes buy: a replay probe against the real
		// model (10 runs, incident context, bare 「要」) produced the substitution
		// 8/8 on unmodified main and 8/10 with this rule present — and neither miss
		// was the rule. So it is a correctly-scoped general statement, NOT the thing
		// that stops this. The control point is the confirmation card, which now
		// states the whole spec change. Do not spend more prompt bytes on this class
		// of behavior expecting a different result.
		//
		// Measured max after the change is 5531 (read-only shape), so the number is
		// the measurement plus a small margin, not a round number chosen to be safe.
		require.LessOrEqual(t, len(system), 5600, "central system prompt grew past its reviewed byte budget")
		require.NotContains(t, system, "更新任务状态",
			"the retired semantic-memory tool must not remain as a model instruction")
		// The production shape includes the SSH diagnosis tool. Keep the budget
		// attached to the exact window sent by the deployed configuration rather
		// than a cheaper no-SSH approximation.
		require.LessOrEqual(t, len(toolJSON), 35000, "model-visible tool window grew past its reviewed byte budget")
		require.LessOrEqual(t, len(system)+len(toolJSON), 40000,
			"static prompt plus tool schemas grew past its reviewed byte budget")
	}
}

func TestOperationWithRequiredSensitiveInputIsNotAdvertised(t *testing.T) {
	for _, tool := range centralAgentToolWindow(true, false) {
		if tool.Function != nil && tool.Function.Name == "RequestResetPassword" {
			t.Fatal("an operation with no model-safe path for its required password must not be advertised")
		}
	}
}

func TestRequestToolCarriesNormalizedEnumUserQuotes(t *testing.T) {
	var parameters map[string]any
	for _, tool := range centralAgentToolWindow(true, false) {
		if tool.Function != nil && tool.Function.Name == "RequestCreateInstance" {
			parameters = tool.Function.Parameters.(map[string]any)
			break
		}
	}
	require.NotNil(t, parameters)
	properties := parameters["properties"].(map[string]any)
	require.Contains(t, properties, proposalChargeTypeUserQuoteField)
	quoteField := properties[proposalChargeTypeUserQuoteField].(map[string]any)
	require.Equal(t, "string", quoteField["type"])
	require.Contains(t, quoteField["description"], "明确肯定选择")
	require.Contains(t, quoteField["description"], "否定、比较、询价、转述他人意见")
	require.Contains(t, parameters["required"], proposalChargeTypeUserQuoteField)
	charge := properties["ChargeType"].(map[string]any)
	require.Contains(t, charge["description"], proposalChargeTypeUserQuoteField)
	require.Contains(t, properties, proposalImageSourceUserQuoteField)
	sourceQuoteField := properties[proposalImageSourceUserQuoteField].(map[string]any)
	require.Equal(t, "string", sourceQuoteField["type"])
	require.Contains(t, sourceQuoteField["description"], "当前消息")
	require.Contains(t, sourceQuoteField["description"], "历史推荐")
	require.Contains(t, parameters["required"], proposalImageSourceUserQuoteField)
	source := properties["ImageSource"].(map[string]any)
	require.Contains(t, source["description"], proposalImageSourceUserQuoteField)
	require.Contains(t, source["enum"], "sharing")

	got := proposalArgsForOperation("CreateInstanceWorkflow", map[string]any{
		"GpuType":                         "4090",
		"ChargeType":                      "Postpay",
		"ImageSource":                     "community",
		proposalChargeTypeUserQuoteField:  "按量",
		proposalImageSourceUserQuoteField: "社区镜像",
	})
	slots := got["slots"].([]any)
	require.Len(t, slots, 3)
	require.NotContains(t, slots, proposalChargeTypeUserQuoteField)
	require.NotContains(t, slots, proposalImageSourceUserQuoteField)
	chargeSlot := slots[0].(map[string]any)
	require.Equal(t, "ChargeType", chargeSlot["name"])
	require.Equal(t, map[string]any{"quote": "按量"}, chargeSlot["evidence"])
	sourceSlot := slots[2].(map[string]any)
	require.Equal(t, "ImageSource", sourceSlot["name"])
	require.Equal(t, map[string]any{"quote": "社区镜像"}, sourceSlot["evidence"])
}

func TestOnlyCreateChargeTypeAdvertisesNormalizedUserQuotes(t *testing.T) {
	for _, tool := range centralAgentToolWindow(true, false) {
		if tool.Function == nil || tool.Function.Name == "RequestCreateInstance" {
			continue
		}
		properties, _ := tool.Function.Parameters.(map[string]any)["properties"].(map[string]any)
		require.NotContains(t, properties, proposalChargeTypeUserQuoteField, tool.Function.Name)
	}
}

func TestOnlyImageSourceOperationsAdvertiseSourceUserQuotes(t *testing.T) {
	for _, tool := range centralAgentToolWindow(true, false) {
		if tool.Function == nil {
			continue
		}
		properties, _ := tool.Function.Parameters.(map[string]any)["properties"].(map[string]any)
		_, hasImageSource := properties["ImageSource"]
		if hasImageSource {
			require.Contains(t, properties, proposalImageSourceUserQuoteField, tool.Function.Name)
			continue
		}
		require.NotContains(t, properties, proposalImageSourceUserQuoteField, tool.Function.Name)
	}
}

func TestKnowledgeOnlyWindowExposesKnowledgeAndSupportHandoffOnly(t *testing.T) {
	names := toolNameSet(centralAgentKnowledgeToolWindow())
	require.Contains(t, names, "SearchKnowledge")
	require.Contains(t, names, "ReadChunk")
	require.Contains(t, names, tools.CustomerSupportHandoffName)

	for name := range names {
		require.NotContains(t, name, "Request", "public Q&A must not expose action proposals")
		require.NotContains(t, name, "DescribeCompShare", "public Q&A must not expose tenant resources")
		require.NotContains(t, name, "Diagnose", "public Q&A must not expose diagnoses")
	}
}

func TestKnowledgeOnlyExecutionAllowlistIsFailClosed(t *testing.T) {
	require.True(t, knowledgeOnlyToolAllowed("SearchKnowledge"))
	require.True(t, knowledgeOnlyToolAllowed("ReadChunk"))
	require.True(t, knowledgeOnlyToolAllowed(tools.CustomerSupportHandoffName))
	require.False(t, knowledgeOnlyToolAllowed("DescribeCompShareInstance"))
	require.False(t, knowledgeOnlyToolAllowed("DiagnoseInstanceInternals"))
	require.False(t, knowledgeOnlyToolAllowed("RequestStopInstance"))
	require.False(t, knowledgeOnlyToolAllowed("invented_tool"))
}

func TestKnowledgeOnlyExecutionBlocksUnadvertisedToolCall(t *testing.T) {
	eng := &Engine{knowledgeOnlyThisTurn: true}
	var step StepEvent
	result := eng.executeTool(context.Background(), openai.ToolCall{
		Function: openai.FunctionCall{
			Name:      "DescribeCompShareInstance",
			Arguments: `{}`,
		},
	}, func(event StepEvent) {
		step = event
	})
	require.Equal(t, StepBlocked, step.Type)
	require.Contains(t, result, "仅允许查询知识库或转接人工客服")
}

func TestPublicPlatformReadOnlyWindowExposesEveryPublicQueryAndNothingElse(t *testing.T) {
	names := toolNameSet(centralAgentPublicPlatformReadOnlyToolWindow())
	require.Len(t, names, len(feishuPublicPlatformReadTools)+3,
		"the public Feishu window is exactly knowledge, support handoff, and the reviewed public read set")
	require.Contains(t, names, "SearchKnowledge")
	require.Contains(t, names, "ReadChunk")
	require.Contains(t, names, tools.CustomerSupportHandoffName)
	for name := range feishuPublicPlatformReadTools {
		require.Contains(t, names, name)
	}
	for _, definition := range capability.ReadDefinitions() {
		if definition.Tool.Function == nil || feishuPublicPlatformReadTools[definition.Tool.Function.Name] {
			continue
		}
		require.NotContains(t, names, definition.Tool.Function.Name,
			"non-mutating does not make a capability safe for an unauthenticated external group")
	}
	for name := range names {
		require.NotContains(t, name, "Request", "public Feishu must not expose action proposals")
		require.NotContains(t, name, "Diagnose", "public Feishu must not expose diagnoses")
		require.NotContains(t, name, "DescribeCompShare", "public Feishu must not expose tenant resources")
	}

	var imageParams, priceParams map[string]any
	for _, tool := range centralAgentPublicPlatformReadOnlyToolWindow() {
		if tool.Function == nil {
			continue
		}
		switch tool.Function.Name {
		case capability.ReadToolName(intent.IntentImageList):
			imageParams = tool.Function.Parameters.(map[string]any)
		case capability.ReadToolName(intent.IntentPricingQuery):
			priceParams = tool.Function.Parameters.(map[string]any)
		}
	}
	require.NotNil(t, imageParams)
	require.NotNil(t, priceParams)
	imageSource := imageParams["properties"].(map[string]any)["source"].(map[string]any)
	require.Equal(t, []string{string(platform.ImageSourcePlatform), string(platform.ImageSourceCommunity)}, imageSource["enum"])
	priceKind := priceParams["properties"].(map[string]any)["price_kind"].(map[string]any)
	require.Equal(t, []string{string(platform.PriceKindCatalog)}, priceKind["enum"])
}

func TestPublicPlatformReadOnlyExecutionBoundaryIsFailClosed(t *testing.T) {
	for name := range feishuPublicPlatformReadTools {
		require.True(t, publicPlatformReadOnlyToolAllowed(name), name)
	}
	require.True(t, publicPlatformReadOnlyToolAllowed("SearchKnowledge"))
	require.True(t, publicPlatformReadOnlyToolAllowed("ReadChunk"))
	require.True(t, publicPlatformReadOnlyToolAllowed(tools.CustomerSupportHandoffName))
	require.False(t, publicPlatformReadOnlyToolAllowed(capability.ReadToolName(intent.IntentResourceInfo)))
	require.False(t, publicPlatformReadOnlyToolAllowed(capability.ReadToolName(intent.IntentImageTagCatalog)))
	require.False(t, publicPlatformReadOnlyToolAllowed(capability.ReadToolName(intent.IntentNetAcceleratorStatus)))
	require.False(t, publicPlatformReadOnlyToolAllowed("DiagnoseInstanceInternals"))
	require.False(t, publicPlatformReadOnlyToolAllowed("RequestStopInstance"))
	require.False(t, publicPlatformReadOnlyToolAllowed("invented_tool"))

	imageName := capability.ReadToolName(intent.IntentImageList)
	require.True(t, publicPlatformReadOnlyArgsAllowed(imageName, map[string]any{"source": "platform"}))
	require.True(t, publicPlatformReadOnlyArgsAllowed(imageName, map[string]any{"source": "community"}))
	require.False(t, publicPlatformReadOnlyArgsAllowed(imageName, map[string]any{"source": "custom"}))
	require.False(t, publicPlatformReadOnlyArgsAllowed(imageName, map[string]any{"source": "shared"}))

	priceName := capability.ReadToolName(intent.IntentPricingQuery)
	defaultPrice := map[string]any{}
	require.True(t, publicPlatformReadOnlyArgsAllowed(priceName, defaultPrice))
	require.Equal(t, string(platform.PriceKindCatalog), defaultPrice["price_kind"])
	require.True(t, publicPlatformReadOnlyArgsAllowed(priceName, map[string]any{"price_kind": "catalog"}))
	require.False(t, publicPlatformReadOnlyArgsAllowed(priceName, map[string]any{"price_kind": "account"}))

	eng := &Engine{publicPlatformReadOnlyThisTurn: true}
	var step StepEvent
	result := eng.executeTool(context.Background(), openai.ToolCall{Function: openai.FunctionCall{
		Name: capability.ReadToolName(intent.IntentResourceInfo), Arguments: `{}`,
	}}, func(event StepEvent) {
		step = event
	})
	require.Equal(t, StepBlocked, step.Type)
	require.Contains(t, result, publicPlatformReadOnlyBoundary)

	result = eng.executeTool(context.Background(), openai.ToolCall{Function: openai.FunctionCall{
		Name: priceName, Arguments: `{"price_kind":"account"}`,
	}}, func(event StepEvent) {
		step = event
	})
	require.Equal(t, StepBlocked, step.Type)
	require.Contains(t, result, "价格仅限目录价")
}

func TestChatWithOptionsUsesPublicPlatformWindowWithKnowledgeOnlyPrecedence(t *testing.T) {
	publicClient := &deltaMockLLM{}
	publicEngine := NewWithDeps(publicClient, &mockExecutor{}, nil)
	publicEngine.InitWithContext("用户当前没有实例。")

	_, err := publicEngine.ChatWithOptions(context.Background(), "A1000 有吗？", noopStep, ChatOptions{
		PublicPlatformReadOnly: true,
	})
	require.NoError(t, err)
	require.Len(t, publicClient.reqs, 1)
	require.Equal(t,
		toolNameSet(centralAgentPublicPlatformReadOnlyToolWindow()),
		toolNameSet(publicClient.reqs[0].Tools),
		"the per-turn public option must reach the actual model request")

	knowledgeClient := &deltaMockLLM{}
	knowledgeEngine := NewWithDeps(knowledgeClient, &mockExecutor{}, nil)
	knowledgeEngine.InitWithContext("用户当前没有实例。")
	_, err = knowledgeEngine.ChatWithOptions(context.Background(), "A1000 有吗？", noopStep, ChatOptions{
		KnowledgeOnly:          true,
		PublicPlatformReadOnly: true,
	})
	require.NoError(t, err)
	require.Len(t, knowledgeClient.reqs, 1)
	require.Equal(t,
		toolNameSet(centralAgentKnowledgeToolWindow()),
		toolNameSet(knowledgeClient.reqs[0].Tools),
		"the legacy strict knowledge-only option must retain precedence")
}
