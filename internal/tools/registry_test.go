package tools

import (
	"slices"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/security"
)

func TestCreatePathToolsAllowRegion(t *testing.T) {
	// The create-path read tools must declare Region (→ AllowedParams) so
	// SafeToolExecutor.filterSafeArgs keeps the Region the create workflow pairs
	// with a non-default Zone. Without it, a cn-bj2-03 / cn-sh2-02 create 230s on
	// "Params [Zone] not available" (live-verified 2026-06-16; resolves PR-β1).
	policies := DefaultToolExecutionPolicies()
	for _, action := range []string{
		"DescribeAvailableCompShareInstanceTypes",
		"CheckCompShareResourceCapacity",
		"GetCompShareInstanceUserPrice",
	} {
		p, ok := policies[action]
		if !ok {
			t.Fatalf("%s should have a policy", action)
		}
		found := false
		for _, param := range p.AllowedParams {
			if param == "Region" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s must allow Region (so filterSafeArgs keeps it for non-default zones)", action)
		}
	}
}

func TestWorkflowSchemasDeclareRequiredSizesForResolver(t *testing.T) {
	for _, action := range []string{"CreateDiskWorkflow", "ResizeDiskWorkflow"} {
		var required []string
		for _, tool := range Registry {
			if tool.Function == nil || tool.Function.Name != action {
				continue
			}
			params, _ := tool.Function.Parameters.(map[string]any)
			required, _ = params["required"].([]string)
			break
		}
		if len(required) == 0 {
			t.Fatalf("%s schema not found", action)
		}
		if !containsString(required, "UHostId") {
			t.Fatalf("%s must still require UHostId", action)
		}
		if !containsString(required, "Size") {
			t.Fatalf("%s must require Size so the Resolver returns a structured missing slot before Workflow", action)
		}
	}
}

func TestDescribeCommunityImagesAllowsPopularSortCondition(t *testing.T) {
	policies := DefaultToolExecutionPolicies()
	p, ok := policies["DescribeCommunityImages"]
	if !ok {
		t.Fatal("DescribeCommunityImages should have a policy")
	}
	if !containsString(p.AllowedParams, "SortCondition") {
		t.Fatal("DescribeCommunityImages must allow SortCondition so workflows can request popular community images")
	}
}

func TestCreateImageIDContractAllowsRecentExactConversationCandidate(t *testing.T) {
	var idDescription, nameDescription string
	for _, tool := range Registry {
		if tool.Function == nil || tool.Function.Name != "CreateInstanceWorkflow" {
			continue
		}
		params, _ := tool.Function.Parameters.(map[string]any)
		properties, _ := params["properties"].(map[string]any)
		imageID, _ := properties["CompShareImageId"].(map[string]any)
		idDescription, _ = imageID["description"].(string)
		imageName, _ := properties["ImageName"].(map[string]any)
		nameDescription, _ = imageName["description"].(string)
		break
	}
	if idDescription == "" || nameDescription == "" {
		t.Fatal("CreateInstanceWorkflow.CompShareImageId schema not found")
	}
	if strings.Contains(idDescription, "只能填本轮") {
		t.Fatal("镜像 ID 不能再被限制为本轮查询结果")
	}
	for _, required := range []string{"近期已提供的对话历史", "实时核验", "ImageSource"} {
		if !strings.Contains(idDescription, required) {
			t.Fatalf("镜像 ID 说明必须包含 %q，实际为 %q", required, idDescription)
		}
	}
	for _, required := range []string{"复述或简称", "不同镜像"} {
		if !strings.Contains(idDescription, required) {
			t.Fatalf("镜像 ID 说明必须包含 %q，实际为 %q", required, idDescription)
		}
	}
	if strings.Contains(idDescription, "必须同时填写 ImageName") {
		t.Fatalf("精确历史 ID 的复述不应再强迫模型同时填写 ImageName，实际为 %q", idDescription)
	}
	if required := "不必同时填写本字段"; !strings.Contains(nameDescription, required) {
		t.Fatalf("镜像名称说明必须包含 %q，实际为 %q", required, nameDescription)
	}
}

func TestCreateInstanceImageSourceIncludesCustom(t *testing.T) {
	for _, tool := range Registry {
		if tool.Function == nil || tool.Function.Name != "CreateInstanceWorkflow" {
			continue
		}
		params, _ := tool.Function.Parameters.(map[string]any)
		properties, _ := params["properties"].(map[string]any)
		source, _ := properties["ImageSource"].(map[string]any)
		enum, _ := source["enum"].([]string)
		if !containsString(enum, "custom") {
			t.Fatalf("CreateInstanceWorkflow.ImageSource must include custom, got %v", enum)
		}
		return
	}
	t.Fatal("CreateInstanceWorkflow.ImageSource schema not found")
}

func TestCreatePathToolsAllowBackendZoneID(t *testing.T) {
	policies := DefaultToolExecutionPolicies()
	for _, action := range []string{
		"DescribeAvailableCompShareInstanceTypes",
		"CheckCompShareResourceCapacity",
		"GetCompShareInstancePrice",
		"GetCompShareInstanceUserPrice",
	} {
		p, ok := policies[action]
		if !ok {
			t.Fatalf("%s should have a policy", action)
		}
		found := false
		for _, param := range p.InternalAllowedParams {
			if param == "zone_id" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s must allow backend-derived zone_id only for internal workflow calls", action)
		}
		for _, param := range p.AllowedParams {
			if param == "zone_id" {
				t.Errorf("%s must not expose zone_id to model-origin calls", action)
			}
		}
	}
}

func TestResizeUpgradePriceAllowsSourcePlacementStrings(t *testing.T) {
	policies := DefaultToolExecutionPolicies()
	p, ok := policies["GetCompShareInstanceUpgradePrice"]
	if !ok {
		t.Fatal("GetCompShareInstanceUpgradePrice should have a policy")
	}

	for _, want := range []string{"Zone", "Region"} {
		found := false
		for _, param := range p.AllowedParams {
			if param == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("GetCompShareInstanceUpgradePrice must allow %s so workflow can keep source instance placement", want)
		}
	}
	for _, forbidden := range []string{"zone_id", "az_group"} {
		for _, param := range p.AllowedParams {
			if param == forbidden {
				t.Fatalf("GetCompShareInstanceUpgradePrice must not expose %s to model-origin calls", forbidden)
			}
		}
	}
}

func TestFirstBatchCapabilityToolsAreRegisteredWithSafeBoundaries(t *testing.T) {
	descriptions := registryDescriptions()
	for _, action := range []string{
		"GetCompShareRefundPrice",
		"DescribeCFS",
		"GetCompShareCFSPrice",
		"GetCompShareCFSUpgradePrice",
		"GetCompShareCFSRefundPrice",
		"EnableNetOptimizerWorkflow",
		"CreateCFSWorkflow",
		"ResizeCFSWorkflow",
	} {
		if _, ok := descriptions[action]; !ok {
			t.Fatalf("%s must be registered for the first upstream capability batch", action)
		}
	}
	if _, ok := descriptions["DescribeCompShareJupyterToken"]; ok {
		t.Fatal("raw Jupyter token API must stay internal; ReadCapability_instance_access owns explicit token retrieval")
	}
	policies := DefaultToolExecutionPolicies()
	if _, ok := policies["DescribeCompShareJupyterToken"]; !ok {
		t.Fatal("internal Jupyter token action still needs a safe execution policy")
	}
	if !slices.Contains(policies["DescribeCompShareJupyterToken"].AllowedParams, "UHostIds") {
		t.Fatal("internal Jupyter token action must accept the verified instance id")
	}
	for _, action := range []string{"EnableNetOptimizerWorkflow", "CreateCFSWorkflow", "ResizeCFSWorkflow"} {
		if !policies[action].NeedsConfirm {
			t.Fatalf("%s must require confirmation in the runtime policy", action)
		}
	}
	if strings.Contains(descriptions["CreateCFSWorkflow"], "cn-pod-01") {
		t.Fatal("CreateCFSWorkflow description must not suggest synthetic Pod zones; real zones come from DescribeCompShareSupportZone")
	}
	if _, ok := descriptions["DeleteCFS"]; ok {
		t.Fatal("DeleteCFS must not be exposed as a user-facing tool")
	}
	if _, ok := descriptions["DetachCFS"]; ok {
		t.Fatal("DetachCFS must not be exposed as a user-facing tool")
	}
}

func TestUpdateInstancePortsWorkflowExposesDeltasNotFullReplacement(t *testing.T) {
	props := registryToolProperties(t, "UpdateInstancePortsWorkflow")
	for _, field := range []string{"UHostId", "AddHttpPorts", "RemoveHttpPorts", "AddTcpPorts", "RemoveTcpPorts"} {
		if _, ok := props[field]; !ok {
			t.Fatalf("UpdateInstancePortsWorkflow must expose %s", field)
		}
	}
	for _, forbidden := range []string{"HttpPorts", "TcpPorts", "UdpPorts", "AddUdpPorts", "RemoveUdpPorts", "zone_id", "Zone", "Region"} {
		if _, ok := props[forbidden]; ok {
			t.Fatalf("UpdateInstancePortsWorkflow must not expose raw/internal field %s", forbidden)
		}
	}
	description := registryDescriptions()["UpdateInstancePortsWorkflow"]
	for _, want := range []string{"全量替换", "保留", "UDP", "不能用它承诺"} {
		mustContain(t, description, want)
	}

	policies := DefaultToolExecutionPolicies()
	raw, ok := policies["UpdateCompShareInstancePorts"]
	if !ok {
		t.Fatal("raw UpdateCompShareInstancePorts requires an internal execution policy")
	}
	if raw.SecurityLevel != security.L1 || !raw.NeedsConfirm {
		t.Fatalf("raw port update must stay L1, got %+v", raw)
	}
	for _, forbidden := range []string{"zone_id", "az_group"} {
		if containsString(raw.AllowedParams, forbidden) {
			t.Fatalf("model-origin arguments must not contain %s", forbidden)
		}
	}
	if !containsString(raw.InternalAllowedParams, "zone_id") {
		t.Fatal("workflow-derived zone_id must reach the raw port API")
	}
}

func registryToolProperties(t *testing.T, name string) map[string]any {
	t.Helper()
	for _, tool := range Registry {
		if tool.Function == nil || tool.Function.Name != name {
			continue
		}
		params, _ := tool.Function.Parameters.(map[string]any)
		props, _ := params["properties"].(map[string]any)
		return props
	}
	t.Fatalf("tool schema %s not found", name)
	return nil
}

func TestAlignedToolSchemasMatchCurrentUpstreamContracts(t *testing.T) {
	byName := make(map[string]map[string]any)
	for _, tool := range Registry {
		if tool.Function == nil {
			continue
		}
		params, _ := tool.Function.Parameters.(map[string]any)
		byName[tool.Function.Name] = params
	}
	properties := func(name string) map[string]any {
		t.Helper()
		params, ok := byName[name]
		if !ok {
			t.Fatalf("tool schema %s not found", name)
		}
		props, _ := params["properties"].(map[string]any)
		return props
	}

	model := properties("DescribeModelRepositoryModels")
	for _, key := range []string{"Keyword", "Source", "Tags", "Categories", "Status", "ReplicaStatus", "ZoneID", "Offset", "Limit", "SortBy", "SortOrder"} {
		if _, ok := model[key]; !ok {
			t.Errorf("current model repository contract must expose %s", key)
		}
	}
	for _, stale := range []string{"Name", "Tag"} {
		if _, ok := model[stale]; ok {
			t.Errorf("stale model repository field %s must not be exposed", stale)
		}
	}

	if _, ok := properties("ReinstallInstanceWorkflow")["Password"]; ok {
		t.Fatal("reinstall must not promise a user-controlled password the upstream ignores")
	}

	for _, tc := range []struct{ tool, field string }{
		{"CreateCFSWorkflow", "ChargeType"},
		{"GetCompShareCFSPrice", "ChargeType"},
	} {
		charge, _ := properties(tc.tool)[tc.field].(map[string]any)
		enums, _ := charge["enum"].([]string)
		if !slices.Equal(enums, []string{"Month", "Year", "Day"}) {
			t.Fatalf("%s new-purchase schema must expose only operational billing modes, got %v", tc.tool, enums)
		}
		desc, _ := charge["description"].(string)
		if strings.Contains(desc, "Dynamic") || strings.Contains(desc, "按量") || strings.Contains(desc, "Postpay") {
			t.Fatalf("%s must not advertise a non-operational hourly mode, got %q", tc.tool, desc)
		}
	}

	for _, tc := range []struct {
		tool, field string
		max         int
	}{
		{"RenameInstanceWorkflow", "Name", 63},
		{"CreateCustomImageWorkflow", "Name", 50},
		{"CloneCustomImageWorkflow", "TargetImageName", 50},
	} {
		field, _ := properties(tc.tool)[tc.field].(map[string]any)
		if got, _ := field["maxLength"].(int); got != tc.max {
			t.Errorf("%s.%s maxLength=%v, want %d", tc.tool, tc.field, field["maxLength"], tc.max)
		}
		if pattern, _ := field["pattern"].(string); pattern == "" {
			t.Errorf("%s.%s must expose the upstream name character contract", tc.tool, tc.field)
		}
	}
	if _, ok := properties("CreateInstanceWorkflow")["Name"]; ok {
		t.Error("CreateInstanceWorkflow must leave naming to the platform; RenameInstanceWorkflow owns explicit names")
	}
}

func TestCFSInternalZoneIDIsWorkflowOnly(t *testing.T) {
	policies := DefaultToolExecutionPolicies()
	for _, action := range []string{
		"GetCompShareCFSPrice",
		"GetCompShareCFSUpgradePrice",
		"GetCompShareCFSRefundPrice",
		"DescribeCFS",
		"CreateCFS",
		"ResizeCFS",
	} {
		p, ok := policies[action]
		if !ok {
			t.Fatalf("%s should have a policy", action)
		}
		for _, param := range p.AllowedParams {
			if param == "zone_id" || param == "az_group" {
				t.Fatalf("%s must not expose %s to model-origin calls", action, param)
			}
		}
	}

	for _, action := range []string{"GetCompShareCFSPrice", "GetCompShareCFSUpgradePrice", "GetCompShareCFSRefundPrice", "ResizeCFS"} {
		p := policies[action]
		found := false
		for _, param := range p.InternalAllowedParams {
			if param == "zone_id" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s must allow workflow-derived zone_id internally", action)
		}
	}

	for _, action := range []string{
		"CheckCompShareNetOptimizer",
		"SyncCompShareNetOptimizer",
		"DescribeCompShareSupportZone",
		"DescribeCompShareGpuInventory",
		"DescribeCFS",
		"GetCompShareCFSPrice",
		"GetCompShareCFSUpgradePrice",
		"GetCompShareCFSRefundPrice",
		"CreateCFS",
		"ResizeCFS",
	} {
		p := policies[action]
		for _, want := range []string{"top_organization_id", "organization_id"} {
			if !containsString(p.InternalAllowedParams, want) {
				t.Fatalf("%s must preserve backend-injected %s internally", action, want)
			}
			if containsString(p.AllowedParams, want) {
				t.Fatalf("%s must not expose %s to model-origin calls", action, want)
			}
		}
	}

	for _, action := range []string{"CheckCompShareNetOptimizer", "SyncCompShareNetOptimizer", "GetCompShareCFSPrice", "CreateCFS"} {
		p := policies[action]
		if !containsString(p.InternalAllowedParams, "az_group") {
			t.Fatalf("%s must preserve backend-derived az_group internally", action)
		}
		if containsString(p.AllowedParams, "az_group") {
			t.Fatalf("%s must not expose az_group to model-origin calls", action)
		}
	}

	for _, action := range []string{"CheckCompShareNetOptimizer", "SyncCompShareNetOptimizer"} {
		p := policies[action]
		for _, want := range []string{"Zone", "Region"} {
			if !containsString(p.InternalAllowedParams, want) {
				t.Fatalf("%s must preserve backend-derived %s internally", action, want)
			}
			if containsString(p.AllowedParams, want) {
				t.Fatalf("%s must not expose %s to model-origin calls", action, want)
			}
		}
	}

	createPolicy := policies["CreateCFS"]
	for _, want := range []string{"Name", "Size", "ChargeType", "Quantity", "Zone", "Region"} {
		if !containsString(createPolicy.AllowedParams, want) {
			t.Fatalf("CreateCFS workflow-internal policy must preserve %s", want)
		}
	}
	if !containsString(createPolicy.InternalAllowedParams, "zone_id") {
		t.Fatal("CreateCFS must allow workflow-derived zone_id internally")
	}
}

func TestCreatePriceToolsAllowUpstreamRequestFields(t *testing.T) {
	policies := DefaultToolExecutionPolicies()
	for _, action := range []string{"GetCompShareInstancePrice", "GetCompShareInstanceUserPrice"} {
		p, ok := policies[action]
		if !ok {
			t.Fatalf("%s should have a policy", action)
		}
		for _, want := range []string{"Region", "CompShareImageId", "Disks"} {
			found := false
			for _, param := range p.AllowedParams {
				if param == want {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("%s must allow %s so create pricing matches upstream request structure", action, want)
			}
		}
	}
}

func TestInventoryToolDescriptionsSetRoutingBoundaries(t *testing.T) {
	descriptions := registryDescriptions()

	mustContain(t, descriptions["DescribeCompShareInstance"], "用户自己账号下")
	mustContain(t, descriptions["DescribeCompShareInstance"], "不用于查询机房库存")

	mustContain(t, descriptions["DescribeAvailableCompShareInstanceTypes"], "是否可售")
	mustContain(t, descriptions["DescribeAvailableCompShareInstanceTypes"], "Status（Normal/SoldOut）")
	mustContain(t, descriptions["DescribeAvailableCompShareInstanceTypes"], "不返回精确剩余数量")
	mustContain(t, descriptions["DescribeAvailableCompShareInstanceTypes"], "不代表实时可创建库存")

	mustContain(t, descriptions["CheckCompShareResourceCapacity"], "具体创建实例配置")
	mustContain(t, descriptions["CheckCompShareResourceCapacity"], "确认该机型当前是否真实可创建")
	mustContain(t, descriptions["CheckCompShareResourceCapacity"], "CompShareImageId 和 ChargeType 必填")
	mustContain(t, descriptions["CheckCompShareResourceCapacity"], "workflow 会按可用区类型补齐内部位置参数")
	mustContain(t, descriptions["CheckCompShareResourceCapacity"], "普通区用 Zone/Region")
	mustContain(t, descriptions["CheckCompShareResourceCapacity"], "Pod 区内部用 zone_id")
	mustContain(t, descriptions["CheckCompShareResourceCapacity"], "不要手填 zone_id/az_group")

	mustContain(t, descriptions["CreateInstanceWorkflow"], "创建算力实例")
	mustNotContain(t, descriptions["CreateInstanceWorkflow"], "必须使用此工具")
	mustContain(t, descriptions["DiagnoseBilling"], "再次询问当前费用时重新调用本工具")
	mustContain(t, descriptions["DiagnoseInstanceInternals"], "绝不能从列表自行挑选")
	mustContain(t, descriptions["DiagnoseInstanceInternals"], "不能把它视为已授权")
}

func TestDescribeCompShareInstanceDoesNotExposeWithoutGpu(t *testing.T) {
	for _, tool := range Registry {
		if tool.Function == nil || tool.Function.Name != "DescribeCompShareInstance" {
			continue
		}
		params, _ := tool.Function.Parameters.(map[string]any)
		props, _ := params["properties"].(map[string]any)
		if _, ok := props["WithoutGpu"]; ok {
			t.Fatal("DescribeCompShareInstance must not expose WithoutGpu to model-origin calls; no-card start is workflow-owned")
		}
		return
	}
	t.Fatal("DescribeCompShareInstance tool not found")
}

func TestCustomImageWorkflowIsUserFacingButRawImageCreateIsNot(t *testing.T) {
	descriptions := registryDescriptions()

	mustContain(t, descriptions["CreateCustomImageWorkflow"], "从实例")
	mustContain(t, descriptions["CreateCustomImageWorkflow"], "自制镜像")
	mustContain(t, descriptions["CreateCustomImageWorkflow"], "确认卡")
	mustContain(t, descriptions["CreateCustomImageWorkflow"], "Making")
	mustContain(t, descriptions["CreateCustomImageWorkflow"], "不会关闭源实例")
	mustNotContain(t, descriptions["CreateCustomImageWorkflow"], "制作前需停机")
	if _, ok := descriptions["CreateCompShareCustomImage"]; ok {
		t.Fatal("raw CreateCompShareCustomImage must not be exposed as a user-facing tool")
	}
	mustContain(t, descriptions["CloneCustomImageWorkflow"], "另一个可用区")
	if _, ok := descriptions["SyncCompShareCustomImage"]; ok {
		t.Fatal("raw SyncCompShareCustomImage must not be exposed as a user-facing tool")
	}
}

func TestCloneCustomImageTargetZonesAreWorkflowOnly(t *testing.T) {
	policy := DefaultToolExecutionPolicies()["SyncCompShareCustomImage"]
	if containsString(policy.AllowedParams, "TargetZoneIds") {
		t.Fatal("the model must not choose numeric destination ids")
	}
	if !containsString(policy.InternalAllowedParams, "TargetZoneIds") {
		t.Fatal("the workflow must be able to pass the resolver-verified destination")
	}
}

func registryDescriptions() map[string]string {
	out := make(map[string]string, len(Registry))
	for _, tool := range Registry {
		if tool.Function == nil {
			continue
		}
		out[tool.Function.Name] = tool.Function.Description
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("description missing %q:\n%s", needle, haystack)
	}
}

func mustNotContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("description should not contain %q:\n%s", needle, haystack)
	}
}
