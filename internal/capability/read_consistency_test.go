package capability

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/require"
)

func jsonFieldNames(t reflect.Type) []string {
	out := []string{}
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func schemaPropertyNames(schema map[string]any) []string {
	props, _ := schema["properties"].(map[string]any)
	out := make([]string, 0, len(props))
	for k := range props {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func schemaRequired(schema map[string]any) []string {
	out := []string{}
	switch req := schema["required"].(type) {
	case []string:
		out = append(out, req...)
	case []any:
		for _, r := range req {
			if s, ok := r.(string); ok {
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

func schemaEnum(schema map[string]any) []string {
	out := []string{}
	switch e := schema["enum"].(type) {
	case []string:
		out = append(out, e...)
	case []any:
		for _, v := range e {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func missingFieldsOnZero(t reflect.Type) []string {
	req, ok := reflect.New(t).Elem().Interface().(platform.ReadRequest)
	if !ok {
		return nil
	}
	out := []string{}
	for _, m := range req.MissingFields() {
		out = append(out, m.Name)
	}
	sort.Strings(out)
	return out
}

// TestReadCatalogSchemaMatchesRequestStruct is the top-level typed-capability
// consistency gate: for EVERY read capability, the JSON schema the model sees
// must declare exactly the request struct's JSON fields, and the schema's
// required set must equal the fields the validator flags as missing on a zero
// request. Adding a field to a request struct without adding it to the schema —
// the model never sees it and go test would otherwise stay green — fails here;
// so does a validator/schema required-set drift.
func TestReadCatalogSchemaMatchesRequestStruct(t *testing.T) {
	for _, def := range ReadDefinitions() {
		t.Run(def.Name, func(t *testing.T) {
			require.NotNil(t, def.RequestType, "definition %s has no RequestType", def.Name)
			require.NotNil(t, def.Tool.Function)
			schema, ok := def.Tool.Function.Parameters.(map[string]any)
			require.True(t, ok, "schema must be a map[string]any")
			require.ElementsMatch(t, jsonFieldNames(def.RequestType), schemaPropertyNames(schema),
				"tool schema properties must equal request JSON fields")
			require.ElementsMatch(t, missingFieldsOnZero(def.RequestType), schemaRequired(schema),
				"schema required set must equal validator-required fields")
		})
	}
}

func TestEveryReadCapabilityHasAnExplicitPresentationContract(t *testing.T) {
	want := map[string]ReadPresentation{
		"resource_info":              ReadPresentationRequired,
		"monitor_query":              ReadPresentationRequired,
		"monitor_history":            ReadPresentationRequired,
		"gpu_specs_query":            ReadPresentationBrowse,
		"stock_availability":         ReadPresentationRequired,
		"image_list":                 ReadPresentationBrowse,
		"image_tag_catalog":          ReadPresentationBrowse,
		"zone_catalog":               ReadPresentationBrowse,
		"model_repository_browse":    ReadPresentationBrowse,
		"network_accelerator_status": ReadPresentationRequired,
		"instance_access":            ReadPresentationExact,
		"pricing_query":              ReadPresentationRequired,
		"refund_estimate":            ReadPresentationRequired,
		"cfs_list":                   ReadPresentationRequired,
		"cfs_create_price":           ReadPresentationRequired,
		"cfs_upgrade_price":          ReadPresentationRequired,
		"cfs_refund_estimate":        ReadPresentationRequired,
		"account_finance_status":     ReadPresentationGuidance,
	}
	definitions := ReadDefinitions()
	require.Len(t, definitions, len(want), "new read capabilities must be classified here")
	for _, def := range definitions {
		require.Equal(t, want[def.Name], def.Presentation, "presentation contract for %s", def.Name)
		require.True(t, def.Presentation.Valid(), "presentation contract for %s must be valid", def.Name)
	}
}

// enumValuesForType is the single binding from a Go enum type to its allowed
// wire values. It is the ONLY place the recursive consistency test re-associates
// a type with a value set, and every entry defers to the platform value
// function that also feeds the capability schema — so schema and expectation
// cannot list different members.
func enumValuesForType(typ reflect.Type) ([]string, bool) {
	switch typ {
	case reflect.TypeOf(platform.TargetRefType("")):
		return platform.TargetRefTypeValues(), true
	case reflect.TypeOf(platform.TargetSource("")):
		return platform.TargetSourceValues(), true
	case reflect.TypeOf(platform.Metric("")):
		return platform.MetricValues(), true
	case reflect.TypeOf(platform.TimeWindowType("")):
		return platform.TimeWindowTypeValues(), true
	case reflect.TypeOf(platform.ImageSource("")):
		return platform.ImageSourceValues(), true
	case reflect.TypeOf(platform.ListMode("")):
		return platform.ListModeValues(), true
	case reflect.TypeOf(platform.PriceKind("")):
		return platform.PriceKindValues(), true
	case reflect.TypeOf(platform.DetailLevel("")):
		return platform.DetailLevelValues(), true
	}
	return nil, false
}

// TestReadSchemaStructurallyMatchesRequestType is the recursive half of the
// field-contract gate. It walks the rendered JSON schema against the Go request
// type node-for-node — object properties, array items, nested objects, scalar
// kinds, enum members and additionalProperties:false — so a capability cannot
// declare a schema whose SHAPE (not just top-level field names) diverges from
// the struct it decodes into. Together with the runtime-validation tests below,
// the schema, the Go type and the enforced constraint are one contract.
func TestReadSchemaStructurallyMatchesRequestType(t *testing.T) {
	for _, def := range ReadDefinitions() {
		t.Run(def.Name, func(t *testing.T) {
			schema, ok := def.Tool.Function.Parameters.(map[string]any)
			require.True(t, ok, "schema must be a map[string]any")
			assertSchemaMatchesType(t, schema, def.RequestType, def.Name)
		})
	}
}

func assertSchemaMatchesType(t *testing.T, schema map[string]any, typ reflect.Type, path string) {
	t.Helper()
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		require.Equal(t, "object", schema["type"], "%s: expected an object schema", path)
		require.Equal(t, false, schema["additionalProperties"], "%s: object schema must be strict (additionalProperties:false)", path)
		props, _ := schema["properties"].(map[string]any)
		require.ElementsMatch(t, jsonFieldNames(typ), schemaPropertyNames(schema),
			"%s: schema properties must equal struct json fields", path)
		for _, r := range schemaRequired(schema) {
			require.Contains(t, props, r, "%s: required key %q is not a declared property", path, r)
		}
		for i := 0; i < typ.NumField(); i++ {
			name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				continue
			}
			child, ok := props[name].(map[string]any)
			require.True(t, ok, "%s: property %q missing or not an object schema", path, name)
			assertSchemaMatchesType(t, child, typ.Field(i).Type, joinPath(path, name))
		}
	case reflect.Slice:
		require.Equal(t, "array", schema["type"], "%s: expected an array schema", path)
		items, ok := schema["items"].(map[string]any)
		require.True(t, ok, "%s: array schema is missing its items schema", path)
		assertSchemaMatchesType(t, items, typ.Elem(), path+"[]")
	case reflect.String:
		require.Equal(t, "string", schema["type"], "%s: expected a string schema", path)
		if want, ok := enumValuesForType(typ); ok {
			require.ElementsMatch(t, want, schemaEnum(schema),
				"%s: schema enum must equal the %s value set", path, typ.Name())
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		require.Equal(t, "integer", schema["type"], "%s: expected an integer schema", path)
	case reflect.Bool:
		require.Equal(t, "boolean", schema["type"], "%s: expected a boolean schema", path)
	default:
		t.Fatalf("%s: unhandled Go kind %s for schema %v", path, typ.Kind(), schema)
	}
}

// TestReadRuntimeValidation_RejectsOutOfContractValues proves the field contract
// is ENFORCED at decode, not merely advertised. Each case is a value the schema
// forbids (an unknown enum member, or an integer outside its declared bounds)
// that the pre-contract decoder accepted and silently defaulted — the exact bug
// class the escalated Issue 3 named (source/price_kind:"bogus" quietly defaulted,
// gpu_count:-1 quietly became 1).
func TestReadRuntimeValidation_RejectsOutOfContractValues(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"pricing unknown price_kind", ReadToolName(intent.IntentPricingQuery), map[string]any{"gpu_type": "4090", "price_kind": "bogus"}},
		{"pricing gpu_count below minimum", ReadToolName(intent.IntentPricingQuery), map[string]any{"gpu_type": "4090", "gpu_count": -1}},
		{"pricing gpu_count zero", ReadToolName(intent.IntentPricingQuery), map[string]any{"gpu_type": "4090", "gpu_count": 0}},
		{"image list unknown source", ReadToolName(intent.IntentImageList), map[string]any{"source": "bogus"}},
		{"image list unknown mode", ReadToolName(intent.IntentImageList), map[string]any{"mode": "bogus"}},
		{"gpu specs unknown detail_level", ReadToolName(intent.IntentGPUSpecsQuery), map[string]any{"detail_level": "bogus"}},
		{"monitor unknown metric element", ReadToolName(intent.IntentMonitorQuery), map[string]any{"metrics": []any{"bogus"}}},
		{"monitor unknown nested target type", ReadToolName(intent.IntentMonitorQuery), map[string]any{"targets": []any{map[string]any{"type": "bogus", "value": "x", "source": "user_text"}}}},
		{"cfs create target_size_gb below minimum", namedReadToolName(readCFSCreatePrice), map[string]any{"zone": "cn-wlcb-01", "target_size_gb": -5}},
		{"instance access port above maximum", namedReadToolName(instanceAccessCapabilityLabel), map[string]any{
			"targets":     []any{map[string]any{"type": "uhost_id_user_input", "value": "uhost-a", "source": "user_text"}},
			"access_type": "custom_port", "protocol": "tcp", "port": 65536,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := DecodeReadRequest(tc.tool, tc.args)
			require.Error(t, err, "an out-of-contract value must be rejected, not silently defaulted")
		})
	}
}

// TestReadRuntimeValidation_AcceptsInContractValues is the adversarial control
// for the rejection test: the same tools decode cleanly for in-contract values
// AND for omitted optional fields (absence stays governed by MissingFields /
// needs_input, so the default path is not broken). Without this, "rejects
// everything" would pass the rejection test vacuously.
func TestReadRuntimeValidation_AcceptsInContractValues(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"pricing full valid", ReadToolName(intent.IntentPricingQuery), map[string]any{"gpu_type": "4090", "gpu_count": 8, "price_kind": "catalog"}},
		{"pricing optionals omitted", ReadToolName(intent.IntentPricingQuery), map[string]any{"gpu_type": "4090"}},
		{"image list valid enums", ReadToolName(intent.IntentImageList), map[string]any{"source": "community", "mode": "filtered"}},
		{"image list empty (default platform)", ReadToolName(intent.IntentImageList), map[string]any{}},
		{"gpu specs valid detail_level", ReadToolName(intent.IntentGPUSpecsQuery), map[string]any{"detail_level": "full"}},
		{"monitor valid metrics + target", ReadToolName(intent.IntentMonitorQuery), map[string]any{"metrics": []any{"gpu", "vram"}, "targets": []any{map[string]any{"type": "name", "value": "train-a", "source": "user_text"}}}},
		{"cfs create valid", namedReadToolName(readCFSCreatePrice), map[string]any{"zone": "cn-wlcb-01", "target_size_gb": 50}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := DecodeReadRequest(tc.tool, tc.args)
			require.NoError(t, err, "an in-contract request must decode cleanly")
		})
	}
}
