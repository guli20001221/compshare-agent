package capability

import (
	"reflect"
	"sort"
	"strings"
	"testing"

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

// TestReadCatalogSchemaMatchesRequestStruct is the typed-capability consistency
// gate: for EVERY read capability, the JSON schema the model sees must declare
// exactly the request struct's JSON fields, and the schema's required set must
// equal the fields the validator flags as missing on a zero request. Adding a
// field to a request struct without adding it to the schema — the model never
// sees it and go test would otherwise stay green — fails here; so does a
// validator/schema required-set drift.
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
