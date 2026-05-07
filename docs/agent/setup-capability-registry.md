# CapabilityRegistry Setup

`internal/llm/capability.go` keeps provider/model behavior in a small lookup
table keyed by `(base_url, model)`. Stage 2 planner code will use this table to
choose the safest structured-output path and avoid provider-specific failures
such as thinking-mode models rejecting object `tool_choice`.

## Built-In Matrix

The built-in table includes the Phase 0 acceptance models:

| Base URL | Model | JSON Schema | JSON Object | Thinking | Object Tool Choice |
| --- | --- | ---: | ---: | ---: | ---: |
| `https://api.modelverse.cn/v1` | `deepseek-v4-flash` | no | yes | no | yes |
| `https://api.modelverse.cn/v1` | `Qwen/Qwen3-Max` | no | yes | no | yes |
| `https://api.modelverse.cn/v1` | `qwen3.6-plus` | no | yes | yes | no |
| `https://api.modelverse.cn/v1` | `glm-5-turbo` | no | yes | yes | no |
| `https://api.modelverse.cn/v1` | `doubao-seed-2-0-lite-260215` | no | yes | yes | yes |
| `https://ark.cn-beijing.volces.com/api/v3` | `doubao-lite` | no | yes | no | yes |

Unknown tuples return conservative false capability values.

Notes from the 2026-05-07 Modelverse probe:

- The current test key has no permission for `deepseek-v4-flash`; the matrix
  keeps the entry for accounts where that model is enabled.
- `Qwen/Qwen3-Max` is reachable and supports `json_object` plus object/required
  `tool_choice`, so it matches the default model in `deploy/conf/agent.yaml.example`.
- `qwen3.6-plus` rejects object/required `tool_choice` in thinking mode.
- `glm-5-turbo` accepts the `tool_choice` parameter but did not return
  `tool_calls` in the probe, so the matrix treats tool-choice support as false.

## Runtime Override

Set `COMPSHARE_LLM_CAPABILITY_FILE` to a YAML file. `LookupCapability` reads the
file on each call, so developers can edit it without recompiling or restarting
unit tests.

```yaml
capabilities:
  - base_url: "https://api.modelverse.cn/v1"
    model: "deepseek-v4-flash"
    supports_json_schema: false
    supports_json_object: true
    is_thinking_mode: false
    supports_object_tool_choice: true
    supports_required_tool_choice: true
    requires_extra_body:
      thinking:
        type: disabled
```

An override with the same `(base_url, model)` replaces the built-in entry. A new
tuple adds a new model. Base URLs are normalized by lowercasing and trimming a
trailing slash.
