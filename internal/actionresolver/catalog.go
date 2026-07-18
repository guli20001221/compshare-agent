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
		catalog.ordered = append(catalog.ordered, operation)
		catalog.specs[operation] = OperationSpec{
			Operation: operation, Description: definition.Description, Fields: fields,
			NeedsConfirm:     capability.Policy.NeedsConfirm,
			Risk:             capability.Policy.SecurityLevel,
			Execution:        workflow.ExecutionContract(definition),
			ValidateResolved: operationValidator(operation),
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
		if name == "ShutdownAt" {
			return CodecTime
		}
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
			_, relative := args["AfterMinutes"]
			_, absolute := args["ShutdownAt"]
			if relative == absolute {
				return fmt.Errorf("exactly one of AfterMinutes or ShutdownAt is required")
			}
			return nil
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
