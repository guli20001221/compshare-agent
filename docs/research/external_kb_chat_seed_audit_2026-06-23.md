# External KB Chat-Seeded Expansion (2026-06-23)

## Scope

- Seed file reviewed: `F:\compshare-agent\docs\聊天记录.md`
- Seed file role: customer-question mining only; raw chat content is not used as authoritative KB evidence.
- Target corpus: `deploy/kb/external_w0.jsonl`
- Prior external corpus: 55 chunks
- Chat-seeded support gap addition: 7 chunks
- AI4Science / professional GPU addition: 8 chunks
- Training/download/remote-dev/CUDA/ComfyUI ecosystem addition: 20 chunks
- Production GPU support addition: 30 chunks
- Community-image-targeted addition (2026-06-24): 13 chunks; see `docs/research/community_image_targeted_external_kb_2026-06-24.md`
- Second-wave external addition (2026-06-24): 25 chunks; see `docs/research/external_kb_second_wave_audit_2026-06-24.md`
- 637-session-targeted external addition (2026-06-24): 18 chunks; see `docs/research/external_kb_session_637_targeted_audit_2026-06-24.md`
- Cleanup/compile/toolkit focused addition (2026-06-24): 4 chunks; see `docs/research/external_kb_cuda_compile_cleanup_audit_2026-06-24.md`
- New external corpus: 180 chunks

## Why The Chat Log Is Useful

The chat log is useful as a demand signal. It shows repeated customer questions around resource shortage, no-GPU startup, billing after shutdown, image/data migration, large file uploads, SSH instability, public ports, model downloads, GPU memory, and multi-GPU behavior.

It is not suitable as a factual source by itself because many answers are short support replies, context-dependent, or platform-specific. For this update, it was used only to choose topics. Chunk facts were grounded in external official documentation.

## Added Chunks

This table records the 2026-06-23 expansion. The 2026-06-24 community-image-targeted
extension is tracked separately in `docs/research/community_image_targeted_external_kb_2026-06-24.md`.

| Chunk ID | Topic | Primary Sources |
| --- | --- | --- |
| `ext-linux-ssh-keepalive-001` | SSH / VS Code Remote disconnect keepalive | OpenSSH `ssh_config` |
| `ext-linux-large-transfer-rsync-001` | Large file and directory transfer with resume | rsync manual, OpenSSH config |
| `ext-webapp-gradio-remote-001` | Gradio remote access | Gradio launch docs |
| `ext-webapp-streamlit-uvicorn-remote-001` | Streamlit / FastAPI remote access | Streamlit config, Uvicorn settings |
| `ext-hf-cache-cli-download-001` | Hugging Face cache, CLI download, token | Hugging Face Hub env vars and CLI |
| `ext-peft-lora-qlora-001` | LoRA / QLoRA memory-oriented setup | Hugging Face PEFT docs |
| `ext-pytorch-nccl-ddp-debug-001` | torchrun / NCCL / DDP debugging | PyTorch distributed docs, NVIDIA NCCL env docs |
| `ext-jax-gpu-install-001` | JAX GPU install and backend verification | JAX installation docs |
| `ext-cupy-gpu-array-001` | CuPy CUDA wheel install and GPU array verification | CuPy install docs |
| `ext-openmm-cuda-platform-001` | OpenMM CUDA platform selection for molecular dynamics | OpenMM user guide |
| `ext-gromacs-gpu-offload-001` | GROMACS mdrun GPU offload and task mapping | GROMACS mdrun performance guide |
| `ext-rapids-cudf-install-001` | RAPIDS/cuDF install checks for GPU data processing | RAPIDS install guide |
| `ext-docker-gpu-container-001` | NVIDIA Container Toolkit GPU visibility checks | NVIDIA Container Toolkit docs |
| `ext-colabfold-local-gpu-001` | ColabFold local GPU prediction, MSA, and database planning | ColabFold README |
| `ext-alphafold3-docker-gpu-001` | AlphaFold3 Docker GPU preparation | AlphaFold3 installation docs |
| `ext-transformers-trainer-gpu-001` | Transformers Trainer GPU training knobs and checkpoint checks | Hugging Face Transformers Trainer docs |
| `ext-accelerate-launch-001` | Accelerate single-node multi-GPU launch | Hugging Face Accelerate launch docs |
| `ext-bitsandbytes-quantization-001` | bitsandbytes 8bit/4bit loading and QLoRA limits | Hugging Face Transformers bitsandbytes docs |
| `ext-llamafactory-lora-train-001` | LLaMA-Factory LoRA/QLoRA training startup and resource planning | LLaMA-Factory README |
| `ext-unsloth-finetune-001` | Unsloth fine-tuning install and GPU compatibility checks | Unsloth README |
| `ext-deepspeed-zero-001` | DeepSpeed ZeRO memory optimization and Accelerate launch | Hugging Face Accelerate DeepSpeed docs, DeepSpeed docs |
| `ext-git-lfs-model-download-001` | git-lfs model repository large file download checks | git-lfs docs |
| `ext-aria2-parallel-download-001` | aria2 concurrent download and resume | aria2 manual |
| `ext-wget-curl-resume-001` | wget/curl interrupted download resume | GNU Wget manual, curl manpage |
| `ext-rclone-sync-copy-001` | rclone object storage / remote copy and sync | rclone copy docs |
| `ext-vscode-remote-server-001` | VS Code Remote SSH port forwarding and server recovery | VS Code Remote SSH docs |
| `ext-jupyter-server-remote-001` | Jupyter Server remote GPU access | Jupyter Server public server docs |
| `ext-ssh-tunnel-reverse-proxy-001` | SSH local forwarding, reverse forwarding, and reverse proxy choice | OpenSSH manual |
| `ext-nccl-advanced-debug-001` | NCCL multi-GPU/multi-node hang debugging | NVIDIA NCCL environment docs |
| `ext-cuda-driver-compatibility-001` | CUDA Toolkit, driver, and runtime compatibility | NVIDIA CUDA compatibility docs |
| `ext-framework-cuda-match-001` | PyTorch/framework CUDA wheel matching | PyTorch install docs |
| `ext-comfyui-manager-001` | ComfyUI Manager custom node install/update | ComfyUI custom node docs, ComfyUI Manager README |
| `ext-comfyui-flux-models-001` | ComfyUI Flux missing models, wrong directories, and low VRAM | ComfyUI Flux docs |
| `ext-comfyui-controlnet-001` | ComfyUI ControlNet / controlnet_aux model and preprocessor errors | ComfyUI ControlNet docs, controlnet_aux README |
| `ext-comfyui-ipadapter-001` | ComfyUI IPAdapter / Flux IPAdapter model paths and node versions | ComfyUI IPAdapter community docs, x-flux-comfyui README |
| `ext-dcgm-exporter-prometheus-001` | DCGM Exporter GPU metrics for Prometheus/Grafana | NVIDIA DCGM Exporter docs |
| `ext-dcgm-diagnostics-run-001` | DCGM Diagnostics for GPU readiness and failure checks | NVIDIA DCGM Diagnostics docs |
| `ext-nsight-systems-profile-001` | Nsight Systems timeline profiling for CUDA/PyTorch | NVIDIA Nsight Systems docs |
| `ext-nsight-compute-kernel-profile-001` | Nsight Compute kernel-level profiling | NVIDIA Nsight Compute docs |
| `ext-gpu-telemetry-mig-metrics-001` | MIG instance telemetry and DCGM metric interpretation | NVIDIA DCGM Exporter docs, MIG guide |
| `ext-docker-run-gpu-access-001` | Docker `--gpus` container GPU access validation | Docker GPU docs, NVIDIA Container Toolkit docs |
| `ext-docker-compose-gpu-001` | Docker Compose GPU device reservations | Docker Compose GPU docs |
| `ext-k8s-gpu-resource-request-001` | Kubernetes `nvidia.com/gpu` resource requests | Kubernetes GPU scheduling docs |
| `ext-gpu-operator-stack-001` | NVIDIA GPU Operator component stack | NVIDIA GPU Operator docs |
| `ext-k8s-gpu-pod-pending-001` | GPU Pod Pending and `nvidia.com/gpu` capacity checks | Kubernetes GPU scheduling docs, GPU Operator docs |
| `ext-mig-partition-001` | MIG resource partitioning concepts and limits | NVIDIA MIG guide |
| `ext-k8s-mig-device-plugin-001` | MIG resources in Kubernetes | NVIDIA MIG on Kubernetes docs |
| `ext-k8s-gpu-time-slicing-001` | GPU time-slicing and sharing caveats | NVIDIA GPU Operator sharing docs |
| `ext-triton-model-repository-001` | Triton model repository and serving entrypoints | NVIDIA Triton docs |
| `ext-triton-dynamic-batching-001` | Triton batching throughput/latency tradeoffs | NVIDIA Triton docs |
| `ext-tensorrt-llm-triton-001` | TensorRT-LLM with Triton for optimized LLM serving | NVIDIA TensorRT-LLM/Triton docs |
| `ext-triton-genai-perf-metrics-001` | Triton / TensorRT-LLM GenAI-Perf and metrics | NVIDIA TensorRT-LLM/Triton docs |
| `ext-kserve-inferenceservice-001` | KServe InferenceService production serving | KServe docs |
| `ext-kserve-multinode-vllm-001` | KServe multi-node/multi-GPU vLLM limitations | KServe multi-node vLLM docs |
| `ext-tgi-maintenance-migration-001` | TGI maintenance-mode selection guidance | Hugging Face TGI README |
| `ext-ray-gpu-tasks-001` | Ray GPU task/actor resource scheduling | Ray docs |
| `ext-ray-train-gpu-scaling-001` | Ray Train GPU ScalingConfig | Ray Train docs |
| `ext-ray-serve-gpu-replicas-001` | Ray Serve GPU replica allocation | Ray Serve docs |
| `ext-slurm-gpu-gres-001` | Slurm GRES GPU job submission | Slurm GRES docs |
| `ext-slurm-gpu-accounting-mig-001` | Slurm GPU accounting, MPS/MIG, and shared resources | Slurm GRES docs |
| `ext-trl-sfttrainer-001` | TRL SFTTrainer supervised fine-tuning | Hugging Face TRL docs |
| `ext-trl-dpotrainer-001` | TRL DPOTrainer preference training | Hugging Face TRL docs |
| `ext-hf-datasets-streaming-cache-001` | Hugging Face Datasets streaming and cache placement | Hugging Face Datasets docs |
| `ext-zarr-science-array-storage-001` | Zarr chunked scientific array storage | Zarr docs |
| `ext-dask-cudf-multigpu-001` | Dask-cuDF / Dask-CUDA multi-GPU data processing | Dask and RAPIDS docs |

## External Sources Checked

- OpenSSH manual: `https://man.openbsd.org/ssh_config`
- rsync manual: `https://download.samba.org/pub/rsync/rsync.1`
- Gradio launch options via official docs.
- Streamlit config: `https://docs.streamlit.io/develop/api-reference/configuration/config.toml`
- Uvicorn settings via official GitHub docs: `https://github.com/encode/uvicorn/blob/master/docs/settings.md`
- Hugging Face Hub env vars: `https://huggingface.co/docs/huggingface_hub/en/package_reference/environment_variables`
- Hugging Face Hub CLI: `https://huggingface.co/docs/huggingface_hub/en/guides/cli`
- PEFT LoRA / quantization docs.
- PyTorch torchrun / distributed docs.
- NVIDIA NCCL environment variable docs: `https://docs.nvidia.com/deeplearning/nccl/user-guide/docs/env.html`
- JAX installation docs: `https://docs.jax.dev/en/latest/installation.html`
- CuPy install docs: `https://docs.cupy.dev/en/stable/install.html`
- OpenMM running simulations guide: `https://docs.openmm.org/latest/userguide/application/02_running_sims.html`
- GROMACS mdrun performance guide: `https://manual.gromacs.org/current/user-guide/mdrun-performance.html`
- RAPIDS install guide: `https://docs.rapids.ai/install/`
- NVIDIA Container Toolkit sample workload and Docker GPU configuration docs.
- ColabFold README: `https://github.com/sokrypton/ColabFold`
- AlphaFold3 installation docs: `https://github.com/google-deepmind/alphafold3/blob/main/docs/installation.md`
- Hugging Face Transformers Trainer docs: `https://huggingface.co/docs/transformers/en/trainer`
- Hugging Face Transformers bitsandbytes docs: `https://huggingface.co/docs/transformers/en/quantization/bitsandbytes`
- Hugging Face Accelerate launch docs: `https://huggingface.co/docs/accelerate/en/basic_tutorials/launch`
- Hugging Face Accelerate DeepSpeed docs: `https://huggingface.co/docs/accelerate/en/usage_guides/deepspeed`
- DeepSpeed config docs: `https://www.deepspeed.ai/docs/config-json/`
- LLaMA-Factory README: `https://github.com/hiyouga/LLaMA-Factory`
- Unsloth README: `https://github.com/unslothai/unsloth`
- git-lfs docs: `https://git-lfs.com/`
- aria2 manual: `https://aria2.github.io/manual/en/html/aria2c.html`
- GNU Wget manual: `https://www.gnu.org/software/wget/manual/wget.html`
- curl manpage: `https://curl.se/docs/manpage.html`
- rclone copy docs: `https://rclone.org/commands/rclone_copy/`
- VS Code Remote SSH docs: `https://code.visualstudio.com/docs/remote/ssh`
- Jupyter Server public server docs: `https://jupyter-server.readthedocs.io/en/latest/operators/public-server.html`
- NVIDIA CUDA compatibility docs: `https://docs.nvidia.com/deploy/cuda-compatibility/`
- ComfyUI custom node docs: `https://docs.comfy.org/installation/install_custom_node`
- ComfyUI Manager README: `https://github.com/Comfy-Org/ComfyUI-Manager`
- ComfyUI Flux docs: `https://docs.comfy.org/tutorials/flux/flux1-krea-dev`
- ComfyUI Flux ControlNet docs: `https://docs.comfy.org/tutorials/flux/flux-1-controlnet`
- ComfyUI Depth ControlNet docs: `https://docs.comfy.org/tutorials/controlnet/depth-controlnet`
- ComfyUI IPAdapter Plus README: `https://github.com/cubiq/ComfyUI_IPAdapter_plus`
- ComfyUI controlnet_aux README: `https://github.com/Fannovel16/comfyui_controlnet_aux`
- x-flux-comfyui README: `https://github.com/XLabs-AI/x-flux-comfyui`
- NVIDIA DCGM Exporter docs: `https://docs.nvidia.com/datacenter/dcgm/latest/gpu-telemetry/dcgm-exporter.html`
- NVIDIA DCGM Diagnostics docs: `https://docs.nvidia.com/datacenter/dcgm/latest/user-guide/dcgm-diagnostics.html`
- NVIDIA Nsight Systems docs: `https://docs.nvidia.com/nsight-systems/UserGuide/index.html`
- NVIDIA Nsight Compute docs: `https://docs.nvidia.com/nsight-compute/ProfilingGuide/index.html`
- Docker GPU docs: `https://docs.docker.com/engine/containers/gpu/`
- Docker Compose GPU docs: `https://docs.docker.com/compose/how-tos/gpu-support/`
- Kubernetes GPU scheduling docs: `https://kubernetes.io/docs/tasks/manage-gpus/scheduling-gpus/`
- NVIDIA GPU Operator docs: `https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/index.html`
- NVIDIA GPU sharing docs: `https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/gpu-sharing.html`
- NVIDIA MIG guide: `https://docs.nvidia.com/datacenter/tesla/mig-user-guide/latest/index.html`
- NVIDIA MIG on Kubernetes docs: `https://docs.nvidia.com/datacenter/cloud-native/kubernetes/latest/index.html`
- NVIDIA Triton docs: `https://docs.nvidia.com/deeplearning/triton-inference-server/user-guide/docs/index.html`
- NVIDIA TensorRT-LLM / Triton docs: `https://docs.nvidia.com/deeplearning/triton-inference-server/user-guide/docs/getting_started/trtllm_user_guide.html`
- KServe docs: `https://kserve.github.io/website/docs/intro`
- KServe multi-node vLLM docs: `https://kserve.github.io/website/docs/model-serving/generative-inference/multi-node`
- Ray docs via Context7/Ray upstream documentation.
- Slurm GRES docs: `https://slurm.schedmd.com/gres.html`
- Hugging Face TRL SFT/DPO docs.
- Hugging Face Datasets streaming/cache docs.
- Zarr docs: `https://zarr.readthedocs.io/`
- Dask GPU docs: `https://docs.dask.org/en/stable/gpu.html`
- RAPIDS Dask-cuDF and cuML Dask docs.
- Hugging Face TGI README: `https://github.com/huggingface/text-generation-inference`

## Promotion Work

- Updated source authoring script: `scripts/rag_ext/build_pilot_chunks.py`
- Regenerated candidate chunks: `scripts/rag_ext/external_candidate_w0.jsonl`
- Promoted candidate to: `deploy/kb/external_w0.jsonl`
- Extended external retrieval golden set: `scripts/rag_ext/external_golden.jsonl` (180 questions; includes direct golden coverage for the 7 chat-seeded support-gap chunks)
- Rebuilt qwen3 embedding sidecar:
  `deploy/kb/embeddings_03d16590076cc8e4eee005962277281b896a595b62a5e9779c5f71dbad832a1c_qwen3-embedding-8b.jsonl`
- Updated external corpus and embedding digest pins in `internal/knowledge/corpus_digest.go`

## Verification

The candidate chunk file passed:

- `scripts/rag_w0/validate_chunks.py`
- `scripts/rag_w0/check_internal_leakage.py`
- `scripts/rag_ext/run_external_retrieval_eval.py` with `qwen3_rrf`: latest run is tracked in `eval/rag_ext_external_retrieval_2026-06-24.json`

The rebuilt external corpus has:

- External corpus digest: `03d16590076cc8e4eee005962277281b896a595b62a5e9779c5f71dbad832a1c`
- External qwen3 sidecar digest: `841a209b522144612010ee9e92ba8b53b90b6c556a939cdcc20e742f4fe46d7d`
