package engine

import (
	"sort"
	"strings"

	"github.com/compshare-agent/internal/entity"
)

// findUniqueInstanceNameInText resolves only exact, boundary-safe names from
// the account snapshot. It is an identity trust check, not an intent or action
// parser; ambiguous and partial matches never bind session state.
func findUniqueInstanceNameInText(userText string, snapshot entity.RegistrySnapshot) (entity.InstanceSnapshot, bool) {
	text := strings.TrimSpace(userText)
	if text == "" || len(snapshot.Instances) == 0 {
		return entity.InstanceSnapshot{}, false
	}
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
		if entity.TextExplicitlyMentionsName(text, name) {
			candidates = append(candidates, candidate{inst: inst, score: len([]rune(name))})
		}
	}
	if len(candidates) == 0 {
		return entity.InstanceSnapshot{}, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].inst.UHostId < candidates[j].inst.UHostId
	})
	bestScore := candidates[0].score
	var best []entity.InstanceSnapshot
	for _, candidate := range candidates {
		if candidate.score != bestScore {
			break
		}
		best = append(best, candidate.inst)
	}
	if len(best) != 1 {
		return entity.InstanceSnapshot{}, false
	}
	return best[0], true
}
