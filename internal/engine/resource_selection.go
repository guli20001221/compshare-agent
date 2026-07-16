package engine

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/readprojection"
)

const maxResourceSelectionCandidates = 20
const pendingSelectionTTLSeconds = 300
const pendingSelectionKindInstance = "instance"

type pendingResourceSelection struct {
	originalUserMsg string
	plan            intent.IntentRoute
	snapshot        entity.RegistrySnapshot
	candidates      []entity.InstanceSnapshot
	truncated       bool
	createdTurn     int
	invalidAttempts int
	// notFoundRef, when non-empty, is a uhost-ID the user typed that did NOT
	// resolve to any of their instances. The prompt then leads with "未找到实例
	// X" so a wrong/typo'd ID is distinguished from "no instance specified".
	notFoundRef string
}

// findExplicitInstanceRef scans a raw user message for an explicit instance ID and
// resolves it against the snapshot. This is the deterministic backstop for the
// intent router intermittently NOT extracting a literal ID into Slots.TargetRefs
// (Rule 5: a regex-matchable literal is resolved by code, not the LLM). Returns
// the matched instance when an ID resolves; otherwise returns the first
// unresolved ID-shaped token so the caller can say "未找到 X".
func findExplicitInstanceRef(msg string, snapshot entity.RegistrySnapshot) (*entity.InstanceSnapshot, string) {
	hits, unresolved := snapshot.ResolveInstanceRefsInText(msg)
	if len(hits) > 0 {
		return hits[0], ""
	}
	if len(unresolved) > 0 {
		return nil, unresolved[0]
	}
	return nil, ""
}

type resourceSelectionMatch struct {
	instance  entity.InstanceSnapshot
	ok        bool
	ambiguous bool
}

func renderResourceSelectionPrompt(p pendingResourceSelection) string {
	var b strings.Builder
	if p.notFoundRef != "" {
		fmt.Fprintf(&b, "\u672a\u627e\u5230\u5b9e\u4f8b %s\uff0c\u8bf7\u786e\u8ba4\u5b9e\u4f8b ID \u662f\u5426\u6b63\u786e\u3002\u4ee5\u4e0b\u662f\u60a8\u540d\u4e0b\u7684\u5b9e\u4f8b\uff0c\u8bf7\u9009\u62e9\u4e00\u4e2a\uff1a\n\n", sanitizeResourceSelectionPromptField(p.notFoundRef))
	} else {
		b.WriteString("\u6211\u9700\u8981\u5148\u786e\u8ba4\u4f60\u8981\u67e5\u770b\u54ea\u53f0\u5b9e\u4f8b\u3002\u8bf7\u9009\u62e9\u4e00\u4e2a\uff1a\n\n")
	}
	for i, inst := range p.candidates {
		fmt.Fprintf(
			&b,
			"%d. %s (%s) - %s, GPU=%s x%d, CPU=%d, \u5185\u5b58=%d MB, %s, charge=%s\n",
			i+1,
			sanitizeResourceSelectionPromptField(inst.Name),
			sanitizeResourceSelectionPromptField(inst.UHostId),
			sanitizeResourceSelectionPromptField(inst.State),
			sanitizeResourceSelectionPromptField(inst.GpuType),
			inst.GPU,
			inst.CPU,
			inst.Memory,
			sanitizeResourceSelectionPromptField(inst.Zone),
			sanitizeResourceSelectionPromptField(inst.ChargeType),
		)
	}
	if p.truncated {
		b.WriteString("\n这里只显示按实例 ID 排序后的前 20 个候选。你可以回复更具体的实例名称或实例 ID 来缩小范围。\n")
	}
	if len(p.candidates) == 1 {
		b.WriteString("\n你可以回复 1、实例 ID 或完整实例名称。")
	} else {
		fmt.Fprintf(&b, "\n你可以回复序号（1-%d）、实例 ID 或完整实例名称。", len(p.candidates))
	}
	return b.String()
}

func matchResourceSelection(input string, p pendingResourceSelection) resourceSelectionMatch {
	query := strings.TrimSpace(input)
	if query == "" {
		return resourceSelectionMatch{}
	}

	for _, inst := range p.candidates {
		if query == inst.UHostId {
			return resourceSelectionMatch{instance: inst, ok: true}
		}
	}

	var nameMatches []entity.InstanceSnapshot
	for _, inst := range p.candidates {
		if query == inst.Name {
			nameMatches = append(nameMatches, inst)
		}
	}
	ordinalMatch, ordinalOK := resourceSelectionOrdinalMatch(query, p)
	if len(nameMatches) == 1 {
		if ordinalOK && ordinalMatch.UHostId != nameMatches[0].UHostId {
			return resourceSelectionMatch{ambiguous: true}
		}
		return resourceSelectionMatch{instance: nameMatches[0], ok: true}
	}
	if len(nameMatches) > 1 {
		return resourceSelectionMatch{ambiguous: true}
	}
	if ordinalOK {
		return resourceSelectionMatch{instance: ordinalMatch, ok: true}
	}
	return resourceSelectionMatch{}
}

func matchResourceSelectionReference(input string, p pendingResourceSelection) (resourceSelectionMatch, bool) {
	exact := matchResourceSelection(input, p)
	if exact.ok || exact.ambiguous {
		return exact, true
	}
	if ordinal, ok := extractResourceSelectionOrdinal(input); ok {
		index := ordinal - 1
		if index < 0 || index >= len(p.candidates) {
			return resourceSelectionMatch{}, false
		}
		return resourceSelectionMatch{instance: p.candidates[index], ok: true}, false
	}
	for _, token := range p.snapshot.InstanceIDTokensInText(input) {
		for _, inst := range p.candidates {
			if token == inst.UHostId {
				return resourceSelectionMatch{instance: inst, ok: true}, false
			}
		}
	}
	return resourceSelectionMatch{}, false
}

func resourceSelectionLooksLikeReply(input string, p pendingResourceSelection) bool {
	query := strings.TrimSpace(input)
	if query == "" {
		return true
	}
	match := matchResourceSelection(query, p)
	if match.ok || match.ambiguous {
		return true
	}
	if _, ok := extractResourceSelectionOrdinal(query); ok {
		return true
	}
	if tokens := p.snapshot.InstanceIDTokensInText(query); len(tokens) == 1 && tokens[0] == query {
		return true
	}
	return false
}

func removeResourceSelectionOrdinalTokens(input string, ordinal int) string {
	if ordinal <= 0 {
		return input
	}
	chinese := ""
	numerals := chineseResourceSelectionNumerals()
	if ordinal <= len(numerals) {
		chinese = numerals[ordinal-1]
	}
	arabic := strconv.Itoa(ordinal)
	tokens := []string{
		"第" + arabic + "台实例",
		"第" + arabic + "实例",
		"第" + arabic + "台",
		"第" + arabic + "机器",
		"第" + arabic + "主机",
		"第" + arabic,
	}
	if chinese != "" {
		tokens = append(tokens,
			"第"+chinese+"台实例",
			"第"+chinese+"实例",
			"第"+chinese+"台",
			"第"+chinese+"机器",
			"第"+chinese+"主机",
			"第"+chinese,
		)
	}
	for _, token := range tokens {
		input = strings.ReplaceAll(input, token, "")
	}
	return input
}

func renderResourceSelectionSelectedReply(inst entity.InstanceSnapshot) string {
	reply := fmt.Sprintf("已选中 %s（%s）。你接下来想查看监控、重启，还是执行其他操作？",
		sanitizeResourceSelectionPromptField(inst.Name),
		sanitizeResourceSelectionPromptField(inst.UHostId),
	)
	if inst.Name == "" {
		reply = fmt.Sprintf("已选中 %s。你接下来想查看监控、重启，还是执行其他操作？",
			sanitizeResourceSelectionPromptField(inst.UHostId),
		)
	}
	return reply
}

func isResourceSelectionExpired(currentTurn int, p pendingResourceSelection) bool {
	return currentTurn > p.createdTurn+2
}

func isPersistedSelectionExpired(nowUnix int64, state SessionState) bool {
	if state.PendingSelectionKind == "" || len(state.PendingSelectionItems) == 0 {
		return true
	}
	ttl := state.PendingSelectionTTLSeconds
	if ttl <= 0 {
		ttl = pendingSelectionTTLSeconds
	}
	if state.PendingSelectionProducedAtUnix <= 0 {
		return true
	}
	return nowUnix > state.PendingSelectionProducedAtUnix+int64(ttl)
}

func isResourceSelectionFallbackReason(reason intent.FallbackReason) bool {
	switch reason {
	case intent.FallbackMissingTarget, intent.FallbackUnresolvedTarget, intent.FallbackAmbiguousTarget:
		return true
	default:
		return false
	}
}

func (e *Engine) recordPendingInstanceSelection(instances []entity.InstanceSnapshot, sourceIntent intent.Intent, originalUserMsg string, total int, truncated bool) {
	if e == nil || !e.sessionStateHydrated || len(instances) == 0 {
		return
	}
	candidates := append([]entity.InstanceSnapshot(nil), instances...)
	if sourceIntent == intent.IntentResourceInfo && len(candidates) > readprojection.DefaultMaxInstancesPerDisplay {
		candidates = candidates[:readprojection.DefaultMaxInstancesPerDisplay]
		truncated = true
	}
	limited := false
	if len(candidates) > maxResourceSelectionCandidates {
		candidates = candidates[:maxResourceSelectionCandidates]
		limited = true
	}
	if total <= 0 {
		total = len(instances)
	}
	items := make([]PendingSelectionItem, 0, len(candidates))
	for i, inst := range candidates {
		if inst.UHostId == "" {
			continue
		}
		items = append(items, pendingSelectionItemFromInstance(i+1, inst))
	}
	if len(items) == 0 {
		return
	}
	e.sessionState.PendingSelectionKind = pendingSelectionKindInstance
	e.sessionState.PendingSelectionIntent = string(sourceIntent)
	e.sessionState.PendingSelectionOriginalUserMsg = strings.TrimSpace(originalUserMsg)
	e.sessionState.PendingSelectionCreatedTurn = e.userTurn
	e.sessionState.PendingSelectionProducedAtUnix = time.Now().Unix()
	e.sessionState.PendingSelectionTTLSeconds = pendingSelectionTTLSeconds
	e.sessionState.PendingSelectionTruncated = truncated || limited || total > len(items)
	e.sessionState.PendingSelectionTotalCount = total
	e.sessionState.PendingSelectionItems = items
}

func (e *Engine) recordPendingSelectionFromHandlerResult(result intent.HandlerResult, plan intent.IntentRoute, originalUserMsg string) {
	if e == nil || result.Status != intent.HandlerStatusHandled || plan.Intent != intent.IntentResourceInfo {
		return
	}
	e.recordPendingInstanceSelection(
		result.ResourceSelectionCandidates,
		plan.Intent,
		originalUserMsg,
		len(result.ResourceSelectionCandidates),
		false,
	)
}

func pendingSelectionItemFromInstance(index int, inst entity.InstanceSnapshot) PendingSelectionItem {
	return PendingSelectionItem{
		Index:      index,
		ID:         inst.UHostId,
		Name:       inst.Name,
		State:      inst.State,
		GPU:        inst.GPU,
		GpuType:    inst.GpuType,
		CPU:        inst.CPU,
		Memory:     inst.Memory,
		Zone:       inst.Zone,
		Region:     inst.Region,
		ChargeType: inst.ChargeType,
	}
}

func instanceFromPendingSelectionItem(item PendingSelectionItem) entity.InstanceSnapshot {
	return entity.InstanceSnapshot{
		UHostId:    item.ID,
		Name:       item.Name,
		State:      item.State,
		GPU:        item.GPU,
		GpuType:    item.GpuType,
		CPU:        item.CPU,
		Memory:     item.Memory,
		Zone:       item.Zone,
		Region:     item.Region,
		ChargeType: item.ChargeType,
	}
}

func (e *Engine) clearPendingSelection() {
	if e == nil {
		return
	}
	e.sessionState.PendingSelectionKind = ""
	e.sessionState.PendingSelectionIntent = ""
	e.sessionState.PendingSelectionOriginalUserMsg = ""
	e.sessionState.PendingSelectionCreatedTurn = 0
	e.sessionState.PendingSelectionProducedAtUnix = 0
	e.sessionState.PendingSelectionTTLSeconds = 0
	e.sessionState.PendingSelectionTruncated = false
	e.sessionState.PendingSelectionTotalCount = 0
	e.sessionState.PendingSelectionItems = nil
}

func (e *Engine) pendingResourceSelectionFromSession() (*pendingResourceSelection, bool) {
	if e == nil || !e.sessionStateHydrated {
		return nil, false
	}
	if e.sessionState.PendingSelectionKind != pendingSelectionKindInstance || len(e.sessionState.PendingSelectionItems) == 0 {
		return nil, false
	}
	if isPersistedSelectionExpired(time.Now().Unix(), e.sessionState) {
		e.clearPendingSelection()
		return nil, false
	}
	candidates := make([]entity.InstanceSnapshot, 0, len(e.sessionState.PendingSelectionItems))
	for _, item := range e.sessionState.PendingSelectionItems {
		inst := instanceFromPendingSelectionItem(item)
		if inst.UHostId == "" {
			continue
		}
		candidates = append(candidates, inst)
	}
	if len(candidates) == 0 {
		e.clearPendingSelection()
		return nil, false
	}
	truncated := e.sessionState.PendingSelectionTruncated
	if len(candidates) > readprojection.DefaultMaxInstancesPerDisplay {
		candidates = candidates[:readprojection.DefaultMaxInstancesPerDisplay]
		truncated = true
	}
	planIntent := intent.Intent(e.sessionState.PendingSelectionIntent)
	if planIntent == "" {
		planIntent = intent.IntentResourceInfo
	}
	snapshot := snapshotFromPendingSelectionCandidates(candidates)
	return &pendingResourceSelection{
		originalUserMsg: e.sessionState.PendingSelectionOriginalUserMsg,
		plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        planIntent,
		},
		snapshot:    snapshot,
		candidates:  candidates,
		truncated:   truncated,
		createdTurn: e.userTurn,
	}, true
}

func snapshotFromPendingSelectionCandidates(candidates []entity.InstanceSnapshot) entity.RegistrySnapshot {
	instances := make(map[string]entity.InstanceSnapshot, len(candidates))
	nameIndex := make(map[string][]string, len(candidates))
	for _, inst := range candidates {
		if inst.UHostId == "" {
			continue
		}
		instances[inst.UHostId] = inst
		if inst.Name != "" {
			nameIndex[strings.ToLower(strings.TrimSpace(inst.Name))] = append(nameIndex[strings.ToLower(strings.TrimSpace(inst.Name))], inst.UHostId)
		}
	}
	return entity.RegistrySnapshot{
		Instances:    instances,
		NameIndex:    nameIndex,
		LastFullSync: time.Now(),
		TotalCount:   len(candidates),
	}
}

func (e *Engine) candidateInstancesForSelection(ctx context.Context, plan intent.IntentRoute, snapshot entity.RegistrySnapshot, _ func(StepEvent)) ([]entity.InstanceSnapshot, entity.RegistrySnapshot, bool, bool, error) {
	snapshot, ok, err := e.freshResourceSelectionSnapshot(ctx, snapshot)
	if err != nil {
		return nil, entity.RegistrySnapshot{}, false, false, err
	}
	if !ok {
		return nil, entity.RegistrySnapshot{}, false, false, nil
	}

	var candidates []entity.InstanceSnapshot
	if len(plan.Slots.TargetRefs) == 0 {
		candidates = instancesFromSelectionSnapshot(snapshot)
	} else {
		candidates = matchingSelectionCandidates(plan, snapshot)
		if len(candidates) == 0 {
			candidates = instancesFromSelectionSnapshot(snapshot)
		}
	}
	candidates, truncated := sortAndLimitResourceSelectionCandidates(candidates)
	return candidates, snapshot, truncated, len(candidates) > 0, nil
}

func (e *Engine) freshResourceSelectionSnapshot(ctx context.Context, snapshot entity.RegistrySnapshot) (entity.RegistrySnapshot, bool, error) {
	if e == nil || e.registry == nil {
		return snapshot, len(snapshot.Instances) > 0 && !snapshot.LastFullSync.IsZero(), nil
	}
	if e.registry.NeedsRefresh(time.Now()) || len(snapshot.Instances) == 0 {
		if _, err := e.refreshRegistry(ctx, entity.RefreshReasonTTL); err != nil {
			return entity.RegistrySnapshot{}, false, err
		}
		return e.RegistrySnapshot(), true, nil
	}
	if snapshot.LastFullSync.IsZero() || len(snapshot.Instances) == 0 {
		snapshot = e.RegistrySnapshot()
	}
	return snapshot, !snapshot.LastFullSync.IsZero() && len(snapshot.Instances) > 0, nil
}

func matchingSelectionCandidates(plan intent.IntentRoute, snapshot entity.RegistrySnapshot) []entity.InstanceSnapshot {
	var candidates []entity.InstanceSnapshot
	for _, ref := range plan.Slots.TargetRefs {
		switch ref.Type {
		case intent.TargetRefName:
			matches, res := snapshot.ResolveByName(ref.Value)
			if res.Status != entity.ResolveHit && res.Status != entity.ResolveAmbiguous {
				continue
			}
			for _, match := range matches {
				if match != nil {
					candidates = append(candidates, *match)
				}
			}
		case intent.TargetRefUHostIDUserInput:
			if inst, res := snapshot.ResolveByID(ref.Value); res.Status == entity.ResolveHit && inst != nil {
				candidates = append(candidates, *inst)
			}
		}
	}
	return dedupeResourceSelectionCandidates(candidates)
}

func instancesFromSelectionSnapshot(snapshot entity.RegistrySnapshot) []entity.InstanceSnapshot {
	candidates := make([]entity.InstanceSnapshot, 0, len(snapshot.Instances))
	for _, inst := range snapshot.Instances {
		candidates = append(candidates, inst)
	}
	return candidates
}

func sortAndLimitResourceSelectionCandidates(candidates []entity.InstanceSnapshot) ([]entity.InstanceSnapshot, bool) {
	candidates = dedupeResourceSelectionCandidates(candidates)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].UHostId < candidates[j].UHostId
	})
	if len(candidates) > maxResourceSelectionCandidates {
		return candidates[:maxResourceSelectionCandidates], true
	}
	return candidates, false
}

func dedupeResourceSelectionCandidates(candidates []entity.InstanceSnapshot) []entity.InstanceSnapshot {
	if len(candidates) < 2 {
		return candidates
	}
	seen := make(map[string]struct{}, len(candidates))
	out := make([]entity.InstanceSnapshot, 0, len(candidates))
	for _, candidate := range candidates {
		if _, ok := seen[candidate.UHostId]; ok {
			continue
		}
		seen[candidate.UHostId] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

func planWithSelectedResource(plan intent.IntentRoute, uhostID string) intent.IntentRoute {
	plan.Slots.TargetRefs = []intent.TargetRef{{
		Type:       intent.TargetRefUHostIDUserInput,
		Value:      uhostID,
		Source:     intent.SourcePriorTurn,
		SourceSpan: uhostID,
	}}
	return plan
}

func sanitizeResourceSelectionPromptField(value string) string {
	var b strings.Builder
	lastWasSpace := false
	for _, r := range value {
		if r == '\r' || r == '\n' || r == '\t' || unicode.IsControl(r) {
			if !lastWasSpace {
				b.WriteByte(' ')
				lastWasSpace = true
			}
			continue
		}
		b.WriteRune(r)
		lastWasSpace = unicode.IsSpace(r)
	}
	return strings.TrimSpace(b.String())
}

func resourceSelectionOrdinalMatch(input string, p pendingResourceSelection) (entity.InstanceSnapshot, bool) {
	ordinal, ok := parseResourceSelectionOrdinal(input)
	if !ok {
		return entity.InstanceSnapshot{}, false
	}
	index := ordinal - 1
	if index < 0 || index >= len(p.candidates) {
		return entity.InstanceSnapshot{}, false
	}
	return p.candidates[index], true
}

func parseResourceSelectionOrdinal(input string) (int, bool) {
	if n, err := strconv.Atoi(input); err == nil {
		return n, true
	}

	for i, numeral := range chineseResourceSelectionNumerals() {
		n := i + 1
		if _, ok := ordinalPhraseSet(n, numeral)[input]; ok {
			return n, true
		}
	}
	return 0, false
}

func extractResourceSelectionOrdinal(input string) (int, bool) {
	query := strings.TrimSpace(input)
	if query == "" {
		return 0, false
	}
	if n, ok := parseResourceSelectionOrdinal(query); ok {
		return n, true
	}
	compact := compactResourceSelectionText(query)
	numerals := chineseResourceSelectionNumerals()
	for i := len(numerals) - 1; i >= 0; i-- {
		n := i + 1
		numeral := numerals[i]
		arabic := strconv.Itoa(n)
		for _, token := range []string{
			"\u7b2c" + numeral + "\u53f0",
			"\u7b2c" + numeral + "\u5b9e\u4f8b",
			"\u7b2c" + numeral + "\u53f0\u5b9e\u4f8b",
			"\u7b2c" + numeral + "\u673a\u5668",
			"\u7b2c" + numeral + "\u4e3b\u673a",
			"\u7b2c" + arabic + "\u53f0",
			"\u7b2c" + arabic + "\u5b9e\u4f8b",
			"\u7b2c" + arabic + "\u53f0\u5b9e\u4f8b",
			"\u7b2c" + arabic + "\u673a\u5668",
			"\u7b2c" + arabic + "\u4e3b\u673a",
		} {
			if strings.Contains(compact, token) {
				return n, true
			}
		}
	}
	return 0, false
}

func compactResourceSelectionText(input string) string {
	var b strings.Builder
	for _, r := range input {
		if unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func ordinalPhraseSet(n int, chinese string) map[string]struct{} {
	arabic := strconv.Itoa(n)
	phrases := []string{
		"\u7b2c" + chinese,
		"\u7b2c" + chinese + "\u53f0",
		"\u9009\u7b2c" + chinese,
		"\u9009\u7b2c" + chinese + "\u53f0",
		"\u7b2c" + arabic,
		"\u7b2c" + arabic + "\u53f0",
		"\u9009\u7b2c" + arabic,
		"\u9009\u7b2c" + arabic + "\u53f0",
	}
	set := make(map[string]struct{}, len(phrases))
	for _, phrase := range phrases {
		set[phrase] = struct{}{}
	}
	return set
}

func chineseResourceSelectionNumerals() []string {
	return []string{
		"\u4e00",
		"\u4e8c",
		"\u4e09",
		"\u56db",
		"\u4e94",
		"\u516d",
		"\u4e03",
		"\u516b",
		"\u4e5d",
		"\u5341",
		"\u5341\u4e00",
		"\u5341\u4e8c",
		"\u5341\u4e09",
		"\u5341\u56db",
		"\u5341\u4e94",
		"\u5341\u516d",
		"\u5341\u4e03",
		"\u5341\u516b",
		"\u5341\u4e5d",
		"\u4e8c\u5341",
	}
}
