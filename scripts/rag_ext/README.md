# scripts/rag_ext — external tool/ops corpus (RAG Phase 1+)

This pipeline builds **`deploy/kb/external_w0.jsonl`** — a *separate* pinned corpus of
general, platform-agnostic tool/ops knowledge (vLLM / sglang / ComfyUI / Ollama +
in-instance GPU troubleshooting, Linux ops, PyTorch basics, model-download, and
chat-seeded support gaps such as SSH keepalive, large transfers, remote web apps,
LoRA/QLoRA, NCCL/DDP debugging, plus AI4Science/professional GPU topics such as
JAX, CuPy, OpenMM, GROMACS, RAPIDS, container GPU access, ColabFold, and
AlphaFold3; and pro-GPU support topics such as Transformers/Accelerate,
bitsandbytes, LLaMA-Factory, Unsloth, DeepSpeed, git-lfs/aria2/wget/curl/rclone,
VS Code Remote/Jupyter/SSH tunnels, CUDA compatibility, and ComfyUI
Manager/Flux/ControlNet/IPAdapter; production GPU support such as DCGM/Nsight
monitoring, Docker/Kubernetes/GPU Operator/MIG, Triton/TensorRT-LLM, KServe,
Ray, Slurm, TRL, HF Datasets, Zarr, and Dask-cuDF; plus community-image-targeted
topics from the live popular image snapshot: LTX, digital humans, voice/TTS,
LoRA training, Qwen/Wan, GGUF, and single-image 3D; plus second-wave topics such
as sd-scripts/kohya/Diffusers LoRA training, faster-whisper/WhisperX/Demucs/FFmpeg,
Nerfstudio/COLMAP/PyTorch3D/Open3D, MONAI/PyG/DGL/PhysicsNeMo, nvitop/TensorBoard/W&B/MLflow,
Caddy/frp/Tailscale/Cloudflare Tunnel, Dockerfile/CUDA-image builds, uv/micromamba,
Hugging Face snapshot preload; plus 637-session-targeted gaps such as
A1111/SD-WebUI, ControlNet, generic WebUI refused-connection triage,
SSH/transfer failures, Ollama cache/context issues, Open WebUI/LiteLLM,
Docker GPU visibility, card-count mismatch, NVIDIA MPS, DVC/object storage,
Label Studio, persistence boundaries, background service patterns; focused
safe GPU cleanup / torch.compile / CUDA Toolkit installation topics; and
production/research platform topics such as FSDP checkpointing and memory
estimation, vLLM/LiteLLM serving governance, NCCL/RDMA validation,
Kueue/Kubeflow/Volcano job queues, lm-eval/MLflow evaluation, YOLO/SAM2,
LAMMPS/PySCF/Apptainer, WebDataset, ONNX Runtime/Optimum, and
AWQ/GPTQ/GGUF model-format guidance). It is loaded *alongside* the platform corpus
(`deploy/kb/stage2b_w0.jsonl`) via `knowledge.LoadPinnedCorporaWithEmbeddings`, so the
platform corpus stays byte-identical.

## Why a separate corpus

- Platform knowledge is maintained elsewhere (a GitLab repo) and is OUT of scope here.
- External chunks carry `source_origin: external_official | external_community` (not
  `official`), so provenance is auditable and the runtime/eval can tell them apart.
- Its own digest pins (`ExternalCorpusDigestExpected` / `ExternalEmbeddingDigestExpectedQwen3`)
  mean rebuilding it never touches the platform pins or the frozen 377-Q platform parity.

## Authoring discipline (correctness-gated)

Curation is done by a strong model (Claude Opus) from **authoritative sources**:

1. Prefer **official tool docs** (vLLM/sglang/ComfyUI/Ollama). `source_origin: external_official`.
2. Competitor-community content may be used only after a **neutral rewrite**: strip the
   competitor platform's name/UI/console references, keep the generally-true technical
   content. `source_origin: external_community`. **Never re-attribute another platform's
   claims to CompShare.** Never fabricate flags / UI steps / field names not in the source.
3. Cite the source in `source_refs` (e.g. `vllm-docs:getting_started/quickstart`).
   `surface_url` stays `null` — external doc URLs are not on the platform surface allowlist.
4. `acl: customer_safe`, `evidence_kind: knowledge`. `product_area` is one of the external
   areas in `scripts/rag_w0/common.py` ALLOWED_PRODUCT_AREAS (`inference_serving`,
   `gpu_troubleshooting`, ...). Content ≤ 4000 runes.

## Build flow (reuses the W0 tail stages — no fork)

```
# 1. Author candidate chunks -> scripts/rag_ext/external_candidate_w0.jsonl
python -m scripts.rag_ext.build_pilot_chunks

# 2. Schema/leakage validate (shared validator, now enforces source_origin)
python -m scripts.rag_w0.validate_chunks --chunks scripts/rag_ext/external_candidate_w0.jsonl
python -m scripts.rag_w0.check_internal_leakage --chunks scripts/rag_ext/external_candidate_w0.jsonl

# 3. Promote to deploy/kb/external_w0.jsonl, then build the qwen3 sidecar
python -m scripts.rag_w0.build_corpus_embeddings \
    --corpus deploy/kb/external_w0.jsonl --out-dir deploy/kb \
    --env F:/compshare-agent/.env.local --embed-model qwen3-embedding-8b

# 4. Pin the two external digests (corpus + qwen3 sidecar) in
#    internal/knowledge/corpus_digest.go, then:
#      - external retrieval eval (Top-3 >= 0.85)  : scripts.rag_w0.evaluate_retrieval
#      - Go loads external_w0.jsonl clean + parity (TestLoadPinnedCorpus...)
```

The offline answer/faithfulness evaluator (`evaluate_answers`) was removed: it
simulated the retired terminal-RAG prompt and could report passes unrelated to the
central Agent's real HTTP/WebSocket answer path. Answer-quality eval should be
re-derived from real traces. Retrieval recall + platform non-regression (377-Q parity
+ Top-3) still apply and must stay unchanged — external is additive.
