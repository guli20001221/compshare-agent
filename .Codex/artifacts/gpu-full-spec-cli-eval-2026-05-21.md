# GPU full spec CLI eval - 2026-05-21

Environment:
- Loaded `F:\compshare-agent\eval\shadow_qa\.env.local`.
- Config: `deploy\conf\agent.yaml.example`.
- CLI binary built from branch `codex/gpu-full-spec-output`.

## Capability route, renderer off

Environment addition:
- `USE_INTENT_PLANNER_FOR=gpu_specs`
- `USE_GROUNDED_RENDERER` unset

Question: `4090 显存多大`

Observed:
- CLI called `DescribeAvailableCompShareInstanceTypes`.
- Reply stayed deterministic and compact:

```text
机型=4090, 性能=83, 显存=24GB, 状态=Normal, 最大卡数=8
机型=4090_48G, 性能=83, 显存=48GB, 状态=Normal, 最大卡数=8
```

Question: `4090 的所有规格`

Observed:
- CLI called `DescribeAvailableCompShareInstanceTypes`.
- Reply expanded all 4090 and 4090_48G combinations returned by the upstream API.
- Key observed combinations:

```text
4090 / cn-wlcb-01: 1卡/16C/64G, 1卡/16C/94G, 2卡/32C/128G, 2卡/32C/192G, 4卡/64C/256G, 4卡/64C/384G, 8卡/92C/940G, 8卡/124C/940G
4090 / cn-sh2-02: 1卡/16C/64G, 1卡/16C/94G, 2卡/32C/128G, 2卡/32C/192G, 4卡/64C/256G, 4卡/64C/384G, 8卡/92C/940G, 8卡/124C/940G
4090_48G / cn-wlcb-01: 1卡/16C/96G, 2卡/32C/224G, 4卡/64C/470G, 8卡/124C/940G
4090_48G / cn-sh2-02: 1卡/16C/96G, 2卡/32C/224G, 4卡/64C/470G, 8卡/124C/940G
```

Question: `列出所有 GPU 规格`

Observed:
- CLI called `DescribeAvailableCompShareInstanceTypes`.
- Reply included all returned models and expanded their full combinations, including 4090, 5090, 4090_48G, 3080Ti, 2080Ti, 3090, 2080, A800, H20, P40, V100S, and A100.

Question: `4090 支持哪些 CPU 和内存`

Observed:
- CLI called `DescribeAvailableCompShareInstanceTypes`.
- Reply used full-spec mode and expanded all CPU/memory combinations for 4090 and 4090_48G.

## Capability route, grounded renderer on

Environment addition:
- `USE_INTENT_PLANNER_FOR=gpu_specs`
- `USE_GROUNDED_RENDERER=llm`

Question: `4090 显存多大`

Observed:
- CLI banner showed `renderer: grounded_renderer=llm`.
- CLI called `DescribeAvailableCompShareInstanceTypes`.
- Reply was LLM-rendered and concise:

```text
根据本次返回的数据，标准版 4090 GPU 的显存为 24 GB。
如需更大显存，还有 4090_48G 版本，显存为 48 GB。
```

Question: `4090 的所有规格`

Observed:
- CLI banner showed `renderer: grounded_renderer=llm`.
- CLI called `DescribeAvailableCompShareInstanceTypes`.
- Reply was LLM-rendered and still preserved all full-spec combinations.
- Key observed combinations:

```text
4090 / cn-wlcb-01: 1卡/16C/64G, 1卡/16C/94G, 2卡/32C/128G, 2卡/32C/192G, 4卡/64C/256G, 4卡/64C/384G, 8卡/92C/940G, 8卡/124C/940G
4090 / cn-sh2-02: 1卡/16C/64G, 1卡/16C/94G, 2卡/32C/128G, 2卡/32C/192G, 4卡/64C/256G, 4卡/64C/384G, 8卡/92C/940G, 8卡/124C/940G
4090_48G / cn-wlcb-01: 1卡/16C/96G, 2卡/32C/224G, 4卡/64C/470G, 8卡/124C/940G
4090_48G / cn-sh2-02: 1卡/16C/96G, 2卡/32C/224G, 4卡/64C/470G, 8卡/124C/940G
```

## Default ReAct route smoke

Environment addition:
- `USE_INTENT_PLANNER_FOR` unset

Question: `4090 的所有规格`

Observed:
- CLI called `DescribeAvailableCompShareInstanceTypes`.
- In one smoke run it also called `GetGPUSpecs`.
- The final answer was produced by the normal ReAct LLM tool loop, not by the grounded renderer.
- This path can format nicely, but deterministic acceptance for this PR is the GPU specs capability route because it renders the upstream response from a controlled facts payload.
