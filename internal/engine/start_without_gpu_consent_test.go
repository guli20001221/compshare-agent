package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/actionresolver"
)

// 2026-08-18, uhost-1twm1ph0l7of. The user's 3090 would not start ("资源不足"),
// the Agent explained why correctly, asked "你要我现在再为它发起一次启动请求吗？",
// and the user answered 「要」. The Agent then called
// RequestStartInstance{UHostId, WithoutGpuSpec:"A"} — a start the user asked for
// carrying a spec change they did not. Upstream applies WithoutGpuSpec as a
// RESIZE to 0 GPU before booting, so the instance came back as 2C/4GB/0 GPU and
// could not be restored while that zone had no 3090; the user watched their
// machine's GPU disappear and threatened to escalate.
//
// Nothing in the pipeline was in a position to stop it. The tool description
// advertised 无卡开机 as a mode of the same tool, the parameter was optional with
// no consent condition, and the resolver accepted agent_inference for it like any
// other enum. These tests pin the gate that now does.
func startInstanceEngine(t *testing.T, question, turnID string) *Engine {
	t.Helper()
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareInstance" {
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1", "Name": "train-a", "State": "Stopped",
				"Zone": "cn-bj2-04", "Region": "cn-bj2", "GpuType": "3090",
				"GPU": float64(1), "CPU": float64(16), "Memory": float64(65536),
				"ChargeType": "Dynamic", "SupportWithoutGpuStart": true,
			}}}, nil
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = question
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, question, turnID, time.Now())
	eng.turnContextViewReady = true
	return eng
}

func resolveStart(t *testing.T, question, turnID string, direct map[string]any) actionresolver.ResolvedAction {
	t.Helper()
	eng := startInstanceEngine(t, question, turnID)
	args := proposalArgsForOperation("StartInstanceWorkflow", direct)
	args["turn_id"] = turnID
	resolved, err := eng.resolveActionProposalShadow(context.Background(), args)
	require.NoError(t, err)
	return resolved.action
}

func rejectionKindFor(action actionresolver.ResolvedAction, slot string) (actionresolver.RejectionKind, bool) {
	for _, problem := range action.RejectedProblems {
		if problem.Slot == slot {
			return problem.Kind, true
		}
	}
	return 0, false
}

// The incident itself: a bare 「要」 authorizes a start, and nothing more.
func TestABareYesDoesNotAuthorizeANoGpuStart(t *testing.T) {
	action := resolveStart(t, "要", "turn-incident", map[string]any{
		"UHostId": "uhost-1", "WithoutGpuSpec": "A",
	})

	require.False(t, action.ReadyForConfirmation,
		"a no-GPU start the user never asked for must not reach the confirmation card")
	kind, ok := rejectionKindFor(action, "WithoutGpuSpec")
	require.True(t, ok, "the rejection must name the offending field: %v", action.RejectedProblems)
	require.Equal(t, actionresolver.RejectRequiresUserRequest, kind)
	require.NotContains(t, action.Arguments, "WithoutGpuSpec",
		"the unrequested spec must not survive as a resolved argument")
	// The target itself was fine — the user did mean this instance. Only the
	// parameter is refused, so the model is told what to drop rather than being
	// left to guess that the whole start was wrong.
	_, targetRejected := rejectionKindFor(action, "UHostId")
	require.False(t, targetRejected, "the instance the user meant is not the problem: %v", action.Rejected)
}

// The same turn without the parameter is the operation the user actually asked
// for, and it is untouched.
func TestTheStartTheUserAskedForStillReachesTheCard(t *testing.T) {
	action := resolveStart(t, "要", "turn-plain-start", map[string]any{"UHostId": "uhost-1"})

	require.True(t, action.ReadyForConfirmation, action.Rejected)
	require.Equal(t, "uhost-1", action.Arguments["UHostId"])
	require.NotContains(t, action.Arguments, "WithoutGpuSpec")
}

// A user who genuinely wants no-GPU mode still gets it, in one turn, by saying so.
// The recognizer anchors on ONE word — 无卡, the platform's own name for the mode —
// so the quote may be any natural span containing it rather than a member of a
// synonym list. Measured over 5262 distinct real user messages in the eval/reports
// prod exports: 无卡 appears 52 times, every paraphrase 0–4, and none of the four
// 不带/不要/不用 GPU hits is an operative request (all are product questions).
func TestAUserWhoAsksForNoGpuModeGetsIt(t *testing.T) {
	tests := []struct{ question, quote string }{
		{question: "把 uhost-1 无卡开机", quote: "无卡开机"},
		{question: "uhost-1 用无卡模式启动一下", quote: "无卡模式"},
		{question: "uhost-1 我要无卡", quote: "无卡"},
		// A span the recognizer never saw as a list member, because it is a phrase
		// rather than a term. This is the case an exact-match table gets wrong.
		{question: "uhost-1 先用无卡的方式开起来", quote: "用无卡的方式"},
	}
	for _, tt := range tests {
		t.Run(tt.quote, func(t *testing.T) {
			action := resolveStart(t, tt.question, "turn-"+tt.quote, map[string]any{
				"UHostId":                        "uhost-1",
				"WithoutGpuSpec":                 "A",
				proposalWithoutGpuUserQuoteField: tt.quote,
			})

			require.True(t, action.ReadyForConfirmation, action.Rejected)
			require.Equal(t, "A", action.Arguments["WithoutGpuSpec"])
			require.Equal(t, actionresolver.SourceUserExplicit, action.Provenance["WithoutGpuSpec"].Source)
		})
	}
}

// The stated cost of anchoring on the platform's own word: a phrasing that never
// names the mode does not authorize it, and the user is asked instead. This is a
// measured trade, not an oversight — 不带卡 / 不挂卡 / 不带显卡 occur zero times in
// 5262 real user messages. It is pinned so that "add another synonym" is a
// decision someone makes against the measurement rather than a silent widening.
func TestAParaphraseThatNeverNamesTheModeDoesNotAuthorizeIt(t *testing.T) {
	action := resolveStart(t, "uhost-1 不带卡开机", "turn-paraphrase", map[string]any{
		"UHostId":                        "uhost-1",
		"WithoutGpuSpec":                 "A",
		proposalWithoutGpuUserQuoteField: "不带卡",
	})

	require.False(t, action.ReadyForConfirmation)
	kind, ok := rejectionKindFor(action, "WithoutGpuSpec")
	require.True(t, ok, "%v", action.RejectedProblems)
	require.Equal(t, actionresolver.RejectRequiresUserRequest, kind)
}

// Either tier is authorized by the same 无卡 quote: the consent this gate is about
// is 无卡-vs-带卡, and which tier is legal is a platform constraint the workflow
// checks (pods take A only), not a choice the user expressed.
func TestEitherTierRidesTheSameNoGpuConsent(t *testing.T) {
	for _, tier := range []string{"A", "B"} {
		action := resolveStart(t, "uhost-1 无卡开机", "turn-tier-"+tier, map[string]any{
			"UHostId":                        "uhost-1",
			"WithoutGpuSpec":                 tier,
			proposalWithoutGpuUserQuoteField: "无卡开机",
		})
		require.True(t, action.ReadyForConfirmation, action.Rejected)
		require.Equal(t, tier, action.Arguments["WithoutGpuSpec"])
	}
}

// The quote is evidence, not a password: it must be a span of THIS message and it
// must actually mean no-GPU mode. Every one of these is the model asserting a user
// request that was not made.
func TestAnUnbackedQuoteDoesNotAuthorizeTheSpecChange(t *testing.T) {
	tests := []struct {
		name     string
		question string
		quote    string
	}{
		{name: "no quote at all", question: "帮我开机", quote: ""},
		{name: "quote not in this message", question: "帮我开机", quote: "无卡"},
		{name: "quote from the agent's own earlier sentence", question: "好的", quote: "无卡模式"},
		{name: "unrelated span of this message", question: "帮我开机", quote: "开机"},
		{name: "duplicate span", question: "无卡还是无卡都行", quote: "无卡"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			direct := map[string]any{"UHostId": "uhost-1", "WithoutGpuSpec": "A"}
			if tt.quote != "" {
				direct[proposalWithoutGpuUserQuoteField] = tt.quote
			}
			action := resolveStart(t, tt.question, "turn-"+tt.name, direct)

			require.False(t, action.ReadyForConfirmation, "%v", action.Arguments)
			kind, ok := rejectionKindFor(action, "WithoutGpuSpec")
			require.True(t, ok, "%v", action.RejectedProblems)
			require.Equal(t, actionresolver.RejectRequiresUserRequest, kind)
		})
	}
}

// The bare wire values are single letters that occur in ordinary sentences. A
// literal "A" in the text is not a request for no-GPU mode, so the automatic
// literal-match promotion must not apply to this field.
func TestALiteralTierLetterInTheSentenceIsNotConsent(t *testing.T) {
	action := resolveStart(t, "把 A 那台 uhost-1 开机", "turn-letter", map[string]any{
		"UHostId": "uhost-1", "WithoutGpuSpec": "A",
	})

	require.False(t, action.ReadyForConfirmation)
	kind, ok := rejectionKindFor(action, "WithoutGpuSpec")
	require.True(t, ok, "%v", action.RejectedProblems)
	require.Equal(t, actionresolver.RejectRequiresUserRequest, kind)
}

// The model cannot award itself the label. deriveProposalProvenance overwrites
// every source the proposal claims before Resolve sees it, so a hand-written
// user_explicit slot with fabricated span offsets is still agent inference.
func TestTheModelCannotLabelItsOwnChoiceAsTheUsers(t *testing.T) {
	eng := startInstanceEngine(t, "要", "turn-forged")

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-forged", "operation": "StartInstanceWorkflow",
		"slots": []any{
			map[string]any{"name": "UHostId", "value": "uhost-1"},
			map[string]any{
				"name": "WithoutGpuSpec", "value": "A", "source": "user_explicit",
				"evidence": map[string]any{
					"message_id": "turn-forged", "start": float64(0), "end": float64(1), "quote": "要",
				},
			},
		},
	})

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation)
	kind, ok := rejectionKindFor(resolved.action, "WithoutGpuSpec")
	require.True(t, ok, "%v", resolved.action.RejectedProblems)
	require.Equal(t, actionresolver.RejectRequiresUserRequest, kind)
}

// The rejection reaches the model verbatim, and what it has to convey is not
// "ask the user for this value" but "drop it and do what they said". A message
// that only reported a failed check would leave the model free to retry the same
// call, which is how the incident would repeat.
func TestTheRejectionTellsTheModelToDropTheParameter(t *testing.T) {
	action := resolveStart(t, "要", "turn-message", map[string]any{
		"UHostId": "uhost-1", "WithoutGpuSpec": "A",
	})

	var message string
	for _, rejected := range action.Rejected {
		if strings.HasPrefix(rejected, "WithoutGpuSpec:") {
			message = rejected
		}
	}
	require.NotEmpty(t, message, "%v", action.Rejected)
	require.Contains(t, message, "去掉该参数")
	require.Contains(t, message, "按用户原话执行")
}

// The gate is a per-workflow declaration, not a global rule about enums. If it
// ever silently spread, every optional enum would start bouncing proposals.
func TestOnlyTheDeclaredFieldCarriesTheGate(t *testing.T) {
	catalog, err := defaultActionCatalog()
	require.NoError(t, err)

	gated := map[string][]string{}
	for _, operation := range catalog.Operations() {
		spec, ok := catalog.Lookup(operation)
		require.True(t, ok)
		for name, field := range spec.Fields {
			if field.RequiresUserRequest {
				gated[operation] = append(gated[operation], name)
			}
		}
	}

	require.Equal(t, map[string][]string{"StartInstanceWorkflow": {"WithoutGpuSpec"}}, gated)
}

// The generated tool the model reads must carry the evidence field, or the gate
// makes a legitimate no-GPU start unreachable rather than merely gated.
func TestTheStartToolOffersTheEvidenceField(t *testing.T) {
	var start *openaiToolFunction
	for _, tool := range centralAgentToolWindow(true, false) {
		if tool.Function != nil && tool.Function.Name == "RequestStartInstance" {
			start = &openaiToolFunction{name: tool.Function.Name, description: tool.Function.Description,
				parameters: tool.Function.Parameters}
		}
	}
	require.NotNil(t, start, "RequestStartInstance must be in the mutating window")

	root, ok := start.parameters.(map[string]any)
	require.True(t, ok)
	properties, ok := root["properties"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, properties, proposalWithoutGpuUserQuoteField)
	require.Contains(t, properties, "WithoutGpuSpec")

	// It is deliberately NOT required: a plain start carries no WithoutGpuSpec, so
	// requiring an empty quote on every start spends a field describing the absence
	// of a rare one. The gate does not depend on it.
	required, _ := root["required"].([]string)
	require.NotContains(t, required, proposalWithoutGpuUserQuoteField)

	// The description carries FACTS about the operation — that it resizes, and that
	// undoing it depends on availability — plus the evidence field it pairs with.
	// It deliberately carries no situational advice: the gate is what refuses an
	// unrequested value, and a description that argues one case goes stale while
	// the gate does not.
	spec, _ := properties["WithoutGpuSpec"].(map[string]any)
	description, _ := spec["description"].(string)
	require.Contains(t, description, "改配")
	require.Contains(t, description, proposalWithoutGpuUserQuoteField)
}

type openaiToolFunction struct {
	name        string
	description string
	parameters  any
}
