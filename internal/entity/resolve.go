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
