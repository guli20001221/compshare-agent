# External KB Second-Wave Expansion - 2026-06-24

## Summary

This update adds a second wave of external support knowledge after the chat-seeded and community-image-targeted expansions. The goal is to cover customer questions around image LoRA training, audio/video GPU workflows, 3D reconstruction, medical/scientific GPU workloads, experiment tracking, reverse proxy/tunnel setup, and reusable CUDA image construction.

The new content uses public upstream documentation and project READMEs as factual sources. Community-image data and chat logs are used only as demand signals.

This report records the second-wave stage. The current corpus was later extended to 180 chunks; see `docs/research/external_kb_session_637_targeted_audit_2026-06-24.md` and `docs/research/external_kb_cuda_compile_cleanup_audit_2026-06-24.md`.

## Added chunks

25 external chunks were added:

- `ext-sd-scripts-lora-training-001`
- `ext-kohya-ss-gui-paths-001`
- `ext-diffusers-lora-dreambooth-001`
- `ext-sd-dataset-caption-buckets-001`
- `ext-sd-lora-merge-convert-001`
- `ext-faster-whisper-gpu-asr-001`
- `ext-whisperx-alignment-diarization-001`
- `ext-demucs-vocal-separation-001`
- `ext-ffmpeg-video-dubbing-chain-001`
- `ext-nerfstudio-install-custom-data-001`
- `ext-colmap-gpu-reconstruction-001`
- `ext-nerfstudio-splatfacto-gsplat-001`
- `ext-pytorch3d-install-cuda-001`
- `ext-open3d-pointcloud-visualization-001`
- `ext-monai-medical-imaging-001`
- `ext-pyg-dgl-gnn-install-001`
- `ext-physicsnemo-sciml-gpu-001`
- `ext-nvitop-user-gpu-monitor-001`
- `ext-tensorboard-pytorch-logging-001`
- `ext-wandb-mlflow-experiment-tracking-001`
- `ext-caddy-reverse-proxy-webui-001`
- `ext-tunnel-frp-tailscale-cloudflare-001`
- `ext-dockerfile-cuda-image-build-001`
- `ext-python-env-uv-micromamba-image-001`
- `ext-hf-snapshot-preload-image-001`

The external corpus increased from 133 to 158 chunks.

The golden retrieval set increased from 126 to 151 questions.

## Coverage added

- Image LoRA training: sd-scripts, kohya_ss, diffusers LoRA/DreamBooth, captions, buckets, LoRA export and ComfyUI placement.
- Audio/video production: faster-whisper, WhisperX, Demucs, FFmpeg extraction, slicing, muxing, and sync checks.
- 3D and spatial workloads: Nerfstudio, COLMAP, splatfacto / gsplat, PyTorch3D, Open3D.
- Professional GPU science workloads: MONAI medical imaging, PyTorch Geometric / DGL graph neural networks, NVIDIA PhysicsNeMo physics ML.
- Training observability: nvitop, TensorBoard, Weights & Biases, MLflow.
- Remote access patterns: Caddy reverse proxy, frp, Tailscale Funnel, Cloudflare Tunnel.
- Reusable image building: CUDA Dockerfile practices, NVIDIA framework containers, uv, micromamba, Hugging Face snapshot preloading.

## Upstream sources used

- sd-scripts: https://github.com/kohya-ss/sd-scripts
- kohya_ss: https://github.com/bmaltais/kohya_ss
- Hugging Face Diffusers LoRA training: https://huggingface.co/docs/diffusers/training/lora
- Hugging Face Diffusers DreamBooth training: https://huggingface.co/docs/diffusers/training/dreambooth
- faster-whisper: https://github.com/SYSTRAN/faster-whisper
- WhisperX: https://github.com/m-bain/whisperX
- Demucs: https://github.com/facebookresearch/demucs
- FFmpeg: https://ffmpeg.org/ffmpeg.html
- FFmpeg filters: https://ffmpeg.org/ffmpeg-filters.html
- Nerfstudio installation: https://docs.nerf.studio/quickstart/installation.html
- Nerfstudio custom data: https://docs.nerf.studio/quickstart/custom_dataset.html
- Nerfstudio splatfacto: https://docs.nerf.studio/nerfology/methods/splat.html
- COLMAP CLI: https://colmap.github.io/cli.html
- COLMAP FAQ: https://colmap.github.io/faq.html
- Open3D: https://github.com/isl-org/Open3D
- PyTorch3D: https://github.com/facebookresearch/pytorch3d
- PyTorch3D install: https://github.com/facebookresearch/pytorch3d/blob/main/INSTALL.md
- MONAI: https://monai.readthedocs.io/
- PyTorch Geometric install: https://pytorch-geometric.readthedocs.io/en/2.7.0/install/installation.html
- DGL: https://github.com/dmlc/dgl
- NVIDIA PhysicsNeMo: https://github.com/NVIDIA/physicsnemo
- nvitop: https://github.com/XuehaiPan/nvitop
- PyTorch TensorBoard: https://docs.pytorch.org/docs/stable/tensorboard.html
- Weights & Biases quickstart: https://docs.wandb.ai/models/quickstart
- Weights & Biases PyTorch integration: https://docs.wandb.ai/models/integrations/pytorch
- MLflow tracking: https://mlflow.org/docs/latest/ml/tracking/
- MLflow PyTorch: https://mlflow.org/docs/latest/ml/deep-learning/pytorch/
- Caddy reverse proxy: https://caddyserver.com/docs/quick-starts/reverse-proxy
- frp: https://github.com/fatedier/frp
- Tailscale Funnel: https://tailscale.com/docs/features/tailscale-funnel
- Cloudflare Tunnel: https://developers.cloudflare.com/tunnel/
- Docker build best practices: https://docs.docker.com/build/building/best-practices/
- NVIDIA framework containers: https://docs.nvidia.com/deeplearning/frameworks/user-guide/index.html
- uv features: https://docs.astral.sh/uv/getting-started/features/
- micromamba installation: https://mamba.readthedocs.io/en/latest/installation/micromamba-installation.html
- Hugging Face Hub file download: https://huggingface.co/docs/huggingface_hub/package_reference/file_download

## Artifact pins after this stage

- External corpus digest: `b9457548a185ca1abbb6932b01c6472626f18c1ab38b39c30b327fbae4d48321`
- External qwen3 sidecar digest: `399b9de622cd84372e1afd06d3c07293fef1ff796168e098fa71a5f0875ada5d`
- Sidecar file: `deploy/kb/embeddings_b9457548a185ca1abbb6932b01c6472626f18c1ab38b39c30b327fbae4d48321_qwen3-embedding-8b.jsonl`
- Retrieval result file: `eval/rag_ext_external_retrieval_2026-06-24.json`

These pins were superseded by the 637-session-targeted digest `16692137b3a4438a9e1e47412cc77fa04904bfcd26f40b43d6779c5bcc652e8f`, and then by the current cleanup/compile/toolkit digest `03d16590076cc8e4eee005962277281b896a595b62a5e9779c5f71dbad832a1c`.

## Verification

Fresh verification passed on 2026-06-24:

- `python scripts/rag_w0/validate_chunks.py --chunks deploy/kb/external_w0.jsonl`
- `python scripts/rag_w0/check_internal_leakage.py --chunks deploy/kb/external_w0.jsonl` -> 158 chunks, 0 flagged
- `python -m py_compile scripts/rag_ext/build_pilot_chunks.py scripts/rag_ext/build_external_golden.py scripts/rag_ext/run_external_retrieval_eval.py`
- `go test ./internal/knowledge -run "TestLoadExternalCorpusPinnedRealData|TestMergePlatformAndExternalRealData" -count=1`
- `go test ./cmd -run "TestExternalKnowledge|TestLoadKnowledgeCorporaMergeAndDegrade" -count=1`
- `python scripts/rag_ext/run_external_retrieval_eval.py --chunks deploy/kb/external_w0.jsonl --questions scripts/rag_ext/external_golden.jsonl --out eval/rag_ext_external_retrieval_2026-06-24.json --embeddings-path deploy/kb/embeddings_b9457548a185ca1abbb6932b01c6472626f18c1ab38b39c30b327fbae4d48321_qwen3-embedding-8b.jsonl --mode qwen3_rrf --env F:\compshare-agent\.env.local`

Retrieval result:

- Overall: 151/151 Top-3 hits
- Second-wave group: 25/25 Top-3 hits
- Gate: pass (`top_3_hit_rate = 1.0 >= 0.85`)
