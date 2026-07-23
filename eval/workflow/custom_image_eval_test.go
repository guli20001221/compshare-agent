package workflow_eval

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type customImageCase struct {
	ID                  string         `json:"id"`
	Params              map[string]any `json:"params"`
	ExpectedActions     []string       `json:"expected_actions"`
	ForbiddenActions    []string       `json:"forbidden_actions"`
	RequireConfirmation bool           `json:"require_confirmation"`
	ExpectSuccess       bool           `json:"expect_success"`
}

type recordingExecutor struct {
	calls     []string
	args      map[string]map[string]any
	stopped   bool
	pollCount int
}

func (e *recordingExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	e.calls = append(e.calls, action)
	if e.args == nil {
		e.args = map[string]map[string]any{}
	}
	e.args[action] = args
	switch action {
	case "DescribeCompShareInstance":
		state := "Running"
		if e.stopped {
			e.pollCount++
			if e.pollCount == 1 {
				state = "Stopping"
			} else {
				state = "Stopped"
			}
		}
		return map[string]any{"UHostSet": []any{
			map[string]any{"UHostId": "uhost-src", "Name": "train-env", "State": state, "Region": "cn-sh2", "Zone": "cn-sh2-02"},
		}}, nil
	case "DescribeCompShareSupportZone":
		return map[string]any{"ZoneInfo": []any{map[string]any{
			"Zone": "cn-sh2-02", "Region": "cn-sh2",
			"ZoneId": float64(8200), "RegionId": float64(1000009),
		}}}, nil
	case "StopCompShareInstance":
		e.stopped = true
		return map[string]any{"RetCode": 0}, nil
	case "CreateCompShareCustomImage":
		return map[string]any{"CompShareImageId": "cimg-custom-001"}, nil
	case "GetCompShareImageCreateProgress":
		return map[string]any{"Process": float64(12.5)}, nil
	default:
		return map[string]any{"RetCode": 0}, nil
	}
}

func TestCustomImageWorkflowEvalCases(t *testing.T) {
	for _, c := range loadCustomImageCases(t) {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			exec := &recordingExecutor{}
			confirmCalls := 0
			eng := workflow.NewEngine(exec, func(action string, args map[string]any) bool {
				confirmCalls++
				assert.Equal(t, "CreateCustomImageWorkflow", action)
				assert.Equal(t, c.Params["Name"], args["Name"])
				return true
			}, nil)

			def := workflow.CreateCustomImageDef()
			for i := range def.Steps {
				if def.Steps[i].Name == "等待源实例关机" {
					def.Steps[i].Poll = &workflow.PollPolicy{Interval: time.Millisecond, Timeout: time.Second}
				}
			}
			result, err := eng.Run(context.Background(), def, c.Params)
			require.NoError(t, err)
			assert.Equal(t, c.ExpectSuccess, result.Success)
			if c.RequireConfirmation {
				assert.Equal(t, 1, confirmCalls)
			} else {
				assert.Zero(t, confirmCalls)
			}

			for _, action := range c.ExpectedActions {
				assert.Contains(t, exec.calls, action)
			}
			for _, action := range c.ForbiddenActions {
				assert.NotContains(t, exec.calls, action)
			}
			if createArgs, ok := exec.args["CreateCompShareCustomImage"]; ok {
				assert.Equal(t, "cn-sh2", createArgs["Region"])
				assert.Equal(t, "cn-sh2-02", createArgs["Zone"])
				assert.Equal(t, uint32(1000009), createArgs["az_group"])
				for _, key := range []string{"Softwares", "SoftwarePorts", "FirewallPorts"} {
					assert.NotContains(t, createArgs, key)
				}
			}
			if progressArgs, ok := exec.args["GetCompShareImageCreateProgress"]; ok {
				assert.Equal(t, "cn-sh2", progressArgs["Region"])
				assert.Equal(t, "cn-sh2-02", progressArgs["Zone"])
			}
		})
	}
}

func loadCustomImageCases(t *testing.T) []customImageCase {
	t.Helper()
	f, err := os.Open("custom_image_cases.jsonl")
	require.NoError(t, err)
	defer f.Close()

	var cases []customImageCase
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		var c customImageCase
		require.NoErrorf(t, json.Unmarshal([]byte(line), &c), "parse case: %s", line)
		require.NotEmpty(t, c.ID)
		cases = append(cases, c)
	}
	require.NoError(t, sc.Err())
	require.NotEmpty(t, cases)
	return cases
}
