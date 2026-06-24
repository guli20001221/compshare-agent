# External KB Cleanup / Compile / Toolkit Expansion - 2026-06-24

## Summary

This update adds focused external knowledge for three support gaps:

- Safe `nvidia-smi` cleanup and process handling.
- PyTorch `torch.compile` use, graph-break troubleshooting, and dynamic-shape behavior.
- CUDA Toolkit installation choices for Linux.

The content is grounded in PyTorch documentation, NVIDIA CUDA installation guides, and the NVIDIA `nvidia-smi` manual.

## Added chunks

4 external chunks were added:

- `ext-nvidia-smi-safe-cleanup-001`
- `ext-pytorch-torch-compile-basic-001`
- `ext-pytorch-torch-compile-debug-001`
- `ext-cuda-toolkit-install-linux-001`

The external corpus increased from 176 to 180 chunks.

The focused addition increased the golden retrieval set from 169 to 173 questions.
After review, 7 direct chat-seeded coverage questions were added, bringing the
current external golden set to 180 questions.

## Upstream sources used

- PyTorch `torch.compile` tutorial: https://docs.pytorch.org/tutorials/intermediate/torch_compile_tutorial.html
- PyTorch compiler troubleshooting: https://docs.pytorch.org/docs/stable/torch.compiler_troubleshooting.html
- PyTorch compiler dynamic shapes: https://docs.pytorch.org/docs/stable/torch.compiler_dynamic_shapes.html
- NVIDIA CUDA Installation Guide for Linux: https://docs.nvidia.com/cuda/cuda-installation-guide-linux/index.html
- NVIDIA CUDA Quick Start Guide: https://docs.nvidia.com/cuda/cuda-quick-start-guide/index.html
- NVIDIA `nvidia-smi` manual: https://docs.nvidia.com/deploy/nvidia-smi/
- NVIDIA CUDA Compatibility: https://docs.nvidia.com/deploy/cuda-compatibility/

## Current artifact pins

- External corpus digest: `03d16590076cc8e4eee005962277281b896a595b62a5e9779c5f71dbad832a1c`
- External qwen3 sidecar digest: `841a209b522144612010ee9e92ba8b53b90b6c556a939cdcc20e742f4fe46d7d`
- Sidecar file: `deploy/kb/embeddings_03d16590076cc8e4eee005962277281b896a595b62a5e9779c5f71dbad832a1c_qwen3-embedding-8b.jsonl`
- Retrieval result file: `eval/rag_ext_external_retrieval_2026-06-24.json`

## Verification

Fresh verification passed on 2026-06-24:

- `python scripts/rag_w0/validate_chunks.py --chunks deploy/kb/external_w0.jsonl`
- `python scripts/rag_w0/check_internal_leakage.py --chunks deploy/kb/external_w0.jsonl` -> 180 chunks, 0 flagged
- `python -m py_compile scripts/rag_ext/build_pilot_chunks.py scripts/rag_ext/build_external_golden.py scripts/rag_ext/run_external_retrieval_eval.py`
- `go test ./internal/knowledge -run "TestLoadExternalCorpusPinnedRealData|TestMergePlatformAndExternalRealData" -count=1`
- `go test ./cmd -run "TestExternalKnowledge|TestLoadKnowledgeCorporaMergeAndDegrade" -count=1`
- `python scripts/rag_ext/run_external_retrieval_eval.py --chunks deploy/kb/external_w0.jsonl --questions scripts/rag_ext/external_golden.jsonl --out eval/rag_ext_external_retrieval_2026-06-24.json --embeddings-path deploy/kb/embeddings_03d16590076cc8e4eee005962277281b896a595b62a5e9779c5f71dbad832a1c_qwen3-embedding-8b.jsonl --mode qwen3_rrf --env F:\compshare-agent\.env.local`

Retrieval result:

- Overall: 180/180 Top-3 hits
- Chat-seeded direct coverage group: 7/7 Top-3 hits
- Cleanup/compile/toolkit group: 4/4 Top-3 hits
- Gate: pass (`top_3_hit_rate = 1.0 >= 0.85`)
