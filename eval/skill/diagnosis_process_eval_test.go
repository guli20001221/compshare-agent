package skill_eval

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/orchestrator"
	"github.com/compshare-agent/internal/skills"
	"github.com/compshare-agent/internal/tools"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type diagnosisProcessClient struct {
	replies []string
	calls   [][]openai.ChatCompletionMessage
}

func (c *diagnosisProcessClient) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	c.calls = append(c.calls, req.Messages)
	idx := len(c.calls) - 1
	if idx >= len(c.replies) {
		return &llm.ChatResponse{Content: `{"final":"下一步：请核对实例状态后再继续排查。"}`}, nil
	}
	return &llm.ChatResponse{Content: c.replies[idx]}, nil
}

type diagnosisProcessExecutor struct {
	calls []string
	args  map[string]map[string]any
}

func (e *diagnosisProcessExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	e.calls = append(e.calls, action)
	if e.args == nil {
		e.args = map[string]map[string]any{}
	}
	e.args[action] = args
	switch action {
	case "DescribeCompShareInstance":
		return map[string]any{"UHostSet": []any{map[string]any{
			"UHostId":         "uhost-diag-001",
			"Name":            "diag-instance",
			"State":           "Running",
			"SshLoginCommand": "ssh root@203.0.113.10 -p 22",
			"Softwares":       []any{map[string]any{"Name": "JupyterLab", "URL": "http://203.0.113.10:8888"}},
		}}}, nil
	case "DescribeCompShareSoftwarePort":
		return map[string]any{"SoftwarePorts": []any{map[string]any{"Software": "JupyterLab", "Port": float64(8888)}}}, nil
	case "GetCompShareInstanceMonitor":
		return map[string]any{"MonitorSet": []any{map[string]any{"Metric": "gpu", "Value": float64(0)}}}, nil
	default:
		return map[string]any{"RetCode": 0}, nil
	}
}

func TestDiagnosisProcessEval(t *testing.T) {
	policies := tools.DefaultToolExecutionPolicies()
	mutating := mutatingActionSet(policies)
	cases := loadSkillCases(t)
	covered := map[string]bool{}

	for _, c := range cases {
		if c.Lane != laneDiagnosis {
			continue
		}
		c := c
		t.Run(c.ID, func(t *testing.T) {
			skill := findGeneratedDiagnosisSkill(t, c.ExpectedSkill)
			require.NotEmptyf(t, skill.RequiredTools, "diagnosis skill %q must declare read tools", skill.Name)
			for _, toolName := range skill.RequiredTools {
				policy, ok := policies[toolName]
				require.Truef(t, ok, "tool %q has no execution policy", toolName)
				assert.NotEqualf(t, tools.ActionClassMutating, policy.Class, "%s exposes mutating tool %s", skill.Name, toolName)
				assert.NotEqualf(t, tools.ActionClassDestructive, policy.Class, "%s exposes destructive tool %s", skill.Name, toolName)
			}

			body, err := skill.Body()
			require.NoError(t, err)
			firstTool := skill.RequiredTools[0]
			client := &diagnosisProcessClient{replies: []string{
				fmt.Sprintf(`{"action":%q,"args":{"UHostIds":["uhost-diag-001"]}}`, firstTool),
				`{"final":"下一步：实例状态已读取，请根据症状继续核对登录入口、端口或镜像环境。"}`,
			}}
			exec := &diagnosisProcessExecutor{}

			reply, err := orchestrator.RunReadOnlySkill(context.Background(), c.Question,
				map[string]any{"UHostId": "uhost-diag-001", "Service": "JupyterLab"},
				orchestrator.SkillExecOptions{
					Body:      body,
					Tools:     tools.VisibleRegistryForSubset(skill.RequiredTools, false),
					Exec:      exec,
					Client:    client,
					MaxRounds: 3,
				})
			require.NoError(t, err)
			assert.Contains(t, reply, "下一步", "diagnosis answer should give an actionable next step")
			require.NotEmpty(t, exec.calls, "process eval must exercise at least one read tool")

			allowed := map[string]bool{}
			for _, toolName := range skill.RequiredTools {
				allowed[toolName] = true
			}
			for _, action := range exec.calls {
				assert.Truef(t, allowed[action], "called tool %q is outside %s required_tools %v", action, skill.Name, skill.RequiredTools)
				assert.Falsef(t, mutating[action], "diagnosis process called mutating/destructive tool %q", action)
			}
			assertNoRawRetrievalLeak(t, client.calls, reply)
			covered[skill.Name] = true
		})
	}

	for _, want := range []string{
		"diagnose_ssh",
		"diagnose_init_failure",
		"diagnose_gpu_not_detected",
		"diagnose_image_issue",
		"diagnose_port_firewall",
	} {
		assert.Truef(t, covered[want], "diagnosis process eval has no case for %s", want)
	}
}

func findGeneratedDiagnosisSkill(t *testing.T, name string) *skills.Skill {
	t.Helper()
	for _, skill := range skills.GeneratedSkills() {
		if skill.Name == name {
			return skill
		}
	}
	t.Fatalf("diagnosis skill %q is not in the generated true skill registry", name)
	return nil
}

func mutatingActionSet(policies map[string]tools.ToolExecutionPolicy) map[string]bool {
	out := map[string]bool{}
	for action, policy := range policies {
		if policy.Class == tools.ActionClassMutating || policy.Class == tools.ActionClassDestructive {
			out[action] = true
		}
	}
	return out
}

func assertNoRawRetrievalLeak(t *testing.T, calls [][]openai.ChatCompletionMessage, reply string) {
	t.Helper()
	sentinels := []string{"KnowledgeEvidence", "ChunkID", "RAW_KB_CHUNK", "retrieved chunk"}
	for _, messages := range calls {
		for _, message := range messages {
			for _, sentinel := range sentinels {
				assert.NotContains(t, message.Content, sentinel)
			}
		}
	}
	for _, sentinel := range sentinels {
		assert.NotContains(t, reply, sentinel)
	}
}

func TestDiagnosisProcessEval_AllTrueSkillsAreCovered(t *testing.T) {
	var names []string
	for _, skill := range skills.GeneratedSkills() {
		names = append(names, skill.Name)
	}
	sort.Strings(names)
	require.Equal(t, []string{
		"diagnose_gpu_not_detected",
		"diagnose_image_issue",
		"diagnose_init_failure",
		"diagnose_port_firewall",
		"diagnose_ssh",
	}, names)
	for _, name := range names {
		assert.True(t, strings.HasPrefix(name, "diagnose_"))
	}
}
