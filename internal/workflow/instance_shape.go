package workflow

import "strings"

func firstInstanceField(result map[string]any, key string) string {
	host, ok := firstInstance(result)
	if !ok {
		return ""
	}
	if v, ok := host[key].(string); ok {
		return v
	}
	return ""
}

func instanceIDFromResult(result map[string]any) string {
	return firstInstanceField(result, "UHostId")
}

func instanceTypeFromResult(result map[string]any) string {
	return firstInstanceField(result, "InstanceType")
}

func isPodInstanceResult(result map[string]any) bool {
	return strings.HasPrefix(instanceIDFromResult(result), "cpod-")
}

func isContainerInstanceResult(result map[string]any) bool {
	return strings.EqualFold(instanceTypeFromResult(result), "Container")
}
