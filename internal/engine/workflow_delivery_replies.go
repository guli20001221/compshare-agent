package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/compshare-agent/internal/workflow"
)

func cloneCustomImageWorkflowReply(result *workflow.Result) string {
	imageID := ""
	state := "submitted"
	progress := ""
	if result != nil && result.Data != nil {
		imageID = strings.TrimSpace(fmt.Sprint(result.Data["CompShareImageId"]))
		if value := strings.TrimSpace(fmt.Sprint(result.Data["DeliveryState"])); value != "" && value != "<nil>" {
			state = strings.ToLower(value)
		}
		if detail, ok := result.Data["Progress"].(map[string]any); ok {
			progress = strings.TrimSpace(fmt.Sprint(detail["Progress"]))
			if progress == "" || progress == "<nil>" {
				progress = strings.TrimSpace(fmt.Sprint(detail["Process"]))
			}
		}
	}
	target := "目标镜像"
	if imageID != "" && imageID != "<nil>" {
		target = "目标镜像 " + imageID
	}
	switch state {
	case "pending":
		if progress != "" && progress != "<nil>" {
			return fmt.Sprintf("✅ 已提交%s的跨区同步，当前进度为 %s%%；进度接口不提供最终可用状态，请稍后查询镜像目录，请勿重复提交。", target, strings.TrimSuffix(progress, "%"))
		}
		return fmt.Sprintf("✅ 已提交%s的跨区同步；最终可用状态尚未确认，请稍后查询镜像目录，请勿重复提交。", target)
	default:
		return fmt.Sprintf("✅ 已提交%s的跨区同步请求；本次尚未取得可验证的进度，请稍后查询镜像状态，请勿重复提交。", target)
	}
}

func scheduledShutdownWorkflowReply(action string, params map[string]any, result *workflow.Result) (string, bool) {
	if action != "SetStopSchedulerWorkflow" && action != "CancelStopSchedulerWorkflow" {
		return "", false
	}
	id := strings.TrimSpace(fmt.Sprint(params["UHostId"]))
	verified := false
	if result != nil && result.Data != nil {
		verified, _ = result.Data["Verified"].(bool)
	}
	if action == "CancelStopSchedulerWorkflow" {
		if verified {
			return fmt.Sprintf("✅ 已取消实例 %s 的定时关机，并已回读确认。", id), true
		}
		return fmt.Sprintf("已提交实例 %s 的定时关机取消请求，但本次回读尚未确认原设置已清除。请稍后查看当前设置，请勿重复提交。", id), true
	}
	want := int64(0)
	if result != nil && result.Data != nil {
		want = int64Value(result.Data["RequestedStopTime"])
	}
	when := "确认的时间"
	if want > 0 {
		when = time.Unix(want, 0).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04（北京时间）")
	}
	if verified {
		return fmt.Sprintf("✅ 已为实例 %s 设置定时关机：%s，并已回读确认。", id, when), true
	}
	return fmt.Sprintf("已提交实例 %s 的定时关机设置（%s），但本次回读尚未确认设置结果。请稍后查看当前定时关机设置，请勿重复提交。", id, when), true
}

func int64Value(value any) int64 {
	switch n := value.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}
