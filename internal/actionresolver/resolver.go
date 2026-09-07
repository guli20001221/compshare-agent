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
	verifier     TargetAdjudicator
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
func New(catalog *Catalog, verifier TargetAdjudicator, machineTypes MachineTypeCatalog) *Resolver {
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
	result := ResolvedAction{TurnID: proposal.TurnID, Operation: proposal.Operation, Arguments: map[string]any{}}
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
		field, exists := spec.Fields[name]
		if !exists {
			reject(name, RejectUnknownField, RejectionActorModel, fmt.Sprintf("unknown slot %s", name))
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
				reject(name, RejectInvalidValue, RejectionActorModel, fmt.Sprintf("%s: %v", name, err))
			}
			adjudicated[name] = struct{}{}
			continue
		}
		candidate.Name, candidate.Value = name, value
		if field.Target {
			switch r.adjudicateTarget(candidate) {
			case TargetAccept:
				// exists this turn, no conflict — may reach the confirmation card.
			case TargetDependencyFailure:
				result.DependencyFailures = append(result.DependencyFailures, fmt.Sprintf("%s: 目标存在性暂时无法验证，请稍后再试", name))
				adjudicated[name] = struct{}{}
				continue
			default: // TargetReject
				// The exact target must exist in the account before confirmation.
				reject(name, RejectTargetNotExist, RejectionActorModel, fmt.Sprintf("%s: target existence could not be confirmed", name))
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
		result.Arguments[name] = winner.Value
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
			// The Agent corrects invalid structured arguments; it may ask the user
			// when a business choice is genuinely missing.
			reject("", RejectOperationContract, RejectionActorModel, err.Error())
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
	// field, target-not-exist, operation contract — is
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

func (r *Resolver) adjudicateTarget(candidate SlotCandidate) TargetVerdict {
	if r.verifier == nil {
		return TargetReject
	}
	return r.verifier.AdjudicateTarget(candidate)
}

func resolveCandidates(candidates []SlotCandidate) (SlotCandidate, bool) {
	first := candidates[0]
	for _, candidate := range candidates[1:] {
		if !sameValue(first.Value, candidate.Value) {
			return SlotCandidate{}, true
		}
	}
	return first, false
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

func sameValue(left, right any) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return string(a) == string(b)
}
