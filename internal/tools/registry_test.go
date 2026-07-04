package tools

import (
	"strings"
	"testing"
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

func TestWorkflowSchemasLetBackendClarifyMissingSizes(t *testing.T) {
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
		if containsString(required, "Size") {
			t.Fatalf("%s must not require Size; workflow missing-slot handling should ask for it", action)
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
		"DescribeCompShareJupyterToken",
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
	mustContain(t, descriptions["DescribeCompShareJupyterToken"], "不要明文展示")
	mustContain(t, descriptions["EnableNetOptimizerWorkflow"], "确认")
	mustContain(t, descriptions["CreateCFSWorkflow"], "确认")
	mustContain(t, descriptions["ResizeCFSWorkflow"], "确认")
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
	mustContain(t, descriptions["CheckCompShareResourceCapacity"], "只传 Zone/Region 字符串")
	mustContain(t, descriptions["CheckCompShareResourceCapacity"], "不要手填 zone_id/az_group")

	mustContain(t, descriptions["CreateInstanceWorkflow"], "Pod 区必须使用容器镜像")
	mustNotContain(t, descriptions["CreateInstanceWorkflow"], "必须使用此工具")
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

	mustContain(t, descriptions["CreateCustomImageWorkflow"], "CreateCompShareCustomImage")
	mustContain(t, descriptions["CreateCustomImageWorkflow"], "GetCompShareImageCreateProgress")
	if _, ok := descriptions["CreateCompShareCustomImage"]; ok {
		t.Fatal("raw CreateCompShareCustomImage must not be exposed as a user-facing tool")
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
