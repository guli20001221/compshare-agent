# DS v4 flash Tool Choice RE-PROBE (supersedes 2026-05-08)

- Date: 2026-06-08
- Base URL: `https://api.modelverse.cn/v1`
- Model: `deepseek-v4-flash`
- Request shape: production first-LLM-request — full `tools.VisibleRegistry(true)` = **37 tools**, real `prompt.BuildSystemWithOptions(...)` ReAct system prompt, and a `system -> user -> assistant -> user` multi-turn history (synthetic resource context, no real-account data).
- Stream: `true`
- Pass definition: HTTP 2xx **and** returned `tool_calls` contains the forced target.

## Why re-probe

The 2026-05-08 probe (`2026-05-08-ds-v4-flash-tool-choice-probe.md`) found **object** `tool_choice {"type":"function","function":{"name":X}}` returned **HTTP 400** uniformly (6/6 tools) on this model in thinking mode, and `internal/llm/capability.go` pinned `SupportsObjectToolChoice: false` for flash as a result. That gate forces the monitor-recall bridge (`engine.go`) to fall back from precise object tool_choice to `"required"` + an ephemeral system note. This re-probe checks whether the upstream rejection still holds at the same production shape.

## Decision Table (OBJECT tool_choice, N=3 each)

| Tool | 2026-05-08 | 2026-06-08 object | Notes |
|---|---|---:|---|
| `GetCompShareInstanceMonitor` | FAIL(400) | **3/3 forced** | 200 + forced call |
| `DescribeCompShareInstance` | FAIL(400) | **3/3 forced** | 200 + forced call |
| `DiagnoseSSH` | FAIL(400) | **3/3 forced** | 200 + forced call |
| `DiagnoseBilling` | FAIL(400) | **3/3 forced** | 200 + forced call |

**12/12 forced, 0×400, 0×other.** Every tool that uniformly 400'd in May now returns HTTP 200 with the forced tool_call. (An earlier N=5 run over the same shape was identical: Monitor 5/5, DescribeInstance 5/5.)

### Caveat — the one 400 in the first run was NOT a capability signal

The first run also forced `SearchKnowledge` and got HTTP 400 — but the message was `Invalid value for 'tool_choice': no function named 'SearchKnowledge' was specified in the 'tools' parameter`. `SearchKnowledge` is gated out of `VisibleRegistry` unless `COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE` enables it, so it simply wasn't in the request — a **probe-input** error (forcing an absent tool), not a thinking-mode tool_choice rejection. The hardened harness asserts target ∈ registry before forcing, so it can only fire on real in-registry tools. Lesson: a forced-tool 400 must be read by its message — "no function named X in tools" ≠ "object tool_choice unsupported in thinking mode".

## Decision

**Flip `internal/llm/capability.go` flash `SupportsObjectToolChoice: false -> true`.** The only behavioral consumer is the monitor-recall force path (`engine.go`, the `if e.supportsObjectToolChoice` branch) — it now forces the exact `GetCompShareInstanceMonitor` tool again instead of the `"required"` + note fallback. The planner does not consult this flag. Hot-rollback without a redeploy: a `COMPSHARE_LLM_CAPABILITY_FILE` YAML override with `supports_object_tool_choice: false`.

## Reproduce

Gated live test (skipped unless `COMPSHARE_LIVE_PROBE=1`); run from a worktree with `deploy/conf/agent.yaml` + the CLI env (`LLM_API_KEY`, STS keys):

```go
// internal/engine/<scratch>_test.go  (package engine; not shipped — throwaway harness)
client := llm.NewClient(cfg.Agent.LLM)
toolset := tools.VisibleRegistry(true) // full production surface (~37)
sys := prompt.BuildSystemWithOptions(syntheticResourceCtx, prompt.BuildOptions{MutatingToolsEnabled: true})
msgs := []openai.ChatCompletionMessage{
    {Role: openai.ChatMessageRoleSystem, Content: sys},
    {Role: openai.ChatMessageRoleUser, Content: "uhost-... 的 GPU/显存使用率？"},
    {Role: openai.ChatMessageRoleAssistant, Content: "好的，我来查询。"},
    {Role: openai.ChatMessageRoleUser, Content: "再看下最新利用率，并查知识库排查建议。"},
}
for _, tgt := range []string{"GetCompShareInstanceMonitor","DescribeCompShareInstance","DiagnoseSSH","DiagnoseBilling"} {
    req := llm.ChatRequest{Messages: msgs, Tools: toolset,
        ToolChoice: openai.ToolChoice{Type: openai.ToolTypeFunction, Function: openai.ToolFunction{Name: tgt}}}
    resp, err := client.Chat(ctx, req) // expect err==nil, resp.ToolCalls forces tgt
}
```

Pace calls ~1.5s apart to avoid the upstream 429 rate limit.
