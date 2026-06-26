package intent

import "encoding/json"

// IntentRouteResponseSchema returns the JSON Schema for the intent router's
// IntentRoute output as a json.RawMessage, suitable as the `schema` of an
// OpenAI-style response_format json_schema. It is built from the live Go enums
// (RuntimeIntents + the validator-accepted target-ref types, metrics, and
// time-window types) so it cannot drift from validator.go — TestIntentRoute
// ResponseSchema_IntentEnumMatchesRuntime guards that.
//
// NON-strict by design. IntentRoute is polymorphic (filter/slot_position
// target_refs legitimately omit the source/source_span that name/uhost refs
// require) and uses omitempty throughout, while time_window is nullable. OpenAI
// strict mode requires every property to be in `required` with
// additionalProperties:false at every level, which cannot express that without
// either over-constraining valid output (breaking ValidateRoute) or fragile
// per-type anyOf. A 2026-06-23 live probe confirmed modelverse enforces the
// enum/const/required constraints even in NON-strict mode, so this still
// constrains the high-value fields — the intent enum, the schema_version const,
// the confidence bounds, and the seven required top-level fields — which is what
// the schema_valid failure modes (unknown intent, missing schema_version) are
// made of, without the strict-mode risk. The schema deliberately allows extra
// properties (Reasoning/Scope/Skills are omitempty struct fields) and leaves
// nested optional fields out of `required`.
func IntentRouteResponseSchema() json.RawMessage {
	return IntentRouteResponseSchemaForIntents(routerRuntimeIntents(false))
}

func IntentRouteResponseSchemaForIntents(runtimeIntents []Intent) json.RawMessage {
	intents := make([]string, 0, len(runtimeIntents))
	for _, i := range runtimeIntents {
		intents = append(intents, string(i))
	}

	stringEnum := func(values ...string) map[string]any {
		return map[string]any{"type": "string", "enum": values}
	}

	targetRefItem := map[string]any{
		"type": "object",
		"properties": map[string]any{
			// Only the validator-accepted target-ref types (validateTargetRef):
			// the C15 Phase-A zone/image/gpu_model types are declared but rejected,
			// so omitting them keeps the model from emitting an always-invalid ref.
			"type": stringEnum(
				string(TargetRefFilter),
				string(TargetRefName),
				string(TargetRefUHostIDUserInput),
				string(TargetRefSlotPosition),
			),
			"value": map[string]any{"type": "string"},
			// source/source_span are required only for name/uhost refs (provenance);
			// filter/slot_position refs omit them, so they are NOT in `required`.
			"source":      stringEnum(string(SourceUserText), string(SourcePriorTurn)),
			"source_span": map[string]any{"type": "string"},
		},
		"required": []string{"type", "value"},
	}

	slots := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target_refs": map[string]any{"type": "array", "items": targetRefItem},
			"metrics": map[string]any{"type": "array", "items": stringEnum(
				string(MetricCPU), string(MetricMemory), string(MetricGPU), string(MetricVRAM),
			)},
			// slots.action (LifecycleAction) is deliberately omitted: the prompt
			// never asks the model to emit it and the engine re-derives it from the
			// user text (inferLifecycleAction). Non-strict + no additionalProperties
			// restriction means a model that does emit it is still accepted.
			// time_window is *TimeWindow (nullable, omitempty) — object or null.
			"time_window": map[string]any{
				"type": []string{"object", "null"},
				"properties": map[string]any{
					"type": stringEnum(
						string(TimeWindowPreset), string(TimeWindowRelative), string(TimeWindowAbsolute),
					),
					"value": map[string]any{"type": "string"},
				},
			},
		},
		"required": []string{"target_refs", "metrics"},
	}

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"schema_version": map[string]any{"type": "string", "const": SchemaVersion},
			"intent":         map[string]any{"type": "string", "enum": intents},
			"slots":          slots,
			"required_tools": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			// retrieval.enabled is pinned false: ValidateRoute rejects Enabled==true
			// (stage-2A RAG is disabled), so const-false constrains decoding and
			// removes a retry-able failure mode (mirrors the schema_version const).
			"retrieval":       map[string]any{"type": "object", "properties": map[string]any{"enabled": map[string]any{"type": "boolean", "const": false}}, "required": []string{"enabled"}},
			"hard_block_hint": map[string]any{"type": "boolean"},
			"confidence":      map[string]any{"type": "number", "minimum": 0, "maximum": 1},
		},
		"required": []string{"schema_version", "intent", "slots", "required_tools", "retrieval", "hard_block_hint", "confidence"},
	}

	raw, err := json.Marshal(schema)
	if err != nil {
		// The schema is composed of plain maps/slices/strings, so Marshal cannot
		// fail; fall back to an empty object rather than panicking in a request path.
		return json.RawMessage(`{"type":"object"}`)
	}
	return json.RawMessage(raw)
}
