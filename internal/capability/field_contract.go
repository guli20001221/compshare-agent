package capability

import (
	"encoding/json"
	"fmt"

	"github.com/compshare-agent/internal/platform"
)

// nodeKind is the JSON-Schema type a contract node renders and validates.
type nodeKind string

const (
	nodeObject  nodeKind = "object"
	nodeString  nodeKind = "string"
	nodeInteger nodeKind = "integer"
	nodeArray   nodeKind = "array"
)

// schemaNode is one node of a read capability's parameter contract. It is the
// SINGLE source for three artifacts that used to be maintained by hand and drift
// apart:
//
//  1. the model-facing JSON Schema — jsonSchema()
//  2. the runtime argument validator — validate()
//  3. the consistency-test expectation — the test walks the rendered schema
//     against the Go request struct
//
// enum members and integer bounds are declared here exactly once; the decoder,
// the handler defaults and the tool schema all read the same tree instead of
// each re-listing (and eventually contradicting) the constraint. Before this
// existed, the schema advertised enum/minimum but the decoder enforced neither,
// so an out-of-contract value (price_kind:"bogus", gpu_count:-1) silently fell
// through to a handler default.
type schemaNode struct {
	kind        nodeKind
	description string
	enum        []string              // string enum members (kind==nodeString); nil = free string
	minimum     *int                  // integer lower bound (kind==nodeInteger); nil = unbounded
	props       map[string]schemaNode // object properties (kind==nodeObject)
	req         []string              // object required keys (kind==nodeObject)
	items       *schemaNode           // array element (kind==nodeArray)
}

func (n schemaNode) described(description string) schemaNode {
	n.description = description
	return n
}

func stringParam() schemaNode { return schemaNode{kind: nodeString} }

// enumParam is a string constrained to a fixed member set. Callers pass
// platform.XxxValues() so the members live in exactly one place.
func enumParam(values ...string) schemaNode {
	return schemaNode{kind: nodeString, enum: values}
}

// integerParam is an integer with an inclusive lower bound.
func integerParam(minimum int) schemaNode {
	m := minimum
	return schemaNode{kind: nodeInteger, minimum: &m}
}

func arrayParam(items schemaNode) schemaNode {
	it := items
	return schemaNode{kind: nodeArray, items: &it}
}

// objectParam declares an object with strict (additionalProperties:false)
// properties and an optional required key list.
func objectParam(props map[string]schemaNode, required ...string) schemaNode {
	return schemaNode{kind: nodeObject, props: props, req: required}
}

// --- Reusable composite fields (relocated from read_catalog.go's hand-written
// schema helpers; enum members now come from the platform value package). --------

func targetRefParam() schemaNode {
	return objectParam(map[string]schemaNode{
		"type":        enumParam(platform.TargetRefTypeValues()...),
		"value":       stringParam(),
		"source":      enumParam(platform.TargetSourceValues()...),
		"source_span": stringParam(),
	}, "type", "value", "source")
}

func targetRefsParam() schemaNode { return arrayParam(targetRefParam()) }

func cfsRefParam() schemaNode {
	return objectParam(map[string]schemaNode{"id": stringParam()}, "id")
}

func metricsParam() schemaNode { return arrayParam(enumParam(platform.MetricValues()...)) }

func timeWindowParam() schemaNode {
	return objectParam(map[string]schemaNode{
		"type":     enumParam(platform.TimeWindowTypeValues()...).described("preset 表示今天/昨天；relative 表示最近 N 分钟/小时；absolute 表示用户明确给出的起止时间。"),
		"preset":   enumParam("yesterday", "today").described("仅 type=preset 时填写；不要把今天或昨天换算为日期。"),
		"amount":   integerParam(1).described("仅 type=relative 时填写的正整数。"),
		"unit":     enumParam("minute", "hour").described("仅 type=relative 时填写。"),
		"start":    stringParam().described("仅 type=absolute；必须来自用户明确写出的时间，格式为 RFC3339 或 YYYY-MM-DD HH:MM。"),
		"end":      stringParam().described("仅 type=absolute；必须来自用户明确写出的时间，格式为 RFC3339 或 YYYY-MM-DD HH:MM。"),
		"timezone": enumParam("Asia/Shanghai", "UTC").described("可省略，默认 Asia/Shanghai。"),
	}, "type")
}

// jsonSchema renders the model-facing JSON parameter schema. Its output is
// byte-equivalent to the previous hand-written objectSchema/enumSchema helpers,
// so the tool the model sees is unchanged.
func (n schemaNode) jsonSchema() map[string]any {
	withDescription := func(out map[string]any) map[string]any {
		if n.description != "" {
			out["description"] = n.description
		}
		return out
	}
	switch n.kind {
	case nodeString:
		out := map[string]any{"type": "string"}
		if len(n.enum) > 0 {
			out["enum"] = append([]string(nil), n.enum...)
		}
		return withDescription(out)
	case nodeInteger:
		out := map[string]any{"type": "integer"}
		if n.minimum != nil {
			out["minimum"] = *n.minimum
		}
		return withDescription(out)
	case nodeArray:
		items := map[string]any{}
		if n.items != nil {
			items = n.items.jsonSchema()
		}
		return withDescription(map[string]any{"type": "array", "items": items})
	case nodeObject:
		props := map[string]any{}
		for name, child := range n.props {
			props[name] = child.jsonSchema()
		}
		out := map[string]any{"type": "object", "additionalProperties": false, "properties": props}
		if len(n.req) > 0 {
			out["required"] = append([]string(nil), n.req...)
		}
		return withDescription(out)
	}
	return map[string]any{}
}

// validate enforces exactly the constraints the schema advertises but the JSON
// decoder does not: string enum membership and integer minimums, recursively
// through objects and arrays. It deliberately does NOT enforce required-field
// presence — an absent required field is a needs_input clarification
// (MissingFields), a different control-flow outcome than an out-of-contract
// value. Only a PRESENT, non-empty value that violates the contract is an error;
// type mismatches remain the strict decoder's job.
func (n schemaNode) validate(value any, path string) error {
	switch n.kind {
	case nodeString:
		s, ok := value.(string)
		if !ok || s == "" || len(n.enum) == 0 {
			return nil
		}
		for _, allowed := range n.enum {
			if s == allowed {
				return nil
			}
		}
		return fmt.Errorf("%s: 值 %q 不在允许取值 %v 内", path, s, n.enum)
	case nodeInteger:
		f, ok := numericFloat(value)
		if !ok {
			return nil
		}
		if n.minimum != nil && f < float64(*n.minimum) {
			return fmt.Errorf("%s: %v 小于允许最小值 %d", path, value, *n.minimum)
		}
		return nil
	case nodeArray:
		items, ok := value.([]any)
		if !ok || n.items == nil {
			return nil
		}
		for i, el := range items {
			if err := n.items.validate(el, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	case nodeObject:
		obj, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		for name, child := range n.props {
			if v, present := obj[name]; present {
				if err := child.validate(v, joinPath(path, name)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return nil
}

func joinPath(path, field string) string {
	if path == "" {
		return field
	}
	return path + "." + field
}

// numericFloat extracts a float from the numeric encodings a decoded JSON value
// (float64), a json.Number, or a Go-literal test map (int) can carry.
func numericFloat(value any) (float64, bool) {
	switch t := value.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}
