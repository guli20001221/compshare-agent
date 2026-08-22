package actionresolver

import (
	"fmt"
	"sort"
	"strings"

	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
)

type Catalog struct {
	ordered []string
	specs   map[string]OperationSpec
}

func BuildCatalog() (*Catalog, error) {
	capabilities := tools.DefaultCapabilityRegistry()
	catalog := &Catalog{specs: map[string]OperationSpec{}}
	for _, operation := range workflow.RegisteredWorkflowActions() {
		capability, ok := capabilities.Lookup(operation)
		if !ok || capability.Tool.Function == nil {
			return nil, fmt.Errorf("workflow %q has no capability schema", operation)
		}
		definition, ok := workflow.GetWorkflow(operation)
		if !ok {
			return nil, fmt.Errorf("workflow %q has no definition", operation)
		}
		fields, err := fieldsFromParameters(capability.Tool.Function.Parameters, capability.Policy.RedactInResult)
		if err != nil {
			return nil, fmt.Errorf("workflow %q: %w", operation, err)
		}
		intake, err := intakeSpecForOperation(definition.GuidedIntake, definition.GuidedIntakeFields, definition.DiscardableOnRejectFields, fields)
		if err != nil {
			return nil, fmt.Errorf("workflow %q: %w", operation, err)
		}
		catalog.ordered = append(catalog.ordered, operation)
		catalog.specs[operation] = OperationSpec{
			Operation: operation, AgentDescription: tools.WorkflowAgentDescription(strings.TrimSpace(capability.AgentInstruction)), Fields: fields,
			ImageCatalogSource: definition.ImageCatalogSource,
			NeedsZoneCatalog:   definition.NeedsZoneCatalog,
			NeedsConfirm:       capability.Policy.NeedsConfirm,
			Risk:               capability.Policy.SecurityLevel,
			Execution:          workflow.ExecutionContract(definition),
			ValidateResolved:   operationValidator(operation),
			Intake:             intake,
		}
	}
	return catalog, nil
}

func (c *Catalog) Lookup(operation string) (OperationSpec, bool) {
	if c == nil {
		return OperationSpec{}, false
	}
	spec, ok := c.specs[operation]
	return spec, ok
}

func (c *Catalog) Operations() []string {
	if c == nil {
		return nil
	}
	return append([]string(nil), c.ordered...)
}

func fieldsFromParameters(raw any, sensitive []string) (map[string]FieldSpec, error) {
	root, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parameters are not an object schema")
	}
	properties, ok := root["properties"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parameters have no properties")
	}
	required := stringSet(root["required"])
	secret := make(map[string]struct{}, len(sensitive))
	for _, name := range sensitive {
		secret[name] = struct{}{}
	}
	fields := make(map[string]FieldSpec, len(properties))
	for name, value := range properties {
		schema, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("field %q schema is invalid", name)
		}
		_, isSecret := secret[name]
		field := FieldSpec{Name: name, Required: required[name], Codec: codecFromSchema(name, schema, isSecret), Target: isTargetField(name), TargetKind: targetKindFromField(name)}
		field.Enum = stringSlice(schema["enum"])
		fields[name] = field
	}
	return fields, nil
}

func codecFromSchema(name string, schema map[string]any, sensitive bool) SlotCodecKind {
	if sensitive {
		return CodecSensitiveText
	}
	if isTargetField(name) {
		return CodecResourceRef
	}
	if name == "GpuType" {
		return CodecMachineType
	}
	if name == "Zone" {
		return CodecZone
	}
	if name == "CompShareImageId" {
		return CodecImage
	}
	if len(stringSlice(schema["enum"])) > 0 {
		return CodecEnum
	}
	switch schema["type"] {
	case "integer":
		return CodecInteger
	case "number":
		if name == "Size" || name == "Memory" {
			return CodecCapacity
		}
		return CodecNumber
	case "boolean":
		return CodecBoolean
	case "array", "object":
		return CodecStructured
	case "string":
		return CodecConstrainedText
	default:
		return CodecStructured
	}
}

// These are protocol field identities, not natural-language aliases. They
// identify the upstream object whose mutation requires server-verified origin.
func isTargetField(name string) bool {
	return targetKindFromField(name) != ""
}

func targetKindFromField(name string) string {
	switch name {
	case "UHostId":
		return "instance"
	case "UDiskId", "DiskId":
		return "disk"
	case "CfsId":
		return "cfs"
	default:
		return ""
	}
}

func operationValidator(operation string) func(map[string]any) error {
	switch operation {
	case "SetStopSchedulerWorkflow":
		return func(args map[string]any) error {
			schedule, exists := args["Schedule"]
			if !exists {
				return fmt.Errorf("schedule is required")
			}
			return workflow.ValidateShutdownSchedule(schedule)
		}
	case "ResizeInstanceWorkflow":
		return func(args map[string]any) error {
			for _, name := range []string{"Cpu", "Memory", "Gpu"} {
				if _, ok := args[name]; ok {
					return nil
				}
			}
			return fmt.Errorf("at least one target specification is required")
		}
	default:
		return nil
	}
}

// intakeSpecForOperation maps a workflow's own guided-intake declaration
// (workflow.Definition.GuidedIntake + GuidedIntakeFields) to the catalog
// IntakeSpec: a guided workflow may open a guided selection form for an
// incomplete-or-correctable proposal instead of a prose back-and-forth. Which
// workflow is guided AND which of its fields the form can collect/correct are
// both properties the workflow declares on its Definition, NOT a workflow-name
// switch or an auto-derivation here. CollectableFields must be the EXPLICIT
// declared set — never every non-secret/non-target field: a create schema
// carries fields the form has no input for (e.g. Name), and a resolver problem on
// such a field is not form-correctable. Errors when a declared field is not a
// real field of the operation (a typo would silently disable correction) or when
// a guided workflow declares no fields.
//
// DiscardableOnRejectFields is the second, narrower declaration: fields the form
// cannot collect but whose invalid value may be dropped so the form still opens.
// Its guards are stricter than the collectable set's, because a discarded value
// is one nobody ever re-supplies. A REQUIRED field may not be declared: the
// resolver marks a rejected slot as adjudicated, so it never reaches Missing,
// and discarding a required value would execute the operation without it. A
// target may not be declared (it would silence a write's addressee) and neither
// may a secret (it would silence a password the user typed).
func intakeSpecForOperation(guidedIntake bool, collectable, discardable []string, fields map[string]FieldSpec) (IntakeSpec, error) {
	if !guidedIntake {
		if len(discardable) > 0 {
			return IntakeSpec{}, fmt.Errorf("DiscardableOnRejectFields declared without GuidedIntake (nothing would re-collect)")
		}
		return IntakeSpec{}, nil
	}
	if len(collectable) == 0 {
		return IntakeSpec{}, fmt.Errorf("guided intake declared with no GuidedIntakeFields")
	}
	for _, name := range collectable {
		field, ok := fields[name]
		if !ok {
			return IntakeSpec{}, fmt.Errorf("GuidedIntakeFields names unknown field %q", name)
		}
		if field.Target || field.Codec == CodecSensitiveText {
			return IntakeSpec{}, fmt.Errorf("GuidedIntakeFields names a non-collectable field %q (target or secret)", name)
		}
	}
	for _, name := range discardable {
		field, ok := fields[name]
		if !ok {
			return IntakeSpec{}, fmt.Errorf("DiscardableOnRejectFields names unknown field %q", name)
		}
		if field.Required {
			return IntakeSpec{}, fmt.Errorf("DiscardableOnRejectFields names required field %q (discarding it would run the operation without it)", name)
		}
		if field.Target || field.Codec == CodecSensitiveText {
			return IntakeSpec{}, fmt.Errorf("DiscardableOnRejectFields names a field %q that must never be silently dropped (target or secret)", name)
		}
	}
	out := append([]string(nil), collectable...)
	sort.Strings(out)
	dropped := append([]string(nil), discardable...)
	sort.Strings(dropped)
	return IntakeSpec{Mode: IntakeGuided, CollectableFields: out, DiscardableOnRejectFields: dropped}, nil
}

func stringSet(value any) map[string]bool {
	out := map[string]bool{}
	for _, item := range stringSlice(value) {
		out[item] = true
	}
	return out
}

func stringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, value := range typed {
			if s, ok := value.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func SortedFieldNames(spec OperationSpec) []string {
	out := make([]string, 0, len(spec.Fields))
	for name := range spec.Fields {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func normalizeName(name string) string { return strings.TrimSpace(name) }
