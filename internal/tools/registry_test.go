package tools

import (
	"strings"
	"testing"
)

func TestInventoryToolDescriptionsSetRoutingBoundaries(t *testing.T) {
	descriptions := registryDescriptions()

	mustContain(t, descriptions["DescribeCompShareInstance"], "用户自己账号下")
	mustContain(t, descriptions["DescribeCompShareInstance"], "不用于查询机房库存")

	mustContain(t, descriptions["DescribeAvailableCompShareInstanceTypes"], "是否可售")
	mustContain(t, descriptions["DescribeAvailableCompShareInstanceTypes"], "Status（Normal/SoldOut）")
	mustContain(t, descriptions["DescribeAvailableCompShareInstanceTypes"], "不返回精确剩余数量")

	mustContain(t, descriptions["CheckCompShareResourceCapacity"], "具体创建实例配置")
	mustContain(t, descriptions["CheckCompShareResourceCapacity"], "确认该机型当前是否真实可创建")
	mustContain(t, descriptions["CheckCompShareResourceCapacity"], "CompShareImageId 和 ChargeType 必填")
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

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("description missing %q:\n%s", needle, haystack)
	}
}
