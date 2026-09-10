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
