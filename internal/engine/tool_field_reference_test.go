package engine

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A read parameter that names the write field its value ends up in — "下单时同一取值填
// RequestCreateInstance.GpuType。" — is the only thing telling the model that the
// quote's snake_case `gpu_type` and the order's PascalCase `GpuType` are one
// value. Prose alone would rot silently the first time a write schema is renamed,
// leaving a confident instruction pointing at a field that no longer exists.
//
// So the reference is a checked contract, not a comment: every
// Request<Operation>.<Field> written in any parameter description must resolve to
// a real property of that tool's schema, in the same window the model receives.
var toolFieldReference = regexp.MustCompile(`(Request[A-Za-z]+)\.([A-Za-z][A-Za-z0-9]*)`)

// identityClaim is the phrase that turns a reference from "these two fields are
// related" into "this exact value goes there unchanged". Only that stronger claim
// is checkable against both value spaces, so only descriptions carrying it are
// held to enum compatibility; a description that maps one vocabulary onto another
// says so in different words and is checked for the field name alone.
const identityClaim = "同一取值填"

// An undescribed parameter is not a small omission: the model sees a bare name
// and type and has to infer the fill rule, the omission rule and the value's
// source from the tool description, which is written about the tool rather than
// about the field. Every model-visible parameter carries its own sentence, so a
// new one fails here until it has one.
func TestEveryModelVisibleParameterIsDescribed(t *testing.T) {
	var undescribed []string
	for _, tool := range centralAgentToolWindow(true, true) {
		if tool.Function == nil {
			continue
		}
		root, ok := tool.Function.Parameters.(map[string]any)
		if !ok {
			continue
		}
		props, _ := root["properties"].(map[string]any)
		for name, raw := range props {
			node, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if description, _ := node["description"].(string); strings.TrimSpace(description) == "" {
				undescribed = append(undescribed, tool.Function.Name+"."+name)
			}
		}
	}
	sort.Strings(undescribed)
	require.Emptyf(t, undescribed, "%d model-visible parameter(s) carry no description", len(undescribed))
}

func TestToolDescriptionsOnlyNameFieldsTheirTargetToolHas(t *testing.T) {
	window := centralAgentToolWindow(true, true)
	require.NotEmpty(t, window)

	properties := map[string]map[string]bool{}
	for _, tool := range window {
		if tool.Function == nil {
			continue
		}
		root, ok := tool.Function.Parameters.(map[string]any)
		if !ok {
			continue
		}
		props, ok := root["properties"].(map[string]any)
		if !ok {
			continue
		}
		names := map[string]bool{}
		for name := range props {
			names[name] = true
		}
		properties[tool.Function.Name] = names
	}

	type reference struct{ from, tool, field string }
	var references []reference
	for _, tool := range window {
		if tool.Function == nil {
			continue
		}
		root, ok := tool.Function.Parameters.(map[string]any)
		if !ok {
			continue
		}
		props, _ := root["properties"].(map[string]any)
		for name, raw := range props {
			node, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			description, _ := node["description"].(string)
			for _, match := range toolFieldReference.FindAllStringSubmatch(description, -1) {
				references = append(references, reference{
					from: tool.Function.Name + "." + name, tool: match[1], field: match[2],
				})
			}
		}
	}

	// A rename that drops every reference would leave nothing to check and the
	// mapping the model relies on would be gone without a failure.
	require.NotEmpty(t, references, "no read parameter names its write-tool field any more")

	for _, ref := range references {
		fields, exposed := properties[ref.tool]
		require.Truef(t, exposed,
			"%s points at %s, which is not in the model-visible window", ref.from, ref.tool)
		if !fields[ref.field] {
			known := make([]string, 0, len(fields))
			for name := range fields {
				known = append(known, name)
			}
			sort.Strings(known)
			t.Errorf("%s points at %s.%s, which that tool does not accept; it has %v",
				ref.from, ref.tool, ref.field, known)
		}
	}
}

// windowParameter is one model-visible parameter node plus where it sits, so the
// nested gates below can report a path rather than a bare field name.
type windowParameter struct {
	path   string
	node   map[string]any
	parent string
}

// walkWindowParameters yields every parameter node in the window, including the
// children of nested objects and of array items. The depth-1 gates above cannot
// see those, and they are where the omission rules are least guessable.
func walkWindowParameters(t *testing.T) []windowParameter {
	t.Helper()
	var out []windowParameter
	var walk func(node map[string]any, path, parent string)
	walk = func(node map[string]any, path, parent string) {
		if props, ok := node["properties"].(map[string]any); ok {
			for name, raw := range props {
				child, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				out = append(out, windowParameter{path: path + "." + name, node: child, parent: path})
				walk(child, path+"."+name, path)
			}
		}
		if items, ok := node["items"].(map[string]any); ok {
			walk(items, path+"[]", parent)
		}
	}
	for _, tool := range centralAgentToolWindow(true, true) {
		if tool.Function == nil {
			continue
		}
		root, ok := tool.Function.Parameters.(map[string]any)
		if !ok {
			continue
		}
		walk(root, tool.Function.Name, "")
	}
	require.NotEmpty(t, out)
	return out
}

// "同一取值填 RequestCreateInstance.ImageSource" claims the value crosses unchanged.
// TestToolDescriptionsOnlyNameFieldsTheirTargetToolHas proves the field exists;
// only this proves the value would be accepted. The resolver matches enum members
// exactly (actionresolver.CodecEnum), so a read enum offering a spelling the write
// enum omits is a rejection the model cannot see coming — it read both schemas and
// obeyed both. That is how ReadCapability_image_list.source could hand
// RequestCreateInstance.ImageSource a "shared" it did not list.
func TestIdentityReferencesCarryValuesTheTargetFieldAccepts(t *testing.T) {
	targets := map[string]map[string][]string{}
	for _, tool := range centralAgentToolWindow(true, true) {
		if tool.Function == nil {
			continue
		}
		root, ok := tool.Function.Parameters.(map[string]any)
		if !ok {
			continue
		}
		props, ok := root["properties"].(map[string]any)
		if !ok {
			continue
		}
		fields := map[string][]string{}
		for name, raw := range props {
			node, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			fields[name] = enumValues(node)
		}
		targets[tool.Function.Name] = fields
	}

	checked := 0
	for _, param := range walkWindowParameters(t) {
		description, _ := param.node["description"].(string)
		if !strings.Contains(description, identityClaim) {
			continue
		}
		source := enumValues(param.node)
		if len(source) == 0 {
			continue
		}
		for _, match := range toolFieldReference.FindAllStringSubmatch(description, -1) {
			accepted, ok := targets[match[1]][match[2]]
			if !ok || len(accepted) == 0 {
				continue
			}
			checked++
			for _, value := range source {
				require.Containsf(t, accepted, value,
					"%s offers %q and claims it is filled verbatim into %s.%s, which accepts only %v",
					param.path, value, match[1], match[2], accepted)
			}
		}
	}
	require.Positivef(t, checked, "no identity reference relates two enums any more")
}

func enumValues(node map[string]any) []string {
	switch raw := node["enum"].(type) {
	case []string:
		return raw
	case []any:
		out := make([]string, 0, len(raw))
		for _, value := range raw {
			if text, ok := value.(string); ok {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}
