package workflow

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/compshare-agent/internal/platform"
)

const (
	// These are protocol invariants of UpdateCompShareInstancePorts, not a
	// product-display mapping. The upstream handler unconditionally restores
	// them on every full-replacement request, so accepting a request to remove
	// either one would make the confirmation card promise an impossible result.
	upstreamRequiredHTTPPort = 8888
	upstreamRequiredTCPPort  = 23
	upstreamMaxPortsPerType  = 10
)

const (
	portPlanBeforeHTTP = "BeforeHttpPorts"
	portPlanBeforeTCP  = "BeforeTcpPorts"
	portPlanBeforeUDP  = "BeforeUdpPorts"
	portPlanAfterHTTP  = "AfterHttpPorts"
	portPlanAfterTCP   = "AfterTcpPorts"
	portPlanAfterUDP   = "AfterUdpPorts"
)

// UpdateInstancePortsDef exposes a delta-shaped operation over an upstream API
// whose wire contract is full replacement. It reads and preserves every current
// protocol list, presents the exact before/after set, re-reads after confirmation
// to detect concurrent edits, sends one full replacement, then verifies the
// observed result. UDP is deliberately preserve-only in the public proposal:
// the API records UDP service ports but returns no public UDP-forward endpoint,
// so this workflow must not imply that adding one makes WebRTC publicly reachable.
func UpdateInstancePortsDef() *Definition {
	return &Definition{
		Name:             "UpdateInstancePortsWorkflow",
		NeedsZoneCatalog: true,
		Steps: []Step{
			stepQueryInstanceForPorts("查询端口配置"),
			stepQuerySupportZones(),
			stepResolvePortPlan(),
			stepConfirmPortPlan(),
			stepQueryInstanceForPortPrecondition(),
			stepUpdateInstancePorts(),
			stepVerifyInstancePorts(),
		},
		ResultData: func(wfCtx *Context) map[string]any {
			return portWorkflowResult(wfCtx)
		},
	}
}

func stepQueryInstanceForPorts(name string) Step {
	return Step{
		Name: name,
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{"UHostIds": []any{wfCtx.Params["UHostId"]}}, nil
		},
		CheckResult: func(wfCtx *Context, result map[string]any) CheckOutcome {
			id := strings.TrimSpace(paramStr(wfCtx.Params, "UHostId", ""))
			if id == "" || !narrowInstanceResultToUHostID(result, id) {
				return CheckFailed("未找到该实例。")
			}
			if !platform.IsPodInstanceID(id) {
				return CheckFailed("平台端口映射仅适用于 Pod 实例；虚机端口暴露应通过其网络和防火墙配置管理。")
			}
			if _, err := instancePortSets(result); err != nil {
				return CheckFailed(err.Error())
			}
			return CheckPassed()
		},
	}
}

func stepResolvePortPlan() Step {
	return Step{
		Name: "计算端口变更",
		Type: StepResolve,
		Resolve: func(wfCtx *Context) (map[string]any, error) {
			before, err := instancePortSets(wfCtx.Result("查询端口配置"))
			if err != nil {
				return nil, err
			}
			addHTTP, err := proposalPortSet(wfCtx.Params["AddHttpPorts"], "AddHttpPorts")
			if err != nil {
				return nil, err
			}
			removeHTTP, err := proposalPortSet(wfCtx.Params["RemoveHttpPorts"], "RemoveHttpPorts")
			if err != nil {
				return nil, err
			}
			addTCP, err := proposalPortSet(wfCtx.Params["AddTcpPorts"], "AddTcpPorts")
			if err != nil {
				return nil, err
			}
			removeTCP, err := proposalPortSet(wfCtx.Params["RemoveTcpPorts"], "RemoveTcpPorts")
			if err != nil {
				return nil, err
			}
			if len(addHTTP)+len(removeHTTP)+len(addTCP)+len(removeTCP) == 0 {
				return nil, NewMissingSlotError("至少需要指定一个要添加或移除的 HTTP/TCP 端口。", "port_change")
			}
			if intersects(addHTTP, removeHTTP) || intersects(addTCP, removeTCP) {
				return nil, fmt.Errorf("同一个端口不能在同一协议中同时添加和移除。")
			}
			if containsPort(removeHTTP, upstreamRequiredHTTPPort) {
				return nil, fmt.Errorf("HTTP %d 是上游端口接口强制保留的基础入口，不能移除。", upstreamRequiredHTTPPort)
			}
			if containsPort(removeTCP, upstreamRequiredTCPPort) {
				return nil, fmt.Errorf("TCP %d 是上游端口接口强制保留的 SSH 入口，不能移除。", upstreamRequiredTCPPort)
			}

			after := portSets{
				HTTP: applyPortDelta(before.HTTP, addHTTP, removeHTTP),
				TCP:  applyPortDelta(before.TCP, addTCP, removeTCP),
				UDP:  append([]int(nil), before.UDP...),
			}
			if len(after.HTTP) > upstreamMaxPortsPerType || len(after.TCP) > upstreamMaxPortsPerType || len(after.UDP) > upstreamMaxPortsPerType {
				return nil, fmt.Errorf("每种协议最多保留 %d 个端口；当前变更会超过上游限制。", upstreamMaxPortsPerType)
			}
			if before.equal(after) {
				return nil, fmt.Errorf("当前端口配置已经满足请求，无需修改。")
			}
			return portPlanMap(before, after), nil
		},
	}
}

func stepConfirmPortPlan() Step {
	return Step{
		Name: "确认端口变更",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			plan := wfCtx.Result("计算端口变更")
			if plan == nil {
				return nil, fmt.Errorf("未生成可确认的端口变更。")
			}
			return map[string]any{
				"UHostId":      wfCtx.Params["UHostId"],
				"CurrentHTTP":  plan[portPlanBeforeHTTP],
				"TargetHTTP":   plan[portPlanAfterHTTP],
				"CurrentTCP":   plan[portPlanBeforeTCP],
				"TargetTCP":    plan[portPlanAfterTCP],
				"PreservedUDP": plan[portPlanAfterUDP],
				"Effect":       "平台将按确认后的完整端口集合替换配置；工作流会保留未修改协议和端口，并在执行前检查并发变更。",
			}, nil
		},
		PromoteOnConfirm: func(wfCtx *Context) error {
			plan := wfCtx.Result("计算端口变更")
			if plan == nil {
				return fmt.Errorf("未生成可执行的端口变更。")
			}
			for _, key := range []string{
				portPlanBeforeHTTP, portPlanBeforeTCP, portPlanBeforeUDP,
				portPlanAfterHTTP, portPlanAfterTCP, portPlanAfterUDP,
			} {
				wfCtx.Params[key] = deepCopyValue(plan[key])
			}
			return nil
		},
	}
}

func stepQueryInstanceForPortPrecondition() Step {
	step := stepQueryInstanceForPorts("执行前复核端口配置")
	baseCheck := step.CheckResult
	step.CheckResult = func(wfCtx *Context, result map[string]any) CheckOutcome {
		if outcome := baseCheck(wfCtx, result); !outcome.OK {
			return outcome
		}
		current, err := instancePortSets(result)
		if err != nil {
			return CheckFailed(err.Error())
		}
		before, err := promotedPortSets(wfCtx.Params, "Before")
		if err != nil {
			return CheckFailed(err.Error())
		}
		if !current.equal(before) {
			return CheckFailed("确认期间平台端口配置已发生变化，本次未覆盖新配置；请重新发起变更。")
		}
		return CheckPassed()
	}
	return step
}

func stepUpdateInstancePorts() Step {
	return Step{
		Name: "更新平台端口",
		Type: StepToolCall,
		Tool: "UpdateCompShareInstancePorts",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			after, err := promotedPortSets(wfCtx.Params, "After")
			if err != nil {
				return nil, err
			}
			args, err := addRequiredPodPlacementArgs(map[string]any{
				"UHostId":   wfCtx.Params["UHostId"],
				"HttpPorts": intsToAny(after.HTTP),
				"TcpPorts":  intsToAny(after.TCP),
				"UdpPorts":  intsToAny(after.UDP),
			}, wfCtx.Result("执行前复核端口配置"), wfCtx.Result("查询支持区"))
			if err != nil {
				return nil, err
			}
			if zoneID, ok := parseUint32Any(args["zone_id"]); !ok || zoneID == 0 {
				return nil, fmt.Errorf("未从实时可用区目录获取到 Pod 内部可用区编号，未执行端口变更。")
			}
			return args, nil
		},
	}
}

func stepVerifyInstancePorts() Step {
	return Step{
		Name: "验证端口配置",
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{"UHostIds": []any{wfCtx.Params["UHostId"]}}, nil
		},
		CheckResult: func(wfCtx *Context, result map[string]any) CheckOutcome {
			id := strings.TrimSpace(paramStr(wfCtx.Params, "UHostId", ""))
			if !narrowInstanceResultToUHostID(result, id) {
				return CheckFailed("更新后未能重新读取目标实例，无法验证端口配置。")
			}
			current, err := instancePortSets(result)
			if err != nil {
				return CheckFailed(err.Error())
			}
			after, err := promotedPortSets(wfCtx.Params, "After")
			if err != nil {
				return CheckFailed(err.Error())
			}
			if !current.equal(after) {
				return CheckPending("平台已接受更新，但实例详情尚未显示确认后的完整端口集合。")
			}
			return CheckPassed()
		},
		Poll: &PollPolicy{Interval: 2 * time.Second, Timeout: 20 * time.Second},
	}
}

type portSets struct {
	HTTP []int
	TCP  []int
	UDP  []int
}

func (p portSets) equal(other portSets) bool {
	return equalInts(p.HTTP, other.HTTP) && equalInts(p.TCP, other.TCP) && equalInts(p.UDP, other.UDP)
}

func instancePortSets(result map[string]any) (portSets, error) {
	host, ok := firstInstance(result)
	if !ok {
		return portSets{}, fmt.Errorf("未找到目标实例的端口事实。")
	}
	raw, ok := host["Ports"].(map[string]any)
	if !ok || raw == nil {
		return portSets{}, fmt.Errorf("实例详情没有返回完整端口集合；为避免全量替换时误删现有入口，本次未执行修改。")
	}
	var out portSets
	var err error
	if out.HTTP, err = platformPortSet(raw["HttpPorts"], "Ports.HttpPorts"); err != nil {
		return portSets{}, err
	}
	if out.TCP, err = platformPortSet(raw["TcpPorts"], "Ports.TcpPorts"); err != nil {
		return portSets{}, err
	}
	if out.UDP, err = platformPortSet(raw["UdpPorts"], "Ports.UdpPorts"); err != nil {
		return portSets{}, err
	}
	return out, nil
}

func proposalPortSet(raw any, field string) ([]int, error) {
	if raw == nil {
		return nil, nil
	}
	return portSetFromAny(raw, field, false)
}

// ValidatePortDeltaProposal is the action-resolver boundary for the public
// delta shape. Live-state checks (Pod kind, current full set, limits after the
// merge and protected defaults) remain in the workflow where those facts exist.
func ValidatePortDeltaProposal(args map[string]any) error {
	sets := map[string][]int{}
	total := 0
	for _, field := range []string{"AddHttpPorts", "RemoveHttpPorts", "AddTcpPorts", "RemoveTcpPorts"} {
		ports, err := proposalPortSet(args[field], field)
		if err != nil {
			return err
		}
		sets[field] = ports
		total += len(ports)
	}
	if total == 0 {
		return fmt.Errorf("at least one HTTP/TCP port change is required")
	}
	if intersects(sets["AddHttpPorts"], sets["RemoveHttpPorts"]) ||
		intersects(sets["AddTcpPorts"], sets["RemoveTcpPorts"]) {
		return fmt.Errorf("the same protocol port cannot be added and removed together")
	}
	return nil
}

func platformPortSet(raw any, field string) ([]int, error) {
	if raw == nil {
		return []int{}, nil
	}
	return portSetFromAny(raw, field, true)
}

func portSetFromAny(raw any, field string, allowTypedSlices bool) ([]int, error) {
	var items []any
	switch values := raw.(type) {
	case []any:
		items = values
	case []int:
		if !allowTypedSlices {
			return nil, fmt.Errorf("%s 必须是端口整数数组。", field)
		}
		items = intsToAny(values)
	case []int32:
		if !allowTypedSlices {
			return nil, fmt.Errorf("%s 必须是端口整数数组。", field)
		}
		for _, value := range values {
			items = append(items, value)
		}
	default:
		return nil, fmt.Errorf("%s 必须是端口整数数组。", field)
	}
	out := make([]int, 0, len(items))
	seen := map[int]bool{}
	for _, rawPort := range items {
		port, ok := exactPort(rawPort)
		if !ok {
			return nil, fmt.Errorf("%s 只能包含 1 到 65535 的端口整数。", field)
		}
		if !seen[port] {
			seen[port] = true
			out = append(out, port)
		}
	}
	sort.Ints(out)
	return out, nil
}

func exactPort(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, v >= 1 && v <= 65535
	case int32:
		return int(v), v >= 1 && v <= 65535
	case int64:
		return int(v), v >= 1 && v <= 65535
	case float64:
		port := int(v)
		return port, v == float64(port) && port >= 1 && port <= 65535
	default:
		return 0, false
	}
}

func portPlanMap(before, after portSets) map[string]any {
	return map[string]any{
		portPlanBeforeHTTP: intsToAny(before.HTTP),
		portPlanBeforeTCP:  intsToAny(before.TCP),
		portPlanBeforeUDP:  intsToAny(before.UDP),
		portPlanAfterHTTP:  intsToAny(after.HTTP),
		portPlanAfterTCP:   intsToAny(after.TCP),
		portPlanAfterUDP:   intsToAny(after.UDP),
	}
}

func promotedPortSets(params map[string]any, prefix string) (portSets, error) {
	var out portSets
	var err error
	if out.HTTP, err = platformPortSet(params[prefix+"HttpPorts"], prefix+"HttpPorts"); err != nil {
		return portSets{}, err
	}
	if out.TCP, err = platformPortSet(params[prefix+"TcpPorts"], prefix+"TcpPorts"); err != nil {
		return portSets{}, err
	}
	if out.UDP, err = platformPortSet(params[prefix+"UdpPorts"], prefix+"UdpPorts"); err != nil {
		return portSets{}, err
	}
	return out, nil
}

func applyPortDelta(current, add, remove []int) []int {
	set := map[int]bool{}
	for _, port := range current {
		set[port] = true
	}
	for _, port := range remove {
		delete(set, port)
	}
	for _, port := range add {
		set[port] = true
	}
	out := make([]int, 0, len(set))
	for port := range set {
		out = append(out, port)
	}
	sort.Ints(out)
	return out
}

func intersects(left, right []int) bool {
	for _, port := range left {
		if containsPort(right, port) {
			return true
		}
	}
	return false
}

func containsPort(ports []int, want int) bool {
	for _, port := range ports {
		if port == want {
			return true
		}
	}
	return false
}

func equalInts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func intsToAny(values []int) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func portWorkflowResult(wfCtx *Context) map[string]any {
	after, err := promotedPortSets(wfCtx.Params, "After")
	if err != nil {
		return nil
	}
	out := map[string]any{
		"UHostId": wfCtx.Params["UHostId"],
		"Ports": map[string]any{
			"HttpPorts": intsToAny(after.HTTP),
			"TcpPorts":  intsToAny(after.TCP),
			"UdpPorts":  intsToAny(after.UDP),
		},
	}
	updated := wfCtx.Result("更新平台端口")
	for _, key := range []string{"AddedServicePorts", "RemovedServicePorts", "AddedIngresses", "RemovedIngresses", "IngressHosts", "TcpForwards"} {
		if value, ok := updated[key]; ok {
			out[key] = deepCopyValue(value)
		}
	}
	return out
}
