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
)

const maxResourceSelectionCandidates = 20

type pendingResourceSelection struct {
	originalUserMsg string
	plan            intent.Plan
	snapshot        entity.RegistrySnapshot
	candidates      []entity.InstanceSnapshot
	truncated       bool
	createdTurn     int
	invalidAttempts int
}

type resourceSelectionMatch struct {
	instance  entity.InstanceSnapshot
	ok        bool
	ambiguous bool
}

func renderResourceSelectionPrompt(p pendingResourceSelection) string {
	var b strings.Builder
	b.WriteString("\u6211\u9700\u8981\u5148\u786e\u8ba4\u4f60\u8981\u67e5\u770b\u54ea\u53f0\u5b9e\u4f8b\u3002\u8bf7\u9009\u62e9\u4e00\u4e2a\uff1a\n\n")
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

func isResourceSelectionExpired(currentTurn int, p pendingResourceSelection) bool {
	return currentTurn > p.createdTurn+2
}

func isResourceSelectionFallbackReason(reason intent.FallbackReason) bool {
	switch reason {
	case intent.FallbackMissingTarget, intent.FallbackUnresolvedTarget, intent.FallbackAmbiguousTarget:
		return true
	default:
		return false
	}
}

func (e *Engine) buildResourceSelectionForPlan(ctx context.Context, result intent.PlannerResult, snapshot entity.RegistrySnapshot, _ func(StepEvent)) (*pendingResourceSelection, bool, error) {
	if result.Plan.Intent != intent.IntentMonitorQuery {
		return nil, false, nil
	}
	candidates, refreshedSnapshot, truncated, ok, err := e.candidateInstancesForSelection(ctx, result.Plan, snapshot, nil)
	if err != nil {
		return nil, false, err
	}
	if !ok || len(candidates) == 0 {
		return nil, false, nil
	}
	return &pendingResourceSelection{
		originalUserMsg: e.lastUserMsg,
		plan:            result.Plan,
		snapshot:        refreshedSnapshot,
		candidates:      candidates,
		truncated:       truncated,
		createdTurn:     e.userTurn,
	}, true, nil
}

func (e *Engine) candidateInstancesForSelection(ctx context.Context, plan intent.Plan, snapshot entity.RegistrySnapshot, _ func(StepEvent)) ([]entity.InstanceSnapshot, entity.RegistrySnapshot, bool, bool, error) {
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

func matchingSelectionCandidates(plan intent.Plan, snapshot entity.RegistrySnapshot) []entity.InstanceSnapshot {
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

func planWithSelectedResource(plan intent.Plan, uhostID string) intent.Plan {
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
