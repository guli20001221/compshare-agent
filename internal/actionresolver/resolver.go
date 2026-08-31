package actionresolver

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/compshare-agent/internal/deployment"
)

type Resolver struct {
	catalog      *Catalog
	verifier     EvidenceVerifier
	machineTypes MachineTypeCatalog
	// zoneCatalog is the live zone snapshot for a CodecZone field, attached via
	// WithZoneCatalog. nil (the default) reports every zone as catalog-unavailable
	// — refuse, never guess — exactly like a failed fetch.
	zoneCatalog *deployment.ZoneCatalogSnapshot
	// imageCatalog is the live image snapshot for a CodecImage field, attached via
	// WithImageCatalog. nil (the default) reports every image id as
	// catalog-unavailable — refuse, never guess — exactly like a failed fetch.
	imageCatalog *deployment.ImageCatalogSnapshot
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
	// reject records a rejection in BOTH the human-readable Rejected[] and the
	// typed RejectedProblems[] in lockstep, so the guided-intake decision can
	// classify the rejection by Kind without parsing the message string.
	reject := func(slot string, kind RejectionKind, actor RejectionActor, msg string) {
		result.Rejected = append(result.Rejected, msg)
		result.RejectedProblems = append(result.RejectedProblems, RejectedProblem{Slot: slot, Kind: kind, Actor: actor})
	}
	spec, ok := r.catalog.Lookup(proposal.Operation)
	if !ok {
		reject("", RejectUnknownOperation, RejectionActorModel, "unknown operation")
		return result
	}
	result.NeedsConfirm = spec.NeedsConfirm
	result.Gate = GateContract{Executor: "SafeToolExecutor", Risk: spec.Risk, RequiresPermission: true, RequiresConfirmation: spec.NeedsConfirm}
	result.Execution = spec.Execution
	grouped := map[string][]SlotCandidate{}
	// adjudicated records every field the resolver formed an opinion about and
	// could not accept — rejected, conflicted, or dependency-failed. Such a field
	// is NOT Missing: the value WAS supplied, we just could not honour it. Missing
	// means only "nobody has said it yet", and mixing the two tells the agent to
	// ask the user for something they already gave us (or, worse, for something
	// our own failed catalog query is to blame for).
	adjudicated := map[string]struct{}{}
	for _, candidate := range proposal.Slots {
		name := normalizeName(candidate.Name)
		if !knownSource(candidate.Source) {
			reject(name, RejectUnknownSource, RejectionActorModel, fmt.Sprintf("%s: unknown candidate source", name))
			adjudicated[name] = struct{}{}
			continue
		}
		field, exists := spec.Fields[name]
		if !exists {
			reject(name, RejectUnknownField, RejectionActorModel, fmt.Sprintf("unknown slot %s", name))
			continue
		}
		// Guided forms cannot re-confirm fields outside their controls. For the
		// explicitly declared optional exceptions, keep a value only when it came
		// from verified current or recent user-authored evidence; otherwise let the workflow
		// derive its live platform default. This prevents model placeholder values
		// from silently becoming part of a sealed create contract.
		if candidate.Source == SourceAgentInference &&
			containsField(spec.Intake.UserSuppliedOptionalFields, name) {
			continue
		}
		// Non-target user_explicit fields are span-verified here; TARGET fields defer
		// entirely to the target adjudicator below, which weighs selection AND
		// existence and routes an outage / conflict to the right channel rather than
		// a blanket "not verified".
		if !field.Target && candidate.Source == SourceUserExplicit && (candidate.Evidence == nil || r.verifier == nil || !r.verifier.VerifyCandidate(candidate)) {
			reject(name, RejectUnverifiedSource, RejectionActorModel, fmt.Sprintf("%s: user-explicit source is not verified", name))
			adjudicated[name] = struct{}{}
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
				reject(name, RejectInvalidValue, rejectionActorForCandidate(candidate), fmt.Sprintf("%s: %v", name, err))
			}
			adjudicated[name] = struct{}{}
			continue
		}
		candidate.Name, candidate.Value = name, value
		if field.Target {
			switch r.adjudicateTarget(candidate) {
			case TargetAccept:
				// exists this turn, no conflict — may reach the confirmation card.
			case TargetConflict:
				result.Conflicts = append(result.Conflicts, Conflict{Slot: name, Reason: "目标引用不唯一，请明确指定要操作的实例"})
				adjudicated[name] = struct{}{}
				continue
			case TargetDependencyFailure:
				result.DependencyFailures = append(result.DependencyFailures, fmt.Sprintf("%s: 目标存在性暂时无法验证，请稍后再试", name))
				adjudicated[name] = struct{}{}
				continue
			default: // TargetReject
				// Under the uniform model a target is rejected when the server could
				// not confirm it EXISTS (a point-query that echoed no matching id, or a
				// fresh+complete registry that authoritatively lacks it) — not a source
				// problem: the confirmation card, not a source label, is the SelectionProof.
				reject(name, RejectTargetNotExist, rejectionActorForCandidate(candidate), fmt.Sprintf("%s: target existence could not be confirmed", name))
				adjudicated[name] = struct{}{}
				continue
			}
		}
		grouped[name] = append(grouped[name], candidate)
	}
	for name, candidates := range grouped {
		winner, conflict := resolveCandidates(candidates)
		if conflict {
			result.Conflicts = append(result.Conflicts, Conflict{Slot: name, Candidates: candidates})
			adjudicated[name] = struct{}{}
			continue
		}
		field := spec.Fields[name]
		result.Arguments[name] = winner.Value
		result.Provenance[name] = ResolvedSlot{Value: winner.Value, Source: winner.Source, Codec: field.Codec}
	}
	for name, field := range spec.Fields {
		if !field.Required {
			continue
		}
		if _, ok := result.Arguments[name]; ok {
			continue
		}
		if _, judged := adjudicated[name]; judged {
			continue
		}
		result.Missing = append(result.Missing, name)
	}
	if len(result.Missing) == 0 && len(result.Conflicts) == 0 && len(result.Rejected) == 0 && len(result.DependencyFailures) == 0 && spec.ValidateResolved != nil {
		if err := spec.ValidateResolved(result.Arguments); err != nil {
			// A cross-field contract error means the requested operation is not
			// fully specified. The resolver cannot know which business choice the
			// user intended, so the Agent must ask instead of inventing a value.
			reject("", RejectOperationContract, RejectionActorUser, err.Error())
		}
	}
	sort.Strings(result.Missing)
	sort.Strings(result.Rejected)
	sort.Strings(result.DependencyFailures)
	sort.Slice(result.Conflicts, func(i, j int) bool { return result.Conflicts[i].Slot < result.Conflicts[j].Slot })
	sort.Slice(result.RejectedProblems, func(i, j int) bool {
		if result.RejectedProblems[i].Slot != result.RejectedProblems[j].Slot {
			return result.RejectedProblems[i].Slot < result.RejectedProblems[j].Slot
		}
		return result.RejectedProblems[i].Kind < result.RejectedProblems[j].Kind
	})
	result.ReadyForConfirmation = len(result.Missing) == 0 && len(result.Conflicts) == 0 &&
		len(result.Rejected) == 0 && len(result.DependencyFailures) == 0
	// ReadyForIntake: the proposal is incomplete OR carries only FORM-CORRECTABLE
	// problems, so opening the guided selection form beats a prose back-and-forth.
	// A problem is form-correctable only when the field is a declared collectable
	// (spec.Intake.CollectableFields) AND it is one of:
	//   - Missing               → the form collects it;
	//   - a Conflict on it       → the form makes the user pick (never guessed);
	//   - Rejected/InvalidValue  → the resolver already dropped the bad value from
	//     Arguments; the form re-collects a valid one (never silently swapped).
	// A DependencyFailure (server outage) or any STRUCTURAL rejection — unknown
	// field/source, unverified source, target-not-exist, operation contract — is
	// NOT form-correctable and blocks the form (falls through to prose). Mutually
	// exclusive with ReadyForConfirmation. The engine still decides whether a guided
	// form is actually available this turn.
	result.ReadyForIntake = !result.ReadyForConfirmation &&
		spec.Intake.Mode == IntakeGuided &&
		len(result.DependencyFailures) == 0 &&
		(len(result.Missing)+len(result.Rejected)+len(result.Conflicts)) > 0 &&
		everyMissingCollectable(result.Missing, spec.Intake.CollectableFields) &&
		everyRejectionFormCorrectable(result.RejectedProblems, spec.Intake.CollectableFields) &&
		everyConflictCollectable(result.Conflicts, spec.Intake.CollectableFields)
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

func rejectionActorForCandidate(candidate SlotCandidate) RejectionActor {
	if candidate.UserAuthored {
		return RejectionActorUser
	}
	switch candidate.Source {
	case SourceUserExplicit, SourceUserConfirmation:
		return RejectionActorUser
	default:
		return RejectionActorModel
	}
}

func containsField(fields []string, name string) bool {
	for _, field := range fields {
		if field == name {
			return true
		}
	}
	return false
}

func collectableSet(collectable []string) map[string]struct{} {
	set := make(map[string]struct{}, len(collectable))
	for _, name := range collectable {
		set[name] = struct{}{}
	}
	return set
}

// everyMissingCollectable reports whether every Missing field is one the guided
// form can collect. Vacuously true when Missing is empty — the caller enforces
// "at least one problem" separately, so a rejection-only proposal still qualifies.
func everyMissingCollectable(missing, collectable []string) bool {
	set := collectableSet(collectable)
	for _, name := range missing {
		if _, ok := set[name]; !ok {
			return false
		}
	}
	return true
}

// everyRejectionFormCorrectable reports whether every rejection is a
// RejectInvalidValue the form can move past — the only Kind it can, since the
// bad value is already dropped from Arguments. Only a COLLECTABLE field qualifies:
// the form must actually be able to re-collect the value. Any other Kind or field
// is not correctable. Vacuously true when there are no rejections.
func everyRejectionFormCorrectable(problems []RejectedProblem, collectable []string) bool {
	collectables := collectableSet(collectable)
	for _, p := range problems {
		if p.Kind != RejectInvalidValue {
			return false
		}
		if _, ok := collectables[p.Slot]; !ok {
			return false
		}
	}
	return true
}

// everyConflictCollectable reports whether every conflict is on a declared
// collectable field (the form makes the user pick). A conflict on a
// non-collectable field — e.g. a target-reference ambiguity — is not correctable
// by the create form. Vacuously true when there are no conflicts.
func everyConflictCollectable(conflicts []Conflict, collectable []string) bool {
	set := collectableSet(collectable)
	for _, c := range conflicts {
		if _, ok := set[c.Slot]; !ok {
			return false
		}
	}
	return true
}

func knownSource(source CandidateSource) bool {
	switch source {
	case SourceUserExplicit, SourceVerifiedContext, SourceToolObservation, SourceUserConfirmation, SourceAgentInference:
		return true
	default:
		return false
	}
}

// adjudicateTarget decides a write target's disposition. It prefers the verifier's
// TargetAdjudicator (the engine, which owns the selection binding and existence
// network); a verifier that implements only the plain bool verify keeps the prior
// accept/reject behaviour.
func (r *Resolver) adjudicateTarget(candidate SlotCandidate) TargetVerdict {
	if adj, ok := r.verifier.(TargetAdjudicator); ok {
		return adj.AdjudicateTarget(candidate)
	}
	if r.trustedTarget(candidate) {
		return TargetAccept
	}
	return TargetReject
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
	case CodecZone:
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("must be a non-empty zone id or display name")
		}
		return r.canonicalZoneValue(strings.TrimSpace(text))
	case CodecImage:
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("must be a non-empty image id")
		}
		return r.canonicalImageValue(strings.TrimSpace(text))
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
		number, ok := NormalizeCapacityGB(value)
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

// NormalizeCapacityGB is the shared capacity codec. It parses one value (for
// example "200G" or "200GiB") and performs unit conversion at this boundary.
func NormalizeCapacityGB(value any) (float64, bool) {
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

// CapacityLiteral is one explicit G/GB/GiB value in user text. Offsets are rune
// offsets so they can be copied directly into SourceEvidence.
type CapacityLiteral struct {
	Text       string
	Start, End int
}

// CapacityLiterals tokenizes capacity values without assigning them to a
// business field. The Agent decides whether a value describes a system disk or
// a data disk; the resolver only supplies the same unit grammar used by
// NormalizeCapacityGB so provenance and validation cannot disagree.
func CapacityLiterals(text string) []CapacityLiteral {
	runes := []rune(text)
	var out []CapacityLiteral
	for start := 0; start < len(runes); start++ {
		if !unicode.IsDigit(runes[start]) {
			continue
		}
		end := start
		for end < len(runes) && unicode.IsDigit(runes[end]) {
			end++
		}
		if end < len(runes) && runes[end] == '.' {
			fraction := end + 1
			for fraction < len(runes) && unicode.IsDigit(runes[fraction]) {
				fraction++
			}
			if fraction > end+1 {
				end = fraction
			}
		}
		for end < len(runes) && (runes[end] == ' ' || runes[end] == '\t') {
			end++
		}
		if end >= len(runes) || unicode.ToLower(runes[end]) != 'g' {
			continue
		}
		end++
		if end < len(runes) && unicode.ToLower(runes[end]) == 'i' {
			if end+1 >= len(runes) || unicode.ToLower(runes[end+1]) != 'b' {
				continue
			}
			end += 2
		} else if end < len(runes) && unicode.ToLower(runes[end]) == 'b' {
			end++
		}
		out = append(out, CapacityLiteral{Text: string(runes[start:end]), Start: start, End: end})
		start = end - 1
	}
	return out
}

func sameValue(left, right any) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return string(a) == string(b)
}
