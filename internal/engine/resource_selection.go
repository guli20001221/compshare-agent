package engine

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/intent"
)

type pendingResourceSelection struct {
	originalUserMsg string
	plan            intent.Plan
	snapshot        entity.RegistrySnapshot
	candidates      []entity.InstanceSnapshot
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
	b.WriteString("\n\u4f60\u53ef\u4ee5\u56de\u590d\u5e8f\u53f7\u3001\u5b9e\u4f8b ID \u6216\u5b9e\u4f8b\u540d\u79f0\u3002")
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
