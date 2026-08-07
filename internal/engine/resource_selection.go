package engine

import (
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/compshare-agent/internal/entity"
)

const maxResourceSelectionCandidates = 20
const pendingSelectionTTLSeconds = 300
const pendingSelectionKindInstance = "instance"

// pendingResourceSelection is the execution-side proof for a numbered instance
// list. It intentionally contains only the candidates needed to resolve an
// explicit ordinal, ID, or exact name on the next turn.
type pendingResourceSelection struct {
	snapshot   entity.RegistrySnapshot
	candidates []entity.InstanceSnapshot
}

// findExplicitInstanceRef scans a raw user message for an explicit instance ID
// and resolves it against the snapshot. It is a deterministic backstop for a
// literal ID that the model did not surface.
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

func (e *Engine) recordPendingInstanceSelection(instances []entity.InstanceSnapshot) {
	if e == nil || !e.sessionStateHydrated || len(instances) == 0 {
		return
	}
	candidates := append([]entity.InstanceSnapshot(nil), instances...)
	if len(candidates) > maxResourceSelectionCandidates {
		candidates = candidates[:maxResourceSelectionCandidates]
	}
	items := make([]PendingSelectionItem, 0, len(candidates))
	for i, inst := range candidates {
		if inst.UHostId != "" {
			items = append(items, pendingSelectionItemFromInstance(i+1, inst))
		}
	}
	if len(items) == 0 {
		return
	}
	e.sessionState.PendingSelectionKind = pendingSelectionKindInstance
	e.sessionState.PendingSelectionProducedAtUnix = time.Now().Unix()
	e.sessionState.PendingSelectionTTLSeconds = pendingSelectionTTLSeconds
	e.sessionState.PendingSelectionItems = items
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
	e.sessionState.PendingSelectionProducedAtUnix = 0
	e.sessionState.PendingSelectionTTLSeconds = 0
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
		if inst := instanceFromPendingSelectionItem(item); inst.UHostId != "" {
			candidates = append(candidates, inst)
		}
	}
	if len(candidates) == 0 {
		e.clearPendingSelection()
		return nil, false
	}
	return &pendingResourceSelection{
		snapshot:   snapshotFromPendingSelectionCandidates(candidates),
		candidates: candidates,
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
			key := strings.ToLower(strings.TrimSpace(inst.Name))
			nameIndex[key] = append(nameIndex[key], inst.UHostId)
		}
	}
	return entity.RegistrySnapshot{
		Instances:    instances,
		NameIndex:    nameIndex,
		LastFullSync: time.Now(),
		TotalCount:   len(candidates),
	}
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
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
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
		"\u4e00", "\u4e8c", "\u4e09", "\u56db", "\u4e94", "\u516d", "\u4e03", "\u516b", "\u4e5d", "\u5341",
		"\u5341\u4e00", "\u5341\u4e8c", "\u5341\u4e09", "\u5341\u56db", "\u5341\u4e94", "\u5341\u516d", "\u5341\u4e03",
		"\u5341\u516b", "\u5341\u4e5d", "\u4e8c\u5341",
	}
}
