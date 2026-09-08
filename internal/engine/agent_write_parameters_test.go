package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The Agent interprets conversation; the server validates its scalar arguments
// and carries them unchanged into the existing confirmation contract.
func TestAgentWriteParametersReachConfirmationWithoutLiteralProof(t *testing.T) {
	for _, tt := range []struct {
		name, question string
		history        []ConversationPair
		args, want     map[string]any
	}{
		{
			name: "equal capacity for both disks", question: "帮我开台 H20，系统盘200g，数据盘也是200g",
			args: map[string]any{"SystemDiskSize": "200GB", "DataDiskSize": "200GB"},
			want: map[string]any{"SystemDiskSize": float64(200), "DataDiskSize": float64(200)},
		},
		{
			name: "Chinese instance name", question: "帮我开台 H20，名字叫测试机",
			args: map[string]any{"Name": "测试机"}, want: map[string]any{"Name": "测试机"},
		},
		{
			name: "negative old image mention", question: "不要用 compshareImage-old，用刚才选好的镜像开台 H20",
			args: map[string]any{"CompShareImageId": "compshareImage-good"}, want: map[string]any{"CompShareImageId": "compshareImage-good"},
		},
		{
			name: "changed capacity survives clarification", question: "先带卡创建",
			history: []ConversationPair{{User: "开台 H20，系统盘100GB"}, {User: "系统盘改成200GB", Assistant: "先带卡创建吗？"}},
			args:    map[string]any{"SystemDiskSize": "200GB"}, want: map[string]any{"SystemDiskSize": float64(200)},
		},
		{
			name: "normalized enum values need no quote protocol", question: "就用社区镜像，按量创建",
			args: map[string]any{"ImageSource": "community", "ChargeType": "Postpay"},
			want: map[string]any{"ImageSource": "community", "ChargeType": "Postpay"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			executor := &mockExecutor{results: map[string]map[string]any{
				"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{map[string]any{"Name": "H20"}}},
				"DescribeCompShareImages": {"ImageSet": []any{
					map[string]any{"CompShareImageId": "compshareImage-old", "Name": "Old image", "Status": "Available"},
					map[string]any{"CompShareImageId": "compshareImage-good", "Name": "Good image", "Status": "Available"},
				}},
			}}
			eng := NewWithDeps(&mockLLM{}, executor, nil)
			eng.lastUserMsg = tt.question
			eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, tt.question, "turn-agent-params", time.Now())
			eng.turnContextViewThisTurn.RecentConversation = tt.history
			eng.turnContextViewReady = true
			tt.args["GpuType"] = "H20"
			resolved, err := eng.resolveActionProposal(context.Background(), proposalArgsForOperation("CreateInstanceWorkflow", tt.args))
			require.NoError(t, err)
			require.Empty(t, resolved.action.Rejected)
			require.True(t, resolved.action.ReadyForConfirmation, resolved.action.DependencyFailures)
			for field, want := range tt.want {
				require.Equal(t, want, resolved.action.Arguments[field], field)
				require.Equal(t, want, resolved.action.Confirmation.Arguments[field], field)
			}
		})
	}
}

func TestWriteRequestSchemaContainsOnlyOperationFields(t *testing.T) {
	catalog, err := defaultActionCatalog()
	require.NoError(t, err)
	spec, ok := catalog.Lookup("CreateInstanceWorkflow")
	require.True(t, ok)
	for _, tool := range centralAgentToolWindow(true, false) {
		if tool.Function == nil || tool.Function.Name != proposalToolName(spec.Operation) {
			continue
		}
		parameters := tool.Function.Parameters.(map[string]any)
		properties := parameters["properties"].(map[string]any)
		for name := range properties {
			require.Contains(t, spec.Fields, name)
		}
		require.Empty(t, parameters["required"], "partial proposals still open the guided form")
		return
	}
	t.Fatal("create request tool missing")
}
