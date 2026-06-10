# External corpus — model-download vertical smoke report

Model-download vertical (ModelScope / HF gated-token / Ollama Modelfile / local-path
serving) added to `deploy/kb/external_w0.jsonl`: **51 → 55 chunks** (+4). Reuses the
existing `inference_serving` product_area — **no enum churn**. A dedicated hf-mirror
chunk was intentionally NOT added: `HF_ENDPOINT` / mirror download is already covered
by `ext-gpu-hf-download-001`, and a near-duplicate crowded the platform HF-download FAQ
out of a golden query's top-3 (the hybrid-recall pattern). The concrete `hf-mirror.com`
naming is delivered operationally by the deploy reply's self-pull guidance (PR #266).

## New chunks (4)
| chunk_id | topic | product_area |
|---|---|---|
| `ext-modelscope-download-001` | ModelScope（魔搭）下载模型（`modelscope download` / `snapshot_download`） | inference_serving |
| `ext-hf-token-001` | 受限(gated)模型的 HuggingFace 访问令牌（`huggingface-cli login` / `HF_TOKEN`） | inference_serving |
| `ext-ollama-modelfile-001` | Ollama 导入本地 GGUF（`Modelfile` + `ollama create -f`） | inference_serving |
| `ext-serve-local-path-001` | 用本地已下载的模型目录起服务（vLLM/SGLang `--model-path /path`） | inference_serving |

## Gates — ALL GREEN
- **Digest-pinned loader** (`go test ./internal/knowledge ./cmd`): external corpus 55 / merged 742 load clean with re-pinned `ExternalCorpusDigestExpected=6058e11b…` + `ExternalEmbeddingDigestExpectedQwen3=1865a211…`.
- **External retrieval Top-3** (`run_external_retrieval_eval.py`, qwen3_rrf): **1.0 (55/55)**; new `model_download` group **4/4 = 1.0**; all prior groups unchanged at 1.0.
- **Platform 256-Q parity** (`run_platform_parity.py`, merged 742 vs platform 687): **PASS** — Top-3 0.8242 → 0.8281 (**+0.0039**, aggregate AND per-group non-regression, **0 per-question displacements**, 1 improvement). The first 55-chunk run regressed `image_types_gpu_specs` 25→24 (w0-golden-0064, the dropped hf-mirror near-duplicate displacing the platform FAQ); removing that chunk restored per-group parity.
- **`go test ./...`** (COMPSHARE_PROJECT_ID=test-project): exit 0.
- **CLI smoke** (authoritative faithfulness; agent.exe, merged 742 index default-on, deepseek-v4-flash, read-only): **5/5**.

## CLI smoke detail (merged index, v4-flash)
Boot: `rag: merged external knowledge corpus deploy/kb/external_w0.jsonl into the index (742 total chunks)`.

| Q | result |
|---|---|
| 除了 huggingface，国内还能从哪儿下载模型？ | → SearchKnowledge → grounded ModelScope answer (`pip install modelscope` / `modelscope download` / `snapshot_download`) ✓ |
| 下载 llama 的时候提示没有权限，需要先申请什么吗？ | → 申请访问 + Access Token + `huggingface-cli login` / `HF_TOKEN` ✓ |
| 我有个 gguf 文件，想让 ollama 直接用它，怎么导入？ | → `Modelfile` `FROM ./model.gguf` + `ollama create -f` + `ollama run` ✓ |
| vllm 怎么加载本地已经下好的模型目录，不想每次重新下？ | → `vllm serve /data/models/...` + 完整仓库 + 先下到数据盘 ✓ |
| 云主机是怎么计费的？包月和按量有什么区别？ (anti-contamination control) | → pure platform 按量/包月 billing answer, **0 model-download leak** ✓ |

Conservative authoring per the external-corpus style: 操作顺序 not 结论, no hardcoded
volatile mirror URLs, platform-specific disk/quota deferred to platform docs, every
chunk ends with an 以官方文档为准 caveat. Traces/scratch gitignored; harness queries
listed above are reproducible from a clean checkout via `agent.exe cli -c
deploy/conf/agent.yaml.example` with `COMPSHARE_EXTERNAL_KNOWLEDGE=1`.
