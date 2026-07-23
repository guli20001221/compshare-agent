package capability

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/diagnosis"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
)

const (
	instanceAccessCapabilityLabel = "instance_access"
	instanceAccessDescribeAction  = "DescribeCompShareInstance"
	instanceAccessPortAction      = "DescribeCompShareSoftwarePort"

	accessTypeSSH        = "ssh"
	accessTypeJupyter    = "jupyter"
	accessTypeCustomPort = "custom_port"

	accessProtocolHTTP = "http"
	accessProtocolTCP  = "tcp"
	accessProtocolUDP  = "udp"

	jupyterSoftwareName = "JupyterLab"
)

// InstanceAccessRequest is one structured cloud-side access precheck. It does
// not accept a URL, password, token or free-form failure text.
type InstanceAccessRequest struct {
	Targets     []platform.TargetRef `json:"targets,omitempty"`
	AccessType  string               `json:"access_type,omitempty"`
	Protocol    string               `json:"protocol,omitempty"`
	Port        int                  `json:"port,omitempty"`
	FailureKind string               `json:"failure_kind,omitempty"`
}

func (r InstanceAccessRequest) MissingFields() []platform.MissingField {
	missing := make([]platform.MissingField, 0, 4)
	if len(r.Targets) == 0 {
		missing = append(missing, platform.Missing("targets"))
	}
	if strings.TrimSpace(r.AccessType) == "" {
		missing = append(missing, platform.Missing("access_type"))
	}
	if r.AccessType == accessTypeCustomPort {
		if strings.TrimSpace(r.Protocol) == "" {
			missing = append(missing, platform.Missing("protocol"))
		}
		if r.Port == 0 {
			missing = append(missing, platform.Missing("port"))
		}
	}
	return missing
}

type InstanceAccessResponse struct {
	InstanceID    string
	InstanceName  string
	State         string
	InstanceKind  string
	AccessType    string
	Protocol      string
	Port          int
	Status        string
	Reason        string
	KnownSoftware string
	SourceActions []string
}

func instanceAccessReadSpec() ReadCapabilitySpec[InstanceAccessRequest, InstanceAccessResponse] {
	return ReadCapabilitySpec[InstanceAccessRequest, InstanceAccessResponse]{
		Label: instanceAccessCapabilityLabel,
		Description: "检查已有实例的 SSH、Jupyter 或自定义端口的云侧配置。它会核实实例状态、平台登记的应用入口和 Pod 端口映射；" +
			"不会连接公网端口、不会进入实例、不会读取或展示令牌，也不会修改防火墙或端口。需要明确一个实例；自定义端口还要给出协议和端口号。",
		Params: objectParam(map[string]schemaNode{
			"targets":     targetRefsParam(),
			"access_type": enumParam(accessTypeSSH, accessTypeJupyter, accessTypeCustomPort),
			"protocol":    enumParam(accessProtocolHTTP, accessProtocolTCP, accessProtocolUDP),
			"port":        boundedIntegerParam(1, 65535),
			"failure_kind": enumParam("timeout", "connection_refused", "authentication_failed", "connection_dropped", "unknown").
				described("仅 SSH 使用；只按用户实际报告的错误选择，无法确定时用 unknown。"),
		}, "targets", "access_type"),
		Handle: instanceAccessHandle,
		Render: instanceAccessRender,
	}
}

func instanceAccessHandle(ctx context.Context, req InstanceAccessRequest, rt ReadRuntime) (InstanceAccessResponse, ReadResult) {
	_, ids, reason := resolveReadTargetSnapshots(req.Targets, rt.Resolver, true, rt.Now)
	if reason != nil {
		return InstanceAccessResponse{}, readTargetFallbackResult(*reason)
	}
	if len(ids) != 1 {
		if len(ids) > 1 {
			return InstanceAccessResponse{}, ReadConflict("访问诊断一次只能检查一个实例，请指定一个实例 ID。")
		}
		return InstanceAccessResponse{}, ReadFallbackBeforeTool(platform.ReadFallbackMissingTarget)
	}

	raw, err := rt.Executor.Execute(ctx, instanceAccessDescribeAction, map[string]any{"UHostIds": []string{ids[0]}})
	if err != nil {
		return InstanceAccessResponse{}, ReadFailureAfterTool(instanceAccessDescribeAction, instanceAccessCapabilityLabel, err)
	}
	host, ok := instanceAccessHostForID(raw, ids[0])
	if !ok {
		return InstanceAccessResponse{}, ReadEmpty("查询成功，但返回结果中没有找到指定实例；请确认实例 ID 和当前账号。")
	}

	resp := InstanceAccessResponse{
		InstanceID:    ids[0],
		InstanceName:  stringField(host, "Name"),
		State:         stringField(host, "State"),
		InstanceKind:  instanceAccessKind(host),
		AccessType:    req.AccessType,
		Protocol:      req.Protocol,
		Port:          req.Port,
		SourceActions: []string{instanceAccessDescribeAction},
	}

	if req.AccessType == accessTypeSSH {
		diag, diagErr := diagnosis.NewEngine(rt.Executor, nil).Run(
			ctx,
			diagnosis.SSHFailureChainWithDescribeResult(raw),
			map[string]any{"UHostId": ids[0], "FailureKind": req.FailureKind},
		)
		if diagErr != nil {
			return InstanceAccessResponse{}, ReadFailureAfterTool("GetCompShareInstanceMonitor", instanceAccessCapabilityLabel, diagErr)
		}
		resp.Status = string(diag.PrecheckStatus)
		if resp.Status == "" {
			resp.Status = "unknown"
		}
		resp.Reason = strings.TrimSpace(strings.Join([]string{diag.Conclusion, diag.Suggestion}, " "))
		if len(diag.Steps) > 1 {
			resp.SourceActions = append(resp.SourceActions, "GetCompShareInstanceMonitor")
		}
		return resp, ReadResult{}
	}

	if !strings.EqualFold(resp.State, "Running") {
		resp.Status = "blocked"
		resp.Reason = "实例当前不是运行状态，云侧访问入口不可用。"
		return resp, ReadResult{}
	}

	switch req.AccessType {
	case accessTypeJupyter:
		portRaw, callErr := rt.Executor.Execute(ctx, instanceAccessPortAction, map[string]any{})
		if callErr != nil {
			return InstanceAccessResponse{}, ReadFailureAfterTool(instanceAccessPortAction, instanceAccessCapabilityLabel, callErr)
		}
		resp.SourceActions = append(resp.SourceActions, instanceAccessPortAction)
		resp.Port, resp.KnownSoftware = jupyterCatalogPort(portRaw)
		resp.Status, resp.Reason = evaluateJupyterAccess(host, resp.InstanceKind, resp.Port)
	case accessTypeCustomPort:
		resp.Status, resp.Reason = evaluateCustomPortAccess(host, resp.InstanceKind, req.Protocol, req.Port)
	default:
		return InstanceAccessResponse{}, ReadFallbackBeforeTool(platform.ReadFallbackValidation)
	}
	return resp, ReadResult{}
}

func instanceAccessRender(resp InstanceAccessResponse) ReadResult {
	subject := resp.InstanceID
	if resp.InstanceName != "" {
		subject += "（" + resp.InstanceName + "）"
	}
	var verdict string
	switch resp.Status {
	case "configured":
		verdict = "云侧配置已登记"
	case "blocked":
		verdict = "云侧存在明确阻断"
	default:
		verdict = "云侧信息不足，无法确认"
	}
	detail := resp.Reason
	if resp.KnownSoftware != "" {
		detail += " 平台端口目录将该端口标记为 " + resp.KnownSoftware + "。"
	}
	reply := fmt.Sprintf("%s 的%s访问预检：%s。%s %s",
		subject, accessTypeDisplay(resp.AccessType), verdict, strings.TrimSpace(detail),
		"该结果没有实际连接公网端口，也没有进入实例检查服务进程、系统防火墙或认证日志。")

	facts := []envelope.Fact{
		{SubjectID: resp.InstanceID, Key: "state", Label: "实例状态", Value: resp.State, Source: envelope.FactSourceAPI},
		{SubjectID: resp.InstanceID, Key: "instance_kind", Label: "实例类型", Value: resp.InstanceKind, Source: envelope.FactSourceAPI},
		{SubjectID: resp.InstanceID, Key: "access_type", Label: "访问类型", Value: resp.AccessType, Source: envelope.FactSourceComputed},
		{SubjectID: resp.InstanceID, Key: "cloud_precheck_status", Label: "云侧预检状态", Value: resp.Status, Source: envelope.FactSourceComputed},
	}
	if resp.Port > 0 {
		facts = append(facts, envelope.Fact{
			SubjectID: resp.InstanceID, Key: "port", Label: "端口", Value: resp.Port, Source: envelope.FactSourceAPI,
		})
	}
	if resp.Protocol != "" {
		facts = append(facts, envelope.Fact{
			SubjectID: resp.InstanceID, Key: "protocol", Label: "协议", Value: resp.Protocol, Source: envelope.FactSourceComputed,
		})
	}
	if resp.KnownSoftware != "" {
		facts = append(facts, envelope.Fact{
			SubjectID: resp.InstanceID, Key: "catalog_software", Label: "平台端口目录应用", Value: resp.KnownSoftware, Source: envelope.FactSourceAPI,
		})
	}

	result := ReadHandled(strings.TrimSpace(reply))
	result.ToolAction = instanceAccessDescribeAction
	result.Envelope = &envelope.Envelope{
		Kind:          envelope.KindInstanceAccess,
		SourceActions: append([]string(nil), resp.SourceActions...),
		Subjects: []envelope.Subject{{
			ID: resp.InstanceID, Name: resp.InstanceName, Type: envelope.SubjectInstance,
		}},
		Facts: facts,
		Constraints: envelope.Constraints{
			DoNotInventInstances: true,
		},
	}
	return result
}

func instanceAccessHostForID(raw map[string]any, id string) (map[string]any, bool) {
	for _, item := range mapSliceAt(raw, "UHostSet") {
		host, _ := item.(map[string]any)
		if strings.TrimSpace(stringField(host, "UHostId")) == id {
			return host, true
		}
	}
	return nil, false
}

func instanceAccessKind(host map[string]any) string {
	if strings.EqualFold(stringField(host, "InstanceType"), "Container") {
		return "pod"
	}
	return "vm"
}

func evaluateJupyterAccess(host map[string]any, kind string, catalogPort int) (string, string) {
	softwareFound, urlPresent := hostSoftware(host, jupyterSoftwareName)
	if kind == "pod" {
		if catalogPort == 0 {
			return "unknown", "平台端口目录没有返回 Jupyter 端口定义。"
		}
		if !podPortPresent(host, accessProtocolHTTP, catalogPort) {
			return "blocked", fmt.Sprintf("Pod 当前端口配置中没有登记 Jupyter 的 HTTP %d 端口。", catalogPort)
		}
		if softwareFound && urlPresent {
			return "configured", fmt.Sprintf("Pod 已登记 HTTP %d 端口，实例详情也返回了 Jupyter 入口。", catalogPort)
		}
		return "configured", fmt.Sprintf("Pod 已登记 HTTP %d 端口；实例详情没有返回可核验的 Jupyter 入口，服务进程仍需在实例内确认。", catalogPort)
	}
	if softwareFound && urlPresent {
		return "configured", "实例详情返回了 Jupyter 入口。"
	}
	if softwareFound {
		return "unknown", "实例详情标记了 Jupyter，但没有返回可核验的入口。"
	}
	return "unknown", "实例详情没有返回 Jupyter 应用入口；这不能证明实例内一定没有自行安装 Jupyter。"
}

func evaluateCustomPortAccess(host map[string]any, kind, protocol string, port int) (string, string) {
	if kind != "pod" {
		return "unknown", "虚机实例详情不返回实例内监听端口或系统防火墙状态。"
	}
	if !podPortPresent(host, protocol, port) {
		return "blocked", fmt.Sprintf("Pod 当前云侧端口配置中没有登记 %s %d。", strings.ToUpper(protocol), port)
	}
	if protocol == accessProtocolTCP && !podTCPForwardPresent(host, port) {
		return "blocked", fmt.Sprintf("Pod 已登记 TCP %d，但没有找到对应的外部 TCP 转发。", port)
	}
	return "configured", fmt.Sprintf("Pod 当前云侧端口配置已登记 %s %d。", strings.ToUpper(protocol), port)
}

func podPortPresent(host map[string]any, protocol string, port int) bool {
	ports, _ := host["Ports"].(map[string]any)
	if ports == nil {
		return false
	}
	key := map[string]string{
		accessProtocolHTTP: "HttpPorts",
		accessProtocolTCP:  "TcpPorts",
		accessProtocolUDP:  "UdpPorts",
	}[protocol]
	for _, raw := range mapSliceAt(ports, key) {
		if value, ok := numericValue(raw); ok && int(value) == port && value == float64(port) {
			return true
		}
	}
	return false
}

func podTCPForwardPresent(host map[string]any, internalPort int) bool {
	for _, raw := range mapSliceAt(host, "TcpForwards") {
		forward, _ := raw.(map[string]any)
		value, ok := numericField(forward, "InternalPort")
		if ok && int(value) == internalPort && value == float64(internalPort) {
			external, externalOK := numericField(forward, "ExternalPort")
			return externalOK && external >= 1 && external <= 65535
		}
	}
	return false
}

func hostSoftware(host map[string]any, contains string) (bool, bool) {
	for _, raw := range mapSliceAt(host, "Softwares") {
		entry, _ := raw.(map[string]any)
		if strings.EqualFold(stringField(entry, "Name"), contains) {
			return true, strings.TrimSpace(stringField(entry, "URL")) != ""
		}
	}
	return false, false
}

func jupyterCatalogPort(raw map[string]any) (int, string) {
	for _, item := range mapSliceAt(raw, "SoftwarePort") {
		entry, _ := item.(map[string]any)
		name := stringField(entry, "Software")
		if !strings.EqualFold(name, jupyterSoftwareName) {
			continue
		}
		port, ok := numericField(entry, "Port")
		if ok && port >= 1 && port <= 65535 && port == float64(int(port)) {
			return int(port), name
		}
	}
	return 0, ""
}

func accessTypeDisplay(value string) string {
	switch value {
	case accessTypeSSH:
		return " SSH "
	case accessTypeJupyter:
		return " Jupyter "
	case accessTypeCustomPort:
		return "自定义端口"
	default:
		return "实例"
	}
}
