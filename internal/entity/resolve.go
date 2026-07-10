package entity

import (
	"sort"
	"strings"
	"unicode"
)

type ResolveStatus string

const (
	ResolveHit                   ResolveStatus = "HIT"
	ResolveNotFoundInAccount     ResolveStatus = "NOT_FOUND_IN_ACCOUNT"
	ResolveRecentlyReleasedGuess ResolveStatus = "RECENTLY_RELEASED_GUESS"
	ResolveAmbiguous             ResolveStatus = "AMBIGUOUS"
)

type ResolveResult struct {
	Status     ResolveStatus
	Query      string
	Candidates []string
}

type FilterSpec struct {
	State   string
	GPUType string
}

func (r *EntityRegistry) ResolveByID(id string) (*InstanceSnapshot, ResolveResult) {
	query := strings.TrimSpace(id)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if inst, ok := r.Instances[query]; ok {
		copy := inst
		return &copy, ResolveResult{Status: ResolveHit, Query: query, Candidates: []string{query}}
	}
	if releasedAt, ok := r.recentlyReleased[query]; ok && r.now().Sub(releasedAt) <= recentlyReleasedTTL {
		return nil, ResolveResult{Status: ResolveRecentlyReleasedGuess, Query: query}
	}
	return nil, ResolveResult{Status: ResolveNotFoundInAccount, Query: query}
}

func (r *EntityRegistry) ResolveByName(name string) ([]*InstanceSnapshot, ResolveResult) {
	query := strings.TrimSpace(name)
	normalized := normalizeName(query)
	if normalized == "" {
		return nil, ResolveResult{Status: ResolveNotFoundInAccount, Query: query}
	}

	r.mu.RLock()
	if ids := append([]string(nil), r.NameIndex[normalized]...); len(ids) > 0 {
		matches := r.instancesForIDsLocked(ids)
		r.mu.RUnlock()
		status := ResolveHit
		if len(matches) > 1 {
			status = ResolveAmbiguous
		}
		return matches, ResolveResult{Status: status, Query: query, Candidates: idsOfSnapshots(matches)}
	}
	r.mu.RUnlock()

	scored := make([]scoredInstance, 0)
	terms := normalizeTerms(query)
	r.mu.RLock()
	for _, inst := range r.Instances {
		if score := fuzzyScore(normalized, terms, normalizeName(inst.Name)); score > 0 {
			scored = append(scored, scoredInstance{snapshot: inst, score: score})
		}
	}
	r.mu.RUnlock()
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		if scored[i].snapshot.Name != scored[j].snapshot.Name {
			return scored[i].snapshot.Name < scored[j].snapshot.Name
		}
		return scored[i].snapshot.UHostId < scored[j].snapshot.UHostId
	})

	matches := make([]*InstanceSnapshot, 0, len(scored))
	for _, item := range scored {
		copy := item.snapshot
		matches = append(matches, &copy)
	}
	if len(matches) == 0 {
		return nil, ResolveResult{Status: ResolveNotFoundInAccount, Query: query}
	}
	status := ResolveHit
	if len(matches) > 1 {
		status = ResolveAmbiguous
	}
	return matches, ResolveResult{Status: status, Query: query, Candidates: idsOfSnapshots(matches)}
}

func (r *EntityRegistry) InstanceIDTokensInText(text string) []string {
	if r == nil {
		return nil
	}
	return r.Snapshot().InstanceIDTokensInText(text)
}

func (r *EntityRegistry) ResolveInstanceRefsInText(text string) ([]*InstanceSnapshot, []string) {
	if r == nil {
		return nil, nil
	}
	return r.Snapshot().ResolveInstanceRefsInText(text)
}

func (r *EntityRegistry) Filter(spec FilterSpec) []*InstanceSnapshot {
	state := strings.ToLower(strings.TrimSpace(spec.State))
	gpuType := strings.ToLower(strings.TrimSpace(spec.GPUType))
	matches := make([]*InstanceSnapshot, 0)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, inst := range r.Instances {
		if state != "" && strings.ToLower(inst.State) != state {
			continue
		}
		if gpuType != "" && strings.ToLower(inst.GpuType) != gpuType {
			continue
		}
		copy := inst
		matches = append(matches, &copy)
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].UHostId < matches[j].UHostId
	})
	return matches
}

func (s RegistrySnapshot) ResolveByID(id string) (*InstanceSnapshot, ResolveResult) {
	query := strings.TrimSpace(id)
	if inst, ok := s.Instances[query]; ok {
		copy := inst
		return &copy, ResolveResult{Status: ResolveHit, Query: query, Candidates: []string{query}}
	}
	return nil, ResolveResult{Status: ResolveNotFoundInAccount, Query: query}
}

func (s RegistrySnapshot) ResolveByName(name string) ([]*InstanceSnapshot, ResolveResult) {
	query := strings.TrimSpace(name)
	normalized := normalizeName(query)
	if normalized == "" {
		return nil, ResolveResult{Status: ResolveNotFoundInAccount, Query: query}
	}

	if ids := append([]string(nil), s.NameIndex[normalized]...); len(ids) > 0 {
		matches := s.instancesForIDs(ids)
		status := ResolveHit
		if len(matches) > 1 {
			status = ResolveAmbiguous
		}
		return matches, ResolveResult{Status: status, Query: query, Candidates: idsOfSnapshots(matches)}
	}

	scored := make([]scoredInstance, 0)
	terms := normalizeTerms(query)
	for _, inst := range s.Instances {
		if score := fuzzyScore(normalized, terms, normalizeName(inst.Name)); score > 0 {
			scored = append(scored, scoredInstance{snapshot: inst, score: score})
		}
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		if scored[i].snapshot.Name != scored[j].snapshot.Name {
			return scored[i].snapshot.Name < scored[j].snapshot.Name
		}
		return scored[i].snapshot.UHostId < scored[j].snapshot.UHostId
	})

	matches := make([]*InstanceSnapshot, 0, len(scored))
	for _, item := range scored {
		copy := item.snapshot
		matches = append(matches, &copy)
	}
	if len(matches) == 0 {
		return nil, ResolveResult{Status: ResolveNotFoundInAccount, Query: query}
	}
	status := ResolveHit
	if len(matches) > 1 {
		status = ResolveAmbiguous
	}
	return matches, ResolveResult{Status: status, Query: query, Candidates: idsOfSnapshots(matches)}
}

func (s RegistrySnapshot) InstanceIDTokensInText(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" || len(s.Instances) == 0 {
		return nil
	}
	prefixes := s.instanceIDPrefixes()
	if len(prefixes) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	tokens := make([]string, 0)
	for i := 0; i < len(text); i++ {
		if text[i] > unicode.MaxASCII {
			continue
		}
		if i > 0 && isInstanceIDTokenByte(text[i-1]) {
			continue
		}
		for _, prefix := range prefixes {
			endPrefix := i + len(prefix)
			if endPrefix >= len(text) || text[endPrefix] != '-' {
				continue
			}
			if !strings.EqualFold(text[i:endPrefix], prefix) {
				continue
			}
			end := endPrefix + 1
			for end < len(text) && isInstanceIDTokenByte(text[end]) {
				end++
			}
			if end == endPrefix+1 {
				continue
			}
			token := text[i:end]
			if _, ok := seen[token]; ok {
				continue
			}
			seen[token] = struct{}{}
			tokens = append(tokens, token)
			i = end - 1
			break
		}
	}
	return tokens
}

func (s RegistrySnapshot) ResolveInstanceRefsInText(text string) ([]*InstanceSnapshot, []string) {
	tokens := s.InstanceIDTokensInText(text)
	if len(tokens) == 0 {
		return nil, nil
	}
	hits := make([]*InstanceSnapshot, 0, len(tokens))
	unresolved := make([]string, 0)
	for _, token := range tokens {
		if inst, res := s.ResolveByID(token); res.Status == ResolveHit && inst != nil {
			hits = append(hits, inst)
			continue
		}
		unresolved = append(unresolved, token)
	}
	return hits, unresolved
}

func (s RegistrySnapshot) instancesForIDs(ids []string) []*InstanceSnapshot {
	matches := make([]*InstanceSnapshot, 0, len(ids))
	for _, id := range ids {
		if inst, ok := s.Instances[id]; ok {
			copy := inst
			matches = append(matches, &copy)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].UHostId < matches[j].UHostId
	})
	return matches
}

func (r *EntityRegistry) instancesForIDsLocked(ids []string) []*InstanceSnapshot {
	matches := make([]*InstanceSnapshot, 0, len(ids))
	for _, id := range ids {
		if inst, ok := r.Instances[id]; ok {
			copy := inst
			matches = append(matches, &copy)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].UHostId < matches[j].UHostId
	})
	return matches
}

func (s RegistrySnapshot) instanceIDPrefixes() []string {
	seen := map[string]struct{}{}
	prefixes := make([]string, 0)
	for id, inst := range s.Instances {
		instanceID := strings.TrimSpace(inst.UHostId)
		if instanceID == "" {
			instanceID = strings.TrimSpace(id)
		}
		prefix, ok := instanceIDPrefix(instanceID)
		if !ok {
			continue
		}
		if _, exists := seen[prefix]; exists {
			continue
		}
		seen[prefix] = struct{}{}
		prefixes = append(prefixes, prefix)
	}
	sort.Slice(prefixes, func(i, j int) bool {
		if len(prefixes[i]) != len(prefixes[j]) {
			return len(prefixes[i]) > len(prefixes[j])
		}
		return prefixes[i] < prefixes[j]
	})
	return prefixes
}

func instanceIDPrefix(id string) (string, bool) {
	idx := strings.IndexByte(id, '-')
	if idx <= 0 || idx >= len(id)-1 {
		return "", false
	}
	prefix := strings.ToLower(id[:idx])
	for i := 0; i < len(prefix); i++ {
		if !isASCIIAlphaNum(prefix[i]) {
			return "", false
		}
	}
	return prefix, true
}

func isInstanceIDTokenByte(b byte) bool {
	return isASCIIAlphaNum(b) || b == '-' || b == '_'
}

func isASCIIAlphaNum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func normalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func normalizeTerms(s string) []string {
	s = strings.ToLower(strings.TrimSpace(s))
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r))
	})
	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		term := normalizeName(field)
		if term != "" {
			terms = append(terms, term)
		}
	}
	return terms
}

func fuzzyScore(query string, terms []string, name string) int {
	if query == "" || name == "" {
		return 0
	}
	if strings.Contains(name, query) {
		return 80
	}
	if len(terms) > 0 {
		for _, term := range terms {
			if !strings.Contains(name, term) {
				return 0
			}
		}
		return 70
	}
	return 0
}

type scoredInstance struct {
	snapshot InstanceSnapshot
	score    int
}

func idsOfSnapshots(items []*InstanceSnapshot) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.UHostId)
	}
	return ids
}
