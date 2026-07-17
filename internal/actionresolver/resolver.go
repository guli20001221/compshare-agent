package actionresolver

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type Resolver struct {
	catalog      *Catalog
	verifier     EvidenceVerifier
	machineTypes MachineTypeCatalog
}

// New builds a resolver over a static operation catalog plus the live
// machine-type snapshot the ENGINE fetched. machineTypes is pure data: this
// package performs no I/O, so a Resolve is replayable from its inputs alone.
// Pass the zero MachineTypeCatalog for operations with no machine-type field —
// SpecNeedsMachineTypeCatalog reports which those are.
func New(catalog *Catalog, verifier EvidenceVerifier, machineTypes MachineTypeCatalog) *Resolver {
	return &Resolver{catalog: catalog, verifier: verifier, machineTypes: machineTypes}
}

// dependencyError marks a value the resolver could not adjudicate because a
// server-side fact was unavailable — not the user's fault, not a rejection.
type dependencyError struct{ detail string }

func (e dependencyError) Error() string { return e.detail }

// ambiguityError marks a value that matched several live catalog entries.
type ambiguityError struct {
	detail     string
	candidates []string
}

func (e ambiguityError) Error() string { return e.detail }

func (r *Resolver) Resolve(proposal ActionProposal) ResolvedAction {
	result := ResolvedAction{TurnID: proposal.TurnID, Operation: proposal.Operation, Arguments: map[string]any{}, Provenance: map[string]ResolvedSlot{}}
	spec, ok := r.catalog.Lookup(proposal.Operation)
	if !ok {
		result.Rejected = append(result.Rejected, "unknown operation")
		return result
	}
	result.NeedsConfirm = spec.NeedsConfirm
	result.Gate = GateContract{Executor: "SafeToolExecutor", Risk: spec.Risk, RequiresPermission: true, RequiresConfirmation: spec.NeedsConfirm, RequiresJournal: true}
	result.Execution = spec.Execution
	grouped := map[string][]SlotCandidate{}
	for _, candidate := range proposal.Slots {
		name := normalizeName(candidate.Name)
		if !knownSource(candidate.Source) {
			result.Rejected = append(result.Rejected, fmt.Sprintf("%s: unknown candidate source", name))
			continue
		}
		field, exists := spec.Fields[name]
		if !exists {
			result.Rejected = append(result.Rejected, fmt.Sprintf("unknown slot %s", name))
			continue
		}
		if candidate.Source == SourceUserExplicit && (candidate.Evidence == nil || r.verifier == nil || !r.verifier.VerifyCandidate(candidate)) {
			result.Rejected = append(result.Rejected, fmt.Sprintf("%s: user-explicit source is not verified", name))
			continue
		}
		value, err := r.normalizeValue(field, candidate.Value)
		if err != nil {
			switch typed := err.(type) {
			case dependencyError:
				result.DependencyFailures = append(result.DependencyFailures, fmt.Sprintf("%s: %v", name, typed))
			case ambiguityError:
				result.Conflicts = append(result.Conflicts, Conflict{
					Slot: name, CatalogCandidates: typed.candidates, Reason: typed.Error(),
				})
			default:
				result.Rejected = append(result.Rejected, fmt.Sprintf("%s: %v", name, err))
			}
			continue
		}
		candidate.Name, candidate.Value = name, value
		if field.Target && !r.trustedTarget(candidate) {
			result.Rejected = append(result.Rejected, fmt.Sprintf("%s: target source is not verified", name))
			continue
		}
		grouped[name] = append(grouped[name], candidate)
	}
	for name, candidates := range grouped {
		winner, conflict := resolveCandidates(candidates)
		if conflict {
			result.Conflicts = append(result.Conflicts, Conflict{Slot: name, Candidates: candidates})
			continue
		}
		field := spec.Fields[name]
		result.Arguments[name] = winner.Value
		result.Provenance[name] = ResolvedSlot{Value: winner.Value, Source: winner.Source, Codec: field.Codec}
	}
	for name, field := range spec.Fields {
		if field.Required {
			if _, ok := result.Arguments[name]; !ok {
				result.Missing = append(result.Missing, name)
			}
		}
	}
	if len(result.Missing) == 0 && len(result.Conflicts) == 0 && len(result.Rejected) == 0 && len(result.DependencyFailures) == 0 && spec.ValidateResolved != nil {
		if err := spec.ValidateResolved(result.Arguments); err != nil {
			result.Rejected = append(result.Rejected, err.Error())
		}
	}
	sort.Strings(result.Missing)
	sort.Strings(result.Rejected)
	sort.Strings(result.DependencyFailures)
	sort.Slice(result.Conflicts, func(i, j int) bool { return result.Conflicts[i].Slot < result.Conflicts[j].Slot })
	result.ReadyForConfirmation = len(result.Missing) == 0 && len(result.Conflicts) == 0 &&
		len(result.Rejected) == 0 && len(result.DependencyFailures) == 0
	if result.ReadyForConfirmation {
		arguments := make(map[string]any, len(result.Arguments))
		for name, value := range result.Arguments {
			if spec.Fields[name].Codec == CodecSensitiveText {
				arguments[name] = "[REDACTED]"
			} else {
				arguments[name] = value
			}
		}
		result.Confirmation = &ConfirmationPreview{Operation: result.Operation, Arguments: arguments}
	}
	return result
}

func knownSource(source CandidateSource) bool {
	switch source {
	case SourceUserExplicit, SourceVerifiedContext, SourceToolObservation, SourceUserConfirmation, SourceAgentInference:
		return true
	default:
		return false
	}
}

func (r *Resolver) trustedTarget(candidate SlotCandidate) bool {
	switch candidate.Source {
	case SourceUserConfirmation, SourceToolObservation:
		return r.verifier != nil && r.verifier.VerifyCandidate(candidate)
	case SourceUserExplicit, SourceVerifiedContext:
		return candidate.Evidence != nil && r.verifier != nil && r.verifier.VerifyCandidate(candidate)
	default:
		return false
	}
}

func resolveCandidates(candidates []SlotCandidate) (SlotCandidate, bool) {
	best := -1
	winners := []SlotCandidate{}
	for _, candidate := range candidates {
		rank := sourceRank(candidate.Source)
		if rank > best {
			best, winners = rank, []SlotCandidate{candidate}
		} else if rank == best {
			winners = append(winners, candidate)
		}
	}
	first := winners[0]
	for _, candidate := range winners[1:] {
		if !sameValue(first.Value, candidate.Value) {
			return SlotCandidate{}, true
		}
	}
	return first, false
}

func sourceRank(source CandidateSource) int {
	switch source {
	case SourceUserConfirmation:
		return 5
	case SourceUserExplicit:
		return 4
	case SourceVerifiedContext:
		return 3
	case SourceToolObservation:
		return 2
	case SourceAgentInference:
		return 1
	default:
		return 0
	}
}

func (r *Resolver) normalizeValue(field FieldSpec, value any) (any, error) {
	switch field.Codec {
	case CodecMachineType:
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("must be a non-empty machine type name")
		}
		return r.canonicalMachineTypeValue(strings.TrimSpace(text))
	case CodecResourceRef:
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("must be a non-empty resource id")
		}
		text = strings.TrimSpace(text)
		if len([]rune(text)) > 128 {
			return nil, fmt.Errorf("resource id is too long")
		}
		for _, r := range text {
			if unicode.IsSpace(r) {
				return nil, fmt.Errorf("resource id cannot contain whitespace")
			}
		}
		return text, nil
	case CodecConstrainedText, CodecSensitiveText, CodecTime:
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("must be a non-empty string")
		}
		text = strings.TrimSpace(text)
		if len([]rune(text)) > 512 {
			return nil, fmt.Errorf("text is too long")
		}
		return text, nil
	case CodecEnum:
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("must be a string")
		}
		for _, allowed := range field.Enum {
			if text == allowed {
				return text, nil
			}
		}
		return nil, fmt.Errorf("value is outside the declared enum")
	case CodecInteger:
		number, ok := asNumber(value)
		if !ok || number <= 0 || math.Trunc(number) != number {
			return nil, fmt.Errorf("must be a positive integer")
		}
		return number, nil
	case CodecCapacity:
		number, ok := asCapacityGB(value)
		if !ok || number <= 0 || math.Trunc(number) != number {
			return nil, fmt.Errorf("must be a positive integer capacity in GB")
		}
		return number, nil
	case CodecNumber:
		number, ok := asNumber(value)
		if !ok || number < 0 {
			return nil, fmt.Errorf("must be a non-negative number")
		}
		return number, nil
	case CodecBoolean:
		boolean, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("must be a boolean")
		}
		return boolean, nil
	case CodecStructured:
		switch value.(type) {
		case []any, map[string]any:
			return value, nil
		default:
			return nil, fmt.Errorf("must be structured JSON")
		}
	default:
		return nil, fmt.Errorf("unsupported codec")
	}
}

func asNumber(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		v, err := n.Float64()
		return v, err == nil
	case string:
		v, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return v, err == nil
	default:
		return 0, false
	}
}

// asCapacityGB is the shared capacity codec. It parses a value, not a user
// sentence: the Agent must pass the exact sourced span (for example "200G")
// and the resolver performs the unit conversion at this boundary.
func asCapacityGB(value any) (float64, bool) {
	if number, ok := asNumber(value); ok {
		return number, true
	}
	text, ok := value.(string)
	if !ok {
		return 0, false
	}
	text = strings.TrimSpace(strings.ToUpper(text))
	for _, suffix := range []string{"GIB", "GB", "G"} {
		if numberText, ok := strings.CutSuffix(text, suffix); ok {
			number, err := strconv.ParseFloat(strings.TrimSpace(numberText), 64)
			return number, err == nil
		}
	}
	return 0, false
}

func sameValue(left, right any) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return string(a) == string(b)
}
