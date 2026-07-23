package workflow

import (
	"strings"

	"github.com/compshare-agent/internal/platform"
)

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

func narrowInstanceResultToUHostID(result map[string]any, uhostID string) bool {
	uhostID = strings.TrimSpace(uhostID)
	if result == nil || uhostID == "" {
		return false
	}
	hostSet, ok := result["UHostSet"].([]any)
	if !ok || len(hostSet) == 0 {
		return false
	}
	for _, raw := range hostSet {
		host, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(stringFieldAny(host["UHostId"])) == uhostID {
			result["UHostSet"] = []any{host}
			result["TotalCount"] = float64(1)
			return true
		}
	}
	return false
}

func instanceIDFromResult(result map[string]any) string {
	return firstInstanceField(result, "UHostId")
}

func instanceTypeFromResult(result map[string]any) string {
	return firstInstanceField(result, "InstanceType")
}

func isPodInstanceResult(result map[string]any) bool {
	return platform.IsPodInstanceID(instanceIDFromResult(result))
}

func isContainerInstanceResult(result map[string]any) bool {
	return strings.EqualFold(instanceTypeFromResult(result), "Container")
}
