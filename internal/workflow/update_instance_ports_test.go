package workflow

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type portWorkflowExecutor struct {
	id      string
	zone    string
	region  string
	ports   portSets
	calls   []executorCall
	updates int
}

func newPortWorkflowExecutor() *portWorkflowExecutor {
	return &portWorkflowExecutor{
		id: "cpod-test", zone: "cn-sh2-01", region: "cn-sh2",
		ports: portSets{HTTP: []int{8888}, TCP: []int{23, 6006}, UDP: []int{3478}},
	}
}

func (e *portWorkflowExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	e.calls = append(e.calls, executorCall{action: action, args: deepCopyParams(args)})
	switch action {
	case "DescribeCompShareInstance":
		return map[string]any{"UHostSet": []any{map[string]any{
			"UHostId": e.id, "Name": "ports-test", "State": "Running",
			"Zone": e.zone, "Region": e.region, "InstanceType": "Container",
			"Ports": map[string]any{
				"HttpPorts": intsToAny(e.ports.HTTP),
				"TcpPorts":  intsToAny(e.ports.TCP),
				"UdpPorts":  intsToAny(e.ports.UDP),
			},
		}}}, nil
	case "DescribeCompShareSupportZone":
		return map[string]any{"ZoneInfo": []any{map[string]any{
			"Zone": e.zone, "Region": e.region, "ZoneId": float64(8201), "RegionId": float64(3002),
		}}}, nil
	case "UpdateCompShareInstancePorts":
		var err error
		if e.ports.HTTP, err = platformPortSet(args["HttpPorts"], "HttpPorts"); err != nil {
			return nil, err
		}
		if e.ports.TCP, err = platformPortSet(args["TcpPorts"], "TcpPorts"); err != nil {
			return nil, err
		}
		if e.ports.UDP, err = platformPortSet(args["UdpPorts"], "UdpPorts"); err != nil {
			return nil, err
		}
		e.updates++
		return map[string]any{
			"UHostId": e.id, "AddedIngresses": []any{"5173"},
			"IngressHosts": []any{"5173-cpod-test-s1.pod.compshare.cn"},
		}, nil
	default:
		return nil, fmt.Errorf("unexpected action %s", action)
	}
}

func TestUpdateInstancePortsPreservesFullSetsAndVerifies(t *testing.T) {
	executor := newPortWorkflowExecutor()
	var card map[string]any
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		assert.Equal(t, "UpdateInstancePortsWorkflow", action)
		card = deepCopyParams(args)
		return true
	}, nil)

	result, err := eng.Run(context.Background(), UpdateInstancePortsDef(), map[string]any{
		"UHostId": "cpod-test", "AddHttpPorts": []any{float64(5173)},
	})
	require.NoError(t, err)
	require.True(t, result.Success, result.Message)
	require.Equal(t, 1, executor.updates)
	require.Equal(t, []int{5173, 8888}, executor.ports.HTTP)
	require.Equal(t, []int{23, 6006}, executor.ports.TCP)
	require.Equal(t, []int{3478}, executor.ports.UDP)
	assert.Equal(t, []any{8888}, card["CurrentHTTP"])
	assert.Equal(t, []any{5173, 8888}, card["TargetHTTP"])
	assert.Equal(t, []any{23, 6006}, card["TargetTCP"])
	assert.Equal(t, []any{3478}, card["PreservedUDP"])

	update, ok := findExecutorCall(executor.calls, "UpdateCompShareInstancePorts")
	require.True(t, ok)
	assert.Equal(t, uint32(8201), update.args["zone_id"])
	assert.Equal(t, "cn-sh2-01", update.args["Zone"])
	assert.Equal(t, "cn-sh2", update.args["Region"])
	assert.Equal(t, []any{5173, 8888}, update.args["HttpPorts"])
	assert.Equal(t, []any{23, 6006}, update.args["TcpPorts"])
	assert.Equal(t, []any{3478}, update.args["UdpPorts"])
	require.Equal(t, []any{"5173-cpod-test-s1.pod.compshare.cn"}, result.Data["IngressHosts"])
	assert.Equal(t, []any{5173, 8888}, result.Data["Ports"].(map[string]any)["HttpPorts"])
}

func TestUpdateInstancePortsDeclineDoesNotWrite(t *testing.T) {
	executor := newPortWorkflowExecutor()
	result, err := NewEngine(executor, func(string, map[string]any) bool { return false }, nil).Run(
		context.Background(), UpdateInstancePortsDef(), map[string]any{
			"UHostId": "cpod-test", "AddHttpPorts": []any{float64(5173)},
		})
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Zero(t, executor.updates)
	_, called := findExecutorCall(executor.calls, "UpdateCompShareInstancePorts")
	assert.False(t, called)
}

func TestUpdateInstancePortsConcurrentChangeFailsBeforeReplacement(t *testing.T) {
	executor := newPortWorkflowExecutor()
	confirm := func(string, map[string]any) bool {
		executor.ports.HTTP = []int{7777, 8888}
		return true
	}
	result, err := NewEngine(executor, confirm, nil).Run(context.Background(), UpdateInstancePortsDef(), map[string]any{
		"UHostId": "cpod-test", "AddHttpPorts": []any{float64(5173)},
	})
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "确认期间")
	assert.Zero(t, executor.updates)
	assert.Equal(t, []int{7777, 8888}, executor.ports.HTTP)
}

func TestUpdateInstancePortsRejectsVMAndMissingFullSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result map[string]any
		want   string
	}{
		{
			name: "vm", want: "仅适用于 Pod",
			result: map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-test", "Zone": "cn-wlcb-01", "Region": "cn-wlcb", "Ports": map[string]any{},
			}}},
		},
		{
			name: "pod missing full ports", want: "避免全量替换时误删",
			result: map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "cpod-test", "Zone": "cn-sh2-01", "Region": "cn-sh2",
			}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := &mockExecutor{results: map[string]map[string]any{"DescribeCompShareInstance": tc.result}}
			id := "cpod-test"
			if tc.name == "vm" {
				id = "uhost-test"
			}
			result, err := NewEngine(executor, func(string, map[string]any) bool {
				t.Fatal("unsafe target must stop before confirmation")
				return true
			}, nil).Run(context.Background(), UpdateInstancePortsDef(), map[string]any{
				"UHostId": id, "AddHttpPorts": []any{float64(5173)},
			})
			require.NoError(t, err)
			assert.False(t, result.Success)
			assert.Contains(t, result.Message, tc.want)
		})
	}
}

func TestUpdateInstancePortsRejectsImpossibleOrUnsafeDeltas(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"same add remove", map[string]any{"AddHttpPorts": []any{5173}, "RemoveHttpPorts": []any{5173}}, "同时添加和移除"},
		{"protected HTTP", map[string]any{"RemoveHttpPorts": []any{8888}}, "强制保留"},
		{"protected TCP", map[string]any{"RemoveTcpPorts": []any{23}}, "强制保留"},
		{"invalid port", map[string]any{"AddHttpPorts": []any{70000}}, "1 到 65535"},
		{"empty", map[string]any{}, "至少需要指定"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor := newPortWorkflowExecutor()
			args := map[string]any{"UHostId": "cpod-test"}
			for key, value := range tc.args {
				args[key] = value
			}
			result, err := NewEngine(executor, func(string, map[string]any) bool {
				t.Fatal("invalid delta must stop before confirmation")
				return true
			}, nil).Run(context.Background(), UpdateInstancePortsDef(), args)
			require.NoError(t, err)
			assert.False(t, result.Success)
			assert.Contains(t, result.Message, tc.want)
			assert.Zero(t, executor.updates)
		})
	}
}

func TestUpdateInstancePortsLimitCountsPreservedPorts(t *testing.T) {
	executor := newPortWorkflowExecutor()
	executor.ports.HTTP = []int{1001, 1002, 1003, 1004, 1005, 1006, 1007, 1008, 1009, 8888}
	result, err := NewEngine(executor, func(string, map[string]any) bool {
		t.Fatal("over-limit replacement must stop before confirmation")
		return true
	}, nil).Run(context.Background(), UpdateInstancePortsDef(), map[string]any{
		"UHostId": "cpod-test", "AddHttpPorts": []any{5173},
	})
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "最多保留 10")
	assert.Zero(t, executor.updates)
}
