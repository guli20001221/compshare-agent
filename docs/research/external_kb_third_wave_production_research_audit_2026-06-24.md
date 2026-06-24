# External KB Third-Wave Production / Research Audit (2026-06-24)

## Summary

This report records the third-wave external KB expansion on branch
`codex/kb-compshare-docs-main-audit`.

The external corpus increased from 180 to 224 chunks. The new 44 chunks focus on
questions customers are likely to ask when using the platform for production AI
serving, multi-GPU or multi-node training, evaluation, professional CV, AI4Science,
and large-scale data/model-format workflows.

The expansion is still a curated support corpus, not a full mirror of external
manuals. Chunks are written as customer support runbooks/FAQs and cite upstream
official or authoritative project sources.

## Added Coverage

| Group | Chunks | Examples |
|---|---:|---|
| `advanced_training` | 8 | Accelerate FSDP, checkpoint merge, FSDP vs DeepSpeed, memory estimation, device map/offload, Megatron-LM, activation checkpointing |
| `production_serving_governance` | 9 | vLLM LoRA/dynamic LoRA, structured outputs, Prometheus metrics, OpenTelemetry, security, LiteLLM keys/budgets/rate limits, prefix cache |
| `cluster_network` | 5 | nccl-tests, GPU-to-NIC topology, ACS/IOMMU/RDMA, InfiniBand low-level tests, minimal multi-node NCCL env |
| `job_scheduling` | 5 | Kueue PyTorchJob, ResourceFlavor/ClusterQueue quota, Kubeflow Trainer, Volcano gang scheduling, failed job triage |
| `eval_observability` | 5 | lm-evaluation-harness, custom eval tasks, MLflow GenAI evaluation, model registry promotion, release gates |
| `cv_ai4science_data` | 12 | Ultralytics YOLO, SAM2, LAMMPS GPU, GPU4PySCF, Apptainer GPU containers, WebDataset, ONNX/Optimum, AWQ/GPTQ/GGUF guidance |

## Generated Artifacts

- External corpus: `deploy/kb/external_w0.jsonl`
- External golden questions: `scripts/rag_ext/external_golden.jsonl`
- External qwen3 sidecar:
  `deploy/kb/embeddings_d76f2cc633987cac4c88bcb3339ea50e262099a7eb14995e7a90b030ab909d38_qwen3-embedding-8b.jsonl`
- Retrieval evaluation:
  `eval/rag_ext_external_retrieval_2026-06-24.json`

## Pins

- External corpus digest:
  `d76f2cc633987cac4c88bcb3339ea50e262099a7eb14995e7a90b030ab909d38`
- External qwen3 sidecar digest:
  `d219162444ae434f213183add8c47adc9b804365d7818cb1bf6fd4d1fcc1b076`

These supersede the cleanup/compile/toolkit-stage external digest
`03d16590076cc8e4eee005962277281b896a595b62a5e9779c5f71dbad832a1c` and
sidecar digest `841a209b522144612010ee9e92ba8b53b90b6c556a939cdcc20e742f4fe46d7d`.

## Verification

- `python scripts/rag_ext/build_pilot_chunks.py` -> 224 chunks
- `python scripts/rag_ext/build_external_golden.py` -> 224 golden questions
- Direct coverage check -> 224/224 chunks covered
- `python scripts/rag_w0/validate_chunks.py --chunks deploy/kb/external_w0.jsonl` -> pass
- `python scripts/rag_w0/check_internal_leakage.py --chunks deploy/kb/external_w0.jsonl` -> 224 chunks, 0 flagged
- `python scripts/rag_ext/run_external_retrieval_eval.py ... --mode qwen3_rrf` -> Top-3 = 1.0 (224/224)

All third-wave groups had Top-3 hit rate 1.0:

- `advanced_training`: 8/8
- `production_serving_governance`: 9/9
- `cluster_network`: 5/5
- `job_scheduling`: 5/5
- `eval_observability`: 5/5
- `cv_ai4science_data`: 12/12
