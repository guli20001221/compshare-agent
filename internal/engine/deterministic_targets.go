package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/compshare-agent/internal/diagnosis"
	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/intent"
	openai "github.com/sashabaranov/go-openai"
)

func augmentPlanTargetRefsFromUserText(plan intent.IntentRoute, userText string, snapshot entity.RegistrySnapshot) intent.IntentRoute {
	if len(plan.Slots.TargetRefs) > 0 {
		return plan
	}
	if plan.Intent != intent.IntentResourceInfo &&
		plan.Intent != intent.IntentMonitorQuery &&
		plan.Intent != intent.IntentOperationLifecycle &&
		plan.Intent != intent.IntentDiagnosis {
		return plan
	}
	if inst, _ := findExplicitInstanceRef(userText, snapshot); inst != nil {
		plan.Slots.TargetRefs = []intent.TargetRef{{
			Type:       intent.TargetRefUHostIDUserInput,
			Value:      inst.UHostId,
			Source:     intent.SourceUserText,
			SourceSpan: inst.UHostId,
		}}
		return plan
	}
	if inst, ok := findUniqueInstanceNameInText(userText, snapshot); ok {
		plan.Slots.TargetRefs = []intent.TargetRef{{
			Type:       intent.TargetRefUHostIDUserInput,
			Value:      inst.UHostId,
			Source:     intent.SourceUserText,
			SourceSpan: inst.Name,
		}}
		return plan
	}
	if plan.Intent == intent.IntentResourceInfo {
		if gpu, ok := findUniqueGPUTypeInText(userText, snapshot); ok {
			plan.Slots.TargetRefs = []intent.TargetRef{{
				Type:   intent.TargetRefFilter,
				Value:  "gpu_type=" + gpu,
				Source: intent.SourceUserText,
			}}
		}
	}
	if plan.Intent == intent.IntentDiagnosis {
		if inst, ok := findUniqueInstanceByGPUTypeInText(userText, snapshot); ok {
			plan.Slots.TargetRefs = []intent.TargetRef{{
				Type:       intent.TargetRefUHostIDUserInput,
				Value:      inst.UHostId,
				Source:     intent.SourceUserText,
				SourceSpan: inst.GpuType,
			}}
		}
	}
	return plan
}

func findUniqueInstanceNameInText(userText string, snapshot entity.RegistrySnapshot) (entity.InstanceSnapshot, bool) {
	text := strings.TrimSpace(userText)
	if text == "" || len(snapshot.Instances) == 0 {
		return entity.InstanceSnapshot{}, false
	}
	compactText := normalizeResourceText(text)
	type candidate struct {
		inst  entity.InstanceSnapshot
		score int
	}
	var candidates []candidate
	for _, inst := range snapshot.Instances {
		name := strings.TrimSpace(inst.Name)
		if name == "" {
			continue
		}
		score := 0
		if strings.Contains(text, name) {
			score = len([]rune(name)) * 10
		} else {
			normalizedName := normalizeResourceText(name)
			if len([]rune(normalizedName)) >= 4 && strings.Contains(compactText, normalizedName) {
				score = len([]rune(normalizedName))*10 - 1
			}
		}
		if score > 0 {
			candidates = append(candidates, candidate{inst: inst, score: score})
		}
	}
	if len(candidates) == 0 {
		return entity.InstanceSnapshot{}, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if len([]rune(candidates[i].inst.Name)) != len([]rune(candidates[j].inst.Name)) {
			return len([]rune(candidates[i].inst.Name)) > len([]rune(candidates[j].inst.Name))
		}
		return candidates[i].inst.UHostId < candidates[j].inst.UHostId
	})
	bestScore := candidates[0].score
	var best []entity.InstanceSnapshot
	for _, c := range candidates {
		if c.score != bestScore {
			break
		}
		best = append(best, c.inst)
	}
	if len(best) != 1 {
		return entity.InstanceSnapshot{}, false
	}
	return best[0], true
}

func findUniqueGPUTypeInText(userText string, snapshot entity.RegistrySnapshot) (string, bool) {
	compactText := normalizeResourceText(userText)
	if compactText == "" {
		return "", false
	}
	seen := map[string]struct{}{}
	var matches []string
	for _, inst := range snapshot.Instances {
		gpuType := strings.TrimSpace(inst.GpuType)
		if gpuType == "" {
			continue
		}
		if _, ok := seen[gpuType]; ok {
			continue
		}
		seen[gpuType] = struct{}{}
		if strings.Contains(compactText, normalizeResourceText(gpuType)) {
			matches = append(matches, gpuType)
		}
	}
	if len(matches) == 0 {
		return "", false
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if len([]rune(matches[i])) != len([]rune(matches[j])) {
			return len([]rune(matches[i])) > len([]rune(matches[j]))
		}
		return matches[i] < matches[j]
	})
	longestLen := len([]rune(matches[0]))
	var longest []string
	for _, match := range matches {
		if len([]rune(match)) != longestLen {
			break
		}
		longest = append(longest, match)
	}
	if len(longest) != 1 {
		return "", false
	}
	return longest[0], true
}

func findUniqueInstanceByGPUTypeInText(userText string, snapshot entity.RegistrySnapshot) (entity.InstanceSnapshot, bool) {
	gpuType, ok := findUniqueGPUTypeInText(userText, snapshot)
	if !ok {
		return entity.InstanceSnapshot{}, false
	}
	var matches []entity.InstanceSnapshot
	for _, inst := range snapshot.Instances {
		if strings.EqualFold(strings.TrimSpace(inst.GpuType), gpuType) {
			matches = append(matches, inst)
		}
	}
	if len(matches) != 1 {
		return entity.InstanceSnapshot{}, false
	}
	return matches[0], true
}

func findInstancesByGPUTypeInText(userText string, snapshot entity.RegistrySnapshot) (string, []entity.InstanceSnapshot) {
	gpuType, ok := findUniqueGPUTypeInText(userText, snapshot)
	if !ok {
		return "", nil
	}
	var matches []entity.InstanceSnapshot
	for _, inst := range snapshot.Instances {
		if strings.EqualFold(strings.TrimSpace(inst.GpuType), gpuType) {
			matches = append(matches, inst)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if strings.TrimSpace(matches[i].Name) != strings.TrimSpace(matches[j].Name) {
			return strings.TrimSpace(matches[i].Name) < strings.TrimSpace(matches[j].Name)
		}
		return matches[i].UHostId < matches[j].UHostId
	})
	return gpuType, matches
}

func renderGPUInstanceSelectionPrompt(gpuType string, matches []entity.InstanceSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "我看到账户里有多台 %s 实例。请回复要诊断的实例名称或实例 ID：", gpuType)
	limit := len(matches)
	if limit > 8 {
		limit = 8
	}
	for i := 0; i < limit; i++ {
		inst := matches[i]
		fmt.Fprintf(&b, "\n%d. %s (%s)，状态：%s", i+1, strings.TrimSpace(inst.Name), inst.UHostId, emptyStateLabel(inst.State))
	}
	if len(matches) > limit {
		fmt.Fprintf(&b, "\n另外还有 %d 台同型号实例未展示。", len(matches)-limit)
	}
	return b.String()
}

func normalizeResourceText(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (e *Engine) tryOperationLifecycleDispatch(ctx context.Context, dispatch routerDispatchResult, userMsg string, onStep func(StepEvent)) (string, bool) {
	result := dispatch.result
	if result.Plan.Intent != intent.IntentOperationLifecycle {
		return "", false
	}
	action := result.Plan.Slots.Action
	if startWithoutGPURequestedByText(userMsg) {
		action = intent.LifecycleActionStart
		result.Plan.Slots.Action = action
	}
	if action == "" {
		action = inferLifecycleAction(userMsg)
		result.Plan.Slots.Action = action
	}
	workflowName, ok := lifecycleWorkflowName(action)
	if !ok {
		return "", false
	}
	if looksLikeLifecycleQuestion(userMsg) {
		return "", false
	}
	snapshot, okSnapshot, err := e.freshResourceSelectionSnapshot(ctx, dispatch.snapshot)
	if err != nil || !okSnapshot {
		return "", false
	}
	result.Plan = augmentPlanTargetRefsFromUserText(result.Plan, userMsg, snapshot)
	inst, status := resolveSingleLifecycleTarget(result.Plan, snapshot)
	switch status {
	case lifecycleTargetMissing:
		return "", false
	case lifecycleTargetUnresolved:
		reply := "没有找到你要操作的实例，请确认实例名称或 ID 是否正确。"
		e.emitPlannerTrace(result, intent.RouteStatusFallbackUnresolvedTarget, dispatch.latency)
		e.messages = append(e.messages, assistantMessage(reply))
		return reply, true
	case lifecycleTargetAmbiguous:
		reply := "找到多台可能的实例，请补充完整实例名称或实例 ID 后再操作。"
		e.emitPlannerTrace(result, intent.RouteStatusSelectionRequired, dispatch.latency)
		e.messages = append(e.messages, assistantMessage(reply))
		return reply, true
	case lifecycleTargetHit:
	default:
		return "", false
	}
	if reply, blocked := lifecycleStateGateReply(action, inst); blocked {
		e.emitPlannerTrace(result, intent.RouteStatusDispatched, dispatch.latency)
		e.recordSelectedInstanceID(inst.UHostId, inst.Name)
		e.recordLastIntentFromPlan(result.Plan)
		e.messages = append(e.messages, assistantMessage(reply))
		return reply, true
	}
	args := map[string]any{"UHostId": inst.UHostId}
	switch action {
	case intent.LifecycleActionResize:
		resizeArgs := resizeWorkflowArgsFromUserText(userMsg)
		if len(resizeArgs) == 0 {
			reply := "请告诉我要改到什么配置，例如 4C8G、16C64G，或补充 GPU 数量。"
			e.emitPlannerTrace(result, intent.RouteStatusSelectionRequired, dispatch.latency)
			e.recordSelectedInstanceID(inst.UHostId, inst.Name)
			e.recordLastIntentFromPlan(result.Plan)
			e.messages = append(e.messages, assistantMessage(reply))
			return reply, true
		}
		for k, v := range resizeArgs {
			args[k] = v
		}
		if strings.EqualFold(strings.TrimSpace(inst.State), "Running") {
			e.emitPlannerTrace(result, intent.RouteStatusDispatched, dispatch.latency)
			reply := e.executeWorkflow(ctx, "StopInstanceWorkflow", map[string]any{"UHostId": inst.UHostId}, onStep)
			if final, ok := isFinalReply(reply); ok {
				reply = final
			} else if msg, ok := workflowFailureMessage(reply); ok {
				reply = msg
			}
			e.recordSelectedInstanceID(inst.UHostId, inst.Name)
			e.recordLastIntentFromPlan(result.Plan)
			e.messages = append(e.messages, assistantMessage(reply))
			return reply, true
		}
	case intent.LifecycleActionRename:
		newName := renameWorkflowNameFromUserText(userMsg)
		if newName == "" {
			reply := "请告诉我要把实例改成什么名称。"
			e.emitPlannerTrace(result, intent.RouteStatusSelectionRequired, dispatch.latency)
			e.recordSelectedInstanceID(inst.UHostId, inst.Name)
			e.recordLastIntentFromPlan(result.Plan)
			e.messages = append(e.messages, assistantMessage(reply))
			return reply, true
		}
		args["Name"] = newName
	case intent.LifecycleActionResetPwd:
		password := resetPasswordFromUserText(userMsg)
		if password == "" {
			reply := "请提供要设置的新密码。"
			e.emitPlannerTrace(result, intent.RouteStatusSelectionRequired, dispatch.latency)
			e.recordSelectedInstanceID(inst.UHostId, inst.Name)
			e.recordLastIntentFromPlan(result.Plan)
			e.messages = append(e.messages, assistantMessage(reply))
			return reply, true
		}
		args["Password"] = password
	case intent.LifecycleActionCreateDisk:
		size := createDiskSizeFromUserText(userMsg)
		if size <= 0 {
			reply := "创建数据盘需要指定磁盘大小（GB）。请告诉我要加多大的数据盘，例如 30GB。"
			e.emitPlannerTrace(result, intent.RouteStatusSelectionRequired, dispatch.latency)
			e.recordSelectedInstanceID(inst.UHostId, inst.Name)
			e.recordLastIntentFromPlan(result.Plan)
			e.messages = append(e.messages, assistantMessage(reply))
			return reply, true
		}
		args["Size"] = size
	}
	e.emitPlannerTrace(result, intent.RouteStatusDispatched, dispatch.latency)
	reply := e.executeWorkflow(ctx, workflowName, args, onStep)
	if final, ok := isFinalReply(reply); ok {
		reply = final
	} else if msg, ok := workflowFailureMessage(reply); ok {
		reply = msg
	}
	e.recordSelectedInstanceID(inst.UHostId, inst.Name)
	e.recordLastIntentFromPlan(result.Plan)
	e.messages = append(e.messages, assistantMessage(reply))
	return reply, true
}

func (e *Engine) tryDirectLifecycleFromUserText(ctx context.Context, userMsg string, onStep func(StepEvent)) (string, bool) {
	if e == nil || !e.mutatingToolsEnabled {
		return "", false
	}
	if looksLikeLifecycleQuestion(userMsg) {
		return "", false
	}
	action := inferLifecycleAction(userMsg)
	if action == "" {
		return "", false
	}
	if action == intent.LifecycleActionCreateDisk {
		return "", false
	}
	if selectedID := strings.TrimSpace(e.sessionState.SelectedInstanceID); selectedID != "" && looksLikeSelectedInstanceFollowup(userMsg) {
		if inst, res := e.RegistrySnapshot().ResolveByID(selectedID); res.Status == entity.ResolveHit && inst != nil {
			plan := lifecyclePlanForSelectedInstance(action, *inst)
			return e.tryLifecycleActionForSelectedInstance(ctx, *inst, action, userMsg, plan, onStep)
		}
		workflowName, ok := lifecycleWorkflowName(action)
		if !ok {
			return "", false
		}
		args := map[string]any{"UHostId": selectedID}
		if reply, ok := populateLifecycleActionArgs(args, action, userMsg); !ok {
			e.messages = append(e.messages, assistantMessage(reply))
			return reply, true
		}
		plan := intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentOperationLifecycle,
			Slots:         intent.Slots{Action: action},
			RequiredTools: []string{"DescribeCompShareInstance"},
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    1,
		}
		reply := e.executeWorkflow(ctx, workflowName, args, onStep)
		if final, ok := isFinalReply(reply); ok {
			reply = final
		} else if msg, ok := workflowFailureMessage(reply); ok {
			reply = msg
		}
		e.recordSelectedInstanceID(selectedID, e.sessionState.SelectedInstanceName)
		e.recordLastIntentFromPlan(plan)
		e.messages = append(e.messages, assistantMessage(reply))
		return reply, true
	}
	cachedSnapshot := e.RegistrySnapshot()
	snapshot := cachedSnapshot
	if _, ok := findUniqueInstanceNameInText(userMsg, cachedSnapshot); !ok {
		if action != intent.LifecycleActionRename &&
			action != intent.LifecycleActionResetPwd &&
			!hasLikelyExplicitInstanceTargetText(userMsg) {
			return "", false
		}
		freshSnapshot, okSnapshot, err := e.freshResourceSelectionSnapshot(ctx, cachedSnapshot)
		if err != nil || !okSnapshot {
			return "", false
		}
		if _, ok := findUniqueInstanceNameInText(userMsg, freshSnapshot); !ok {
			return "", false
		}
		snapshot = freshSnapshot
	}
	inst, ok := resolveContinuationInstance(userMsg, snapshot)
	if !ok {
		return "", false
	}
	plan := intent.IntentRoute{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentOperationLifecycle,
		Slots: intent.Slots{
			Action: action,
			TargetRefs: []intent.TargetRef{{
				Type:       intent.TargetRefUHostIDUserInput,
				Value:      inst.UHostId,
				Source:     intent.SourceUserText,
				SourceSpan: inst.Name,
			}},
		},
		RequiredTools: []string{"DescribeCompShareInstance"},
		Retrieval:     intent.Retrieval{Enabled: false},
		Confidence:    1,
	}
	return e.tryOperationLifecycleDispatch(ctx, routerDispatchResult{
		result:   intent.IntentRouterResult{Plan: plan},
		snapshot: snapshot,
	}, userMsg, onStep)
}

func (e *Engine) tryCFSWorkflowDispatch(ctx context.Context, dispatch routerDispatchResult, userMsg string, onStep func(StepEvent)) (string, bool) {
	if e == nil || !e.mutatingToolsEnabled {
		return "", false
	}
	if dispatch.result.Plan.Intent != intent.IntentOperationLifecycle {
		return "", false
	}
	workflowName, ok := cfsWriteWorkflowFromUserText(userMsg)
	if !ok {
		return "", false
	}
	args, reply, ok := cfsWorkflowArgsFromUserText(workflowName, userMsg)
	if !ok {
		e.messages = append(e.messages, assistantMessage(reply))
		return reply, true
	}
	plan := intent.IntentRoute{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentOperationLifecycle,
		Slots:         intent.Slots{},
		RequiredTools: []string{workflowName},
		Retrieval:     intent.Retrieval{Enabled: false},
		Confidence:    1,
	}
	e.emitPlannerTrace(dispatch.result, intent.RouteStatusDispatched, dispatch.latency)
	raw := e.executeWorkflow(ctx, workflowName, args, onStep)
	if final, ok := isFinalReply(raw); ok {
		raw = final
	} else if msg, ok := workflowFailureMessage(raw); ok {
		raw = msg
	}
	e.recordLastIntentFromPlan(plan)
	e.messages = append(e.messages, assistantMessage(raw))
	return raw, true
}

func lifecyclePlanForSelectedInstance(action intent.LifecycleAction, inst entity.InstanceSnapshot) intent.IntentRoute {
	return intent.IntentRoute{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentOperationLifecycle,
		Slots: intent.Slots{
			Action: action,
			TargetRefs: []intent.TargetRef{{
				Type:       intent.TargetRefUHostIDUserInput,
				Value:      inst.UHostId,
				Source:     intent.SourceUserText,
				SourceSpan: inst.Name,
			}},
		},
		RequiredTools: []string{"DescribeCompShareInstance"},
		Retrieval:     intent.Retrieval{Enabled: false},
		Confidence:    1,
	}
}

func (e *Engine) tryLifecycleActionForSelectedInstance(ctx context.Context, inst entity.InstanceSnapshot, action intent.LifecycleAction, userMsg string, plan intent.IntentRoute, onStep func(StepEvent)) (string, bool) {
	workflowName, ok := lifecycleWorkflowName(action)
	if !ok {
		return "", false
	}
	args := map[string]any{"UHostId": inst.UHostId}
	if reply, ok := populateLifecycleActionArgs(args, action, userMsg); !ok {
		e.messages = append(e.messages, assistantMessage(reply))
		return reply, true
	}
	if action == intent.LifecycleActionResize && strings.EqualFold(strings.TrimSpace(inst.State), "Running") {
		workflowName = "StopInstanceWorkflow"
		args = map[string]any{"UHostId": inst.UHostId}
	}
	reply := e.executeWorkflow(ctx, workflowName, args, onStep)
	if final, ok := isFinalReply(reply); ok {
		reply = final
	} else if msg, ok := workflowFailureMessage(reply); ok {
		reply = msg
	}
	e.recordSelectedInstanceID(inst.UHostId, inst.Name)
	e.recordLastIntentFromPlan(plan)
	e.messages = append(e.messages, assistantMessage(reply))
	return reply, true
}

func populateLifecycleActionArgs(args map[string]any, action intent.LifecycleAction, userMsg string) (string, bool) {
	switch action {
	case intent.LifecycleActionResize:
		resizeArgs := resizeWorkflowArgsFromUserText(userMsg)
		if len(resizeArgs) == 0 {
			return "请告诉我要改到什么配置，例如 4C8G、16C64G，或补充 GPU 数量。", false
		}
		for k, v := range resizeArgs {
			args[k] = v
		}
	case intent.LifecycleActionRename:
		newName := renameWorkflowNameFromUserText(userMsg)
		if newName == "" {
			return "请告诉我要把实例改成什么名称。", false
		}
		args["Name"] = newName
	case intent.LifecycleActionResetPwd:
		password := resetPasswordFromUserText(userMsg)
		if password == "" {
			return "请提供要设置的新密码。", false
		}
		args["Password"] = password
	case intent.LifecycleActionCreateDisk:
		size := createDiskSizeFromUserText(userMsg)
		if size <= 0 {
			return "创建数据盘需要指定磁盘大小（GB）。请告诉我要加多大的数据盘，例如 30GB。", false
		}
		args["Size"] = size
	}
	return "", true
}

func (e *Engine) tryDirectStopSchedulerFromUserText(ctx context.Context, userMsg string, onStep func(StepEvent)) (string, bool) {
	if e == nil || !e.mutatingToolsEnabled {
		return "", false
	}
	compact := normalizeResourceText(userMsg)
	if !looksLikeScheduledShutdownText(compact) {
		return "", false
	}
	if looksLikeLifecycleQuestion(userMsg) {
		return "", false
	}
	cachedSnapshot := e.RegistrySnapshot()
	snapshot, okSnapshot, err := e.freshResourceSelectionSnapshot(ctx, cachedSnapshot)
	if err != nil || !okSnapshot {
		return "", false
	}
	if _, ok := findUniqueInstanceNameInText(userMsg, snapshot); !ok {
		return "", false
	}
	inst, ok := resolveContinuationInstance(userMsg, snapshot)
	if !ok {
		return "", false
	}
	workflowName := "SetStopSchedulerWorkflow"
	args := map[string]any{"UHostId": inst.UHostId}
	if strings.Contains(compact, "取消定时关机") ||
		strings.Contains(compact, "取消自动关机") ||
		strings.Contains(compact, "取消延时关机") ||
		(strings.Contains(compact, "取消") && (strings.Contains(compact, "定时关机") || strings.Contains(compact, "自动关机") || strings.Contains(compact, "延时关机"))) {
		workflowName = "CancelStopSchedulerWorkflow"
	} else {
		afterMinutes, ok := stopSchedulerAfterMinutesFromUserText(userMsg)
		if !ok {
			reply := "请告诉我要在多久后关机，例如 30 分钟后或 1 小时后。"
			e.recordSelectedInstanceID(inst.UHostId, inst.Name)
			e.messages = append(e.messages, assistantMessage(reply))
			return reply, true
		}
		args["AfterMinutes"] = float64(afterMinutes)
	}
	reply := e.executeWorkflow(ctx, workflowName, args, onStep)
	if final, ok := isFinalReply(reply); ok {
		reply = final
	} else if msg, ok := workflowFailureMessage(reply); ok {
		reply = msg
	}
	e.recordSelectedInstanceID(inst.UHostId, inst.Name)
	e.messages = append(e.messages, assistantMessage(reply))
	return reply, true
}

func hasLikelyExplicitInstanceTargetText(userText string) bool {
	lower := strings.ToLower(userText)
	if strings.Contains(lower, "uhost-") || strings.Contains(lower, "cpod-") {
		return false
	}
	for _, field := range strings.FieldsFunc(userText, func(r rune) bool {
		return unicode.IsSpace(r) || r == '，' || r == ',' || r == '。' || r == ';' || r == '；'
	}) {
		field = strings.Trim(field, "“”\"'()（）")
		if len([]rune(field)) >= 6 && (strings.Contains(field, "-") || strings.Contains(field, "_")) {
			return true
		}
	}
	return false
}

func (e *Engine) tryDeterministicProductFactReply(userMsg string) (string, bool) {
	if e == nil {
		return "", false
	}
	if reply, ok := deterministicGenericStockShortageReply(userMsg); ok {
		e.messages = append(e.messages, assistantMessage(reply))
		return reply, true
	}
	return "", false
}

func deterministicGenericStockShortageReply(userMsg string) (string, bool) {
	text := strings.TrimSpace(userMsg)
	if text == "" {
		return "", false
	}
	compact := normalizeResourceText(text)
	if compact == "" {
		return "", false
	}
	triggers := []string{
		"暂无资源", "没有卡了", "没卡了", "没货了", "没资源", "无资源", "资源不足",
		"无法创建", "都被占用", "可用的没有了", "可用资源没了",
	}
	matched := false
	for _, trigger := range triggers {
		if strings.Contains(text, trigger) || strings.Contains(compact, normalizeResourceText(trigger)) {
			matched = true
			break
		}
	}
	if !matched {
		return "", false
	}
	return "“暂无资源/没有卡”通常指当前具体配置没有可创建容量，不等于机型已经下架。\n\n这里要分开看：机型状态 Normal 只表示平台仍在售；真正能不能创建，要看 GPU、卡数、CPU、内存、镜像和可用区组合的容量预检结果。某个组合可能因为资源被占用、某个可用区没容量、镜像和可用区形态不匹配而创建失败。\n\n如果你要继续下单，建议换可用区、减少卡数、换相近卡型，或稍后重试；如果你给出具体卡型和可用区，我可以按当前配置再帮你做创建前校验。", true
}

func looksLikeLifecycleQuestion(userText string) bool {
	text := strings.TrimSpace(userText)
	if text == "" {
		return false
	}
	compact := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(strings.ToLower(text))
	for _, marker := range []string{
		"吗", "么", "是否", "是不是", "有没有", "能不能", "可不可以", "可以吗",
		"状态", "为什么", "怎么", "如何", "是什么", "啥是", "规则", "流程", "要求", "说明", "文档", "教程",
		"注意事项", "影响", "指南", "限制", "风险",
		"多少", "价格", "费用", "计费", "收费", "扣费", "花费", "成本",
	} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	return strings.Contains(compact, "?") || strings.Contains(compact, "？")
}

func (e *Engine) tryDiagnosisDispatch(ctx context.Context, dispatch routerDispatchResult, userMsg string, onStep func(StepEvent)) (string, bool) {
	result := dispatch.result
	if result.Plan.Intent != intent.IntentDiagnosis {
		return "", false
	}
	action := inferDiagnosisActionFromText(userMsg)
	if action == "" {
		return "", false
	}
	snapshot, okSnapshot, err := e.freshResourceSelectionSnapshot(ctx, dispatch.snapshot)
	if err != nil || !okSnapshot {
		return "", false
	}
	result.Plan = augmentPlanTargetRefsFromUserText(result.Plan, userMsg, snapshot)
	inst, status := resolveSingleLifecycleTarget(result.Plan, snapshot)
	if status != lifecycleTargetHit {
		return "", false
	}
	raw := e.executeDiagnosis(ctx, action, map[string]any{"UHostId": inst.UHostId}, onStep)
	reply := renderDiagnosisContinuationReply(inst, action, raw)
	e.emitPlannerTrace(result, intent.RouteStatusDispatched, dispatch.latency)
	e.recordSelectedInstanceID(inst.UHostId, inst.Name)
	e.recordLastIntentFromPlan(result.Plan)
	e.messages = append(e.messages, assistantMessage(reply))
	return reply, true
}

func (e *Engine) tryDirectDiagnosisFromUserText(ctx context.Context, userMsg string, onStep func(StepEvent)) (string, bool) {
	if e == nil {
		return "", false
	}
	action := inferDiagnosisActionFromText(userMsg)
	if action == "" {
		return "", false
	}
	snapshot, okSnapshot, err := e.freshResourceSelectionSnapshot(ctx, e.RegistrySnapshot())
	if err != nil || !okSnapshot {
		return "", false
	}
	if inst, ok := resolveContinuationInstance(userMsg, snapshot); ok {
		return e.runDeterministicDiagnosis(ctx, inst, action, onStep), true
	}
	if inst, ok := findUniqueInstanceByGPUTypeInText(userMsg, snapshot); ok {
		return e.runDeterministicDiagnosis(ctx, inst, action, onStep), true
	}
	if gpuType, matches := findInstancesByGPUTypeInText(userMsg, snapshot); gpuType != "" && len(matches) > 1 {
		reply := renderGPUInstanceSelectionPrompt(gpuType, matches)
		e.messages = append(e.messages, assistantMessage(reply))
		return reply, true
	}
	return "", false
}

func (e *Engine) runDeterministicDiagnosis(ctx context.Context, inst entity.InstanceSnapshot, action string, onStep func(StepEvent)) string {
	raw := e.executeDiagnosis(ctx, action, map[string]any{"UHostId": inst.UHostId}, onStep)
	reply := renderDiagnosisContinuationReply(inst, action, raw)
	e.recordSelectedInstanceID(inst.UHostId, inst.Name)
	e.recordLastIntentFromPlan(intent.IntentRoute{Intent: intent.IntentDiagnosis})
	e.messages = append(e.messages, assistantMessage(reply))
	return reply
}

func assistantMessage(reply string) openai.ChatCompletionMessage {
	return openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: reply,
	}
}

func (e *Engine) tryDiagnosisTargetContinuation(ctx context.Context, userMsg string, onStep func(StepEvent)) (string, bool) {
	if e == nil || !assistantAskedForInstanceTarget(e.lastAssistantContent()) {
		return "", false
	}
	action := inferPendingDiagnosisActionFromHistory(e.messages)
	if action == "" {
		return "", false
	}
	snapshot, ok, err := e.freshResourceSelectionSnapshot(ctx, e.RegistrySnapshot())
	if err != nil || !ok {
		return "", false
	}
	inst, ok := resolveContinuationInstance(userMsg, snapshot)
	if !ok {
		return "", false
	}
	raw := e.executeDiagnosis(ctx, action, map[string]any{"UHostId": inst.UHostId}, onStep)
	reply := renderDiagnosisContinuationReply(inst, action, raw)
	e.recordSelectedInstanceID(inst.UHostId, inst.Name)
	e.recordLastIntentFromPlan(intent.IntentRoute{Intent: intent.IntentDiagnosis})
	e.messages = append(e.messages, assistantMessage(reply))
	return reply, true
}

func assistantAskedForInstanceTarget(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	compact := normalizeResourceText(text)
	if compact == "" {
		return false
	}
	if !strings.Contains(compact, "实例") && !strings.Contains(strings.ToLower(text), "uhost") {
		return false
	}
	for _, marker := range []string{
		"哪台实例", "哪一台实例", "哪台机器", "哪个实例", "实例名称", "实例id", "实例ID", "提供实例", "告诉我哪台",
	} {
		if strings.Contains(text, marker) || strings.Contains(compact, normalizeResourceText(marker)) {
			return true
		}
	}
	return false
}

func inferPendingDiagnosisActionFromHistory(messages []openai.ChatCompletionMessage) string {
	seen := 0
	for i := len(messages) - 1; i >= 0 && seen < 10; i-- {
		msg := messages[i]
		if msg.Role != openai.ChatMessageRoleUser {
			continue
		}
		seen++
		text := strings.TrimSpace(msg.Content)
		if text == "" {
			continue
		}
		if action := inferDiagnosisActionFromText(text); action != "" {
			return action
		}
	}
	return ""
}

func inferDiagnosisActionFromText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	compact := normalizeResourceText(text)
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(compact, "ssh") &&
		(strings.Contains(text, "连不上") || strings.Contains(text, "进不去") ||
			strings.Contains(text, "超时") || strings.Contains(text, "打不开") ||
			strings.Contains(lower, "timeout") || strings.Contains(lower, "refused")):
		return "DiagnoseSSH"
	case (strings.Contains(compact, "nvidiasmi") || strings.Contains(compact, "gpu") || strings.Contains(text, "显卡")) &&
		(strings.Contains(text, "检测不到") || strings.Contains(text, "找不到") ||
			strings.Contains(text, "看不见") || strings.Contains(text, "不可见") ||
			strings.Contains(text, "报错") || strings.Contains(text, "不可用")):
		return "DiagnoseGPU"
	case strings.Contains(text, "初始化") &&
		(strings.Contains(text, "失败") || strings.Contains(text, "卡住") || strings.Contains(text, "一直")):
		return "DiagnoseInitFailure"
	case (strings.Contains(text, "端口") || strings.Contains(compact, "jupyter") ||
		strings.Contains(compact, "jupyterlab") || strings.Contains(compact, "filebrowser")) &&
		(strings.Contains(text, "打不开") || strings.Contains(text, "访问不了") ||
			strings.Contains(text, "不通") || strings.Contains(text, "连不上")):
		return "DiagnosePortOrFirewall"
	default:
		return ""
	}
}

func resolveContinuationInstance(userMsg string, snapshot entity.RegistrySnapshot) (entity.InstanceSnapshot, bool) {
	if inst, _ := findExplicitInstanceRef(userMsg, snapshot); inst != nil {
		return *inst, true
	}
	if inst, ok := findUniqueInstanceNameInText(userMsg, snapshot); ok {
		return inst, true
	}
	return entity.InstanceSnapshot{}, false
}

func renderDiagnosisContinuationReply(inst entity.InstanceSnapshot, action, raw string) string {
	if final, ok := isFinalReply(raw); ok {
		return final
	}
	var result diagnosis.DiagResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil || (result.Conclusion == "" && result.Suggestion == "") {
		return raw
	}
	label := diagnosisActionLabel(action)
	var b strings.Builder
	gpuSuffix := ""
	if strings.TrimSpace(inst.GpuType) != "" {
		gpuSuffix = "，GPU=" + strings.TrimSpace(inst.GpuType)
	}
	fmt.Fprintf(&b, "实例「%s」(%s%s) 的%s结果：%s", inst.Name, inst.UHostId, gpuSuffix, label, result.Conclusion)
	if result.Suggestion != "" {
		b.WriteString("\n\n建议：")
		b.WriteString(result.Suggestion)
	}
	return b.String()
}

func (e *Engine) synthesizeLoopCeilingFromInstanceContext(userMsg string) (string, bool) {
	if e == nil {
		return "", false
	}
	snapshot := e.RegistrySnapshot()
	if len(snapshot.Instances) == 0 {
		return "", false
	}
	if _, hasExplicitName := extractLikelyMissingInstanceName(userMsg); hasExplicitName {
		if _, found := resolveContinuationInstance(userMsg, snapshot); !found && requiresSpecificInstanceForLoopCeiling(userMsg) {
			return renderMissingInstanceLoopCeiling(userMsg), true
		}
	}
	inst, ok := resolveLoopCeilingInstance(userMsg, snapshot, e.sessionState.SelectedInstanceID)
	if !ok {
		if requiresSpecificInstanceForLoopCeiling(userMsg) {
			return renderMissingInstanceLoopCeiling(userMsg), true
		}
		if looksLikeAmbiguousInstanceListFollowup(userMsg, e.lastAssistantContent()) {
			return renderInstanceListLoopCeilingSummary(snapshot.Instances), true
		}
		return "", false
	}
	e.recordSelectedInstanceID(inst.UHostId, inst.Name)
	return renderInstanceLoopCeilingSummary(inst), true
}

func (e *Engine) correctFalseInstanceNotFoundReply(userMsg, content string) (string, bool) {
	if e == nil || !containsInstanceNotFoundClaim(content) {
		return "", false
	}
	snapshot := e.RegistrySnapshot()
	if len(snapshot.Instances) == 0 {
		return "", false
	}
	inst, ok := resolveLoopCeilingInstance(userMsg, snapshot, e.sessionState.SelectedInstanceID)
	if !ok {
		return "", false
	}
	if !notFoundClaimMentionsTargetOrInstance(content, inst) {
		return "", false
	}
	e.recordSelectedInstanceID(inst.UHostId, inst.Name)
	return renderInstanceFoundCorrection(inst), true
}

func (e *Engine) mayCorrectFalseInstanceNotFoundReply(userMsg string) bool {
	if e == nil {
		return false
	}
	snapshot := e.RegistrySnapshot()
	if len(snapshot.Instances) == 0 {
		return false
	}
	_, ok := resolveLoopCeilingInstance(userMsg, snapshot, e.sessionState.SelectedInstanceID)
	return ok
}

func containsInstanceNotFoundClaim(content string) bool {
	text := strings.TrimSpace(content)
	if text == "" {
		return false
	}
	for _, phrase := range []string{"没有找到", "未找到", "找不到", "查不到", "不存在"} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func notFoundClaimMentionsTargetOrInstance(content string, inst entity.InstanceSnapshot) bool {
	text := strings.TrimSpace(content)
	if text == "" {
		return false
	}
	for _, target := range []string{strings.TrimSpace(inst.UHostId), strings.TrimSpace(inst.Name)} {
		if target != "" && targetMissingClaimMentionsInstance(text, target) {
			return true
		}
	}
	for _, phrase := range []string{
		"没有找到目标实例", "未找到目标实例", "找不到目标实例", "查不到目标实例",
		"没有找到你要操作的实例", "未找到你要操作的实例", "找不到你要操作的实例", "查不到你要操作的实例",
		"没有找到您要操作的实例", "未找到您要操作的实例", "找不到您要操作的实例", "查不到您要操作的实例",
	} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func targetMissingClaimMentionsInstance(text, target string) bool {
	if !strings.Contains(text, target) || targetMentionLooksNestedResource(text, target) {
		return false
	}
	if strings.Contains(text, "名为") && strings.Contains(text, "实例") && containsInstanceNotFoundClaim(text) {
		return true
	}
	for _, prefix := range []string{"没有找到", "未找到", "找不到", "查不到"} {
		if strings.Contains(text, prefix+target) || strings.Contains(text, prefix+" "+target) ||
			strings.Contains(text, prefix+"「"+target+"」") || strings.Contains(text, prefix+"“"+target+"”") ||
			strings.Contains(text, prefix+" **\""+target+"\"**") {
			return true
		}
		if strings.Contains(text, prefix+"实例 "+target) || strings.Contains(text, prefix+"实例「"+target+"」") {
			return true
		}
	}
	return false
}

func targetMentionLooksNestedResource(text, target string) bool {
	nestedMarkers := []string{
		target + " 上", target + "上",
		target + " 中", target + "中",
		target + " 里", target + "里",
		target + " 内", target + "内",
		target + " 这台实例里", target + "这台实例里",
		target + " 这台实例中", target + "这台实例中",
		target + " 这个实例里", target + "这个实例里",
		target + " 这个实例中", target + "这个实例中",
	}
	for _, marker := range nestedMarkers {
		if strings.Contains(text, marker) && containsNestedResourceWord(text) {
			return true
		}
	}
	return false
}

func containsNestedResourceWord(text string) bool {
	for _, word := range []string{"文件", "模型", "目录", "路径", "节点", "服务", "端口", "容器", "环境"} {
		if strings.Contains(text, word) {
			return true
		}
	}
	return false
}

func resolveLoopCeilingInstance(userMsg string, snapshot entity.RegistrySnapshot, selectedID string) (entity.InstanceSnapshot, bool) {
	if inst, ok := resolveContinuationInstance(userMsg, snapshot); ok {
		return inst, true
	}
	if selectedID != "" && looksLikeSelectedInstanceFollowup(userMsg) {
		if inst, res := snapshot.ResolveByID(selectedID); res.Status == entity.ResolveHit && inst != nil {
			return *inst, true
		}
	}
	return entity.InstanceSnapshot{}, false
}

func renderInstanceFoundCorrection(inst entity.InstanceSnapshot) string {
	return "这台实例存在于当前实例列表中，当前可确认的是：\n\n" + renderInstanceSummaryBullets(inst)
}

func requiresSpecificInstanceForLoopCeiling(userMsg string) bool {
	text := strings.TrimSpace(userMsg)
	if text == "" {
		return false
	}
	if inferLifecycleAction(text) != "" || inferDiagnosisActionFromText(text) != "" {
		return true
	}
	return strings.Contains(text, "无卡模式")
}

func renderMissingInstanceLoopCeiling(userMsg string) string {
	if target, ok := extractLikelyMissingInstanceName(userMsg); ok {
		return fmt.Sprintf("我已经查过当前实例列表，但没有找到名为「%s」的实例。请确认实例名称是否正确，或直接提供 uhost- 开头的实例 ID。", target)
	}
	return "我已经查过当前实例列表，但没有定位到你要操作的具体实例。请提供准确的实例名称，或直接提供 uhost- 开头的实例 ID。"
}

func extractLikelyMissingInstanceName(userMsg string) (string, bool) {
	text := strings.TrimSpace(userMsg)
	if text == "" {
		return "", false
	}
	for _, token := range splitResourceTokens(text) {
		if strings.HasPrefix(strings.ToLower(token), "uhost-") {
			return token, true
		}
	}
	for _, marker := range []string{"这台实例", "这臺实例", "这个实例", "這個实例", "这台机器", "這台机器", "这个机器"} {
		idx := strings.Index(text, marker)
		if idx <= 0 {
			continue
		}
		if name, ok := cleanLikelyInstanceName(text[:idx]); ok {
			return name, true
		}
	}
	return "", false
}

func splitResourceTokens(text string) []string {
	var tokens []string
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		tokens = append(tokens, b.String())
		b.Reset()
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			b.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return tokens
}

func cleanLikelyInstanceName(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	value = strings.Trim(value, " \t\r\n:：,，.。;；!！?？[]【】()（）\"'`")
	for {
		trimmed := value
		for _, prefix := range []string{"帮我", "请帮我", "麻烦帮我", "麻烦", "请", "将", "把", "给我", "帮"} {
			trimmed = strings.TrimPrefix(trimmed, prefix)
		}
		trimmed = strings.TrimSpace(trimmed)
		if trimmed == value {
			break
		}
		value = trimmed
	}
	fields := splitResourceTokens(value)
	if len(fields) > 0 {
		value = fields[len(fields)-1]
	}
	value = strings.TrimSpace(value)
	if len([]rune(value)) < 3 {
		return "", false
	}
	switch normalizeResourceText(value) {
	case "", "instance", "host", "机器", "实例":
		return "", false
	default:
		return value, true
	}
}

func looksLikeSelectedInstanceFollowup(userMsg string) bool {
	text := strings.TrimSpace(userMsg)
	if text == "" {
		return false
	}
	if text == "?" || text == "？" {
		return true
	}
	compact := normalizeResourceText(text)
	if compact == "" {
		return false
	}
	for _, marker := range []string{"这台", "这臺", "它", "刚才那台", "刚刚那台", "上面那台", "当前这台", "那台"} {
		if strings.Contains(text, marker) || strings.Contains(compact, normalizeResourceText(marker)) {
			return true
		}
	}
	return false
}

func looksLikeInstanceStatusQuestion(userMsg string) bool {
	text := strings.TrimSpace(userMsg)
	if text == "" {
		return false
	}
	compact := normalizeResourceText(text)
	return strings.Contains(text, "实例") ||
		strings.Contains(text, "机器") ||
		strings.Contains(text, "状态") ||
		strings.Contains(strings.ToLower(text), "uhost") ||
		strings.Contains(compact, "gpu") ||
		looksLikeSelectedInstanceFollowup(text)
}

func renderInstanceLoopCeilingSummary(inst entity.InstanceSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "我已经查到这台实例的信息，但本轮没有继续生成完整说明。当前可确认的是：\n\n")
	b.WriteString(renderInstanceSummaryBullets(inst))
	b.WriteString("\n如果你要继续操作这台实例，请直接说明要做什么，例如开机、关机、重启、查监控或排查 SSH。")
	return b.String()
}

func looksLikeAmbiguousInstanceListFollowup(userMsg, lastAssistant string) bool {
	text := strings.TrimSpace(userMsg)
	if text == "" {
		return false
	}
	compact := normalizeResourceText(text)
	ambiguous := text == "?" || text == "？" ||
		compact == "什么意思" || compact == "啥意思" ||
		strings.Contains(text, "没看懂") || strings.Contains(text, "没明白") ||
		strings.Contains(text, "哪些") || strings.Contains(text, "哪几个") ||
		strings.Contains(text, "列全") || strings.Contains(text, "完整")
	if !ambiguous {
		return false
	}
	last := strings.TrimSpace(lastAssistant)
	return strings.Contains(last, "实例") || strings.Contains(strings.ToLower(last), "uhost")
}

func renderInstanceListLoopCeilingSummary(instances map[string]entity.InstanceSnapshot) string {
	total := len(instances)
	if total == 0 {
		return ""
	}
	copied := make([]entity.InstanceSnapshot, 0, total)
	for _, inst := range instances {
		copied = append(copied, inst)
	}
	sort.SliceStable(copied, func(i, j int) bool {
		if strings.TrimSpace(copied[i].Name) != strings.TrimSpace(copied[j].Name) {
			return strings.TrimSpace(copied[i].Name) < strings.TrimSpace(copied[j].Name)
		}
		return copied[i].UHostId < copied[j].UHostId
	})
	limit := total
	if limit > 10 {
		limit = 10
	}
	var b strings.Builder
	fmt.Fprintf(&b, "我刚才查到的是你当前账户里的实例列表，共 %d 台。你这条追问还不够具体，先把可确认的信息列出来：", total)
	for i := 0; i < limit; i++ {
		inst := copied[i]
		fmt.Fprintf(&b, "\n%d. %s（%s），状态：%s", i+1, emptyLabel(inst.Name), inst.UHostId, emptyStateLabel(inst.State))
		if strings.TrimSpace(inst.GpuType) != "" {
			fmt.Fprintf(&b, "，GPU：%s", inst.GpuType)
		}
		if strings.TrimSpace(inst.Zone) != "" {
			fmt.Fprintf(&b, "，可用区：%s", inst.Zone)
		}
	}
	if total > limit {
		fmt.Fprintf(&b, "\n另外还有 %d 台未展示。", total-limit)
	}
	b.WriteString("\n请直接告诉我你想看哪台实例，或想继续做什么，例如查某台状态、开机、关机、查监控。")
	return b.String()
}

func renderInstanceSummaryBullets(inst entity.InstanceSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- 实例：%s（%s）\n", emptyLabel(inst.Name), inst.UHostId)
	fmt.Fprintf(&b, "- 状态：%s\n", emptyStateLabel(inst.State))
	if strings.TrimSpace(inst.GpuType) != "" || inst.GPU > 0 {
		fmt.Fprintf(&b, "- GPU：%s", emptyLabel(inst.GpuType))
		if inst.GPU > 0 {
			fmt.Fprintf(&b, " × %d", inst.GPU)
		}
		b.WriteString("\n")
	}
	if inst.CPU > 0 || inst.Memory > 0 {
		fmt.Fprintf(&b, "- 配置：%d 核 CPU / %d MB 内存\n", inst.CPU, inst.Memory)
	}
	if strings.TrimSpace(inst.Zone) != "" {
		fmt.Fprintf(&b, "- 可用区：%s\n", inst.Zone)
	}
	return b.String()
}

func emptyLabel(value string) string {
	if strings.TrimSpace(value) == "" {
		return "未知"
	}
	return value
}

func diagnosisActionLabel(action string) string {
	switch action {
	case "DiagnoseSSH":
		return " SSH 诊断"
	case "DiagnoseGPU":
		return " GPU 诊断"
	case "DiagnoseInitFailure":
		return "初始化诊断"
	case "DiagnosePortOrFirewall":
		return "端口诊断"
	default:
		return "诊断"
	}
}

type lifecycleTargetStatus int

const (
	lifecycleTargetMissing lifecycleTargetStatus = iota
	lifecycleTargetUnresolved
	lifecycleTargetAmbiguous
	lifecycleTargetHit
)

func resolveSingleLifecycleTarget(plan intent.IntentRoute, snapshot entity.RegistrySnapshot) (entity.InstanceSnapshot, lifecycleTargetStatus) {
	if len(plan.Slots.TargetRefs) == 0 {
		return entity.InstanceSnapshot{}, lifecycleTargetMissing
	}
	var matches []entity.InstanceSnapshot
	for _, ref := range plan.Slots.TargetRefs {
		switch ref.Type {
		case intent.TargetRefUHostIDUserInput:
			inst, res := snapshot.ResolveByID(ref.Value)
			if res.Status != entity.ResolveHit || inst == nil {
				return entity.InstanceSnapshot{}, lifecycleTargetUnresolved
			}
			matches = append(matches, *inst)
		case intent.TargetRefName:
			found, res := snapshot.ResolveByName(ref.Value)
			if res.Status == entity.ResolveNotFoundInAccount || len(found) == 0 {
				return entity.InstanceSnapshot{}, lifecycleTargetUnresolved
			}
			if res.Status == entity.ResolveAmbiguous || len(found) > 1 {
				return entity.InstanceSnapshot{}, lifecycleTargetAmbiguous
			}
			matches = append(matches, *found[0])
		}
	}
	matches = dedupeResourceSelectionCandidates(matches)
	if len(matches) == 0 {
		return entity.InstanceSnapshot{}, lifecycleTargetMissing
	}
	if len(matches) > 1 {
		return entity.InstanceSnapshot{}, lifecycleTargetAmbiguous
	}
	return matches[0], lifecycleTargetHit
}

func lifecycleWorkflowName(action intent.LifecycleAction) (string, bool) {
	switch action {
	case intent.LifecycleActionStop:
		return "StopInstanceWorkflow", true
	case intent.LifecycleActionStart:
		return "StartInstanceWorkflow", true
	case intent.LifecycleActionReboot:
		return "RebootInstanceWorkflow", true
	case intent.LifecycleActionResize:
		return "ResizeInstanceWorkflow", true
	case intent.LifecycleActionRename:
		return "RenameInstanceWorkflow", true
	case intent.LifecycleActionResetPwd:
		return "ResetPasswordWorkflow", true
	case intent.LifecycleActionCreateDisk:
		return "CreateDiskWorkflow", true
	default:
		return "", false
	}
}

func lifecycleStateGateReply(action intent.LifecycleAction, inst entity.InstanceSnapshot) (string, bool) {
	state := strings.TrimSpace(inst.State)
	switch action {
	case intent.LifecycleActionStop:
		if strings.EqualFold(state, "Stopped") {
			return fmt.Sprintf("实例「%s」(%s) 当前已经是关机状态，无需再次关机。", inst.Name, inst.UHostId), true
		}
		if !strings.EqualFold(state, "Running") {
			return fmt.Sprintf("实例「%s」(%s) 当前状态为 %s，暂不能直接关机。", inst.Name, inst.UHostId, emptyStateLabel(state)), true
		}
	case intent.LifecycleActionStart:
		if strings.EqualFold(state, "Running") {
			return fmt.Sprintf("实例「%s」(%s) 当前已经是运行中，无需再次开机。", inst.Name, inst.UHostId), true
		}
		if !strings.EqualFold(state, "Stopped") {
			return fmt.Sprintf("实例「%s」(%s) 当前状态为 %s，暂不能直接开机。", inst.Name, inst.UHostId, emptyStateLabel(state)), true
		}
	case intent.LifecycleActionReboot:
		if !strings.EqualFold(state, "Running") {
			return fmt.Sprintf("实例「%s」(%s) 当前状态为 %s，只有运行中的实例才能重启。", inst.Name, inst.UHostId, emptyStateLabel(state)), true
		}
	}
	return "", false
}

func emptyStateLabel(state string) string {
	if strings.TrimSpace(state) == "" {
		return "未知"
	}
	return state
}

func inferLifecycleAction(userText string) intent.LifecycleAction {
	compact := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(strings.ToLower(userText))
	if looksLikeScheduledShutdownText(compact) {
		return ""
	}
	switch {
	case (strings.Contains(compact, "数据盘") || strings.Contains(compact, "datadisk")) &&
		(strings.Contains(compact, "创建") || strings.Contains(compact, "新建") || strings.Contains(compact, "加") || strings.Contains(compact, "挂载")):
		return intent.LifecycleActionCreateDisk
	case strings.Contains(compact, "重置密码") || strings.Contains(compact, "改密码") ||
		strings.Contains(compact, "重设密码") || strings.Contains(compact, "改密") ||
		strings.Contains(compact, "resetpassword") ||
		(strings.Contains(compact, "重置") && strings.Contains(compact, "密码")) ||
		(strings.Contains(compact, "reset") && strings.Contains(compact, "password")):
		return intent.LifecycleActionResetPwd
	case strings.Contains(compact, "改名") || strings.Contains(compact, "重命名") ||
		strings.Contains(compact, "修改名称") || strings.Contains(compact, "rename"):
		return intent.LifecycleActionRename
	case strings.Contains(compact, "改配") || strings.Contains(compact, "变配") ||
		strings.Contains(compact, "升级配置") || strings.Contains(compact, "调整配置") ||
		strings.Contains(compact, "resize"):
		return intent.LifecycleActionResize
	case strings.Contains(compact, "重启") || strings.Contains(compact, "reboot"):
		return intent.LifecycleActionReboot
	case strings.Contains(compact, "开机") || strings.Contains(compact, "启动") || strings.Contains(compact, "start"):
		return intent.LifecycleActionStart
	case strings.Contains(compact, "关机") || strings.Contains(compact, "关闭") ||
		strings.Contains(compact, "关掉") || strings.Contains(compact, "关下") ||
		strings.Contains(compact, "停机") || strings.Contains(compact, "停了") ||
		strings.Contains(compact, "shutdown") || strings.Contains(compact, "stop"):
		return intent.LifecycleActionStop
	default:
		return ""
	}
}

func looksLikeScheduledShutdownText(compact string) bool {
	if compact == "" {
		return false
	}
	if strings.Contains(compact, "取消定时关机") || strings.Contains(compact, "取消自动关机") || strings.Contains(compact, "取消延时关机") {
		return true
	}
	if strings.Contains(compact, "定时关机") || strings.Contains(compact, "自动关机") || strings.Contains(compact, "延时关机") {
		return true
	}
	if !(strings.Contains(compact, "关机") || strings.Contains(compact, "停机") || strings.Contains(compact, "shutdown") || strings.Contains(compact, "stop")) {
		return false
	}
	for _, unit := range []string{"分钟后", "小时后", "天后", "min后", "mins后", "hour后", "hours后"} {
		if strings.Contains(compact, unit) {
			return true
		}
	}
	return strings.Contains(compact, "之后关机") || strings.Contains(compact, "以后关机")
}

var (
	resizeCPUMemorySpecRE = regexp.MustCompile(`(?i)(\d+)\s*(?:c|cpu|vcpu|核)\s*[/,， ]*\s*(\d+)\s*(?:g|gb|gib)`)
	resizeGPUCountSpecRE  = regexp.MustCompile(`(?i)(\d+)\s*(?:张\s*)?(?:gpu|卡)`)
	resetPasswordSpecRE   = regexp.MustCompile(`(?i)(?:密码|改密|password)\s*(?:为|成|是|:|：)?\s*([^\s，。；;]+)`)
	diskSizeSpecRE        = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(?:g|gb|gib)`)
	cfsNameSpecRE         = regexp.MustCompile(`(?i)(?:名字|名称|name)\s*(?:叫|为|是|:|：)?\s*([a-zA-Z0-9][a-zA-Z0-9_-]{1,62})`)
	cfsIDSpecRE           = regexp.MustCompile(`(?i)cfs-[a-z0-9-]+`)
	cfsZoneIDSpecRE       = regexp.MustCompile(`(?i)cn-[a-z0-9]+-\d+`)
	stopAfterHoursRE      = regexp.MustCompile(`(?i)(\d+)\s*(?:个)?\s*(?:小时|hour|hours|h)\s*后`)
	stopAfterMinutesRE    = regexp.MustCompile(`(?i)(\d+)\s*(?:分钟|分|min|mins|minute|minutes|m)\s*后`)
)

const maxInlineResizeGPUCount = 16

func resizeWorkflowArgsFromUserText(userText string) map[string]any {
	args := map[string]any{}
	if m := resizeCPUMemorySpecRE.FindStringSubmatch(userText); len(m) == 3 {
		if cpu, ok := parseFloatString(m[1]); ok && cpu > 0 {
			args["Cpu"] = cpu
		}
		if memGB, ok := parseFloatString(m[2]); ok && memGB > 0 {
			args["Memory"] = memGB * 1024
		}
	}
	if m := resizeGPUCountSpecRE.FindStringSubmatch(userText); len(m) == 2 {
		if gpu, ok := parseFloatString(m[1]); ok && gpu >= 0 && gpu <= maxInlineResizeGPUCount {
			args["Gpu"] = gpu
		}
	}
	return args
}

func createDiskSizeFromUserText(userText string) float64 {
	matches := diskSizeSpecRE.FindAllStringSubmatch(userText, -1)
	for _, m := range matches {
		if len(m) != 2 {
			continue
		}
		if size, ok := parseFloatString(m[1]); ok && size > 0 {
			return size
		}
	}
	return 0
}

func cfsWriteWorkflowFromUserText(userText string) (string, bool) {
	compact := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(strings.ToLower(userText))
	if compact == "" {
		return "", false
	}
	if !(strings.Contains(compact, "cfs") || strings.Contains(compact, "共享文件存储")) {
		return "", false
	}
	if cfsReadOnlyCue(compact) {
		return "", false
	}
	if strings.Contains(compact, "扩容") || strings.Contains(compact, "扩大") || strings.Contains(compact, "resize") {
		return "ResizeCFSWorkflow", true
	}
	if strings.Contains(compact, "创建") || strings.Contains(compact, "新建") || strings.Contains(compact, "开通") || strings.Contains(compact, "购买") {
		return "CreateCFSWorkflow", true
	}
	return "", false
}

func cfsReadOnlyCue(compact string) bool {
	for _, cue := range []string{
		"多少钱", "价格", "费用", "询价", "退费", "退款",
		"有哪些", "列表", "查询", "状态", "能不能", "是否", "怎么",
	} {
		if strings.Contains(compact, cue) {
			return true
		}
	}
	return false
}

func cfsWorkflowArgsFromUserText(workflowName, userText string) (map[string]any, string, bool) {
	switch workflowName {
	case "CreateCFSWorkflow":
		name := cfsNameFromUserText(userText)
		if name == "" {
			return nil, "创建 CFS 需要指定名称，请告诉我要创建的 CFS 名称。", false
		}
		size := createDiskSizeFromUserText(userText)
		if size <= 0 {
			return nil, "创建 CFS 需要指定容量（GB），例如 100GB。", false
		}
		zone := cfsZoneFromUserText(userText)
		if zone == "" {
			return nil, "创建 CFS 需要指定可用区；CFS 只支持上游返回的 Pod/容器可用区。", false
		}
		return map[string]any{"Name": name, "Size": size, "Zone": zone}, "", true
	case "ResizeCFSWorkflow":
		cfsID := strings.ToLower(strings.TrimSpace(cfsIDSpecRE.FindString(userText)))
		if cfsID == "" {
			return nil, "扩容 CFS 需要指定 CFS ID，例如 cfs-xxxx。", false
		}
		size := createDiskSizeFromUserText(userText)
		if size <= 0 {
			return nil, "扩容 CFS 需要指定目标容量（GB），例如 200GB。", false
		}
		return map[string]any{"CfsId": cfsID, "Size": size}, "", true
	default:
		return nil, "", false
	}
}

func cfsNameFromUserText(userText string) string {
	if m := cfsNameSpecRE.FindStringSubmatch(strings.TrimSpace(userText)); len(m) == 2 {
		return strings.Trim(m[1], "“”\"'，。；; ")
	}
	return ""
}

func cfsZoneFromUserText(userText string) string {
	text := strings.TrimSpace(userText)
	if zone := cfsZoneIDSpecRE.FindString(text); zone != "" {
		return strings.TrimSpace(zone)
	}
	for _, marker := range []string{"创建", "新建", "开通", "购买"} {
		if idx := strings.Index(text, marker); idx > 0 {
			prefix := strings.TrimSpace(text[:idx])
			if at := strings.LastIndex(prefix, "在"); at >= 0 {
				prefix = strings.TrimSpace(prefix[at+len("在"):])
			}
			prefix = strings.Trim(prefix, "“”\"'，。；; ")
			if prefix != "" && len([]rune(prefix)) <= 12 {
				return prefix
			}
		}
	}
	return ""
}

func parseFloatString(s string) (float64, bool) {
	var v float64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%f", &v); err != nil {
		return 0, false
	}
	return v, true
}

func renameWorkflowNameFromUserText(userText string) string {
	text := strings.TrimSpace(userText)
	for _, marker := range []string{"修改名称为", "重命名为", "改名为", "改名叫", "命名为"} {
		if idx := strings.LastIndex(text, marker); idx >= 0 {
			name := strings.TrimSpace(text[idx+len(marker):])
			return strings.Trim(name, "“”\"'")
		}
	}
	return ""
}

func resetPasswordFromUserText(userText string) string {
	m := resetPasswordSpecRE.FindStringSubmatch(strings.TrimSpace(userText))
	if len(m) != 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func stopSchedulerAfterMinutesFromUserText(userText string) (int, bool) {
	if m := stopAfterHoursRE.FindStringSubmatch(userText); len(m) == 2 {
		if hours, ok := parsePositiveInt(m[1]); ok {
			return hours * 60, true
		}
	}
	if m := stopAfterMinutesRE.FindStringSubmatch(userText); len(m) == 2 {
		if minutes, ok := parsePositiveInt(m[1]); ok {
			return minutes, true
		}
	}
	return 0, false
}

func parsePositiveInt(s string) (int, bool) {
	var v int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &v); err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

func workflowFailureMessage(raw string) (string, bool) {
	var result struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return "", false
	}
	if result.Success || strings.TrimSpace(result.Message) == "" {
		return "", false
	}
	return workflowStepPrefixRE.ReplaceAllString(strings.TrimSpace(result.Message), ""), true
}
