package capability

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/require"
)

func TestImageListSemanticQueryLimitComesFromFieldContract(t *testing.T) {
	reg := NewReadCapability(imageListReadSpec())
	properties := reg.Schema()["properties"].(map[string]any)
	semanticQueries := properties["semantic_queries"].(map[string]any)
	require.Equal(t, 3, semanticQueries["maxItems"])

	for _, tc := range []struct {
		name    string
		queries any
		valid   bool
	}{
		{"empty", []any{}, true},
		{"at limit", []any{"vLLM", "SGLang", "Ollama"}, true},
		{"over limit", []any{"vLLM", "SGLang", "Ollama", "ComfyUI"}, false},
		{"typed slice at limit", []string{"vLLM", "SGLang", "Ollama"}, true},
		{"typed slice over limit", []string{"vLLM", "SGLang", "Ollama", "ComfyUI"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reg.Decode(map[string]any{
				"source": "platform", "query": "大模型推理", "semantic_queries": tc.queries,
			})
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "semantic_queries")
			}
		})
	}
}

func TestArrayFieldContractMaxItemsIsOptional(t *testing.T) {
	unbounded := arrayParam(stringParam())
	require.NotContains(t, unbounded.jsonSchema(), "maxItems")
	require.NoError(t, unbounded.validate([]any{"a", "b", "c", "d"}, "queries"))

	emptyOnly := boundedArrayParam(stringParam(), 0)
	require.Equal(t, 0, emptyOnly.jsonSchema()["maxItems"])
	require.NoError(t, emptyOnly.validate([]any{}, "queries"))
	require.ErrorContains(t, emptyOnly.validate([]any{"a"}, "queries"), "queries")
	bounded := boundedArrayParam(enumParam("allowed"), 1)
	require.NoError(t, bounded.validate([]string{"allowed"}, "queries"))
	require.ErrorContains(t, bounded.validate([]string{"invalid"}, "queries"), "queries[0]")
}

func TestImageListAllModeRejectsFiltersBeforeUpstream(t *testing.T) {
	reg := NewReadCapability(imageListReadSpec())
	for _, source := range platform.ImageSourceValues() {
		for _, tc := range []struct {
			name    string
			query   string
			queries []any
		}{
			{"query", "数字人", nil},
			{"expansion", "", []any{"LiveTalking"}},
			{"query and expansion", "数字人", []any{"LiveTalking"}},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				exec := &communityQueryExec{}
				request, err := reg.Decode(map[string]any{
					"source": source, "mode": "all", "query": tc.query, "semantic_queries": tc.queries,
				})
				require.NoError(t, err)
				// Use the same decode -> grounding -> Run order as the engine.
				err = ValidateCurrentTurnGrounding(request, "推荐数字人镜像")
				if err == nil {
					reg.Run(context.Background(), request, ReadRuntime{Executor: exec})
				}
				require.ErrorContains(t, err, "mode=all")
				require.Empty(t, exec.calls, "a conflicting browse/filter request must not issue a catalog read")
			})
		}
	}

	for _, query := range []string{"", " \n\t"} {
		request, err := reg.Decode(map[string]any{"mode": "all", "query": query, "semantic_queries": []any{}})
		require.NoError(t, err)
		require.NoError(t, ValidateCurrentTurnGrounding(request, "浏览所有镜像"))
	}
}

func TestImageListFilteredAndOmittedModeKeepLegalExpansion(t *testing.T) {
	reg := NewReadCapability(imageListReadSpec())
	for _, mode := range []string{"", "filtered"} {
		t.Run(mode, func(t *testing.T) {
			args := map[string]any{
				"source": "platform", "query": "Ubuntu", "semantic_queries": []any{"vLLM"},
			}
			if mode != "" {
				args["mode"] = mode
			}
			request, err := reg.Decode(args)
			require.NoError(t, err)
			require.NoError(t, ValidateCurrentTurnGrounding(request, "推荐 Ubuntu 镜像"))
			exec := &platformCatalogExec{}
			result := reg.Run(context.Background(), request, ReadRuntime{Executor: exec})
			names := envelopeSubjectNames(t, result)
			require.Contains(t, names, "Ubuntu 22.04 64位")
			require.Contains(t, names, "vLLM v0.25.1")
			require.NotContains(t, names, "SGLang v0.5.15")
			require.Equal(t, 1, exec.calls)
		})
	}
}
