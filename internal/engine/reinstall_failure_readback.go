package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/readprojection"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
)

// reinstallFailureReply reconciles a failed, authorised reinstall with one
// current-state read. Pod reinstall is a compound upstream operation: it may
// stop/delete the old pod before recreating the new one. A transport or recreate
// failure therefore cannot truthfully be described as "nothing happened".
func (e *Engine) reinstallFailureReply(ctx context.Context, params map[string]any, result *workflow.Result) (string, bool) {
	if result == nil || result.Success || result.Failure == nil ||
		result.Failure.Step != "重装系统" || !result.Failure.ExecutionAuthorized {
		return "", false
	}
	id := strings.TrimSpace(fmt.Sprint(params["UHostId"]))
	if id == "" || id == "<nil>" {
		return reinstallUnknownReply(""), true
	}
	raw, err := e.executeRawTool(ctx, "DescribeCompShareInstance", map[string]any{
		"UHostIds": []any{id},
	}, tools.OriginWorkflowInternal)
	if err != nil {
		return reinstallUnknownReply(id), true
	}
	row := describedInstanceByID(raw, id)
	if row == nil {
		return reinstallUnknownReply(id), true
	}

	snap := entity.InstanceFromMap(row)
	state := readprojection.ResourceStateLabel(snap.State)
	draft := result.Failure.Draft
	targetID := strings.TrimSpace(fmt.Sprint(draft["TargetImageId"]))
	targetName := strings.TrimSpace(fmt.Sprint(draft["TargetImageName"]))
	observedID := strings.TrimSpace(fmt.Sprint(row["CompShareImageId"]))
	imageMatches := targetID != "" && targetID != "<nil>" && strings.EqualFold(targetID, observedID)
	// Once the confirmed contract carries an exact image ID, an equal display
	// name is not proof: community/custom catalogs may have multiple versions
	// with the same name. Name fallback exists only for legacy drafts without ID.
	if (targetID == "" || targetID == "<nil>") && targetName != "" && targetName != "<nil>" {
		imageMatches = strings.EqualFold(targetName, snap.ImageName)
	}
	if imageMatches {
		return fmt.Sprintf("重装接口返回失败，但实时回读显示实例 %s 已切换到目标镜像，当前状态为%s。请以实时状态为准，请勿重复提交。", id, state), true
	}

	isPod, _ := draft["IsPod"].(bool)
	initialState := strings.ToLower(strings.TrimSpace(fmt.Sprint(draft["InitialState"])))
	if isPod && strings.EqualFold(snap.State, "Stopped") && (initialState == "running" || initialState == "stopping") {
		return fmt.Sprintf("重装接口返回失败；实时回读显示 Pod 实例 %s 已停止，但目标镜像的重建和启动尚未确认完成。请勿重复提交，请先按当前状态处理。", id), true
	}

	image := strings.TrimSpace(snap.ImageName)
	if image == "" {
		image = "未返回"
	}
	return fmt.Sprintf("重装接口返回失败；实时回读显示实例 %s 当前状态为%s、当前镜像为%s，不能确认重装已完整生效。请勿重复提交，请先按当前状态处理。", id, state, image), true
}

func reinstallUnknownReply(id string) string {
	target := "该实例"
	if id != "" {
		target = "实例 " + id
	}
	return fmt.Sprintf("重装接口返回失败，随后未能读取%s的当前状态。本次结果不确定，请勿重复提交，请先查询实例状态和镜像。", target)
}
