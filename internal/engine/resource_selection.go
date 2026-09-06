package engine

import (
	"time"

	"github.com/compshare-agent/internal/entity"
)

const maxResourceSelectionCandidates = 20
const pendingSelectionTTLSeconds = 300
const pendingSelectionKindInstance = "instance"

// pendingResourceSelection retains the numbered candidates displayed to the user
// so the Agent can resolve references from the same list on a later turn.
type pendingResourceSelection struct {
	candidates []entity.InstanceSnapshot
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
