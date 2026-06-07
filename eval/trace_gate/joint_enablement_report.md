# Joint-enablement preview eval — external KB + agentic SearchKnowledge BOTH on

**Goal (the user's gate):** before flipping `COMPSHARE_EXTERNAL_KNOWLEDGE` + `COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE`
default-ON together, confirm the assistant does **not** confuse CompShare's **platform-hosted
ModelVerse API** ("平台托管 API") with **self-hosted vLLM/SGLang/Ollama services** ("自建服务").

**This is a PRE-MERGE PREVIEW.** Run against worktree `exec/p5-agentic-flip` @ `5f9d607`
(the #239+#240 stack = faithful preview of post-merge main). The authoritative run must be
re-done on merged `main` after #239→#240 land.

## Config
`ext=1 agentic=1`, `ds-v4-flash`, qwen3 RRF, N=3, 34 probes = 102 live runs.
Probe set + per-run observations + per-probe adversarial verdicts committed alongside:
`joint_enablement_probes.json`, `joint_enablement_observations.jsonl`, `joint_enablement_analysis.json`.

Probe set (workflow-designed, adversarial): 8 self_hosted, 9 platform_api, 12 boundary/confusion-traps, 5 regression.
Judged by one agent per probe → adversarial skeptic re-verify on every boundary/platform/confusion verdict → synthesis.

## Result on the CORE axis (platform-hosted-API vs self-hosted-service): essentially clean, ONE genuine edge confusion

| class | n | pass | on-core-axis confusion | notes |
|---|---|---|---|---|
| self_hosted | 8 | 8 | 0 | all grounded on `ext-*`; gave `vllm serve` / `sglang.launch_server` / `ollama pull` / `localhost:8000\|11434/v1`; **zero** redirected to the ModelVerse console / platform base_url |
| platform_api | 9 | 6 | 0 (no self-host swap) | 6 clean `w0-modelverse-*`; 2 honest corpus-gap refusals; 1 planner routing defect (off-axis, below) |
| boundary | 12 | 8 | **1 genuine** | comparative/disentangling probes PASS with explicit 请勿混淆 separation; one one-sided-premise case fails |
| regression | 5 | 5 | 0 | pricing/stock→handlers, off-topic/account-billing→refuse, instance-symptom→diagnosis; **zero SearchKnowledge misfire** |

### The one genuine, on-axis confusion
- **`bd-apikey-where-selfhost-mixup`** — Q: "我起了个本地服务，调的时候 api key 在哪拿". All 3 runs sent
  the user to grab a ModelVerse **platform-console** API Key and put it in `Authorization`, never flagging
  that a **self-hosted** vllm/ollama service needs **no** platform key. The corpus has no self-hosted-key
  material, so the model collapsed onto the platform interpretation. Skeptic AGREED, 3/3.
  *This is the real platform-hosted-API vs self-hosted-service confusion the gate guards against.*

### Adjacent findings (real, but NOT the core axis — do not, by themselves, gate the platform/self-host decision)
- **`bd-plan-vs-gpu-hour-billing`** — the adversarial judge flagged this as confusion, but fact-check (full
  replies read) shows it is **grounded-correct for the dominant reading**: the question is framed around GPU
  instances + hourly rental, so "套餐" naturally means the GPU-instance **prepaid package (包日/包月)**, which
  the platform genuinely offers; the reply correctly opened "套餐…和按小时租机器…是两种**不同的计费方式**，不是一回事"
  and grounded on `w0-billing_rule`. Nit: it did not proactively note the **separate** ModelVerse Coding-Plan 套餐.
  This is **platform-billing vs platform-billing**, off the platform-API-vs-self-hosted axis.
- **`pa-modelverse-models-list`** (1/3 runs) — planner mis-routed a hosted-API "拉模型列表" request to
  `model_repository_browse` (DescribeModelRepositoryModels = deploy-side model-FILE repo) instead of ModelVerse
  `GET /v1/models`. A **routing defect** (deploy-repo vs hosted-API), not a self-hosted swap. Runs 1–2 correct.
- **`pa-modelverse-deepseek-pricing` / `pa-modelverse-seedance-async`** — honest "当前知识库未覆盖" / empty
  abstentions (`intent=unknown` short-circuits to refusal **before** the agent loop, so SearchKnowledge can't
  rescue them). Corpus/planner-coverage gaps, **safe (no fabrication), pre-existing, not caused by enablement.**

## Enablement-safety confirmations
- **No regression drift:** all 5 regression probes byte-stable; SearchKnowledge stayed OFF on deterministic/refusal/diagnosis lanes and fired only on knowledge_qa/terminal_rag.
- **Relevance floor working:** no false-grounding on irrelevant platform chunks; co-retrieved distractor `ext-ollama` chunks were retrieved-but-not-cited on platform-API probes (model correctly ignored them).
- **Directional grounding correct both ways:** self-hosted→external, platform-API→platform.

## Verdict & recommendation
On the exact axis the gate names (平台托管 API vs 自建服务), the joint config is **mostly clean with ONE genuine
edge-case confusion** (self-host-context → platform-key default). That single case means a **pure default-on flip
is not yet justified by the gate as stated**. Recommended next step (small, targeted — NOT planner slimming):

1. Add a focused **anti-confusion prompt anchor**: when the user states a self-hosted-service context
   (本地服务 / 自建 / vllm / ollama / sglang / localhost), the assistant must NOT default to the ModelVerse
   console API Key; clarify or state a self-hosted service needs no platform key. (Optionally also add the
   套餐-line disambiguation: ModelVerse Coding-Plan 套餐 ≠ GPU-instance 包日/包月.)
2. Re-run the 3 affected probes at **N≥5** to confirm resolution + stability (jitter check).
3. Then flip both flags default-on together.

The two corpus gaps (modelverse pricing + seedance async chunks) and the model-list planner mis-route are
**independent** of the confusion gate — separate follow-up tickets, not blockers for it.

Reproduce: `eval/agentic_rag_probe.ps1 -ProbesPath eval/trace_gate/joint_enablement_probes.json -Runs 3 -External 1 -AgenticSearch 1`

---

## Anchor validation (2026-06-07, N=5, ext=1 agentic=1, anchor-patched binary)
Fix = narrow self-host-key anti-confusion bullets added to the existing #237 block in
`internal/prompt/rag_system_segments/third_party_tool_addendum.txt` (+ protective test extended).
Probes: `anchor_validation_probes.json` / `anchor_validation_observations.jsonl` (13 probes).

| probe | before | after (N=5) | result |
|---|---|---|---|
| `bd-apikey-where-selfhost-mixup` (generic "本地服务") | PLATFORM-key 3/3 (confusion) | **ABSTAIN 5/5** | confusion ELIMINATED; safely abstains (no positive disentangle — no tool named ⇒ no citable self-host chunk) |
| `vsk-vllm-apikey-selfhost` (explicit vLLM) | — | SELFHOST-correct 4/5 (1 abstain jitter) | ✓ |
| `vsk-ollama-apikey-selfhost` (explicit Ollama) | — | SELFHOST-correct 5/5 | ✓ |
| `sh-ollama-openai-api`, `sh-vllm-serve-qwen` | correct | SELFHOST-correct 5/5 | ✓ no regression |
| `pa-modelverse-apikey-where` (over-corr guard) | platform-key | **PLATFORM-key 5/5** | ✓ NOT over-corrected |
| `pa-modelverse-openai-sdk-qwen` | platform | answer 4/5 (1 abstain jitter) | ✓ |
| regression ×5 (pricing/stock/diagnosis/offtopic/account) | clean | correct intents, **SK 0/5** | ✓ no misfire |

**Gate met:** zero platform↔self-host confusion (5/5), platform-key path intact (5/5), explicit self-host correct, regression clean.
**Residual:** generic "本地服务" (no tool named) abstains instead of stating "self-hosted needs no platform key" — cite-or-refuse has no self-host-auth chunk to cite. Two paths: (A) ship anchor as-is (gate met; generic case safely abstains); (B) also add ONE small external self-host-auth chunk so the generic case answers positively (corpus re-pin, heavier).

---

## Authoritative re-run on MERGED MAIN + anchor (2026-06-07, N=5)
After #239→#240 merged (origin/main = 2a8f12a), the anchor was rebased onto merged main (5ad6bed),
rebuilt, full `go test ./...` green. Re-ran the FULL 34-probe joint enablement at N=5 (170 runs),
ext=1 agentic=1 — the authoritative on-main evidence (preview on the pre-merge stack does NOT count).
Raw: `joint_onmain_anchor_observations.jsonl`.

**Phase-2 gate MET on merged main:**
- core confusion: `bd-apikey-where-selfhost-mixup` TRUE-ABSTAIN 5/5 (the one on-axis confusion is fixed); NO self_hosted→platform swap, NO platform_api→selfhost swap across all 34 probes.
- self-hosted (8/8) ground external: vllm serve / sglang.launch_server / ollama / CUDA_VISIBLE_DEVICES.
- platform-key not over-corrected: pa-modelverse-apikey-where PLATFORM 5/5, pa-…-openai-sdk-qwen PLATFORM 5/5.
- regression (5/5): SearchKnowledge 0/5 misfire; pricing/stock→handlers, off-topic/account→refuse, instance-symptom→diagnosis.
- bonus: bd-ollama-selfhost-deepseek SELFHOST 5/5 (preview was mixed), pa-…-seedance-async PLATFORM 5/5.

CAVEAT (method): a crude keyword classifier first mis-flagged correct self-hosted serving answers as
"ABSTAIN" because they contain a hedge phrase about a sub-detail; reading the replies confirmed every
self-hosted probe returns the correct self-host command. Don't trust the keyword metric over the text.
