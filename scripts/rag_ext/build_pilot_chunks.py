#!/usr/bin/env python3
"""Author the external tool/ops RAG chunks (vLLM / SGLang / Ollama / ComfyUI +
GPU ops + Linux-ops/env-management + PyTorch-basics).

Curated by Claude Opus from authoritative tool docs + well-established ops
knowledge. Content is platform-neutral (no CompShare / competitor references) and
uses only flags/behaviors documented by the cited sources. Writes
scripts/rag_ext/external_candidate_w0.jsonl; promote to deploy/kb/external_w0.jsonl
after validate_chunks + check_internal_leakage pass.

Authoring discipline (user calibration; memory
feedback-external-corpus-authoring-conservative):
  - Don't pin version-varying defaults (e.g. --gpu-memory-utilization) — point to
    `<tool> --help` / current docs instead.
  - Don't over-claim a flag's purpose (--enforce-eager is a CUDA-Graph diagnostic,
    not a stable memory saver).
  - Per-chunk shape: 适用场景 + 典型问法 + 3-6 处理动作 + 1-2 段最小代码 + 注意事项.
  - Troubleshooting chunks are 排查顺序, not definitive conclusions.

Sources:
  vllm-docs:getting_started/quickstart       https://docs.vllm.ai/en/latest/getting_started/quickstart.html
  vllm-docs:configuration/conserving_memory  https://docs.vllm.ai/en/latest/configuration/conserving_memory/
  vllm-docs:usage/troubleshooting            https://docs.vllm.ai/en/latest/usage/troubleshooting/
  vllm-docs:cli/serve                        https://docs.vllm.ai/en/stable/cli/serve
  sglang-docs:basic_usage/openai_api         https://docs.sglang.ai/basic_usage/openai_api.html
  sglang-docs:advanced_features/server_arguments  https://docs.sglang.ai/advanced_features/server_arguments.html
  sglang-docs:backend/hyperparameter_tuning  https://docs.sglang.ai/backend/hyperparameter_tuning.html
  ollama-docs:api/openai-compatibility       https://docs.ollama.com/api/openai-compatibility
  ollama-docs:faq                            https://docs.ollama.com/
  ollama-docs:modelfile                      https://docs.ollama.com/modelfile
  pytorch-docs:notes/cuda                    https://docs.pytorch.org/docs/stable/notes/cuda.html
  nvidia-docs:nvidia-smi                      https://docs.nvidia.com/deploy/nvidia-smi/
  nvidia-docs:cuda-install-linux              https://docs.nvidia.com/cuda/cuda-installation-guide-linux/index.html
  nvidia-docs:cuda-quick-start                https://docs.nvidia.com/cuda/cuda-quick-start-guide/index.html
  hf-docs:installation                        https://huggingface.co/docs/huggingface_hub/installation
  comfyui-docs:getting_started               https://docs.comfy.org/
  comfyui-repo:README                        https://github.com/comfyanonymous/ComfyUI
  comfyui-repo:comfy/cli_args.py             https://github.com/comfyanonymous/ComfyUI/blob/master/comfy/cli_args.py
  comfyui-repo:extra_model_paths.yaml.example  https://github.com/comfyanonymous/ComfyUI/blob/master/extra_model_paths.yaml.example
  tmux-wiki:getting-started                   https://github.com/tmux/tmux/wiki/Getting-Started
  linux-man:nohup                             https://man7.org/linux/man-pages/man1/nohup.1.html
  linux-man:coreutils                         https://www.gnu.org/software/coreutils/manual/
  procps-docs:top                             https://man7.org/linux/man-pages/man1/top.1.html
  conda-docs:user-guide                       https://docs.conda.io/projects/conda/en/stable/user-guide/
  python-docs:venv                            https://docs.python.org/3/library/venv.html
  pip-docs:user-guide                         https://pip.pypa.io/en/stable/user_guide/
  openssh-docs:ssh_config                     https://man.openbsd.org/ssh_config
  openssh-docs:ssh-keygen                     https://man.openbsd.org/ssh-keygen.1
  openssh-docs:scp                            https://man.openbsd.org/scp.1
  rsync-docs:man                              https://download.samba.org/pub/rsync/rsync.1
  gradio-docs:blocks-launch                   https://www.gradio.app/docs/blocks
  streamlit-docs:config                       https://docs.streamlit.io/develop/api-reference/configuration/config.toml
  uvicorn-docs:settings                       https://github.com/encode/uvicorn/blob/master/docs/settings.md
  hf-docs:environment_variables               https://huggingface.co/docs/huggingface_hub/en/package_reference/environment_variables
  hf-docs:cli                                 https://huggingface.co/docs/huggingface_hub/en/guides/cli
  hf-docs:security-tokens                     https://huggingface.co/docs/hub/security-tokens
  hf-docs:models-gated                        https://huggingface.co/docs/hub/models-gated
  peft-docs:lora                              https://huggingface.co/docs/peft/developer_guides/lora
  peft-docs:quantization                      https://huggingface.co/docs/peft/developer_guides/quantization
  pytorch-docs:get-started                    https://pytorch.org/get-started/locally/
  pytorch-docs:notes/cuda                     https://docs.pytorch.org/docs/stable/notes/cuda.html
  pytorch-docs:ddp-tutorial                   https://docs.pytorch.org/tutorials/intermediate/ddp_tutorial.html
  pytorch-docs:elastic-run                    https://docs.pytorch.org/docs/stable/elastic/run.html
  pytorch-docs:distributed                    https://docs.pytorch.org/docs/stable/distributed.html
  pytorch-docs:data                           https://docs.pytorch.org/docs/stable/data.html
  pytorch-docs:amp                            https://docs.pytorch.org/docs/stable/amp.html
  pytorch-docs:saving-loading-models          https://docs.pytorch.org/tutorials/beginner/saving_loading_models.html
  pytorch-docs:torch-compile-tutorial         https://docs.pytorch.org/tutorials/intermediate/torch_compile_tutorial.html
  pytorch-docs:torch-compiler-troubleshooting https://docs.pytorch.org/docs/stable/torch.compiler_troubleshooting.html
  pytorch-docs:torch-compiler-dynamic-shapes  https://docs.pytorch.org/docs/stable/torch.compiler_dynamic_shapes.html
  nvidia-docs:nccl-env                        https://docs.nvidia.com/deeplearning/nccl/user-guide/docs/env.html
  jax-docs:installation                       https://docs.jax.dev/en/latest/installation.html
  cupy-docs:install                           https://docs.cupy.dev/en/stable/install.html
  openmm-docs:running-sims                    https://docs.openmm.org/latest/userguide/application/02_running_sims.html
  gromacs-docs:mdrun-performance              https://manual.gromacs.org/current/user-guide/mdrun-performance.html
  rapids-docs:install                         https://docs.rapids.ai/install/
  nvidia-docs:container-toolkit-sample         https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/sample-workload.html
  nvidia-docs:docker-specialized               https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/docker-specialized.html
  colabfold-repo:README                        https://github.com/sokrypton/ColabFold
  alphafold3-docs:installation                 https://github.com/google-deepmind/alphafold3/blob/main/docs/installation.md
  hf-transformers:trainer                      https://huggingface.co/docs/transformers/en/trainer
  hf-transformers:bitsandbytes                 https://huggingface.co/docs/transformers/en/quantization/bitsandbytes
  hf-accelerate:launch                         https://huggingface.co/docs/accelerate/en/basic_tutorials/launch
  hf-accelerate:deepspeed                      https://huggingface.co/docs/accelerate/en/usage_guides/deepspeed
  deepspeed-docs:config-json                   https://www.deepspeed.ai/docs/config-json/
  llamafactory-repo:README                     https://github.com/hiyouga/LLaMA-Factory
  unsloth-repo:README                          https://github.com/unslothai/unsloth
  git-lfs:site                                 https://git-lfs.com/
  git-lfs:pull                                 https://github.com/git-lfs/git-lfs/blob/main/docs/man/git-lfs-pull.adoc
  aria2-docs:manual                            https://aria2.github.io/manual/en/html/aria2c.html
  wget-docs:manual                             https://www.gnu.org/software/wget/manual/wget.html
  curl-docs:manpage                            https://curl.se/docs/manpage.html
  rclone-docs:copy                             https://rclone.org/commands/rclone_copy/
  vscode-docs:remote-ssh                       https://code.visualstudio.com/docs/remote/ssh
  jupyter-server-docs:public-server            https://jupyter-server.readthedocs.io/en/latest/operators/public-server.html
  nvidia-docs:cuda-compatibility               https://docs.nvidia.com/deploy/cuda-compatibility/
  comfyui-docs:custom-nodes                    https://docs.comfy.org/installation/install_custom_node
  comfyui-manager-repo:README                  https://github.com/Comfy-Org/ComfyUI-Manager
  comfyui-docs:flux-krea                       https://docs.comfy.org/tutorials/flux/flux1-krea-dev
  comfyui-docs:flux-controlnet                 https://docs.comfy.org/tutorials/flux/flux-1-controlnet
  comfyui-docs:depth-controlnet                https://docs.comfy.org/tutorials/controlnet/depth-controlnet
  comfyui-ipadapter-plus:README                https://github.com/cubiq/ComfyUI_IPAdapter_plus
  comfyui-controlnet-aux:README                https://github.com/Fannovel16/comfyui_controlnet_aux
  x-flux-comfyui:README                        https://github.com/XLabs-AI/x-flux-comfyui
  nvidia-dcgm:exporter                         https://docs.nvidia.com/datacenter/dcgm/latest/gpu-telemetry/dcgm-exporter.html
  nvidia-dcgm:diagnostics                     https://docs.nvidia.com/datacenter/dcgm/latest/user-guide/dcgm-diagnostics.html
  nvidia-nsight:systems-user-guide             https://docs.nvidia.com/nsight-systems/UserGuide/index.html
  nvidia-nsight:compute-profiling              https://docs.nvidia.com/nsight-compute/ProfilingGuide/index.html
  docker-docs:engine-gpu                       https://docs.docker.com/engine/containers/gpu/
  docker-docs:compose-gpu                      https://docs.docker.com/compose/how-tos/gpu-support/
  kubernetes-docs:schedule-gpus                https://kubernetes.io/docs/tasks/manage-gpus/scheduling-gpus/
  nvidia-docs:gpu-operator                     https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/index.html
  nvidia-docs:gpu-sharing                      https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/gpu-sharing.html
  nvidia-docs:mig-user-guide                   https://docs.nvidia.com/datacenter/tesla/mig-user-guide/latest/index.html
  nvidia-docs:kubernetes-mig                   https://docs.nvidia.com/datacenter/cloud-native/kubernetes/latest/index.html
  nvidia-triton:overview                       https://docs.nvidia.com/deeplearning/triton-inference-server/user-guide/docs/index.html
  nvidia-triton:tensorrt-llm                   https://docs.nvidia.com/deeplearning/triton-inference-server/user-guide/docs/getting_started/trtllm_user_guide.html
  kserve-docs:intro                            https://kserve.github.io/website/docs/intro
  kserve-docs:multinode-vllm                   https://kserve.github.io/website/docs/model-serving/generative-inference/multi-node
  ray-docs:gpu-resources                       https://docs.ray.io/
  ray-docs:train-serve-resources               https://docs.ray.io/
  slurm-docs:gres                              https://slurm.schedmd.com/gres.html
  hf-trl:sft                                   https://huggingface.co/docs/trl/en/sft_trainer
  hf-trl:dpo                                   https://huggingface.co/docs/trl/en/dpo_trainer
  hf-datasets:stream                           https://huggingface.co/docs/datasets/en/stream
  hf-datasets:cache                            https://huggingface.co/docs/datasets/en/cache
  zarr-docs:index                              https://zarr.readthedocs.io/
  dask-docs:gpu                                https://docs.dask.org/en/stable/gpu.html
  rapids-docs:dask-cudf                        https://docs.rapids.ai/api/dask-cudf/stable/
  rapids-docs:cuml-dask                        https://docs.rapids.ai/api/cuml/stable/dask_multigpu_guide/
  tgi-repo:README                              https://github.com/huggingface/text-generation-inference
  ltx-video-repo:README                        https://github.com/Lightricks/LTX-Video
  comfyui-ltxvideo-repo:README                 https://github.com/Lightricks/ComfyUI-LTXVideo
  livetalking-repo:README                      https://github.com/lipku/LiveTalking
  infinitetalk-repo:README                     https://github.com/MeiGen-AI/InfiniteTalk
  multitalk-repo:README                        https://github.com/MeiGen-AI/MultiTalk
  so-vits-svc-repo:README                      https://github.com/svc-develop-team/so-vits-svc
  amphion-repo:README                          https://github.com/open-mmlab/Amphion
  dots-tts-repo:README                         https://github.com/rednote-hilab/dots.tts
  cosyvoice-repo:README                        https://github.com/FunAudioLLM/CosyVoice
  voxcpm-repo:README                           https://github.com/OpenBMB/VoxCPM
  seed-tts-eval-repo:README                    https://github.com/BytedanceSpeech/seed-tts-eval
  ai-toolkit-repo:README                       https://github.com/ostris/ai-toolkit
  qwen-image-repo:README                       https://github.com/QwenLM/Qwen-Image
  wan22-repo:README                            https://github.com/Wan-Video/Wan2.2
  comfyui-gguf-repo:README                     https://github.com/city96/ComfyUI-GGUF
  triposplat-repo:README                       https://github.com/VAST-AI-Research/TripoSplat
  gpt-sovits-repo:README                       https://github.com/RVC-Boss/GPT-SoVITS
  f5-tts-repo:README                           https://github.com/SWivid/F5-TTS
  sd-scripts-repo:README                       https://github.com/kohya-ss/sd-scripts
  kohya-ss-repo:README                         https://github.com/bmaltais/kohya_ss
  hf-diffusers:lora-training                   https://huggingface.co/docs/diffusers/training/lora
  hf-diffusers:dreambooth-training             https://huggingface.co/docs/diffusers/training/dreambooth
  faster-whisper-repo:README                   https://github.com/SYSTRAN/faster-whisper
  whisperx-repo:README                         https://github.com/m-bain/whisperX
  demucs-repo:README                           https://github.com/facebookresearch/demucs
  ffmpeg-docs:ffmpeg                           https://ffmpeg.org/ffmpeg.html
  ffmpeg-docs:filters                          https://ffmpeg.org/ffmpeg-filters.html
  nerfstudio-docs:installation                 https://docs.nerf.studio/quickstart/installation.html
  nerfstudio-docs:custom-data                  https://docs.nerf.studio/quickstart/custom_dataset.html
  nerfstudio-docs:splatfacto                   https://docs.nerf.studio/nerfology/methods/splat.html
  colmap-docs:cli                              https://colmap.github.io/cli.html
  colmap-docs:faq                              https://colmap.github.io/faq.html
  open3d-repo:README                           https://github.com/isl-org/Open3D
  pytorch3d-repo:README                        https://github.com/facebookresearch/pytorch3d
  pytorch3d-repo:INSTALL                       https://github.com/facebookresearch/pytorch3d/blob/main/INSTALL.md
  monai-docs:index                             https://monai.readthedocs.io/
  pyg-docs:install                             https://pytorch-geometric.readthedocs.io/en/2.7.0/install/installation.html
  dgl-repo:README                              https://github.com/dmlc/dgl
  physicsnemo-repo:README                      https://github.com/NVIDIA/physicsnemo
  nvitop-repo:README                           https://github.com/XuehaiPan/nvitop
  pytorch-docs:tensorboard                     https://docs.pytorch.org/docs/stable/tensorboard.html
  wandb-docs:quickstart                        https://docs.wandb.ai/models/quickstart
  wandb-docs:pytorch                           https://docs.wandb.ai/models/integrations/pytorch
  mlflow-docs:tracking                         https://mlflow.org/docs/latest/ml/tracking/
  mlflow-docs:pytorch                          https://mlflow.org/docs/latest/ml/deep-learning/pytorch/
  caddy-docs:reverse-proxy                     https://caddyserver.com/docs/quick-starts/reverse-proxy
  frp-repo:README                              https://github.com/fatedier/frp
  tailscale-docs:funnel                        https://tailscale.com/docs/features/tailscale-funnel
  cloudflare-docs:tunnel                       https://developers.cloudflare.com/tunnel/
  docker-docs:build-best-practices             https://docs.docker.com/build/building/best-practices/
  nvidia-docs:framework-containers             https://docs.nvidia.com/deeplearning/frameworks/user-guide/index.html
  uv-docs:features                             https://docs.astral.sh/uv/getting-started/features/
  micromamba-docs:install                      https://mamba.readthedocs.io/en/latest/installation/micromamba-installation.html
  hf-hub-docs:download                         https://huggingface.co/docs/huggingface_hub/package_reference/file_download
  a1111-repo:README                             https://github.com/AUTOMATIC1111/stable-diffusion-webui
  a1111-wiki:command-line-arguments             https://github.com/AUTOMATIC1111/stable-diffusion-webui/wiki/Command-Line-Arguments-and-Settings
  sd-webui-controlnet-repo:README               https://github.com/Mikubill/sd-webui-controlnet
  openwebui-docs:quick-start                    https://docs.openwebui.com/getting-started/quick-start/
  openwebui-repo:README                         https://github.com/open-webui/open-webui
  litellm-docs:getting-started                  https://docs.litellm.ai/docs/
  litellm-docs:openai-compatible                https://docs.litellm.ai/docs/providers/openai_compatible
  ollama-docs:cli                               https://docs.ollama.com/cli
  ollama-docs:context-length                    https://docs.ollama.com/context-length
  linux-man:ss                                  https://man7.org/linux/man-pages/man8/ss.8.html
  nvidia-docs:mps                               https://docs.nvidia.com/deploy/mps/index.html
  dvc-docs:start                                https://doc.dvc.org/start
  dvc-docs:remote-storage                       https://doc.dvc.org/user-guide/data-management/remote-storage
  s5cmd-repo:README                             https://github.com/peak/s5cmd
  minio-mc-repo:README                          https://github.com/minio/mc
  minio-docs:mc-mirror                          https://docs.min.io/aistor/reference/cli/mc-mirror/
  label-studio-repo:README                      https://github.com/HumanSignal/label-studio
  label-studio-docs:guide                       https://labelstud.io/guide/
  hf-accelerate:fsdp                            https://huggingface.co/docs/accelerate/en/usage_guides/fsdp
  hf-accelerate:model-size-estimator            https://huggingface.co/docs/accelerate/en/usage_guides/model_size_estimator
  hf-accelerate:big-model-inference             https://huggingface.co/docs/accelerate/en/concept_guides/big_model_inference
  hf-accelerate:megatron-lm                     https://huggingface.co/docs/accelerate/en/usage_guides/megatron_lm
  pytorch-docs:fsdp-advanced                    https://docs.pytorch.org/tutorials/intermediate/FSDP_advanced_tutorial.html
  pytorch-docs:activation-checkpointing          https://pytorch.org/docs/stable/checkpoint.html
  vllm-docs:features/lora                       https://docs.vllm.ai/en/stable/features/lora/
  vllm-docs:features/structured_outputs          https://docs.vllm.ai/en/stable/features/structured_outputs/
  vllm-docs:features/automatic_prefix_caching    https://docs.vllm.ai/en/stable/features/automatic_prefix_caching/
  vllm-docs:design/metrics                      https://docs.vllm.ai/en/stable/design/metrics/
  vllm-docs:observability-config                https://docs.vllm.ai/en/stable/api/vllm/config/observability/
  vllm-docs:usage/security                      https://docs.vllm.ai/en/stable/usage/security/
  litellm-docs:virtual-keys                     https://docs.litellm.ai/docs/proxy/virtual_keys
  litellm-docs:users-budgets                    https://docs.litellm.ai/docs/proxy/users
  nvidia-docs:nccl-troubleshooting              https://docs.nvidia.com/deeplearning/nccl/user-guide/docs/troubleshooting.html
  nvidia-nccl-tests-repo:README                 https://github.com/NVIDIA/nccl-tests
  kueue-docs:pytorchjob                         https://kueue.sigs.k8s.io/docs/tasks/run/kubeflow/pytorchjobs/
  kubeflow-docs:job-scheduling                  https://www.kubeflow.org/docs/components/trainer/legacy-v1/user-guides/job-scheduling/
  kubeflow-docs:trainer-overview                https://www.kubeflow.org/docs/components/trainer/overview/
  volcano-docs:kubeflow                         https://volcano.sh/docs/Ecosystem/KubeflowOnVolcano
  lm-eval-repo:README                           https://github.com/EleutherAI/lm-evaluation-harness
  lm-eval-repo:task-guide                       https://github.com/EleutherAI/lm-evaluation-harness/blob/main/docs/task_guide.md
  mlflow-docs:genai-eval                        https://mlflow.org/docs/latest/genai/eval-monitor/
  mlflow-docs:model-registry                    https://mlflow.org/docs/latest/ml/model-registry/
  ultralytics-docs:train                        https://docs.ultralytics.com/modes/train/
  ultralytics-docs:predict                      https://docs.ultralytics.com/modes/predict/
  sam2-repo:README                              https://github.com/facebookresearch/sam2
  lammps-docs:gpu-package                       https://docs.lammps.org/Speed_gpu.html
  pyscf-docs:gpu                                https://pyscf.org/user/gpu.html
  gpu4pyscf-repo:README                         https://github.com/pyscf/gpu4pyscf
  apptainer-docs:gpu                            https://apptainer.org/docs/user/main/gpu.html
  hf-docs:datasets-webdataset                   https://huggingface.co/docs/hub/en/datasets-webdataset
  webdataset-repo:README                        https://github.com/webdataset/webdataset
  onnxruntime-docs:quantization                 https://onnxruntime.ai/docs/performance/model-optimizations/quantization.html
  hf-optimum-onnx:gpu                           https://huggingface.co/docs/optimum-onnx/en/onnxruntime/usage_guides/gpu
  hf-transformers:quantization                  https://huggingface.co/docs/transformers/main/en/quantization
  llama-cpp-repo:README                         https://github.com/ggml-org/llama.cpp
"""
from __future__ import annotations

import json
from pathlib import Path

KB_VERSION = "kb.external.w0.2026-06-06"
VALID_FROM = "2026-06-06"


def chunk(
    *,
    chunk_id: str,
    product_area: str,
    source_type: str,
    source_origin: str,
    title: str,
    question_patterns: list[str],
    content: str,
    source_refs: list[str],
    confidence: str = "high",
) -> dict:
    return {
        "chunk_id": chunk_id,
        "kb_version": KB_VERSION,
        "source_type": source_type,
        "source_origin": source_origin,
        "product_area": product_area,
        "acl": "customer_safe",
        "title": title,
        "question_patterns": question_patterns,
        "content": content.strip(),
        "source_refs": source_refs,
        "asset_refs": [],
        "confidence": confidence,
        "valid_from": VALID_FROM,
        "evidence_kind": "knowledge",
        "surface_url": None,
        "retrieval_score_hint": None,
    }


# ============================== vLLM ==============================
VLLM_CHUNKS = [
    chunk(
        chunk_id="ext-vllm-serving-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="用 vLLM 启动 OpenAI 兼容的推理服务",
        question_patterns=[
            "vllm 怎么启动 api 服务",
            "如何用 vllm 部署一个 openai 兼容接口",
            "vllm serve 怎么用",
            "vllm 怎么对外提供 http 接口",
        ],
        content=(
            "适用场景:想把一个模型在实例里起成 HTTP 接口对外提供推理。vLLM 自带与 OpenAI API 兼容的"
            "推理服务器,起好后可直接用 OpenAI 的 SDK 或 curl 调用。\n\n"
            "启动(以 Qwen/Qwen2.5-1.5B-Instruct 为例):\n"
            "```\nvllm serve Qwen/Qwen2.5-1.5B-Instruct\n```\n"
            "默认监听 http://localhost:8000。常用参数:`--host` 指定监听地址,`--port` 指定端口,"
            "`--api-key` 开启后会校验请求里携带的 API Key。\n\n"
            "用 OpenAI Python 客户端调用(未开启 --api-key 时 api_key 可填 \"EMPTY\"):\n"
            "```\nfrom openai import OpenAI\n"
            "client = OpenAI(api_key=\"EMPTY\", base_url=\"http://localhost:8000/v1\")\n"
            "resp = client.chat.completions.create(\n"
            "    model=\"Qwen/Qwen2.5-1.5B-Instruct\",\n"
            "    messages=[{\"role\": \"user\", \"content\": \"你好\"}],\n"
            ")\n```\n"
            "也可以直接用 curl 请求 `/v1/completions` 或 `/v1/chat/completions`,请求体与 OpenAI API 一致。\n\n"
            "注意:`model` 要填实际加载的模型名;具体参数以 `vllm serve --help` 为准。"
        ),
        source_refs=["vllm-docs:getting_started/quickstart", "vllm-docs:cli/serve"],
    ),
    chunk(
        chunk_id="ext-vllm-offline-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="用 vLLM 做离线批量推理(不起服务)",
        question_patterns=[
            "vllm 怎么批量推理",
            "vllm 离线生成怎么写",
            "vllm LLM 类怎么用",
            "不起服务直接用 vllm 跑推理",
        ],
        content=(
            "适用场景:本地批量跑一批 prompt,不需要起 HTTP 服务,直接用 vLLM 的 `LLM` 类即可。\n\n"
            "```\nfrom vllm import LLM, SamplingParams\n\n"
            "prompts = [\n    \"Hello, my name is\",\n    \"The capital of France is\",\n]\n"
            "sampling_params = SamplingParams(temperature=0.8, top_p=0.95)\n\n"
            "llm = LLM(model=\"facebook/opt-125m\")\n"
            "outputs = llm.generate(prompts, sampling_params)\n\n"
            "for output in outputs:\n"
            "    print(output.prompt, output.outputs[0].text)\n```\n\n"
            "`model` 换成你要用的模型;`SamplingParams` 里可设置 temperature、top_p、max_tokens 等采样参数;"
            "`llm.generate` 一次性接收整批 prompt,vLLM 会自动做批处理。"
        ),
        source_refs=["vllm-docs:getting_started/quickstart"],
    ),
    chunk(
        chunk_id="ext-vllm-tensor-parallel-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="vLLM 多卡部署:张量并行 tensor_parallel_size",
        question_patterns=[
            "vllm 怎么用多张卡",
            "vllm 单卡放不下模型怎么办",
            "vllm tensor parallel 怎么设置",
            "vllm 多 gpu 部署",
        ],
        content=(
            "适用场景:单张卡放不下的模型,用张量并行(tensor parallelism)把模型切分到多张 GPU 上。\n\n"
            "Python(LLM 类),切到 2 张卡:\n"
            "```\nfrom vllm import LLM\n"
            "llm = LLM(model=\"ibm-granite/granite-3.1-8b-instruct\", tensor_parallel_size=2)\n```\n"
            "起服务时用对应的命令行参数:\n"
            "```\nvllm serve <model> --tensor-parallel-size 2\n```\n\n"
            "`tensor_parallel_size` 一般设为可用 GPU 的张数;它把单个模型拆到多卡协同推理,既能跑下更大的"
            "模型,也能分摊单卡显存压力。\n\n"
            "注意:张数通常需与模型结构(如注意力头数)整除匹配,具体约束以官方文档为准。"
        ),
        source_refs=["vllm-docs:configuration/conserving_memory"],
    ),
    chunk(
        chunk_id="ext-vllm-served-name-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="vLLM 自定义对外模型名 --served-model-name",
        question_patterns=[
            "vllm 模型名怎么改",
            "vllm 请求里 model 字段填什么",
            "vllm served model name",
            "vllm 对外用别名",
        ],
        content=(
            "适用场景:客户端里想用一个简短/固定的模型名,而不是完整的权重路径。\n\n"
            "启动时加 `--served-model-name <别名>`,之后请求体里的 `model` 字段用这个别名即可:\n"
            "```\nvllm serve <model-path> --served-model-name my-model\n```\n\n"
            "注意:不指定时默认用 `model` 路径作为对外名字;具体以 `vllm serve --help` 为准。"
        ),
        source_refs=["vllm-docs:cli/serve"],
    ),
    chunk(
        chunk_id="ext-gpu-oom-vllm-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="vLLM 报显存不足 / CUDA out of memory 怎么降显存",
        question_patterns=[
            "vllm 显存不足怎么办",
            "cuda out of memory vllm",
            "vllm 加载模型 oom",
            "vllm 显存爆了怎么降",
        ],
        content=(
            "适用场景:模型放不进 GPU 显存时会报 out-of-memory(OOM),多在加载模型或高并发推理时出现。"
            "可按下面几种方式降低显存占用,通常组合使用:\n\n"
            "1. 缩短上下文长度:`--max-model-len`(Python `max_model_len`)设为略大于 实际最大输入+输出 长度,"
            "过长会占用更多 KV cache。\n"
            "2. 降低并发批量:减小 `--max-num-seqs`(同时处理的序列数)或 `--max-num-batched-tokens`,"
            "并发越低占用 KV cache 越少。\n"
            "3. 多卡张量并行:`--tensor-parallel-size`(Python `tensor_parallel_size`)把模型切到多张卡。\n"
            "4. 使用量化模型:加载已量化的权重或开启 `quantization`,以较低精度换更小的显存占用。\n"
            "5. `--enforce-eager`(或 `enforce_eager=True`)关闭 CUDA Graph:主要用于排查 CUDA Graph 相关的"
            "显存/启动异常,可能影响推理性能,不要当成稳定的常规省显存手段。\n"
            "6. `--gpu-memory-utilization`(0~1)控制 vLLM 预分配给 KV cache 的显存比例;调高给 KV cache 更多"
            "空间,卡上还有其他进程占显存时需调低。默认值随 vLLM 版本和入口可能不同,实际以 "
            "`vllm serve --help` 或当前官方文档为准。\n\n"
            "最小示例(Python):\n"
            "```\nfrom vllm import LLM\n"
            "llm = LLM(model=\"adept/fuyu-8b\", max_model_len=2048, max_num_seqs=2)\n```\n"
            "注意:具体取值要结合实例的显卡显存和模型大小,先小后大逐步调。"
        ),
        source_refs=[
            "vllm-docs:configuration/conserving_memory",
            "vllm-docs:usage/troubleshooting",
            "vllm-docs:cli/serve",
        ],
    ),
    chunk(
        chunk_id="ext-vllm-quantization-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="vLLM 用量化模型降低显存",
        question_patterns=[
            "vllm 怎么用量化模型",
            "vllm 量化降显存",
            "vllm awq gptq 怎么加载",
            "vllm 显存不够能不能量化",
        ],
        content=(
            "适用场景:显存吃紧,想用量化版模型降低权重显存占用。两种常见方式:\n\n"
            "1. 直接加载社区已量化好的权重(如 AWQ、GPTQ 等格式的模型)。\n"
            "2. 加载时指定 `quantization`(CLI `--quantization`)。\n\n"
            "量化以较低数值精度换更小显存(通常也更快),代价是可能有一定精度损失。\n\n"
            "注意:不同量化格式对硬件/模型的支持范围不同,选用前以官方文档与该模型卡的说明为准。"
        ),
        source_refs=["vllm-docs:configuration/conserving_memory", "vllm-docs:cli/serve"],
    ),
    chunk(
        chunk_id="ext-vllm-startup-hang-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="vLLM 启动卡住 / 模型加载很慢怎么排查",
        question_patterns=[
            "vllm 启动卡住不动",
            "vllm 加载模型很慢",
            "vllm 起不来怎么排查",
            "vllm 一直卡在加载",
        ],
        content=(
            "适用场景:vLLM 启动慢或长时间卡住。根因可能在下载/磁盘、CPU 内存、I/O 等多个环节,建议按顺序排查:\n\n"
            "1. 模型来源与磁盘:权重放在网络/共享文件系统会很慢,尽量放到本地磁盘再加载;确认磁盘空间足够、"
            "下载是否完成。\n"
            "2. CPU 内存:内存吃满会触发操作系统换页(swap)拖慢加载,观察内存占用是否接近上限。\n"
            "3. 定位是否卡在权重 I/O:用 `--load-format dummy` 跳过真正的权重加载,如果加上之后很快起来,"
            "说明瓶颈在模型 I/O。\n"
            "4. 看卡在哪一步:打开调试日志 `export VLLM_LOGGING_LEVEL=DEBUG`。\n\n"
            "注意:`--load-format dummy` 不会加载真实权重(输出无意义,仅用于定位);调试环境变量排查完记得移除,"
            "避免影响性能。"
        ),
        source_refs=["vllm-docs:usage/troubleshooting"],
    ),
    chunk(
        chunk_id="ext-vllm-cuda-error-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="vLLM 运行中报 CUDA error 怎么定位",
        question_patterns=[
            "vllm cuda error 怎么办",
            "vllm 跑着跑着崩了 cuda",
            "怎么定位是哪个 cuda 操作出错",
            "vllm graph replay 崩溃",
        ],
        content=(
            "适用场景:运行中报 CUDA error。根因可能是 CUDA Graph、某个具体 kernel、或多卡通信等,"
            "建议按顺序缩小范围:\n\n"
            "1. 若崩溃发生在 CUDA Graph 回放(报错栈出现 `self.graph.replay()` 附近),加 `--enforce-eager`"
            "(或 `enforce_eager=True`)关闭 CUDA Graph,把出错的 CUDA 操作暴露出来。\n"
            "2. 让报错定位到真正出错的 kernel:`export CUDA_LAUNCH_BLOCKING=1` 让 CUDA 同步执行,报错位置更准确"
            "(会变慢,仅排查用)。\n"
            "3. 多卡通信(NCCL)相关的卡死/报错:`export NCCL_DEBUG=TRACE` 打开详细日志;复杂网络下 vLLM 选错"
            "网卡时,可用 `export VLLM_HOST_IP=<本机IP>` 指定。\n\n"
            "注意:这些都是排查用的开关,定位完成后应移除,以免拖慢正常推理。报错要结合显存是否不足一起看——"
            "OOM 也常以 CUDA error 形式出现。"
        ),
        source_refs=["vllm-docs:usage/troubleshooting"],
    ),
]

# ============================== SGLang ==============================
SGLANG_CHUNKS = [
    chunk(
        chunk_id="ext-sglang-serving-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="用 SGLang 启动 OpenAI 兼容的推理服务",
        question_patterns=[
            "sglang 怎么启动服务",
            "sglang launch_server 怎么用",
            "sglang openai 接口怎么调",
            "sglang 怎么部署模型",
        ],
        content=(
            "适用场景:用 SGLang 起一个 OpenAI 兼容的推理服务。\n\n"
            "启动:\n"
            "```\npython -m sglang.launch_server --model-path meta-llama/Meta-Llama-3-8B-Instruct\n```\n"
            "默认监听端口 30000,可用 `--host` / `--port` 调整。\n\n"
            "用 OpenAI Python 客户端调用,base_url 指向服务的 /v1:\n"
            "```\nfrom openai import OpenAI\n"
            "client = OpenAI(base_url=\"http://127.0.0.1:30000/v1\", api_key=\"EMPTY\")\n"
            "resp = client.chat.completions.create(model=\"default\", messages=[{\"role\":\"user\",\"content\":\"你好\"}])\n```\n\n"
            "注意:`--model-path` 填实际模型;全部参数以 `python3 -m sglang.launch_server --help` 为准。"
        ),
        source_refs=[
            "sglang-docs:basic_usage/openai_api",
            "sglang-docs:advanced_features/server_arguments",
        ],
    ),
    chunk(
        chunk_id="ext-sglang-oom-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="SGLang 报显存不足 / CUDA OOM 怎么降",
        question_patterns=[
            "sglang 显存不足怎么办",
            "sglang out of memory",
            "sglang mem-fraction-static 怎么调",
            "sglang oom 降显存",
        ],
        content=(
            "适用场景:SGLang serving 时报 OOM / CUDA out of memory。按下面处理:\n\n"
            "1. 降低 `--mem-fraction-static`:它是 KV cache 显存池占总显存的比例,默认约 0.9;调到 0.8 或 0.7 "
            "可减少 KV cache 显存,缓解 prefill 和 decode 阶段的 OOM,代价是最大并发与峰值吞吐下降。\n"
            "2. 多卡张量并行:`--tp <N>` 把模型切到多张卡,分摊单卡显存。\n"
            "3. 适当限制并发与上下文长度(相关参数见 `--help`)。\n\n"
            "示例:\n"
            "```\npython -m sglang.launch_server --model-path <model> --mem-fraction-static 0.7\n```\n"
            "注意:参数默认值/名称以 `python3 -m sglang.launch_server --help` 与当前官方文档为准。"
        ),
        source_refs=[
            "sglang-docs:backend/hyperparameter_tuning",
            "sglang-docs:advanced_features/server_arguments",
        ],
    ),
    chunk(
        chunk_id="ext-sglang-tp-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="SGLang 多卡张量并行 --tp",
        question_patterns=[
            "sglang 怎么用多张卡",
            "sglang tensor parallel",
            "sglang --tp 怎么设",
            "sglang 单卡放不下",
        ],
        content=(
            "适用场景:单卡放不下模型,用张量并行切到多卡。\n\n"
            "用 `--tp <N>` 指定张量并行的 GPU 张数,把模型切到多卡协同推理:\n"
            "```\npython -m sglang.launch_server --model-path <model> --tp 2\n```\n\n"
            "注意:张数需与模型结构匹配,具体约束以官方文档为准。"
        ),
        source_refs=["sglang-docs:advanced_features/server_arguments"],
    ),
]

# ============================== Ollama ==============================
OLLAMA_CHUNKS = [
    chunk(
        chunk_id="ext-ollama-run-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="Ollama 拉取并运行模型",
        question_patterns=[
            "ollama 怎么运行模型",
            "ollama pull run 怎么用",
            "ollama 怎么下载模型",
            "ollama 看有哪些模型",
        ],
        content=(
            "适用场景:本地快速跑一个模型。\n\n"
            "常用命令:`ollama pull <model>` 下载模型;`ollama run <model>` 运行并进入交互对话;"
            "`ollama list` 查看已下载的模型;`ollama ps` 查看正在运行的模型(含是否在 GPU 上)。\n\n"
            "注意:使用某个模型前要先 `ollama pull` 到本地;模型名以 Ollama 模型库为准。"
        ),
        source_refs=["ollama-docs:api/openai-compatibility", "ollama-docs:faq"],
    ),
    chunk(
        chunk_id="ext-ollama-openai-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="用 OpenAI 接口调用 Ollama",
        question_patterns=[
            "ollama openai 接口怎么用",
            "ollama 怎么用 openai sdk 调",
            "ollama 的 api 地址是什么",
            "ollama 兼容 openai 吗",
        ],
        content=(
            "适用场景:用 OpenAI SDK 或工具直接对接本地 Ollama。Ollama 在 `http://localhost:11434/v1/` "
            "提供 OpenAI 兼容接口。\n\n"
            "curl:\n"
            "```\ncurl http://localhost:11434/v1/chat/completions -H \"Content-Type: application/json\" \\\n"
            "  -d '{\"model\":\"<model>\",\"messages\":[{\"role\":\"user\",\"content\":\"你好\"}]}'\n```\n"
            "Python:\n"
            "```\nfrom openai import OpenAI\n"
            "client = OpenAI(base_url=\"http://localhost:11434/v1/\", api_key=\"ollama\")  # api_key 必填但被忽略\n```\n\n"
            "支持 `/v1/chat/completions`、`/v1/completions`、`/v1/models`、`/v1/embeddings` 等端点。\n\n"
            "注意:用之前先 `ollama pull <model>`;`model` 填本地已有的模型名。"
        ),
        source_refs=["ollama-docs:api/openai-compatibility"],
    ),
    chunk(
        chunk_id="ext-ollama-host-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="让 Ollama 监听对外地址(OLLAMA_HOST)",
        question_patterns=[
            "ollama 怎么从外部访问",
            "ollama 只能本机访问怎么办",
            "ollama_host 怎么设",
            "ollama 改监听地址",
        ],
        content=(
            "适用场景:Ollama 默认只监听本机 127.0.0.1:11434,想从其他机器访问。\n\n"
            "把环境变量 `OLLAMA_HOST` 设为对外地址(如 `OLLAMA_HOST=0.0.0.0:11434`)后重启 Ollama 服务。\n\n"
            "注意:对外暴露要做好访问控制(限制来源 IP / 前面加网关鉴权),避免接口被公网随意调用;"
            "具体配置以官方文档为准。"
        ),
        source_refs=["ollama-docs:faq"],
    ),
    chunk(
        chunk_id="ext-ollama-gpu-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="确认 Ollama 是否在用 GPU",
        question_patterns=[
            "ollama 有没有用 gpu",
            "ollama 跑在 cpu 上怎么办",
            "ollama 怎么看是不是 gpu 推理",
            "ollama 很慢是不是没用显卡",
        ],
        content=(
            "适用场景:想确认 Ollama 跑模型用的是 GPU 还是 CPU。\n\n"
            "`ollama ps` 会列出正在运行的模型及其占用,包含是否在 GPU 上。如果发现跑在 CPU 上,常见原因:"
            "显存不足放不下模型、或容器/驱动没有正确把 GPU 暴露给 Ollama。\n\n"
            "注意:先用 `nvidia-smi` 确认 GPU 与驱动本身正常,再排查 Ollama 侧。"
        ),
        source_refs=["ollama-docs:faq"],
    ),
]

# ============================== ComfyUI ==============================
# Scope is deliberately CLI / filesystem / troubleshooting only — launch flags,
# model directories, custom-node install, connectivity. The node-graph visual UI
# (menu paths / field names / per-node behavior) is intentionally NOT described:
# it is version- and custom-node-dependent, and the third_party_tool_addendum
# already forbids asserting a tool's UI steps/field names. Stay on documented,
# stable command-line + directory facts.
COMFYUI_CHUNKS = [
    chunk(
        chunk_id="ext-comfyui-start-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="在实例里启动 ComfyUI 并从外部访问",
        question_patterns=[
            "comfyui 怎么启动",
            "comfyui 怎么从外部访问",
            "comfyui 默认只能本机打开怎么办",
            "comfyui --listen 怎么用",
        ],
        content=(
            "适用场景:在 GPU 实例里把 ComfyUI 跑起来,并希望从本机以外打开它的网页。\n\n"
            "在 ComfyUI 目录里启动:\n"
            "```\npython main.py\n```\n"
            "默认监听 `127.0.0.1:8188`,只能本机访问。要让其它机器能访问,用 `--listen` 监听所有网卡、"
            "用 `--port` 指定端口:\n"
            "```\npython main.py --listen 0.0.0.0 --port 8188\n```\n"
            "启动后控制台会打印实际监听的地址和端口,以日志为准。\n\n"
            "注意:从公网/外部访问还要确认实例的安全组 / 端口映射放行了该端口(这属于平台侧配置,"
            "以平台文档为准);完整参数以 `python main.py --help` 为准。"
        ),
        source_refs=["comfyui-repo:README", "comfyui-repo:comfy/cli_args.py"],
    ),
    chunk(
        chunk_id="ext-comfyui-oom-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="ComfyUI 显存不足 / 爆显存怎么降",
        question_patterns=[
            "comfyui 显存不足怎么办",
            "comfyui 出图爆显存",
            "comfyui lowvram 怎么用",
            "comfyui 显存不够报错",
        ],
        content=(
            "适用场景:ComfyUI 生成时报显存不足 / 爆显存,常见于大图、视频或较大的模型。可按下面处理,"
            "通常组合使用:\n\n"
            "1. 启动时加省显存开关:`--lowvram` 把模型尽量分块、少占显存;显存特别紧张可用 `--novram`;"
            "完全没有可用显存时用 `--cpu` 退回 CPU(很慢,仅兜底)。\n"
            "2. 降低单次生成负载:减小出图分辨率、batch 数量、视频帧数——分辨率和帧数对显存影响最大。\n"
            "3. 释放/腾出显存:用 `nvidia-smi` 看是否有其它进程占着显存并酌情停掉;换更小或量化过的模型。\n\n"
            "注意:`--lowvram` / `--novram` 是以速度换显存,不是凭空增加显存;具体开关以 "
            "`python main.py --help` 与当前版本为准。"
        ),
        source_refs=["comfyui-repo:README", "comfyui-repo:comfy/cli_args.py"],
    ),
    chunk(
        chunk_id="ext-comfyui-models-dir-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="ComfyUI 模型放在哪 / 列表里选不到模型",
        question_patterns=[
            "comfyui 模型放哪个目录",
            "comfyui 选不到 checkpoint",
            "comfyui 加载不到模型",
            "comfyui 怎么用已有的模型目录",
        ],
        content=(
            "适用场景:ComfyUI 列表里看不到刚下载的模型,或不确定权重该放哪。\n\n"
            "ComfyUI 在自己的 `models/` 子目录里按类型找模型:大模型(checkpoint)放 `models/checkpoints`,"
            "LoRA 放 `models/loras`,VAE 放 `models/vae`,ControlNet 放 `models/controlnet`,其余类型类推。"
            "放好后刷新模型列表或重启 ComfyUI 即可看到。\n\n"
            "已有模型不想再复制一份:把 `extra_model_paths.yaml.example` 复制成 `extra_model_paths.yaml`,"
            "在里面填已有模型所在的目录,让 ComfyUI 直接读取。\n\n"
            "注意:目录要对应模型类型放对;改了 `extra_model_paths.yaml` 后需重启 ComfyUI 生效。"
        ),
        source_refs=["comfyui-repo:README", "comfyui-repo:extra_model_paths.yaml.example"],
    ),
    chunk(
        chunk_id="ext-comfyui-custom-nodes-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="安装 ComfyUI 自定义节点(custom nodes)",
        question_patterns=[
            "comfyui 怎么装自定义节点",
            "comfyui 缺少某个节点怎么办",
            "comfyui custom_nodes 怎么用",
            "comfyui 别人的工作流提示缺节点",
        ],
        content=(
            "适用场景:别人的工作流用到你没有的节点,需要安装自定义节点(custom node)。\n\n"
            "手动安装:把该节点的仓库 `git clone` 到 ComfyUI 的 `custom_nodes/` 目录下;如果它带有 "
            "`requirements.txt`,在该目录里 `pip install -r requirements.txt` 装好依赖;然后重启 ComfyUI。\n\n"
            "社区也有 ComfyUI-Manager 这类扩展可以帮忙安装/更新节点(它本身也是装在 `custom_nodes/` 下的一个节点)。\n\n"
            "注意:自定义节点来自第三方,安装前留意来源是否可信、依赖是否冲突;装完必须重启 ComfyUI 才会加载;"
            "加载报错多半是缺依赖或版本不匹配,按控制台日志补装即可。"
        ),
        source_refs=["comfyui-repo:README"],
    ),
    chunk(
        chunk_id="ext-comfyui-install-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="在实例里安装 / 更新 ComfyUI",
        question_patterns=[
            "comfyui 怎么安装",
            "怎么在实例上装 comfyui",
            "comfyui 怎么更新到新版本",
            "comfyui 装好怎么启动",
        ],
        content=(
            "适用场景:在 Linux GPU 实例里从头安装 ComfyUI 或更新到新版本。\n\n"
            "安装:\n"
            "```\ngit clone https://github.com/comfyanonymous/ComfyUI\ncd ComfyUI\n"
            "# 先按实例的 CUDA 版本装好对应的 PyTorch,再装其余依赖\npip install -r requirements.txt\n"
            "python main.py\n```\n"
            "更新:在 ComfyUI 目录里 `git pull` 拉取最新代码,必要时再次 `pip install -r requirements.txt` "
            "更新依赖,然后重启。\n\n"
            "注意:PyTorch 要装与实例 CUDA 匹配的版本(见相关条目);依赖较多,建议在独立的 Python 环境"
            "(venv / conda)里安装,避免污染系统环境。"
        ),
        source_refs=["comfyui-repo:README", "comfyui-docs:getting_started"],
    ),
    chunk(
        chunk_id="ext-comfyui-cant-connect-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="ComfyUI 起来了但浏览器打不开 / 连不上怎么排查",
        question_patterns=[
            "comfyui 打不开网页",
            "comfyui 启动了连不上",
            "comfyui 浏览器访问不了",
            "comfyui 页面白屏打不开",
        ],
        content=(
            "适用场景:ComfyUI 进程在跑,但从浏览器访问不到页面。按顺序排查:\n\n"
            "1. 是不是只监听了本机:默认 `127.0.0.1:8188` 只能本机访问,要从外部访问需用 `--listen 0.0.0.0` 启动;"
            "看启动日志里打印的实际监听地址。\n"
            "2. 端口有没有放行:确认访问的端口(默认 8188)在实例的安全组 / 端口映射里放行了;需要换端口用 `--port`。\n"
            "3. 进程是否真的起好:看 ComfyUI 控制台日志有没有报错、有没有打印出监听地址(类似 "
            "“Starting server” / “To see the GUI go to …”)。\n\n"
            "注意:平台侧的端口放行属于平台配置(以平台文档 / 控制台为准),这里只判断 ComfyUI 自身的监听设置;"
            "本机能开、外部打不开,基本就是 `--listen` 或端口放行的问题。"
        ),
        source_refs=["comfyui-repo:README"],
    ),
    chunk(
        chunk_id="ext-comfyui-api-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="用 API / 脚本方式触发 ComfyUI 工作流(不手点界面)",
        question_patterns=[
            "comfyui 怎么用 api 调",
            "comfyui 能不能脚本自动跑",
            "comfyui 后台批量生成",
            "comfyui /prompt 接口怎么用",
        ],
        content=(
            "适用场景:想用脚本/服务自动触发 ComfyUI 生成,而不是每次手动在界面里点。\n\n"
            "ComfyUI 服务端本身提供 HTTP 接口:先在界面里开启开发者选项,把要跑的工作流导出成 “API 格式” 的 JSON;"
            "再把这段 JSON 作为请求体 POST 到服务端的 `/prompt` 接口排队执行,产出可通过历史记录或 WebSocket 获取。\n\n"
            "注意:工作流 JSON 的具体结构取决于你自己搭的节点图,并会随版本/节点变化——请以你当前 ComfyUI 版本"
            "实际导出的 API JSON 和官方文档为准,不要照搬别处的字段;这里不预设具体字段名。"
        ),
        source_refs=["comfyui-docs:getting_started", "comfyui-repo:README"],
    ),
]

# ============================== General GPU ops ==============================
GPU_OPS_CHUNKS = [
    chunk(
        chunk_id="ext-gpu-nvidia-smi-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="用 nvidia-smi 查看显存、利用率和占用进程",
        question_patterns=[
            "怎么看显存占用",
            "nvidia-smi 怎么用",
            "gpu 利用率怎么查",
            "看哪个进程占显存",
        ],
        content=(
            "适用场景:想知道 GPU 显存用了多少、利用率多高、被哪个进程占着。\n\n"
            "`nvidia-smi` 一次性显示每张卡的显存使用/总量与 GPU 利用率,底部进程表列出各进程的 PID 和显存占用。"
            "持续观察用 `nvidia-smi -l 1`(每秒刷新)或 `watch -n 1 nvidia-smi`。\n\n"
            "注意:显存一直被占着不释放,多半是对应进程还在运行或没有正常退出。"
        ),
        source_refs=["nvidia-docs:nvidia-smi"],
    ),
    chunk(
        chunk_id="ext-gpu-driver-cuda-version-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="查看 GPU 驱动和 CUDA 版本",
        question_patterns=[
            "怎么看驱动版本",
            "怎么看 cuda 版本",
            "nvidia-smi 和 nvcc 版本不一样",
            "驱动和 cuda 不匹配怎么办",
        ],
        content=(
            "适用场景:排查“驱动/CUDA 版本不匹配”类问题。\n\n"
            "`nvidia-smi` 右上角显示驱动版本和该驱动支持的最高 CUDA 版本;`nvcc --version` 显示本地 CUDA "
            "编译器(toolkit)版本。框架(如 PyTorch)自带的 CUDA runtime 版本不必和系统 CUDA toolkit 完全一致,"
            "关键是驱动版本要足够新以支持框架所需的 CUDA。\n\n"
            "注意:报版本不匹配时,优先确认驱动是否过旧。"
        ),
        source_refs=["nvidia-docs:nvidia-smi"],
    ),
    chunk(
        chunk_id="ext-gpu-not-detected-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="程序检测不到 GPU 怎么排查",
        question_patterns=[
            "torch 检测不到 gpu",
            "cuda is_available 是 false",
            "程序用不上显卡",
            "gpu 不可用怎么办",
        ],
        content=(
            "适用场景:代码里用不上 GPU / `torch.cuda` 不可用。按顺序排查:\n\n"
            "1. `nvidia-smi` 能否正常列出 GPU——不行说明是驱动或硬件层面的问题,先解决这一层。\n"
            "2. 框架层面:`torch.cuda.is_available()` 是否为 True,`torch.cuda.device_count()` 数量对不对。\n"
            "3. 是否装成了 CPU 版框架(如只装了 CPU 版 PyTorch)——重装对应 CUDA 版本。\n"
            "4. `CUDA_VISIBLE_DEVICES` 是否被设成空或错误值,把可见 GPU 屏蔽了。\n\n"
            "注意:先确认 nvidia-smi 正常,再查框架层,不要一上来就重装框架。"
        ),
        source_refs=["pytorch-docs:notes/cuda", "nvidia-docs:nvidia-smi"],
    ),
    chunk(
        chunk_id="ext-gpu-pytorch-oom-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="PyTorch 训练/推理 CUDA out of memory 通用降显存",
        question_patterns=[
            "pytorch cuda out of memory",
            "torch 显存不足怎么办",
            "训练爆显存怎么降",
            "gpu oom 通用处理",
        ],
        content=(
            "适用场景:PyTorch 报 CUDA out of memory。组合处理:\n\n"
            "1. 减小 batch size——最直接有效。\n"
            "2. 推理/评估阶段用 `torch.no_grad()` 或 `torch.inference_mode()`,不保存梯度省显存。\n"
            "3. 训练用混合精度(AMP)与梯度累积(小 batch 多步累积,等效大 batch)。\n"
            "4. 大模型用梯度检查点(gradient checkpointing),用计算时间换显存。\n"
            "5. 及时删除不再用的中间张量。\n\n"
            "注意:`torch.cuda.empty_cache()` 只是把缓存的空闲显存还给驱动,并不能真正解决 OOM(见相关条目)。"
        ),
        source_refs=["pytorch-docs:notes/cuda"],
    ),
    chunk(
        chunk_id="ext-gpu-empty-cache-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="torch.cuda.empty_cache() 到底有没有用",
        question_patterns=[
            "empty_cache 有用吗",
            "torch 怎么清显存",
            "nvidia-smi 显存比实际大",
            "释放了张量显存没降",
        ],
        content=(
            "适用场景:想搞清 empty_cache 能不能“清显存”。\n\n"
            "PyTorch 用缓存分配器,释放掉的张量显存会被缓存复用、不立刻还给驱动,所以 `nvidia-smi` 显示的占用"
            "可能比当前张量实际占用大。`torch.cuda.empty_cache()` 把这部分缓存的空闲显存还给驱动"
            "(让 nvidia-smi 数字下降),但不会释放仍被张量引用的显存,也不能修复真正的 OOM。\n\n"
            "看占用:`torch.cuda.memory_allocated()`(张量实际占用)、`torch.cuda.memory_reserved()`"
            "(分配器保留总量)。"
        ),
        source_refs=["pytorch-docs:notes/cuda"],
    ),
    chunk(
        chunk_id="ext-gpu-fragmentation-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="显存总量够却报 OOM(碎片化)怎么办",
        question_patterns=[
            "显存够却 oom",
            "显存碎片化",
            "pytorch_cuda_alloc_conf 怎么设",
            "expandable_segments 是什么",
        ],
        content=(
            "适用场景:明明总显存还有富余、却报 CUDA out of memory,常因显存碎片化。\n\n"
            "可尝试设置环境变量 `PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True` 缓解碎片;另一个选项是 "
            "`max_split_size_mb:<N>`。\n\n"
            "注意:这是缓解碎片、提高现有显存利用率,不是凭空增加显存;若是根本不够,仍要降 batch / 换更小或"
            "量化的模型。"
        ),
        source_refs=["pytorch-docs:notes/cuda"],
    ),
    chunk(
        chunk_id="ext-gpu-kill-process-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="显存被占用 / 释放不掉怎么处理",
        question_patterns=[
            "显存被占着不释放",
            "怎么杀掉占显存的进程",
            "新任务起不来显存满了",
            "进程结束显存没释放",
        ],
        content=(
            "适用场景:GPU 显存被占着,新任务起不来。\n\n"
            "用 `nvidia-smi` 底部进程表找到占显存的进程 PID,确认是自己的、确实无用的进程后,用 `kill <PID>`"
            "(或正常停掉对应训练/服务)释放。\n\n"
            "注意:不要误杀他人或系统进程;正常情况下进程结束显存会自动释放,若结束后仍不释放,可能是残留/僵尸"
            "进程,需进一步排查。"
        ),
        source_refs=["nvidia-docs:nvidia-smi"],
    ),
    chunk(
        chunk_id="ext-gpu-oom-vs-ram-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="区分“显存不足”和“内存不足”",
        question_patterns=[
            "显存不足还是内存不足",
            "进程被 killed 是什么原因",
            "oomkilled 和 cuda oom 区别",
            "程序莫名被杀",
        ],
        content=(
            "适用场景:程序崩了但分不清是显存还是内存的问题。\n\n"
            "判断:报 `CUDA out of memory` 是 GPU 显存(VRAM)不足,用降 batch、量化、多卡等手段(见相关条目);"
            "而进程被系统直接 `Killed` / 容器 `OOMKilled`、`dmesg` 里出现 oom-killer,通常是系统内存(RAM)不足,"
            "要降低数据加载/预处理的内存占用或增加内存。\n\n"
            "注意:两者处理方向不同,先看报错措辞和系统日志再动手。"
        ),
        source_refs=["pytorch-docs:notes/cuda"],
    ),
    chunk(
        chunk_id="ext-gpu-visible-devices-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="用 CUDA_VISIBLE_DEVICES 指定使用哪几张卡",
        question_patterns=[
            "怎么指定用哪张 gpu",
            "cuda_visible_devices 怎么用",
            "只用某几张卡跑",
            "多卡怎么分给不同任务",
        ],
        content=(
            "适用场景:多卡机器上想让程序只用指定的 GPU,或把不同任务分到不同卡。\n\n"
            "设置环境变量 `CUDA_VISIBLE_DEVICES`,值是 GPU 编号(从 0 起,与 nvidia-smi 的顺序一致),如 "
            "`CUDA_VISIBLE_DEVICES=0,1` 只用前两张;设为空字符串则不使用 GPU。\n\n"
            "注意:这里写的是物理卡序;程序内部看到的设备会被重新编号为从 0 开始。"
        ),
        source_refs=["pytorch-docs:notes/cuda"],
    ),
    chunk(
        chunk_id="ext-gpu-hf-download-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="HuggingFace 模型下载慢或失败怎么办",
        question_patterns=[
            "huggingface 模型下载很慢",
            "hf 模型拉不下来",
            "模型下载中断怎么办",
            "hf 缓存目录怎么改",
        ],
        content=(
            "适用场景:拉取 HuggingFace 模型很慢或中断。按顺序处理:\n\n"
            "1. 先确认磁盘空间足够(`df -h`);缓存默认在 `~/.cache/huggingface`,可用环境变量 `HF_HOME` 改到"
            "更大的盘。\n"
            "2. 网络访问不畅时可改用镜像端点,设置环境变量 `HF_ENDPOINT` 指向可用镜像。\n"
            "3. 大模型用 `huggingface-cli download` 等带断点续传的方式下载更稳。\n\n"
            "注意:镜像端点/可用地址以你所用工具与当前实际可用的镜像为准,不要写死。"
        ),
        source_refs=["hf-docs:installation"],
    ),
]

# ============================== Extras ==============================
EXTRA_CHUNKS = [
    chunk(
        chunk_id="ext-vllm-port-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="vLLM 端口被占用 / 改监听端口",
        question_patterns=[
            "vllm 端口被占用",
            "address already in use vllm",
            "vllm 怎么换端口",
            "vllm 8000 端口冲突",
        ],
        content=(
            "适用场景:启动 vLLM 报端口已被占用(address already in use),或想换个端口。\n\n"
            "1. 换端口:`vllm serve <model> --port 8001`。\n"
            "2. 查谁占了端口:`ss -ltnp | grep 8000`(或 `lsof -i:8000`),必要时停掉占用进程。\n\n"
            "注意:默认端口 8000;若要从外部访问,还要确认实例的安全组/防火墙放行了该端口。"
        ),
        source_refs=["vllm-docs:cli/serve"],
    ),
    chunk(
        chunk_id="ext-sglang-context-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="SGLang 限制上下文长度 --context-length",
        question_patterns=[
            "sglang 上下文长度怎么设",
            "sglang context-length",
            "sglang 限制输入长度省显存",
            "sglang 上下文太长 oom",
        ],
        content=(
            "适用场景:想限制最大上下文长度,以节省显存或匹配实际需求。\n\n"
            "用 `--context-length <N>` 设置最大上下文长度;设小一些可减少 KV cache 显存占用。\n\n"
            "注意:不能超过模型本身支持的最大长度;参数以 `python3 -m sglang.launch_server --help` 与官方文档为准。"
        ),
        source_refs=[
            "sglang-docs:backend/hyperparameter_tuning",
            "sglang-docs:advanced_features/server_arguments",
        ],
    ),
    chunk(
        chunk_id="ext-ollama-models-dir-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="改 Ollama 模型存储目录(OLLAMA_MODELS)",
        question_patterns=[
            "ollama 模型存哪里",
            "ollama 模型目录怎么改",
            "ollama 占满系统盘",
            "ollama_models 怎么设",
        ],
        content=(
            "适用场景:模型默认存在系统盘/用户目录,想换到数据盘避免占满。\n\n"
            "把环境变量 `OLLAMA_MODELS` 设为目标目录后重启 Ollama 服务,新拉取的模型会存到该目录。\n\n"
            "注意:已有模型可能需要手动迁移到新目录;具体以官方文档为准。"
        ),
        source_refs=["ollama-docs:faq"],
    ),
    chunk(
        chunk_id="ext-gpu-vram-estimate-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_community",
        title="估算一个模型大概需要多大显存",
        question_patterns=[
            "这个模型要多大显存",
            "跑 7b 14b 模型需要几张卡",
            "模型显存怎么估算",
            "多大的卡能跑这个模型",
        ],
        content=(
            "适用场景:选卡/选模型前想粗估显存需求。\n\n"
            "粗略估算:权重显存 ≈ 参数量 × 每参数字节数(FP16/BF16 约 2 字节,8-bit 量化约 1 字节,4-bit 约 0.5 "
            "字节)。例如 7B 模型 FP16 权重约 14GB。\n\n"
            "权重之外,推理还要给 KV cache 和激活留额外开销(随上下文长度、并发增长,经验上常再预留 20%~40% "
            "以上);训练的显存需求远高于推理(还要存梯度和优化器状态)。\n\n"
            "注意:这只是粗估,实际占用以真机试跑 + nvidia-smi 观察为准;显存紧张时用量化 / 多卡 / 降并发"
            "(见相关条目)。"
        ),
        source_refs=["general-gpu-ops:vram-estimation"],
    ),
]

# ===================== Linux ops + env management =====================
# Generic, platform-neutral Linux operation + Python-environment knowledge for
# running training/inference on a GPU instance: terminal multiplexing, background
# jobs, disk/resource inspection, virtual environments, file transfer, SSH keys.
# Scope stays on standard, documented command behavior; platform-specific actions
# (disk RESIZE / mounting a new data disk / security-group port rules) are called
# out as out-of-scope and deferred to the platform docs.
LINUX_OPS_CHUNKS = [
    chunk(
        chunk_id="ext-linux-tmux-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="用 tmux 让训练/服务在后台跑(断开 SSH 不中断)",
        question_patterns=[
            "关掉 ssh 后台任务就停了怎么办",
            "tmux 怎么用",
            "训练怎么挂后台不被断开",
            "断开连接程序还能继续跑吗",
        ],
        content=(
            "适用场景:跑训练或推理服务,担心 SSH 断开后进程被一起杀掉。tmux 是终端复用器,"
            "会话留在后台持续运行,断开 SSH 不影响里面的程序。\n\n"
            "常用操作:\n"
            "- 新建并进入会话:`tmux new -s train`\n"
            "- 在会话里正常运行命令(如启动训练)\n"
            "- 暂时离开(detach)但让它继续跑:先按 `Ctrl-b`,松开再按 `d`\n"
            "- 重新连回:`tmux attach -t train`(`tmux ls` 列出所有会话)\n"
            "- 结束会话:在会话里 `exit`,或 `tmux kill-session -t train`\n\n"
            "注意:tmux 会话存在于实例本机,实例重启后会话会丢失;前缀键默认是 `Ctrl-b`。"
        ),
        source_refs=["tmux-wiki:getting-started"],
    ),
    chunk(
        chunk_id="ext-linux-nohup-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="用 nohup 把命令丢到后台跑并把输出写进日志",
        question_patterns=[
            "nohup 怎么用",
            "命令怎么放后台运行",
            "后台跑的程序日志在哪看",
            "关了终端程序就停了",
        ],
        content=(
            "适用场景:想让一条命令在后台运行、关掉终端也不中断,并把输出留到日志里"
            "(适合不想用 tmux 的简单场景)。\n\n"
            "```\nnohup python train.py > train.log 2>&1 &\n```\n"
            "- `nohup` 让进程忽略挂断信号,终端关闭也不退出\n"
            "- `> train.log 2>&1` 把标准输出和错误都写进 train.log\n"
            "- 结尾的 `&` 让命令在后台运行,立刻返回\n\n"
            "查看与管理:\n"
            "- 实时看日志:`tail -f train.log`\n"
            "- 找到进程:`ps aux | grep train.py`,需要时 `kill <PID>` 停止\n\n"
            "注意:多个后台任务建议各写到不同日志文件;需要长期运行或随时连回交互的场景,"
            "更推荐用 tmux(见相关条目)。"
        ),
        source_refs=["linux-man:nohup"],
    ),
    chunk(
        chunk_id="ext-linux-disk-cleanup-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="磁盘空间满了怎么查和清理",
        question_patterns=[
            "磁盘满了怎么办",
            "怎么看哪个目录占空间大",
            "no space left on device",
            "实例硬盘不够用了",
        ],
        content=(
            "适用场景:报 `No space left on device`,或想清理实例磁盘。按顺序排查:\n\n"
            "1. 看整体使用:`df -h`,确认是哪个挂载点满了(常见是根分区 `/` 或数据盘)。\n"
            "2. 定位大目录:在某目录下 `du -sh *` 看各项大小,或 "
            "`du -h --max-depth=1 / | sort -h` 逐层找占用大户。\n"
            "3. 常见可清理项:框架/包缓存(如 `~/.cache`、pip / conda 缓存)、不再需要的模型权重和"
            "数据集、旧日志。删除前务必确认文件确实不用。\n\n"
            "注意:删数据不可逆,删前先确认;扩容数据盘 / 挂载新数据盘属于平台侧操作"
            "(以平台文档 / 控制台为准),不在本条范围内。"
        ),
        source_refs=["linux-man:coreutils"],
    ),
    chunk(
        chunk_id="ext-linux-resource-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="用 top / free 看 CPU 和内存占用",
        question_patterns=[
            "怎么看 cpu 占用",
            "free 怎么看内存",
            "实例内存满了怎么查",
            "哪个进程占内存多",
        ],
        content=(
            "适用场景:想知道实例的 CPU、内存用了多少,是哪个进程在占。\n\n"
            "- `top`(或更友好的 `htop`):实时显示各进程的 CPU / 内存占用并排序;`top` 里按 `q` 退出。\n"
            "- `free -h`:看内存总量、已用、可用(`-h` 以人类可读单位显示)。\n"
            "- 找占内存最多的进程:`ps aux --sort=-%mem | head`。\n\n"
            "注意:Linux 会用空闲内存做缓存,`free` 里的 available 才是真正可用的量,buff/cache 高"
            "不代表内存不够;GPU 显存要用 `nvidia-smi` 看(见相关条目),`top` / `free` 看的是 CPU 内存。"
        ),
        source_refs=["procps-docs:top"],
    ),
    chunk(
        chunk_id="ext-linux-conda-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="用 conda 管理 Python 虚拟环境",
        question_patterns=[
            "conda 怎么创建环境",
            "conda 怎么激活环境",
            "怎么用 conda 装包",
            "conda 环境怎么切换",
        ],
        content=(
            "适用场景:想用独立的 Python 环境隔离不同项目的依赖,避免互相污染。\n\n"
            "```\nconda create -n myenv python=3.10   # 创建,指定 python 版本\n"
            "conda activate myenv                # 激活\n"
            "conda deactivate                    # 退出当前环境\n"
            "conda env list                      # 列出所有环境\n"
            "conda remove -n myenv --all         # 删除环境\n```\n"
            "装包可用 `conda install <包>`,或在激活环境后用 `pip install <包>`。\n\n"
            "注意:`conda activate` 首次使用可能需要先 `conda init` 再重开 shell;环境占磁盘空间,"
            "长期不用可删除回收(见磁盘清理相关条目)。"
        ),
        source_refs=["conda-docs:user-guide"],
    ),
    chunk(
        chunk_id="ext-linux-venv-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="用 Python 自带的 venv 建轻量虚拟环境",
        question_patterns=[
            "venv 怎么用",
            "python 怎么建虚拟环境",
            "不想用 conda 怎么隔离环境",
            "virtualenv 怎么激活",
        ],
        content=(
            "适用场景:不想装 conda,用 Python 标准库的 venv 建一个轻量虚拟环境。\n\n"
            "```\npython -m venv .venv          # 在当前目录创建 .venv\n"
            "source .venv/bin/activate     # 激活(Linux)\n"
            "pip install -r requirements.txt\n"
            "deactivate                    # 退出\n```\n"
            "注意:venv 绑定创建时所用的那个 Python 解释器版本(不像 conda 能任意指定 python 版本);"
            "激活后 `pip` / `python` 都指向环境内的版本;删除环境直接删掉 `.venv` 目录即可。"
        ),
        source_refs=["python-docs:venv"],
    ),
    chunk(
        chunk_id="ext-linux-pip-mirror-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="pip 安装慢:换用国内镜像源加速",
        question_patterns=[
            "pip 太慢怎么办",
            "pip 怎么换源",
            "pip 装包总是超时",
            "怎么用国内镜像装 python 包",
        ],
        content=(
            "适用场景:`pip install` 从默认源下载很慢或超时,想换成国内镜像源加速。\n\n"
            "临时为单次安装指定源:\n"
            "```\npip install -i <镜像源地址> <包名>\n```\n"
            "长期生效写进配置:\n"
            "```\npip config set global.index-url <镜像源地址>\n```\n"
            "国内常用镜像有清华 TUNA、阿里云等;具体地址以对应镜像站官网的说明为准(可能变动,不要写死)。\n\n"
            "注意:换源只改变下载来源,不改变包本身;遇到个别包在某镜像缺失时,可临时用 `-i` 指定其它源安装。"
        ),
        source_refs=["pip-docs:user-guide"],
    ),
    chunk(
        chunk_id="ext-linux-transfer-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="用 scp / rsync 在本地和实例之间传文件",
        question_patterns=[
            "怎么把文件传到实例",
            "scp 怎么用",
            "本地文件上传到服务器",
            "rsync 怎么同步目录",
        ],
        content=(
            "适用场景:在本地电脑和 GPU 实例之间拷贝文件 / 目录。\n\n"
            "scp(适合少量文件):\n"
            "```\n# 本地 -> 实例\nscp ./local.txt user@host:/remote/path/\n"
            "# 实例 -> 本地\nscp user@host:/remote/file.txt ./\n"
            "# 传整个目录加 -r\nscp -r ./mydir user@host:/remote/path/\n```\n"
            "rsync(适合大目录、可续传,只传有变化的部分):\n"
            "```\nrsync -avP ./mydir/ user@host:/remote/mydir/\n```\n"
            "注意:`user@host` 用实例实际的登录用户和地址(以平台给出的 SSH 登录信息为准);"
            "若 SSH 端口不是默认 22,scp 用 `-P <端口>`、rsync 用 `-e \"ssh -p <端口>\"` 指定。"
        ),
        source_refs=["openssh-docs:scp", "rsync-docs:man"],
    ),
    chunk(
        chunk_id="ext-linux-ssh-key-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="配置 SSH 免密登录(密钥对)",
        question_patterns=[
            "ssh 怎么免密登录",
            "每次登录都要输密码很麻烦",
            "ssh-keygen 怎么用",
            "怎么配置公钥登录",
        ],
        content=(
            "适用场景:不想每次 SSH 都输密码,用密钥对实现免密登录。\n\n"
            "1. 本地生成密钥对(已有可跳过):`ssh-keygen -t ed25519`,一路回车,生成私钥 "
            "`~/.ssh/id_ed25519` 和公钥 `~/.ssh/id_ed25519.pub`。\n"
            "2. 把公钥加到目标机器:`ssh-copy-id user@host`;若没有该命令,手动把本地 `.pub` 文件的"
            "内容追加到目标机的 `~/.ssh/authorized_keys`。\n"
            "3. 之后 `ssh user@host` 即可免密登录。\n\n"
            "注意:私钥(不带 `.pub` 的那个)是机密,不要外传或上传到代码仓库;目标机 `~/.ssh` 权限需为 "
            "700、`authorized_keys` 为 600,权限过松会导致免密不生效。"
        ),
        source_refs=["openssh-docs:ssh-keygen"],
    ),
]

# ===================== PyTorch / CUDA basics =====================
# Usage-basics for running PyTorch on a GPU instance: matching-CUDA install,
# device placement, single-node multi-GPU (DDP), DataLoader throughput, AMP,
# checkpointing. Troubleshooting (OOM / driver / not-detected) lives in the
# gpu_troubleshooting set above; this set is "how to use it", not "how to fix it".
PYTORCH_BASICS_CHUNKS = [
    chunk(
        chunk_id="ext-pytorch-install-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="安装与实例 CUDA 匹配的 PyTorch(避免装成 CPU 版)",
        question_patterns=[
            "怎么装 pytorch",
            "pytorch 装成了 cpu 版怎么办",
            "torch 用不了 gpu 是不是装错了",
            "pytorch 对应的 cuda 版本怎么选",
        ],
        content=(
            "适用场景:装完 PyTorch 发现用不了 GPU,常因为装成了 CPU 版或 CUDA 版本不匹配。\n\n"
            "步骤:\n"
            "1. 看实例驱动支持的 CUDA 版本:`nvidia-smi` 右上角(见相关条目)。\n"
            "2. 到 PyTorch 官网 Get Started 按系统 / 包管理器 / CUDA 版本选择,用它生成的命令安装——"
            "关键是选一个不高于驱动所支持 CUDA 版本对应的 PyTorch 构建。\n"
            "3. 装完验证:\n"
            "```\nimport torch\n"
            "print(torch.__version__, torch.version.cuda, torch.cuda.is_available())\n```\n"
            "`torch.cuda.is_available()` 为 True 即可用 GPU。\n\n"
            "注意:PyTorch 自带它所需的 CUDA runtime,不必和系统 CUDA toolkit 完全一致,但要求驱动版本"
            "足够新;具体安装命令以 PyTorch 官网当前生成的为准(版本会变,不要照搬旧命令)。"
        ),
        source_refs=["pytorch-docs:get-started"],
    ),
    chunk(
        chunk_id="ext-pytorch-device-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="PyTorch 里把模型和张量放到 GPU 上",
        question_patterns=[
            "pytorch 怎么用 gpu",
            "模型怎么放到显卡上跑",
            "张量怎么 to device",
            "pytorch 怎么指定用 cuda",
        ],
        content=(
            "适用场景:写好的 PyTorch 代码默认在 CPU 上跑,想让它用 GPU。\n\n"
            "通用写法(自动适配有无 GPU):\n"
            "```\nimport torch\n"
            "device = torch.device(\"cuda\" if torch.cuda.is_available() else \"cpu\")\n"
            "model = model.to(device)\n"
            "for x, y in loader:\n"
            "    x, y = x.to(device), y.to(device)   # 数据也要搬到同一设备\n"
            "    out = model(x)\n```\n"
            "注意:模型和参与运算的张量必须在同一设备,否则会报 device mismatch;多卡上可用 "
            "`torch.device(\"cuda:0\")` 指定具体卡,或用 `CUDA_VISIBLE_DEVICES` 选卡(见相关条目)。"
            "`.to(device)` 对张量返回新张量、对 model 是原地移动。"
        ),
        source_refs=["pytorch-docs:notes/cuda"],
    ),
    chunk(
        chunk_id="ext-pytorch-ddp-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="PyTorch 单机多卡训练(DDP + torchrun)",
        question_patterns=[
            "pytorch 怎么多卡训练",
            "torchrun 怎么用",
            "单机多卡 ddp 怎么写",
            "怎么用多张卡一起训练",
        ],
        content=(
            "适用场景:单机多张卡,想用数据并行加速训练。PyTorch 推荐用 DistributedDataParallel(DDP)"
            "配合 torchrun 启动。\n\n"
            "要点:\n"
            "1. 用 torchrun 启动,它会按卡数拉起多个进程:\n"
            "```\ntorchrun --nproc_per_node=<卡数> train.py\n```\n"
            "2. 训练脚本里初始化进程组、按 `LOCAL_RANK` 绑定本进程的卡,并用 DDP 包住模型:\n"
            "```\nimport os, torch, torch.distributed as dist\n"
            "from torch.nn.parallel import DistributedDataParallel as DDP\n"
            "dist.init_process_group(\"nccl\")\n"
            "local_rank = int(os.environ[\"LOCAL_RANK\"])\n"
            "torch.cuda.set_device(local_rank)\n"
            "model = DDP(model.to(local_rank), device_ids=[local_rank])\n```\n"
            "3. DataLoader 配 `DistributedSampler`,保证各进程拿到不同的数据分片。\n\n"
            "注意:多卡通信用 NCCL 后端;DDP 比老的 DataParallel 更高效,优先用 DDP;具体接口以当前"
            " PyTorch 版本的文档为准。"
        ),
        source_refs=["pytorch-docs:ddp-tutorial", "pytorch-docs:elastic-run"],
    ),
    chunk(
        chunk_id="ext-pytorch-dataloader-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="DataLoader 用 num_workers / pin_memory 加速数据加载",
        question_patterns=[
            "dataloader 太慢怎么办",
            "num_workers 设多少合适",
            "gpu 利用率低数据加载慢",
            "pin_memory 有用吗",
        ],
        content=(
            "适用场景:训练时 GPU 利用率上不去、像是卡在数据加载,可调 DataLoader 参数加速。\n\n"
            "```\nloader = DataLoader(dataset, batch_size=64,\n"
            "                    shuffle=True, num_workers=8, pin_memory=True)\n```\n"
            "- `num_workers`:用多个子进程并行预取数据,>0 能明显加速;具体取值与 CPU 核数、数据"
            "预处理开销有关,从 4 / 8 起试,过大反而抢占 CPU。\n"
            "- `pin_memory=True`:把批数据放进锁页内存,配合 `.to(device, non_blocking=True)` 加快"
            "主机到 GPU 的拷贝(用 GPU 时才有意义)。\n\n"
            "注意:`num_workers` 过大可能吃满 CPU 内存或引发子进程问题,按实例资源调;判断瓶颈是否在"
            "数据,可观察 `nvidia-smi` 的 GPU 利用率是否经常掉到低位。"
        ),
        source_refs=["pytorch-docs:data"],
    ),
    chunk(
        chunk_id="ext-pytorch-amp-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="用混合精度(AMP)省显存、加速训练",
        question_patterns=[
            "pytorch 混合精度怎么用",
            "amp 怎么写",
            "训练怎么既省显存又加速",
            "autocast 和 gradscaler 怎么用",
        ],
        content=(
            "适用场景:想在不大改模型的前提下降低训练显存占用、提升速度,可用自动混合精度(AMP)。\n\n"
            "```\nfrom torch.cuda.amp import autocast, GradScaler\n"
            "scaler = GradScaler()\n"
            "for x, y in loader:\n"
            "    optimizer.zero_grad()\n"
            "    with autocast():\n"
            "        loss = loss_fn(model(x), y)\n"
            "    scaler.scale(loss).backward()\n"
            "    scaler.step(optimizer)\n"
            "    scaler.update()\n```\n"
            "`autocast` 让前向在半精度下计算、`GradScaler` 缩放梯度防止下溢。\n\n"
            "注意:AMP 主要在支持的 GPU 上有收益;少数对数值精度敏感的部分可能需保持全精度;具体 API"
            "(如新的 `torch.amp` 写法)以当前 PyTorch 版本文档为准。"
        ),
        source_refs=["pytorch-docs:amp"],
    ),
    chunk(
        chunk_id="ext-pytorch-checkpoint-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="PyTorch 保存和加载模型(state_dict)",
        question_patterns=[
            "pytorch 怎么保存模型",
            "torch.save 怎么用",
            "怎么加载训练好的权重",
            "state_dict 怎么用",
        ],
        content=(
            "适用场景:训练后保存模型,之后加载继续用或推理。推荐保存 `state_dict`(只存参数)而不是"
            "整个模型对象。\n\n"
            "```\n# 保存\ntorch.save(model.state_dict(), \"model.pth\")\n"
            "# 加载(需先有同样结构的 model)\n"
            "model.load_state_dict(torch.load(\"model.pth\", map_location=device))\n"
            "model.eval()   # 推理前切到 eval 模式\n```\n"
            "注意:加载到不同设备用 `map_location` 指定(如把 GPU 上存的权重加载到 CPU);"
            "`model.eval()` 会关闭 dropout / BN 的训练行为,推理前别忘了;若要继续训练,还需另存"
            "优化器状态、epoch 等。"
        ),
        source_refs=["pytorch-docs:saving-loading-models"],
    ),
]

# ============================== 模型下载 ==============================
# Download vertical: the serving side (vLLM/SGLang/Ollama launch, tensor-parallel,
# quantized loading) is already covered; the genuine gap is GETTING the weights in
# China (hf-mirror / ModelScope / gated-model tokens / local GGUF import) and then
# pointing a serving engine at the locally-downloaded path. Conservative authoring:
# name the common tools, hedge volatile mirror addresses, defer platform-specific
# disk/quota concerns to platform docs.
MODEL_DOWNLOAD_CHUNKS = [
    # NOTE: a dedicated hf-mirror chunk was intentionally NOT added — HF_ENDPOINT /
    # mirror download is already covered by ext-gpu-hf-download-001, and a near-
    # duplicate crowded the platform HF-download FAQ out of a golden query's top-3
    # (parity regression on w0-golden-0064, the textbook hybrid-recall pattern). The
    # concrete hf-mirror.com naming is delivered operationally by the deploy reply's
    # self-pull guidance instead.
    chunk(
        chunk_id="ext-modelscope-download-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="用 ModelScope(魔搭)下载模型",
        question_patterns=[
            "modelscope 怎么下载模型",
            "魔搭怎么下模型",
            "国内下载模型除了 huggingface 还有什么",
            "modelscope download 怎么用",
        ],
        content=(
            "适用场景:很多模型在国内的 ModelScope(魔搭)社区有镜像,可作为 HuggingFace 之外的下载来源。\n\n"
            "安装:\n```\npip install -U modelscope\n```\n"
            "命令行下载到指定目录:\n```\nmodelscope download --model <模型ID> --local_dir ./model\n```\n"
            "或在 Python 里下载:\n```\nfrom modelscope import snapshot_download\n"
            "snapshot_download('<模型ID>', local_dir='./model')\n```\n\n"
            "注意:模型 ID 以 ModelScope 站点上的名称为准;同一模型在 ModelScope 与 HuggingFace 的 ID 可能不同。"
        ),
        source_refs=["modelscope-docs:download"],
    ),
    chunk(
        chunk_id="ext-hf-token-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="下载受限(gated)模型需要 HuggingFace 访问令牌",
        question_patterns=[
            "huggingface 模型需要登录怎么办",
            "下载 llama 提示没有权限",
            "hf token 怎么设置",
            "gated model 401 403 怎么解决",
        ],
        content=(
            "适用场景:部分模型(如 Llama 等)是受限(gated)模型,下载会提示无权限/需要登录。\n\n"
            "1. 先在该模型在 HuggingFace 上的页面申请访问并等待通过。\n"
            "2. 在 HuggingFace 账号设置里创建一个访问令牌(Access Token)。\n"
            "3. 在实例里登录,或用环境变量提供令牌:\n"
            "```\nhuggingface-cli login   # 按提示粘贴令牌\n"
            "# 或:\nexport HF_TOKEN=<你的令牌>\n```\n"
            "之后再下载即可带上凭证。\n\n"
            "注意:访问令牌是私密凭证,不要写进代码仓库或分享给他人;泄露后应及时在账号设置里吊销重建。"
        ),
        source_refs=["hf-docs:security-tokens", "hf-docs:models-gated"],
    ),
    chunk(
        chunk_id="ext-ollama-modelfile-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="Ollama 导入本地模型(Modelfile / GGUF)",
        question_patterns=[
            "ollama 怎么导入自己的模型",
            "ollama 加载本地 gguf",
            "ollama modelfile 怎么写",
            "ollama 用自定义模型",
        ],
        content=(
            "适用场景:已有一个本地 GGUF 权重文件,想让 Ollama 使用它(而不是从模型库拉取)。\n\n"
            "1. 新建一个 Modelfile,用 `FROM` 指向本地权重文件:\n"
            "```\nFROM ./your-model.gguf\n```\n"
            "可按需追加 `PARAMETER`(如温度)、`TEMPLATE`、`SYSTEM` 等指令。\n"
            "2. 创建并运行:\n```\nollama create my-model -f Modelfile\n"
            "ollama run my-model\n```\n\n"
            "注意:GGUF 文件的来源与转换方式以实际为准;Modelfile 的可用指令以 Ollama 官方文档为准。"
        ),
        source_refs=["ollama-docs:modelfile", "ollama-docs:import"],
    ),
    chunk(
        chunk_id="ext-serve-local-path-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="用本地已下载的模型目录起服务(避免重复下载)",
        question_patterns=[
            "vllm 怎么加载本地模型",
            "模型已经下载好了怎么用 vllm 起服务",
            "sglang 怎么指定本地模型路径",
            "怎么不让每次都重新下载模型",
        ],
        content=(
            "适用场景:模型已经下载到本地(如数据盘),想让推理引擎直接用本地目录,避免每次重新下载。\n\n"
            "把模型名换成本地目录的路径即可:\n"
            "```\n# vLLM\nvllm serve /data/models/Qwen2.5-7B-Instruct\n"
            "# SGLang\npython -m sglang.launch_server --model-path /data/models/Qwen2.5-7B-Instruct\n```\n"
            "该目录应是一个完整的模型仓库(包含 config.json、权重文件、tokenizer 等)。\n\n"
            "建议先用 huggingface-cli 或 modelscope 把模型下载到数据盘的固定目录,再让服务指向它;"
            "这样既不占系统盘,也能在重建/多次启动时复用。注意:具体参数以各引擎的 `--help` / 官方文档为准。"
        ),
        source_refs=["vllm-docs:cli/serve", "sglang-docs:server_arguments"],
    ),
]

CHAT_SEEDED_EXTERNAL_CHUNKS = [
    chunk(
        chunk_id="ext-linux-ssh-keepalive-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="SSH / VS Code Remote 经常断开:配置客户端心跳",
        question_patterns=[
            "ssh 过一会儿自动断开",
            "connection closed by remote host",
            "vscode remote ssh 老是重连",
            "cursor 远程开发隔一会儿断联",
            "ssh 怎么设置心跳包",
        ],
        content=(
            "适用场景:SSH、VS Code Remote、Cursor 远程连接空闲一段时间后断开,但实例本身还能登录。"
            "优先在本地 SSH 客户端配置协议级心跳,减少中间网络设备清理空闲连接导致的断连。\n\n"
            "在本地 `~/.ssh/config` 增加针对该实例或所有主机的配置:\n"
            "```\nHost my-gpu\n"
            "    HostName <实例公网地址或域名>\n"
            "    User <登录用户名>\n"
            "    Port <SSH端口>\n"
            "    ServerAliveInterval 30\n"
            "    ServerAliveCountMax 6\n```\n"
            "`ServerAliveInterval` 表示空闲多久后向服务器发一次加密通道内的探测消息;"
            "`ServerAliveCountMax` 表示连续多少次没有响应后才断开。也可临时在命令行使用:\n"
            "```\nssh -o ServerAliveInterval=30 -o ServerAliveCountMax=6 -p <端口> <用户>@<地址>\n```\n\n"
            "注意:心跳只能缓解空闲连接被网络断开的情况。如果实例内存耗尽、VS Code server 被杀、"
            "实例重启或网络本身不稳定,仍需要分别排查内存、进程和链路。"
        ),
        source_refs=["openssh-docs:ssh_config"],
    ),
    chunk(
        chunk_id="ext-linux-large-transfer-rsync-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="大文件 / 大目录上传中断:用 rsync 续传",
        question_patterns=[
            "上传几十 G 文件总是断",
            "文件管理上传到一半退出怎么办",
            "大模型文件怎么断点续传",
            "scp 中断后怎么继续传",
            "rsync 怎么传大目录",
        ],
        content=(
            "适用场景:网页文件管理器或一次性 scp 传大文件时中断,希望能看进度并从中断处继续。"
            "优先用 `rsync -avP` 通过 SSH 传输。\n\n"
            "本地传到实例:\n"
            "```\nrsync -avP -e \"ssh -p <SSH端口>\" ./local-dir/ <用户>@<实例地址>:/data/local-dir/\n```\n"
            "实例传回本地:\n"
            "```\nrsync -avP -e \"ssh -p <SSH端口>\" <用户>@<实例地址>:/data/model.bin ./\n```\n"
            "`-a` 保留目录结构和常用文件属性,`-v` 显示过程,`-P` 等价于保留未传完部分并显示进度。"
            "再次执行同一条命令时,rsync 会跳过已经一致的部分,继续同步缺失或变化的内容。\n\n"
            "注意:目标路径建议放在容量足够的数据盘或云存储挂载目录;传输前先用 `df -h` 确认空间。"
            "如果登录端口、用户名或地址不确定,以平台提供的 SSH 登录信息为准。"
        ),
        source_refs=["rsync-docs:man", "openssh-docs:ssh_config"],
    ),
    chunk(
        chunk_id="ext-webapp-gradio-remote-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="Gradio 在远程实例上只本机可访问:设置 server_name 和端口",
        question_patterns=[
            "gradio 外部打不开",
            "gradio 只能 127.0.0.1 访问",
            "gradio 怎么指定端口",
            "gradio share 和 server_name 怎么选",
            "gradio 页面本机能开外面打不开",
        ],
        content=(
            "适用场景:Gradio 应用已经启动,但浏览器从外部访问不到。远程实例上通常需要让 Gradio "
            "监听所有网卡,并确认平台侧端口已放行。\n\n"
            "最小写法:\n"
            "```\ndemo.launch(server_name=\"0.0.0.0\", server_port=7860)\n```\n"
            "也可以通过环境变量设置:\n"
            "```\nexport GRADIO_SERVER_NAME=0.0.0.0\n"
            "export GRADIO_SERVER_PORT=7860\n```\n"
            "如果只是临时公网分享,可用 `share=True` 生成分享链接;如果是长期服务,更建议固定端口并做好访问控制。"
            "需要登录保护时可配置 `auth`。\n\n"
            "注意:应用监听 `0.0.0.0` 只是实例内服务开放;外部仍需平台安全组/端口映射允许该端口。"
            "不要把未鉴权的 Gradio 应用直接暴露到公网。"
        ),
        source_refs=["gradio-docs:blocks-launch"],
    ),
    chunk(
        chunk_id="ext-webapp-streamlit-uvicorn-remote-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="Streamlit / FastAPI 在远程实例上对外访问",
        question_patterns=[
            "streamlit 外部打不开",
            "fastapi uvicorn 外部访问不了",
            "服务启动了但是公网打不开",
            "python web 服务怎么监听 0.0.0.0",
            "怎么指定 streamlit 端口",
        ],
        content=(
            "适用场景:Python Web 服务在实例里启动成功,本机地址能访问,但外部浏览器打不开。"
            "先确认服务监听地址不是只绑定 `127.0.0.1`,再检查端口放行。\n\n"
            "Streamlit:\n"
            "```\nstreamlit run app.py --server.address 0.0.0.0 --server.port 8501\n```\n"
            "Uvicorn / FastAPI:\n"
            "```\nuvicorn main:app --host 0.0.0.0 --port 8000\n```\n"
            "Uvicorn 也支持 `UVICORN_HOST` / `UVICORN_PORT` 环境变量。开发时 `--reload` 方便调试,"
            "但不要和 `--workers` 同时使用。\n\n"
            "注意:监听地址和端口只是应用侧配置;平台侧安全组/端口映射也要放行同一个端口。"
            "对公网开放前建议加登录、反向代理或其它访问控制。"
        ),
        source_refs=["streamlit-docs:config", "uvicorn-docs:settings"],
    ),
    chunk(
        chunk_id="ext-hf-cache-cli-download-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="Hugging Face 大模型下载:指定目录、缓存和令牌",
        question_patterns=[
            "huggingface 下载到数据盘",
            "hf 模型缓存占满系统盘",
            "hf download 怎么用",
            "大模型下载中断会不会重下",
            "HF_HOME 和 HF_HUB_CACHE 怎么设置",
        ],
        content=(
            "适用场景:下载大模型时系统盘空间不足、重复下载、或需要把模型放到固定目录。"
            "先把缓存或目标目录指到容量更大的磁盘。\n\n"
            "设置缓存目录:\n"
            "```\nexport HF_HOME=/data/huggingface\n"
            "export HF_HUB_CACHE=/data/huggingface/hub\n```\n"
            "下载完整模型仓库到指定目录:\n"
            "```\nhf download <repo_id> --local-dir /data/models/<name>\n```\n"
            "只下载部分文件:\n"
            "```\nhf download <repo_id> --include \"*.safetensors\" --local-dir /data/models/<name>\n```\n"
            "受限模型需要先申请权限,再登录或设置访问令牌:\n"
            "```\nhf auth login\n# 或 export HF_TOKEN=<访问令牌>\n```\n\n"
            "注意:令牌是私密凭证,不要写进脚本或聊天记录。下载前用 `df -h` 看目标盘空间;"
            "如果网络慢或超时,可适当设置下载超时或使用实际可用的镜像/加速方式。"
        ),
        source_refs=["hf-docs:environment_variables", "hf-docs:cli", "hf-docs:security-tokens"],
    ),
    chunk(
        chunk_id="ext-peft-lora-qlora-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="LoRA / QLoRA 微调显存不够:先调这些参数",
        question_patterns=[
            "lora 微调显存不够",
            "qlora 怎么省显存",
            "peft lora_config 怎么写",
            "target_modules 怎么选",
            "bitsandbytes 4bit 和 lora 什么关系",
        ],
        content=(
            "适用场景:想在单卡或小显存实例上微调大模型。LoRA 只训练少量适配器参数;"
            "QLoRA 通常把基础模型量化到 4bit,再叠加 LoRA,以进一步降低显存。\n\n"
            "典型 PEFT 配置:\n"
            "```\nfrom peft import LoraConfig\n"
            "config = LoraConfig(\n"
            "    r=16,\n"
            "    lora_alpha=32,\n"
            "    lora_dropout=0.05,\n"
            "    target_modules=\"all-linear\",\n"
            "    bias=\"none\",\n"
            "    task_type=\"CAUSAL_LM\",\n"
            ")\n```\n"
            "排查顺序:先确认基础模型是否已用 4bit/8bit 量化加载;再降低 batch size、序列长度和梯度累积组合;"
            "最后再调整 LoRA 的 `r`、`target_modules` 等训练范围。`target_modules=\"all-linear\"` 常用于 QLoRA 风格训练,"
            "但不同模型结构可能需要按模型层名调整。\n\n"
            "注意:LoRA 参数越多越可能提升适配能力,也会增加显存和训练时间;显存问题不能只靠 LoRA 解决,"
            "还要结合数据长度、batch、优化器和量化方式一起看。"
        ),
        source_refs=["peft-docs:lora", "peft-docs:quantization"],
    ),
    chunk(
        chunk_id="ext-pytorch-nccl-ddp-debug-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="PyTorch 多卡训练卡住 / NCCL 报错怎么排查",
        question_patterns=[
            "torchrun 卡住不动",
            "ddp nccl 报错",
            "多卡训练只跑一张卡",
            "NCCL_SOCKET_IFNAME 怎么设置",
            "LOCAL_RANK 是什么",
        ],
        content=(
            "适用场景:单机多卡训练启动后卡住、NCCL 报错、或进程没有按 GPU 数量正确启动。"
            "先确认启动方式和每个进程绑定的 GPU。\n\n"
            "单机多卡启动:\n"
            "```\ntorchrun --standalone --nnodes=1 --nproc-per-node=<GPU数量> train.py\n```\n"
            "训练脚本中通常按 `LOCAL_RANK` 绑定设备:\n"
            "```\nlocal_rank = int(os.environ[\"LOCAL_RANK\"])\n"
            "torch.cuda.set_device(local_rank)\n"
            "dist.init_process_group(backend=\"nccl\")\n```\n"
            "只想让程序看到部分卡时,先设置:\n"
            "```\nexport CUDA_VISIBLE_DEVICES=0,1\n```\n"
            "NCCL 排障可临时打开日志:\n"
            "```\nexport NCCL_DEBUG=INFO\nexport TORCH_DISTRIBUTED_DEBUG=DETAIL\n```\n"
            "多网卡环境下 NCCL 选错网卡时,可按实际网卡名设置 `NCCL_SOCKET_IFNAME`。\n\n"
            "注意:这些日志和 NCCL 环境变量主要用于定位问题,不要长期无脑打开。"
            "如果不同 rank 的输入形状、batch 数或代码分支不一致,也可能导致 all-reduce 卡住。"
        ),
        source_refs=["pytorch-docs:elastic-run", "pytorch-docs:distributed", "nvidia-docs:nccl-env"],
    ),
]

AI4SCIENCE_GPU_CHUNKS = [
    chunk(
        chunk_id="ext-jax-gpu-install-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="JAX 在 GPU 实例上安装和确认后端",
        question_patterns=[
            "jax 怎么装 gpu 版",
            "jax.devices 只看到 cpu",
            "jax 没用上 gpu 怎么查",
            "jax cuda13 怎么安装",
            "jax 在科学计算里怎么确认跑在 gpu",
        ],
        content=(
            "适用场景:客户用 JAX 做科学计算、扩散模型或蛋白/分子相关任务,安装后不确定是否真的使用 GPU。"
            "先确认实例能正常 `nvidia-smi`,再安装与驱动/平台匹配的 JAX GPU 包。\n\n"
            "官方当前推荐的 CUDA 13 pip 安装示例:\n"
            "```\npython -m pip install -U pip\npip install -U \"jax[cuda13]\"\n```\n"
            "CPU 版是 `pip install -U jax`;如果误装 CPU 版,程序仍可运行但只会落到 CPU。"
            "安装后用下面命令确认后端:\n"
            "```\npython - <<'PY'\nimport jax\nprint(jax.devices())\nPY\n```\n"
            "如果只看到 CPU,优先检查 NVIDIA 驱动、CUDA 版本支持范围、Python 环境是否装错包,以及是否在容器内漏传 GPU。\n\n"
            "注意:JAX 的 CUDA 12/13、ROCm、TPU 等安装方式不同;不要把旧教程里的 `jaxlib` 固定版本照搬到新环境。"
        ),
        source_refs=["jax-docs:installation"],
    ),
    chunk(
        chunk_id="ext-cupy-gpu-array-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="CuPy 安装 CUDA wheel 并验证 GPU 数组计算",
        question_patterns=[
            "cupy 怎么安装 gpu 版",
            "cupy-cuda12x 和 cupy-cuda13x 怎么选",
            "cupy.show_config 怎么看",
            "cupy 能不能替代 numpy 跑 gpu",
            "cupy 安装后找不到 cuda 头文件",
        ],
        content=(
            "适用场景:客户把 NumPy 风格的数组计算迁到 GPU,常见于仿真前后处理、图像/矩阵计算和科学数据处理。"
            "CuPy 的 PyPI 包名按 CUDA 大版本区分,同一环境里只应安装一个 CuPy 包。\n\n"
            "CUDA 12/13 常见安装:\n"
            "```\npython -m pip install -U setuptools pip\npip install cupy-cuda12x\n# 或\npip install cupy-cuda13x\n```\n"
            "如果希望由 pip 安装 CUDA 组件 wheel,可按官方说明使用 `[ctk]` 变体。检查当前环境:\n"
            "```\npip freeze | grep cupy\npython -c \"import cupy; cupy.show_config()\"\npython - <<'PY'\nimport cupy as cp\nx = cp.arange(5)\nprint(x.device)\nPY\n```\n"
            "如果构建自定义 CUDA kernel 时缺头文件,再按实际 CUDA 版本补装对应的 `nvidia-cuda-runtime-cu12` 等包。\n\n"
            "注意:不要同时装 `cupy`、`cupy-cuda12x`、`cupy-cuda13x`;混装容易导致导入或链接异常。"
        ),
        source_refs=["cupy-docs:install"],
    ),
    chunk(
        chunk_id="ext-openmm-cuda-platform-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="OpenMM 分子动力学任务指定 CUDA 平台",
        question_patterns=[
            "openmm 怎么指定 cuda",
            "openmm 为什么跑在 cpu",
            "openmm 怎么看用了哪个平台",
            "openmm 多张 gpu 怎么指定",
            "分子动力学 openmm 没用上显卡",
        ],
        content=(
            "适用场景:客户运行分子动力学模拟,OpenMM 能跑但速度很慢,怀疑落到 CPU 或没有选中目标 GPU。"
            "OpenMM 会自动选择可用的较快平台,但生产任务建议显式指定平台并打印确认。\n\n"
            "脚本里指定 CUDA:\n"
            "```\nfrom openmm import Platform\nplatform = Platform.getPlatform('CUDA')\nproperties = {'DeviceIndex': '0', 'Precision': 'mixed'}\nsimulation = Simulation(topology, system, integrator, platform, properties)\nprint(simulation.context.getPlatform().getName())\n```\n"
            "多卡可把 `DeviceIndex` 写成逗号分隔,例如 `'0,1'`;也可以用 `OPENMM_DEFAULT_PLATFORM=CUDA` 影响默认选择。"
            "安装后可用 `python -m openmm.testInstallation` 查看可用平台。\n\n"
            "注意:如果 CUDA 平台不可用,先查驱动、OpenMM 安装方式、容器 GPU 透传;不要只看进程存在就判断已经在 GPU 上计算。"
        ),
        source_refs=["openmm-docs:running-sims"],
    ),
    chunk(
        chunk_id="ext-gromacs-gpu-offload-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="GROMACS mdrun GPU offload 和多卡任务分配",
        question_patterns=[
            "gromacs 怎么用 gpu 跑",
            "gmx mdrun -nb gpu 怎么用",
            "gromacs pme gpu 怎么开",
            "gromacs 多卡怎么分配任务",
            "gromacs 跑分子动力学 gpu 利用率低",
        ],
        content=(
            "适用场景:客户用 GROMACS 跑分子动力学,希望把非键相互作用、PME、bonded/update 等计算尽量放到 GPU。"
            "先确认当前 GROMACS 构建支持 CUDA/SYCL 等 GPU 后端,再调 `mdrun` 参数。\n\n"
            "常见起步命令:\n"
            "```\ngmx mdrun -s topol.tpr -nb gpu -pme gpu -npme 1\n```\n"
            "多卡或共享节点上可限制可见 GPU:\n"
            "```\nexport GMX_GPU_ID=0,1\n```\n"
            "需要更细地把 GPU 任务映射到不同卡时使用 `-gputasks`,例如让不同 rank 的 PP/PME 任务落到指定 GPU。"
            "官方文档也给出 `-bonded gpu`、`-update gpu` 等进一步 offload 的组合,但是否更快取决于体系和硬件。\n\n"
            "注意:`-gpu_id` 和 `-gputasks` 不能同时用;性能低不一定是 GPU 没工作,也可能是 CPU 线程、PME rank、I/O 或体系规模不匹配。"
        ),
        source_refs=["gromacs-docs:mdrun-performance"],
    ),
    chunk(
        chunk_id="ext-rapids-cudf-install-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="RAPIDS cuDF 加速表格数据处理的安装检查",
        question_patterns=[
            "rapids cudf 怎么装",
            "pandas 太慢能不能用 gpu",
            "cudf-cu12 安装失败",
            "rapids pip conda docker 怎么选",
            "gpu 数据分析环境怎么配",
        ],
        content=(
            "适用场景:客户做大表格、特征工程、图计算或数据清洗,希望用 RAPIDS/cuDF 把部分 pandas 风格流程迁到 GPU。"
            "RAPIDS 对 Python、CUDA、系统平台和安装方式的组合要求较严格,应优先用官方安装选择器生成命令。\n\n"
            "排查顺序:\n"
            "1. 用 `python --version`、`nvidia-smi` 确认 Python 和驱动/CUDA 信息。\n"
            "2. 在官方安装页选择 pip、conda 或 Docker,不要混用旧博客命令。\n"
            "3. pip 安装时 wheel 后缀要匹配系统 CUDA 大版本,例如 CUDA 12 环境选 `-cu12` 包。\n"
            "4. conda 环境避免混入 `defaults` 与 `conda-forge` 的不兼容组合,必要时重建干净环境。\n\n"
            "最小验证:\n"
            "```\npython - <<'PY'\nimport cudf\ns = cudf.Series([1, 2, 3])\nprint(s.sum())\nPY\n```\n"
            "注意:RAPIDS 不等于所有 pandas 代码无修改加速;先从数据规模大、GPU 支持成熟的步骤迁移。"
        ),
        source_refs=["rapids-docs:install"],
    ),
    chunk(
        chunk_id="ext-docker-gpu-container-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="Docker 容器里看不到 GPU:验证 NVIDIA Container Toolkit",
        question_patterns=[
            "docker 里面 nvidia-smi 看不到 gpu",
            "容器怎么使用显卡",
            "docker --gpus all 怎么用",
            "nvidia container toolkit 怎么验证",
            "容器里 torch cuda false 怎么排查",
        ],
        content=(
            "适用场景:宿主机 `nvidia-smi` 正常,但 Docker 容器内看不到 GPU,导致 PyTorch、JAX、OpenMM、RAPIDS 等只跑 CPU。"
            "先验证容器运行时是否能把 GPU 透传进去。\n\n"
            "官方示例验证:\n"
            "```\nsudo docker run --rm --runtime=nvidia --gpus all ubuntu nvidia-smi\n```\n"
            "较新的 Docker 也常直接使用:\n"
            "```\ndocker run --rm --gpus all nvidia/cuda nvidia-smi\n```\n"
            "只暴露指定 GPU:\n"
            "```\ndocker run --rm --gpus '\"device=0,1\"' nvidia/cuda nvidia-smi\n```\n"
            "如果失败,按顺序检查宿主机驱动、NVIDIA Container Toolkit 安装与 Docker runtime 配置,以及容器镜像是否包含需要的 CUDA 用户态库。\n\n"
            "注意:`CUDA_VISIBLE_DEVICES` 只影响程序看到哪些卡;如果容器 runtime 没透传 GPU,设置它也不会让 GPU 出现。"
        ),
        source_refs=["nvidia-docs:container-toolkit-sample", "nvidia-docs:docker-specialized"],
    ),
    chunk(
        chunk_id="ext-colabfold-local-gpu-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="ColabFold 本地 GPU 预测:安装、MSA 与资源预估",
        question_patterns=[
            "colabfold 怎么在本地 gpu 跑",
            "colabfold_batch 怎么用",
            "colabfold 数据库要多大空间",
            "蛋白结构预测需要什么 gpu",
            "colabfold msa-only 是什么",
        ],
        content=(
            "适用场景:客户做蛋白结构预测,想把 ColabFold 从 Notebook 迁到自己的 GPU 实例。"
            "先根据是否需要本地数据库决定资源规模:小规模可用公共 MSA server,大规模本地库会占用大量磁盘和内存。\n\n"
            "官方 README 的直接安装示例包含 CUDA 版依赖:\n"
            "```\nconda create -n colabfold -c conda-forge -c bioconda python=3.13 kalign2 hhsuite mmseqs2\nconda activate colabfold\npip install colabfold[alphafold,openmm] jax[cuda12] openmm[cuda12]\n```\n"
            "小规模直接预测:\n"
            "```\ncolabfold_batch input_sequences.fasta out_dir\n```\n"
            "也可先生成 MSA,再把 GPU 预测拆出来:\n"
            "```\ncolabfold_batch input_sequences.fasta out_dir --msa-only\ncolabfold_batch input_sequences.fasta out_dir\n```\n"
            "大规模本地库流程需要先准备数据库目录;官方说明提示数据库和内存需求可能达到数百 GB 到 TB 级。\n\n"
            "注意:公共 MSA server 不适合多机并发滥用;商业/敏感序列还要考虑数据合规与本地化。"
        ),
        source_refs=["colabfold-repo:README", "jax-docs:installation", "openmm-docs:running-sims"],
    ),
    chunk(
        chunk_id="ext-alphafold3-docker-gpu-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="AlphaFold3 Docker 运行前的 GPU 和驱动检查",
        question_patterns=[
            "alphafold3 docker 怎么用 gpu",
            "alphafold3 安装前要检查什么",
            "alphafold3 nvidia-smi failed",
            "alphafold3 容器看不到显卡",
            "蛋白结构预测 docker gpu 环境怎么准备",
        ],
        content=(
            "适用场景:客户按 AlphaFold3 安装文档准备 Docker 环境,但 GPU 驱动或容器 GPU 支持未就绪。"
            "先把宿主机驱动和容器运行时打通,再构建/运行应用镜像。\n\n"
            "检查顺序:\n"
            "1. 宿主机执行 `nvidia-smi`,确认能看到 GPU、驱动版本和 CUDA runtime 信息。\n"
            "2. 如果 `nvidia-smi` 报无法和驱动通信,先处理驱动安装、内核模块或重启问题。\n"
            "3. 安装并配置 NVIDIA Container Toolkit 后,用官方样例验证容器内 GPU:\n"
            "```\ndocker run --rm --gpus all nvidia/cuda nvidia-smi\n```\n"
            "4. 再按 AlphaFold3 文档继续 Docker、模型参数和数据库相关步骤。\n\n"
            "注意:AlphaFold3/蛋白结构预测对显存、磁盘、CPU 内存和数据库下载都有要求;GPU 可见只是第一关。"
        ),
        source_refs=["alphafold3-docs:installation", "nvidia-docs:container-toolkit-sample"],
    ),
]

PRO_GPU_SUPPORT_CHUNKS = [
    chunk(
        chunk_id="ext-transformers-trainer-gpu-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="Transformers Trainer 微调时的 GPU、batch 和保存检查",
        question_patterns=[
            "transformers trainer 怎么用 gpu 微调",
            "trainer 训练显存不够怎么调",
            "trainingarguments batch size 怎么设",
            "transformers 训练结果保存到哪里",
            "trainer 为什么只用 cpu",
        ],
        content=(
            "适用场景:客户用 Hugging Face Transformers 的 `Trainer` 做分类、生成或 SFT 前置实验。"
            "`Trainer` 负责训练循环,`TrainingArguments` 控制 batch、保存、评估、混合精度和分布式策略。\n\n"
            "最小骨架:\n"
            "```\nfrom transformers import Trainer, TrainingArguments\nargs = TrainingArguments(\n    output_dir=\"/data/output\",\n    per_device_train_batch_size=1,\n    gradient_accumulation_steps=8,\n    fp16=True,\n    save_steps=500,\n)\ntrainer = Trainer(model=model, args=args, train_dataset=train_ds)\ntrainer.train()\n```\n"
            "排查顺序:先用 `nvidia-smi` 和 `python -c \"import torch; print(torch.cuda.is_available())\"` 确认 GPU;再降低"
            "`per_device_train_batch_size`、序列长度,增加 `gradient_accumulation_steps`;最后考虑 LoRA/QLoRA 或 DeepSpeed。\n\n"
            "注意:`fp16`/`bf16` 是否可用取决于 GPU 和框架支持;多卡运行通常交给 Accelerate、torchrun 或 Trainer 分布式配置。"
        ),
        source_refs=["hf-transformers:trainer", "pytorch-docs:get-started"],
    ),
    chunk(
        chunk_id="ext-accelerate-launch-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="Accelerate 单机多卡启动训练脚本",
        question_patterns=[
            "accelerate launch 怎么用",
            "accelerate 多卡训练怎么启动",
            "accelerate config 是什么",
            "accelerate 指定两张 gpu",
            "accelerate 和 torchrun 有什么关系",
        ],
        content=(
            "适用场景:训练脚本已经能单卡运行,想用 Accelerate 启动单机多卡或统一配置混合精度。"
            "推荐先生成配置,再用同一配置启动。\n\n"
            "常用流程:\n"
            "```\naccelerate config\naccelerate launch train.py --arg1 value\n```\n"
            "也可以不预先配置,直接指定关键参数:\n"
            "```\naccelerate launch --multi_gpu --num_processes=2 --mixed_precision=fp16 train.py\n```\n"
            "如果只想让程序看到部分卡,先设置 `CUDA_VISIBLE_DEVICES=0,1`。"
            "`accelerate launch` 本质上负责启动多个训练进程,类似 torchrun 的角色,但会读取 Accelerate 配置文件。\n\n"
            "注意:脚本本身仍要按分布式方式正确处理数据、日志和保存;不要让每个进程都写同一个文件。"
        ),
        source_refs=["hf-accelerate:launch", "pytorch-docs:elastic-run"],
    ),
    chunk(
        chunk_id="ext-bitsandbytes-quantization-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="bitsandbytes 8bit/4bit 加载和 QLoRA 常见限制",
        question_patterns=[
            "bitsandbytes 怎么装",
            "load_in_4bit 怎么用",
            "qlora 为什么只能训练 lora 参数",
            "bitsandbytes cuda 版本不匹配",
            "transformers 4bit 加载模型显存不够",
        ],
        content=(
            "适用场景:客户想用 8bit/4bit 量化降低大模型显存,或用 QLoRA 在小显存卡上微调。"
            "先安装当前 Transformers 生态常用组合:\n"
            "```\npip install --upgrade transformers accelerate bitsandbytes\n```\n"
            "4bit 加载示例:\n"
            "```\nfrom transformers import AutoModelForCausalLM, BitsAndBytesConfig\nbnb = BitsAndBytesConfig(load_in_4bit=True)\nmodel = AutoModelForCausalLM.from_pretrained(\n    \"<model>\", device_map=\"auto\", quantization_config=bnb\n)\n```\n"
            "8bit/4bit 主要减少权重显存;官方文档提示低比特训练通常只训练额外参数,例如 LoRA adapter。"
            "如果导入失败或提示 CUDA 不兼容,先核对 bitsandbytes 支持的 CUDA/GPU 范围和当前 Python 环境里的 torch 版本。\n\n"
            "注意:量化不能解决所有 OOM;长上下文、batch、优化器状态、KV cache 仍会占显存。"
        ),
        source_refs=["hf-transformers:bitsandbytes", "peft-docs:quantization"],
    ),
    chunk(
        chunk_id="ext-llamafactory-lora-train-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="LLaMA-Factory LoRA/QLoRA 微调启动和资源预估",
        question_patterns=[
            "llama factory 怎么训练 lora",
            "llamafactory-cli train 怎么用",
            "llama factory qlora 需要多大显存",
            "llama factory deepspeed 怎么装",
            "llamafactory webui 怎么启动",
        ],
        content=(
            "适用场景:客户想用 LLaMA-Factory 做 SFT、LoRA/QLoRA 或图形化微调。"
            "先安装项目和可选依赖,再从示例 YAML 改模型、数据和输出目录。\n\n"
            "安装和快速启动:\n"
            "```\ngit clone --depth 1 https://github.com/hiyouga/LlamaFactory.git\ncd LlamaFactory\npip install -e .\npip install -r requirements/metrics.txt\n# 需要 DeepSpeed 时再装 requirements/deepspeed.txt\nllamafactory-cli train examples/train_lora/qwen3_lora_sft.yaml\n```\n"
            "WebUI:\n"
            "```\nllamafactory-cli webui\n```\n"
            "官方 README 给出不同模型大小和方法的显存估算:LoRA/QLoRA 相比全参训练显存低得多,但仍受序列长度、batch 和精度影响。\n\n"
            "注意:训练前先确认 PyTorch GPU 版可用;数据集需要在 `data/dataset_info.json` 或配置文件中正确声明。"
        ),
        source_refs=["llamafactory-repo:README", "pytorch-docs:get-started"],
    ),
    chunk(
        chunk_id="ext-unsloth-finetune-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="Unsloth 微调环境安装和 GPU 兼容检查",
        question_patterns=[
            "unsloth 怎么安装",
            "unsloth 微调显存不够",
            "unsloth 和 qlora 怎么用",
            "unsloth torch backend auto 是什么",
            "unsloth 在 windows wsl 怎么装",
        ],
        content=(
            "适用场景:客户使用 Unsloth 做更省显存/更快的 LoRA、QLoRA 或本地模型训练。"
            "优先按官方仓库当前安装方式创建隔离环境,不要在旧 PyTorch 环境里混装。\n\n"
            "官方 README 当前给出的 Linux/WSL 安装骨架:\n"
            "```\ncurl -LsSf https://astral.sh/uv/install.sh | sh\nuv venv unsloth_env --python 3.13\nsource unsloth_env/bin/activate\nuv pip install unsloth --torch-backend=auto\n```\n"
            "安装后先确认 `torch.cuda.is_available()` 和 `nvidia-smi`;如果仍 OOM,先降 batch、max sequence length,再考虑 4bit、梯度累积或换更小模型。\n\n"
            "注意:Unsloth 对 PyTorch、CUDA、Python 和 GPU 架构较敏感;Windows 通常建议 WSL 或按官方 Windows 指南处理。"
        ),
        source_refs=["unsloth-repo:README", "pytorch-docs:get-started"],
    ),
    chunk(
        chunk_id="ext-deepspeed-zero-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="DeepSpeed ZeRO 显存优化和 Accelerate 启动",
        question_patterns=[
            "deepspeed zero2 zero3 怎么选",
            "deepspeed 显存不够怎么配置",
            "accelerate deepspeed 怎么启动",
            "zero3_save_16bit_model 是什么",
            "deepspeed offload cpu nvme 怎么用",
        ],
        content=(
            "适用场景:LoRA 或全参训练单卡/多卡显存不够,需要用 DeepSpeed ZeRO 切分优化器、梯度和参数状态。"
            "Accelerate 可以生成 DeepSpeed 配置并统一启动。\n\n"
            "快速流程:\n"
            "```\naccelerate config   # 选择 DeepSpeed、ZeRO stage、混合精度、offload 等\naccelerate launch train.py --args_to_script\n```\n"
            "也可显式指定常见参数:\n"
            "```\naccelerate launch --mixed_precision=fp16 --zero_stage=3 \\\n  --offload_param_device=cpu --offload_optimizer_device=cpu train.py\n```\n"
            "ZeRO-2 通常先切优化器/梯度状态,ZeRO-3 会进一步切模型参数;CPU/NVMe offload 可继续省显存,但会增加 CPU 内存、磁盘和通信压力。\n\n"
            "注意:ZeRO-3 保存完整权重可能需要额外聚合,`zero3_save_16bit_model` 会更慢且更吃显存/内存,只在确实需要导出完整模型时开启。"
        ),
        source_refs=["hf-accelerate:deepspeed", "deepspeed-docs:config-json"],
    ),
    chunk(
        chunk_id="ext-git-lfs-model-download-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="git-lfs 下载模型仓库大文件和指针文件排查",
        question_patterns=[
            "git clone 下来的模型只有指针文件",
            "git lfs pull 怎么用",
            "模型仓库大文件没有下载下来",
            "git-lfs 怎么安装",
            "safetensors 文件很小是怎么回事",
        ],
        content=(
            "适用场景:客户用 `git clone` 下载模型/数据仓库后,发现 `.safetensors`、`.bin` 等大文件只有几百字节,"
            "内容像 LFS pointer。先确认安装并启用 Git LFS。\n\n"
            "常用命令:\n"
            "```\ngit lfs install\ngit clone <repo-url>\ncd <repo>\ngit lfs pull\n```\n"
            "如果只想在克隆后手动拉大文件,可用 `GIT_LFS_SKIP_SMUDGE=1 git clone <repo-url>`,然后再 `git lfs pull`。"
            "拉取失败时检查仓库权限、网络代理、磁盘空间和是否需要登录令牌。\n\n"
            "注意:Git LFS 管的是仓库里的大文件对象;它不是通用断点续传下载器。下载超大模型时,HF CLI 或 aria2 可能更适合。"
        ),
        source_refs=["git-lfs:site", "git-lfs:pull", "hf-docs:security-tokens"],
    ),
    chunk(
        chunk_id="ext-aria2-parallel-download-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="aria2c 并发下载、断点续传和限速",
        question_patterns=[
            "aria2 怎么多线程下载",
            "aria2c 断点续传怎么用",
            "大文件下载太慢 aria2 怎么加速",
            "aria2 下载模型怎么指定文件名",
            "aria2 怎么限制连接数",
        ],
        content=(
            "适用场景:客户下载公开直链模型、数据集或压缩包,希望并发连接、断点续传和更稳定的日志。"
            "aria2c 适合 HTTP/HTTPS/FTP 等直链,不负责登录授权逻辑。\n\n"
            "常用命令:\n"
            "```\naria2c -c -x 8 -s 8 -k 1M -o model.safetensors \"<URL>\"\n```\n"
            "`-c` 尝试继续未完成下载;`-x` 限制单服务器最大连接数;`-s` 指定分片连接数;`-o` 指定文件名。"
            "如果服务器不支持 Range 或限制并发,分片和续传可能无效或被限速。\n\n"
            "注意:受限模型不要把 token 直接写进命令历史;优先使用官方 CLI 登录或短期凭证。"
        ),
        source_refs=["aria2-docs:manual", "hf-docs:security-tokens"],
    ),
    chunk(
        chunk_id="ext-wget-curl-resume-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="wget/curl 下载中断后的断点续传",
        question_patterns=[
            "wget 下载中断怎么继续",
            "curl 断点续传怎么写",
            "下载大文件断了不想重下",
            "wget -c 和 curl -C 是什么",
            "命令行下载模型怎么续传",
        ],
        content=(
            "适用场景:客户用直链下载大文件,网络中断后希望接着下。wget 和 curl 都支持从已有本地文件继续,"
            "但前提是服务端支持断点续传。\n\n"
            "wget:\n"
            "```\nwget -c \"<URL>\" -O model.bin\n```\n"
            "curl:\n"
            "```\ncurl -L -C - -o model.bin \"<URL>\"\n```\n"
            "`wget -c` 会根据本地已有文件长度继续;`curl -C -` 表示自动从已有文件位置续传。"
            "如果目标文件已损坏或服务端不支持 Range,建议删掉不完整文件重新下载并校验哈希。\n\n"
            "注意:带认证的 URL 可能过期;不要把私密 token 写进共享脚本或聊天记录。"
        ),
        source_refs=["wget-docs:manual", "curl-docs:manpage"],
    ),
    chunk(
        chunk_id="ext-rclone-sync-copy-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="rclone 在对象存储/网盘和实例之间同步数据",
        question_patterns=[
            "rclone 怎么传数据集",
            "rclone copy 和 sync 区别",
            "对象存储数据同步到实例",
            "rclone 大量小文件怎么传",
            "rclone 配置 remote 怎么用",
        ],
        content=(
            "适用场景:客户需要在对象存储、网盘、NAS 或另一台机器之间迁移数据集/模型目录。"
            "先用 `rclone config` 配好 remote,再选择只复制还是镜像同步。\n\n"
            "常用命令:\n"
            "```\nrclone copy remote:dataset /data/dataset --progress\nrclone copy /data/output remote:output --progress\n```\n"
            "`copy` 会复制新增/变化内容,通常不会删除目标端多余文件;如果需要严格镜像才考虑 `sync`,并先用 `--dry-run` 检查。\n"
            "大量小文件可结合 `--transfers`、`--checkers` 调整并发;目标目录很大且只少量更新时可按官方建议考虑 `--no-traverse`。\n\n"
            "注意:`sync` 有删除目标端文件的风险;客户不明确时优先建议 `copy`。"
        ),
        source_refs=["rclone-docs:copy"],
    ),
    chunk(
        chunk_id="ext-vscode-remote-server-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="VS Code Remote SSH 连接、端口转发和 Server 异常排查",
        question_patterns=[
            "vscode remote ssh 连不上",
            "vs code remote server 怎么清理",
            "vscode 怎么转发远程端口",
            "remote ssh 下载 server 很慢",
            "cursor vscode 远程开发卡住",
        ],
        content=(
            "适用场景:客户用 VS Code Remote / Cursor 连接 GPU 实例做开发,出现连接失败、Server 卡住或本地访问远程端口。"
            "先确认普通 `ssh` 能登录,再排查 VS Code Server 和端口转发。\n\n"
            "端口转发可在 VS Code 的 Ports 视图添加,也可写入 SSH config:\n"
            "```\nHost gpu-dev\n    HostName <实例地址>\n    User <用户>\n    LocalForward 127.0.0.1:8888 127.0.0.1:8888\n```\n"
            "如果 Remote Server 状态异常,可在命令面板执行 Remote-SSH 的 Kill VS Code Server,或登录实例检查 `~/.vscode-server`/`~/.cursor-server`。"
            "下载 Server 很慢时,先确认网络、代理和磁盘空间。\n\n"
            "注意:端口转发只绑定本地访问,比直接把 Jupyter/服务暴露公网更安全。"
        ),
        source_refs=["vscode-docs:remote-ssh", "openssh-docs:ssh_config"],
    ),
    chunk(
        chunk_id="ext-jupyter-server-remote-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="Jupyter Server 在远程 GPU 实例上的安全访问",
        question_patterns=[
            "jupyter 外部打不开",
            "jupyter server 怎么设置密码",
            "jupyter 远程访问 token 是什么",
            "jupyter notebook 监听 0.0.0.0 安全吗",
            "jupyter 端口 8888 怎么访问",
        ],
        content=(
            "适用场景:客户在实例里启动 Jupyter 做开发,本机能访问但外部打不开,或想长期访问。"
            "优先建议通过 SSH 本地端口转发访问,避免把未加固的 Jupyter 直接暴露公网。\n\n"
            "实例上启动:\n"
            "```\njupyter server --ip=127.0.0.1 --port=8888 --no-browser\n```\n"
            "本地转发:\n"
            "```\nssh -L 8888:127.0.0.1:8888 <用户>@<实例地址>\n```\n"
            "需要公网访问时,至少设置密码并配合 HTTPS/反向代理/访问控制:\n"
            "```\njupyter server password\n```\n"
            "然后确认安全组或端口映射只放行必要来源。\n\n"
            "注意:token、密码和 URL 都是敏感信息,不要贴到聊天记录或工单里。"
        ),
        source_refs=["jupyter-server-docs:public-server", "openssh-docs:ssh"],
    ),
    chunk(
        chunk_id="ext-ssh-tunnel-reverse-proxy-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="SSH 端口转发、反向转发和反向代理怎么选",
        question_patterns=[
            "ssh -L 和 -R 有什么区别",
            "端口转发和反向代理怎么选",
            "本地访问远程 jupyter 怎么转发",
            "远程服务不想直接暴露公网怎么办",
            "nginx 反向代理 gpu 实例服务",
        ],
        content=(
            "适用场景:客户在实例里跑 Jupyter、Gradio、FastAPI 或模型服务,需要从本地或公网访问。"
            "先按用途选择方式:\n\n"
            "本地访问远程端口,用本地转发:\n"
            "```\nssh -L 7860:127.0.0.1:7860 <用户>@<实例地址>\n```\n"
            "需要让远端某端口转回本地服务,用反向转发:\n"
            "```\nssh -R 9000:127.0.0.1:9000 <用户>@<跳板机或实例>\n```\n"
            "需要长期公网服务,再考虑 Nginx/Caddy 等反向代理,并配置鉴权、HTTPS、来源限制和日志。\n\n"
            "注意:SSH 转发适合开发和临时访问;反向代理适合长期服务。不要把无鉴权的开发服务直接开放到公网。"
        ),
        source_refs=["openssh-docs:ssh", "vscode-docs:remote-ssh"],
    ),
    chunk(
        chunk_id="ext-nccl-advanced-debug-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="NCCL 多卡/多机卡住的进阶排查",
        question_patterns=[
            "nccl 多卡训练卡住",
            "nccl_socket_ifname 怎么设置",
            "nccl_ib_disable 什么时候用",
            "nccl debug info 怎么开",
            "多机训练 all reduce 卡死",
        ],
        content=(
            "适用场景:PyTorch/DeepSpeed/Accelerate 多卡或多机训练卡住,NCCL 报错或 all-reduce 不返回。"
            "先确认每个 rank 能看到对应 GPU,再看 NCCL 网络选择和通信日志。\n\n"
            "临时打开日志:\n"
            "```\nexport NCCL_DEBUG=INFO\nexport TORCH_DISTRIBUTED_DEBUG=DETAIL\n```\n"
            "多网卡机器上限制 NCCL 使用的网卡:\n"
            "```\nexport NCCL_SOCKET_IFNAME=eth0\n```\n"
            "如果 IB/RoCE 配置不完整或疑似 RDMA 问题,可临时禁用验证是否回退到 TCP 后恢复:\n"
            "```\nexport NCCL_IB_DISABLE=1\n```\n"
            "还要检查防火墙、端口、主机名解析、每个 rank 的数据 batch 数是否一致。\n\n"
            "注意:这些环境变量用于定位问题,不要长期固化为默认配置;禁用 IB 会影响性能。"
        ),
        source_refs=["nvidia-docs:nccl-env", "pytorch-docs:distributed"],
    ),
    chunk(
        chunk_id="ext-cuda-driver-compatibility-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="CUDA Toolkit、驱动和运行时兼容关系",
        question_patterns=[
            "cuda 版本和驱动版本不匹配",
            "nvidia-smi 显示的 cuda 和 torch cuda 不一样",
            "cuda toolkit 要不要安装",
            "驱动太旧能不能跑新 cuda",
            "cuda forward compatibility 是什么",
        ],
        content=(
            "适用场景:客户看到 `nvidia-smi`、`nvcc --version`、PyTorch CUDA 版本不一致,不知道是否正常。"
            "先区分三件事:驱动支持的最高 CUDA 能力、系统安装的 CUDA Toolkit、框架 wheel 自带/依赖的 CUDA runtime。\n\n"
            "检查命令:\n"
            "```\nnvidia-smi\nnvcc --version || true\npython - <<'PY'\nimport torch\nprint(torch.__version__, torch.version.cuda, torch.cuda.is_available())\nPY\n```\n"
            "NVIDIA CUDA Compatibility 文档说明,同一 CUDA 大版本内存在 minor version compatibility;跨大版本或新 toolkit 配旧驱动时,"
            "需要按文档确认是否支持 forward compatibility 包。\n\n"
            "注意:`nvidia-smi` 里的 CUDA Version 不是当前 Python 环境的 torch CUDA 版本;不要只凭一个数字判断环境错。"
        ),
        source_refs=["nvidia-docs:cuda-compatibility", "pytorch-docs:get-started", "nvidia-docs:nvidia-smi"],
    ),
    chunk(
        chunk_id="ext-framework-cuda-match-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="PyTorch/Transformers/训练框架与 CUDA wheel 匹配",
        question_patterns=[
            "torch cuda is available false",
            "pytorch 装成 cpu 版了怎么办",
            "transformers 训练为什么不用 gpu",
            "cuda 12.8 torch 怎么装",
            "框架和 cuda 版本怎么匹配",
        ],
        content=(
            "适用场景:训练框架安装成功但检测不到 GPU,或客户按旧教程装错了 CPU 版 PyTorch。"
            "先以 PyTorch 官方安装选择器生成当前系统对应命令,不要照搬旧 CUDA 版本命令。\n\n"
            "检查和重装示例:\n"
            "```\npython -c \"import torch; print(torch.__version__, torch.version.cuda, torch.cuda.is_available())\"\npip uninstall -y torch torchvision torchaudio\n# 到 PyTorch 官网选择 Linux/Windows + pip/conda + CUDA 版本后使用生成命令\n```\n"
            "如果使用 Transformers、Accelerate、LLaMA-Factory 或 Unsloth,底层仍依赖 torch 的 GPU 可用性。"
            "容器内还要先确认 `docker run --gpus all ... nvidia-smi` 成功。\n\n"
            "注意:系统 CUDA Toolkit 不是必须和 torch wheel 完全同名,但 NVIDIA 驱动必须满足 wheel/runtime 的最低要求。"
        ),
        source_refs=["pytorch-docs:get-started", "hf-transformers:trainer", "nvidia-docs:container-toolkit-sample"],
    ),
    chunk(
        chunk_id="ext-comfyui-manager-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="ComfyUI Manager 安装/更新自定义节点",
        question_patterns=[
            "comfyui manager 怎么安装",
            "comfyui manager 不显示",
            "comfyui 自定义节点怎么更新",
            "install custom nodes 找不到",
            "comfyui manager 安装节点失败",
        ],
        content=(
            "适用场景:客户用 ComfyUI Manager 安装、更新、禁用或启用自定义节点,但 Manager 不显示或节点安装失败。"
            "当前 ComfyUI 文档建议优先用 Manager;手动安装时仍要保证目录结构正确。\n\n"
            "手动安装节点的通用流程:\n"
            "```\ncd /path/to/ComfyUI/custom_nodes\ngit clone <node-repo-url>\ncd <node-dir>\npip install -r requirements.txt\n```\n"
            "ComfyUI Manager 自身目录应位于 `ComfyUI/custom_nodes/comfyui-manager`,不要解压成双层目录或把文件直接散落到 custom_nodes。"
            "安装/更新后重启 ComfyUI,查看启动日志里的 `import failed`。\n\n"
            "注意:自定义节点是第三方代码,只安装可信来源;依赖要装进 ComfyUI 使用的同一个 Python 环境。"
        ),
        source_refs=["comfyui-docs:custom-nodes", "comfyui-manager-repo:README"],
    ),
    chunk(
        chunk_id="ext-comfyui-flux-models-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="ComfyUI Flux 模型缺失、放错目录和低显存处理",
        question_patterns=[
            "comfyui flux 缺模型",
            "flux t5xxl clip_l ae 放哪里",
            "flux 低显存怎么跑",
            "load diffusion model 找不到 flux",
            "flux workflow nodes missing",
        ],
        content=(
            "适用场景:客户加载 Flux 工作流时报缺模型、节点缺失或显存不足。先确认 ComfyUI 版本和模型文件目录。"
            "Flux 工作流常同时需要 diffusion model、text encoder 和 VAE。\n\n"
            "常见目录:\n"
            "```\nComfyUI/models/diffusion_models/<flux模型>.safetensors\nComfyUI/models/text_encoders/clip_l.safetensors\nComfyUI/models/text_encoders/t5xxl_fp16.safetensors  # 或低显存 fp8 版本\nComfyUI/models/vae/ae.safetensors\n```\n"
            "官方 Flux Krea 文档建议低显存优先用 fp8 版本;若 workflow 中核心节点缺失,先更新 ComfyUI 或等待稳定版包含该节点。"
            "仍 OOM 时再用低显存模型、降低分辨率/批量或启动参数。\n\n"
            "注意:部分 Flux 模型需要先在模型站点同意协议;未授权会表现为下载失败或文件不完整。"
        ),
        source_refs=["comfyui-docs:flux-krea", "comfyui-docs:flux-controlnet"],
    ),
    chunk(
        chunk_id="ext-comfyui-controlnet-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="ComfyUI ControlNet / controlnet_aux 模型和预处理器错误",
        question_patterns=[
            "comfyui controlnet 找不到模型",
            "controlnet aux 节点缺失",
            "depth controlnet 怎么放模型",
            "canny depth 预处理器安装失败",
            "flux controlnet 需要什么节点",
        ],
        content=(
            "适用场景:ControlNet 工作流缺少 ControlNet 模型、预处理节点或输入图处理失败。"
            "先分清两类文件:ControlNet 权重放模型目录,预处理器来自自定义节点包。\n\n"
            "SD1.5 Depth ControlNet 示例目录:\n"
            "```\nComfyUI/models/checkpoints/<基础模型>.safetensors\nComfyUI/models/controlnet/<controlnet模型>.safetensors\n```\n"
            "安装 ControlNet 辅助预处理器:\n"
            "```\ncd ComfyUI/custom_nodes\ngit clone https://github.com/Fannovel16/comfyui_controlnet_aux/\ncd comfyui_controlnet_aux\npip install -r requirements.txt\n```\n"
            "Flux ControlNet 还可能需要 Advanced-ControlNet、controlnet_aux 等预处理节点,并按工作流要求放 text encoder、VAE、diffusion/controlnet 权重。\n\n"
            "注意:模型放错目录和节点依赖装错 Python 环境是最常见原因;重启后看启动日志比只看前端报错更可靠。"
        ),
        source_refs=["comfyui-docs:depth-controlnet", "comfyui-docs:flux-controlnet", "comfyui-controlnet-aux:README"],
    ),
    chunk(
        chunk_id="ext-comfyui-ipadapter-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_community",
        title="ComfyUI IPAdapter / Flux IPAdapter 模型路径和节点版本",
        question_patterns=[
            "comfyui ipadapter 模型放哪里",
            "ipadapter unified loader 找不到模型",
            "flux ipadapter 怎么用",
            "clip vision 模型缺失",
            "ipadapter faceid lora 缺失",
        ],
        content=(
            "适用场景:IPAdapter 工作流报找不到 IPAdapter、CLIP Vision 或 FaceID LoRA,或 Flux IPAdapter 节点不可用。"
            "先确认节点版本、ComfyUI 版本和模型命名都匹配。\n\n"
            "常见路径:\n"
            "```\nComfyUI/models/clip_vision/<clip_vision模型>\nComfyUI/models/ipadapter/<ipadapter模型>\nComfyUI/models/loras/<FaceID对应LoRA>\n```\n"
            "Flux IPAdapter 的 x-flux-comfyui 项目还要求把 Flux IPAdapter 放到 `ComfyUI/models/xlabs/ipadapters/`,"
            "并使用 `Flux Load IPAdapter` / `Apply Flux IPAdapter` 节点。"
            "如果 Manager 自动更新失败,按项目 README 手动 `git pull` 或重新安装节点。\n\n"
            "注意:IPAdapter_plus 仓库已处于维护模式,新模型或新 ComfyUI 版本可能需要查看项目最新 README/issue;不要混用不同体系的模型路径。"
        ),
        source_refs=["comfyui-ipadapter-plus:README", "x-flux-comfyui:README", "comfyui-docs:custom-nodes"],
    ),
]

PRODUCTION_GPU_SUPPORT_CHUNKS = [
    chunk(
        chunk_id="ext-dcgm-exporter-prometheus-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="用 DCGM Exporter 持续监控 GPU 指标",
        question_patterns=[
            "怎么长期监控 gpu",
            "gpu 指标怎么接 prometheus",
            "dcgm exporter 怎么用",
            "想看每张卡的显存温度功耗",
            "grafana 怎么看 gpu 利用率",
        ],
        content=(
            "适用场景:不只是临时看 `nvidia-smi`,而是要把 GPU 温度、显存、功耗、SM 利用率等指标接入 Prometheus/Grafana。"
            "NVIDIA DCGM Exporter 会暴露 `/metrics` HTTP 端点,常见端口是 `9400`。\n\n"
            "单机容器快速验证:\n"
            "```\ndocker run -d --rm --gpus all --net host --cap-add SYS_ADMIN \\\n"
            "  nvcr.io/nvidia/k8s/dcgm-exporter:<版本>-ubuntu<版本>\n"
            "curl localhost:9400/metrics | head\n```\n"
            "Kubernetes 里通常以 DaemonSet 部署到 GPU 节点,再由 Prometheus 抓取。"
            "如果主机已经运行 `nv-hostengine`,要注意 DCGM Exporter 和主机 DCGM 版本兼容,必要时让 exporter 连接已有 hostengine。\n\n"
            "注意:监控能说明趋势和异常,不能直接替代一次性的故障诊断;XID、温度、功耗、显存和进程信息要结合任务日志一起看。"
        ),
        source_refs=["nvidia-dcgm:exporter"],
    ),
    chunk(
        chunk_id="ext-dcgm-diagnostics-run-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="用 DCGM Diagnostics 做 GPU 节点就绪和故障检查",
        question_patterns=[
            "gpu 节点上线前怎么验收",
            "dcgmi diag 怎么跑",
            "怎么检查 gpu 硬件是不是有问题",
            "训练失败后想测显卡健康",
            "dcgm diagnostics level 选哪个",
        ],
        content=(
            "适用场景:怀疑 GPU、驱动、NVML/CUDA 访问、PCIe/NVLink、显存或温度功耗存在问题,需要在任务前后做节点就绪检查。"
            "DCGM Diagnostics 通过 `dcgmi diag` 提供不同耗时等级的检查。\n\n"
            "常用顺序:\n"
            "```\n# 快速系统验证\nsudo dcgmi diag -r 1\n"
            "# 更完整的扩展验证\nsudo dcgmi diag -r 2\n"
            "# 只跑指定项目,例如 PCIe 和基础诊断\nsudo dcgmi diag -r pcie,diagnostic\n```\n"
            "如果需要机器可读输出,可加 JSON 输出参数;如果任务刚失败,优先保存诊断日志、`nvidia-smi -q` 和训练日志。\n\n"
            "注意:DCGM Diagnostics 用来减少失败作业和定位问题,不是 RMA 或硬件维修流程本身;长测试会占用 GPU,不要和生产任务同时跑。"
        ),
        source_refs=["nvidia-dcgm:diagnostics"],
    ),
    chunk(
        chunk_id="ext-nsight-systems-profile-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="用 Nsight Systems 定位训练/推理慢在哪里",
        question_patterns=[
            "gpu 利用率低但是不知道卡在哪里",
            "nsys profile pytorch 怎么用",
            "训练慢想看 cpu gpu 时间线",
            "cuda kernel 和 dataloader 谁慢",
            "多卡任务怎么只 profile 一个 rank",
        ],
        content=(
            "适用场景:GPU 利用率低、吞吐不稳定、CPU 数据准备和 CUDA kernel 之间有空洞,需要看端到端时间线。"
            "Nsight Systems 更适合看系统级时间线,再决定是否进入 Nsight Compute 看单个 kernel。\n\n"
            "Python/CUDA 任务示例:\n"
            "```\nnsys profile --trace=cuda,cudnn,cublas,osrt,nvtx \\\n"
            "  --python-sampling=true -o run_profile python train.py\n```\n"
            "PyTorch 任务可打开函数跟踪:\n"
            "```\nnsys profile --trace=cuda,cudnn,cublas,osrt,nvtx \\\n"
            "  --pytorch=functions-trace-shapes,autograd-nvtx -o torch_profile python train.py\n```\n"
            "多进程任务不要让所有 rank 写同一个报告文件;可用 rank 或进程号放进输出名,或只 profile 本机 rank 0。\n\n"
            "注意:profile 本身会增加开销。先采短时间窗口,确认瓶颈后再扩大范围。"
        ),
        source_refs=["nvidia-nsight:systems-user-guide"],
    ),
    chunk(
        chunk_id="ext-nsight-compute-kernel-profile-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="用 Nsight Compute 深入分析单个 CUDA kernel",
        question_patterns=[
            "nsight compute 和 nsight systems 区别",
            "ncu 怎么分析 kernel",
            "cuda kernel 慢看哪些指标",
            "occupancy 低怎么定位",
            "memory workload analysis 是什么",
        ],
        content=(
            "适用场景:已经知道慢在某个 CUDA kernel,需要看 SM、内存、占用率、warp stall、NVLink 等细节。"
            "Nsight Compute 面向 kernel 级分析,比 Nsight Systems 更细,但开销更高。\n\n"
            "常用方式:\n"
            "```\nncu --set basic -o kernel_basic python script.py\n"
            "ncu --set full -o kernel_full python script.py\n"
            "ncu --list-sets\n```\n"
            "`basic` 更快,适合初筛;`full` 信息更多,适合已经能稳定复现的短任务。"
            "优先关注高层指标:SM 利用率、内存吞吐、occupancy、warp stall reason,再决定是否改 batch、kernel、数据布局或算子实现。\n\n"
            "注意:很多指标需要 replay,任务必须尽量可复现;长训练不建议全程用 `full`。"
        ),
        source_refs=["nvidia-nsight:compute-profiling"],
    ),
    chunk(
        chunk_id="ext-gpu-telemetry-mig-metrics-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="MIG 场景下监控 GPU 实例指标",
        question_patterns=[
            "mig 模式怎么监控每个实例",
            "dcgm exporter 看不到 mig 指标",
            "mig gpu 利用率怎么看",
            "gpu instance 指标怎么区分",
            "prometheus 怎么区分 mig 分片",
        ],
        content=(
            "适用场景:A100/H100 等开启 MIG 后,一张物理卡被拆成多个 GPU Instance,希望分别看每个实例的显存和利用率。"
            "DCGM Exporter 支持 MIG 指标,需要使用支持 MIG 的 DCGM/DCGM Exporter 版本,并按 GPU instance 维度采集。\n\n"
            "排查顺序:\n"
            "1. `nvidia-smi -L` 确认主机能看到 MIG instance。\n"
            "2. 检查 exporter 版本和主机 DCGM 版本是否兼容。\n"
            "3. 查看 `/metrics` 中是否带有 GPU instance/profile 相关标签。\n"
            "4. Grafana 面板按 instance/profile 维度拆分,不要只按物理 GPU 聚合。\n\n"
            "注意:MIG 的调度、隔离和监控都应按实例理解;物理 GPU 汇总指标可能掩盖单个分片的热点。"
        ),
        source_refs=["nvidia-dcgm:exporter", "nvidia-docs:mig-user-guide"],
    ),
    chunk(
        chunk_id="ext-docker-run-gpu-access-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="Docker 容器访问 GPU 的最小验证",
        question_patterns=[
            "docker 容器里怎么用 gpu",
            "docker run --gpus all 怎么验证",
            "容器里 nvidia-smi 找不到",
            "nvidia container toolkit 装好怎么测试",
            "docker 只给容器一张卡",
        ],
        content=(
            "适用场景:主机 `nvidia-smi` 正常,但容器内看不到 GPU 或不确定 Docker GPU 运行时是否可用。"
            "Docker 官方方式是在启动容器时加 `--gpus`。\n\n"
            "最小验证:\n"
            "```\ndocker run --rm --gpus all ubuntu nvidia-smi\n"
            "docker run --rm --gpus 'device=0' ubuntu nvidia-smi\n```\n"
            "如果容器需要 `nvidia-smi`,通常还要有对应 driver capability;基础 CUDA 镜像更适合测试。"
            "如果主机正常但容器不正常,优先检查 NVIDIA Container Toolkit、Docker daemon runtime 配置、镜像 CUDA 版本和权限。\n\n"
            "注意:`--gpus all` 只是把 GPU 设备暴露进容器,不保证 Python 框架版本已经装对。"
        ),
        source_refs=["docker-docs:engine-gpu", "nvidia-docs:container-toolkit-sample"],
    ),
    chunk(
        chunk_id="ext-docker-compose-gpu-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="Docker Compose 服务声明 GPU 设备",
        question_patterns=[
            "docker compose 怎么用 gpu",
            "compose 里 --gpus all 怎么写",
            "docker compose nvidia-smi 不工作",
            "compose 只分配一张 gpu",
            "capabilities gpu 必须写吗",
        ],
        content=(
            "适用场景:单条 `docker run --gpus all` 可以用 GPU,但换成 Docker Compose 后容器看不到 GPU。"
            "Compose 需要在 service 的 device reservation 里声明 NVIDIA driver、数量或设备号,并设置 `capabilities: [gpu]`。\n\n"
            "示例:\n"
            "```\nservices:\n  test:\n    image: nvidia/cuda:12.9.0-base-ubuntu22.04\n    command: nvidia-smi\n    deploy:\n      resources:\n        reservations:\n          devices:\n            - driver: nvidia\n              count: 1\n              capabilities: [gpu]\n```\n"
            "如果要指定设备,用 `device_ids: ['0', '3']`。不要同时混用 `count` 和 `device_ids`。\n\n"
            "注意:Compose 的 GPU 声明和应用内 `CUDA_VISIBLE_DEVICES` 是两层控制;先确认容器能看到卡,再看框架是否使用。"
        ),
        source_refs=["docker-docs:compose-gpu"],
    ),
    chunk(
        chunk_id="ext-k8s-gpu-resource-request-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="Kubernetes Pod 申请 NVIDIA GPU 资源",
        question_patterns=[
            "k8s pod 怎么申请 gpu",
            "nvidia.com/gpu 怎么写",
            "pod 里看不到 gpu",
            "kubernetes gpu limit request 怎么配",
            "gpu pod 一直 pending",
        ],
        content=(
            "适用场景:在 Kubernetes 集群里运行训练或推理 Pod,需要让调度器把 Pod 放到有 GPU 的节点。"
            "Kubernetes 通过设备插件把 GPU 暴露为扩展资源,常见名称是 `nvidia.com/gpu`。\n\n"
            "Pod 资源示例:\n"
            "```\nresources:\n  limits:\n    nvidia.com/gpu: 1\n```\n"
            "GPU 扩展资源通常写在 `limits` 中;调度前要确认节点已经安装驱动和 NVIDIA device plugin,并且 `kubectl describe node` 能看到 `nvidia.com/gpu` capacity/allocatable。\n\n"
            "注意:Pod Pending 常见原因不是镜像问题,而是节点没有可分配 GPU、插件没启动、节点 taint/selector 不匹配或资源名写错。"
        ),
        source_refs=["kubernetes-docs:schedule-gpus"],
    ),
    chunk(
        chunk_id="ext-gpu-operator-stack-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="NVIDIA GPU Operator 管理 Kubernetes GPU 软件栈",
        question_patterns=[
            "gpu operator 是干什么的",
            "k8s gpu 节点要装哪些组件",
            "gpu operator 包含 device plugin 吗",
            "kubernetes gpu 驱动怎么统一管理",
            "dcgm 监控和 gpu operator 关系",
        ],
        content=(
            "适用场景:要在 Kubernetes 集群里统一管理 NVIDIA GPU 节点,不想手工逐台安装驱动、容器运行时、device plugin 和监控组件。"
            "NVIDIA GPU Operator 用 operator 方式自动管理 GPU 相关组件。\n\n"
            "它通常涉及这些组件:驱动、Kubernetes device plugin、NVIDIA Container Toolkit、节点标签发现、DCGM 监控等。"
            "如果云厂商或镜像已经预装驱动,可按文档选择不由 Operator 安装 driver。"
            "检查时看 `gpu-operator` 命名空间下各组件 Pod 状态,再看节点是否暴露 `nvidia.com/gpu`。\n\n"
            "注意:GPU Operator 解决的是集群软件栈管理,不等同于自动解决模型 OOM、NCCL 网络或应用版本兼容问题。"
        ),
        source_refs=["nvidia-docs:gpu-operator", "kubernetes-docs:schedule-gpus"],
    ),
    chunk(
        chunk_id="ext-k8s-gpu-pod-pending-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="Kubernetes GPU Pod Pending 的排查顺序",
        question_patterns=[
            "gpu pod pending 怎么查",
            "0/1 nodes available insufficient nvidia.com/gpu",
            "kubectl describe node 没有 nvidia.com/gpu",
            "device plugin 正常但 pod 调度不上",
            "k8s gpu 资源为 0",
        ],
        content=(
            "适用场景:GPU Pod 一直 Pending,事件里出现 `Insufficient nvidia.com/gpu` 或节点没有 GPU 扩展资源。"
            "先看调度事件,再看节点资源,最后看 device plugin/driver。\n\n"
            "排查命令:\n"
            "```\nkubectl describe pod <pod>\n"
            "kubectl describe node <gpu-node> | grep -A5 -E 'Capacity|Allocatable|nvidia.com/gpu'\n"
            "kubectl get pods -A | grep -i nvidia\n```\n"
            "若节点没有 `nvidia.com/gpu`,检查驱动、device plugin、GPU Operator 组件。"
            "若有资源但仍 Pending,检查 GPU 数量是否被其它 Pod 占用、nodeSelector/taint/toleration 是否匹配。\n\n"
            "注意:Pod 内镜像是否装 CUDA 是运行阶段问题;Pending 阶段通常还没启动容器。"
        ),
        source_refs=["kubernetes-docs:schedule-gpus", "nvidia-docs:gpu-operator"],
    ),
    chunk(
        chunk_id="ext-mig-partition-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="MIG 把一张数据中心 GPU 切成多个隔离实例",
        question_patterns=[
            "mig 是什么",
            "一张 a100 能不能切给多个任务",
            "multi instance gpu 怎么用",
            "mig 和 time slicing 区别",
            "gpu 分片后显存怎么隔离",
        ],
        content=(
            "适用场景:希望把支持 MIG 的 NVIDIA 数据中心 GPU 切成多个独立 GPU Instance,让多个小任务共享一张大卡但互相隔离。"
            "MIG 支持的硬件和驱动版本有要求,并不是所有 GPU 都支持。\n\n"
            "基本理解:\n"
            "1. MIG 是硬件级分区,每个实例有固定显存和计算资源。\n"
            "2. `nvidia-smi -L` 可以查看 GPU Instance / Compute Instance。\n"
            "3. 容器和 Kubernetes 需要使用支持 MIG 的 NVIDIA Container Toolkit / device plugin。\n"
            "4. profile 大小要按任务显存需求选择,不是越小越好。\n\n"
            "注意:MIG 更偏隔离和资源切分;time-slicing 更偏超分共享,隔离能力不同。生产多租户场景不要把两者混为一谈。"
        ),
        source_refs=["nvidia-docs:mig-user-guide"],
    ),
    chunk(
        chunk_id="ext-k8s-mig-device-plugin-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="Kubernetes 中使用 MIG 资源",
        question_patterns=[
            "k8s 怎么调度 mig",
            "nvidia.com/mig-1g.5gb 怎么来的",
            "pod 申请 mig 资源",
            "mig 在 kubernetes 里看不到",
            "gpu operator mig strategy 怎么选",
        ],
        content=(
            "适用场景:节点已开启 MIG,希望 Kubernetes 能按 MIG profile 暴露资源并让 Pod 申请。"
            "需要驱动、NVIDIA Container Toolkit、device plugin/GPU Operator 都支持 MIG。\n\n"
            "排查顺序:\n"
            "```\nnvidia-smi -L\nkubectl describe node <node> | grep -i mig\nkubectl describe node <node> | grep nvidia.com\n```\n"
            "如果节点只暴露整卡 `nvidia.com/gpu`,检查 device plugin 的 MIG 策略和 GPU 是否已经实际切分。"
            "Pod 里资源名要和节点上暴露的 profile 名一致,例如 `nvidia.com/mig-1g.5gb`。\n\n"
            "注意:动态修改 MIG profile 会影响正在运行的任务;生产环境要先排空节点或按平台流程变更。"
        ),
        source_refs=["nvidia-docs:kubernetes-mig", "nvidia-docs:mig-user-guide", "kubernetes-docs:schedule-gpus"],
    ),
    chunk(
        chunk_id="ext-k8s-gpu-time-slicing-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="Kubernetes GPU Time-Slicing 适合轻量共享但不等于显存隔离",
        question_patterns=[
            "k8s gpu time slicing 是什么",
            "一张 gpu 能不能给多个 pod 用",
            "gpu 超分会隔离显存吗",
            "time slicing 和 mps 有什么区别",
            "gpu operator replicas 是什么意思",
        ],
        content=(
            "适用场景:多个轻量推理、Notebook 或开发任务希望共享同一张 GPU。"
            "NVIDIA GPU Operator 可通过 device plugin 的扩展选项配置 time-slicing,让同一物理 GPU 被报告为多个可调度副本。\n\n"
            "使用前要讲清楚限制:time-slicing 是时间片共享,不是硬件显存隔离;多个 Pod 之间仍可能互相影响显存和性能。"
            "如果需要更强隔离,优先看 MIG;如果要显式控制共享资源,再评估 MPS 或平台级隔离方案。\n\n"
            "注意:把 `replicas` 配大只会增加可调度副本数,不会增加真实显存。客户报 OOM 时不能用 time-slicing 当作扩容手段。"
        ),
        source_refs=["nvidia-docs:gpu-sharing", "nvidia-docs:mig-user-guide"],
    ),
    chunk(
        chunk_id="ext-triton-model-repository-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="Triton Inference Server 的模型仓库和请求入口",
        question_patterns=[
            "triton 怎么部署模型",
            "triton model repository 是什么",
            "triton 支持 pytorch onnx 吗",
            "triton http grpc 端口怎么理解",
            "多个模型怎么放进 triton",
        ],
        content=(
            "适用场景:客户从单模型脚本转向生产推理服务,需要一个统一服务管理多个模型和后端。"
            "Triton 支持 TensorRT、PyTorch、ONNX、OpenVINO、Python、RAPIDS FIL 等多种后端,请求可通过 HTTP/REST 或 gRPC 进入。\n\n"
            "基本结构是 model repository:每个模型一个目录,下面放版本目录和配置。"
            "上线前先确认模型目录结构、后端类型、输入输出张量名称和 batch 维度。"
            "启动后先用健康检查和 metadata 接口确认模型是否 READY,再做压测。\n\n"
            "注意:Triton 解决的是生产服务框架和多后端管理;LLM 场景若追求生成式吞吐,还要比较 vLLM/SGLang/TensorRT-LLM 后端。"
        ),
        source_refs=["nvidia-triton:overview"],
    ),
    chunk(
        chunk_id="ext-triton-dynamic-batching-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="Triton 动态 batching 和吞吐/延迟取舍",
        question_patterns=[
            "triton 吞吐低怎么优化",
            "triton dynamic batching 是什么",
            "triton 延迟和 batch 怎么平衡",
            "推理请求很多怎么合批",
            "triton 实时推理和批量推理区别",
        ],
        content=(
            "适用场景:模型能跑起来,但 QPS 低或 GPU 利用率不高。"
            "Triton 支持按模型配置调度和 batching,可以把短时间内到达的请求合并成更大的 batch,提高 GPU 利用率。\n\n"
            "排查顺序:\n"
            "1. 先确认模型是否支持 batch 维度。\n"
            "2. 对比单请求延迟、并发吞吐和 GPU 利用率。\n"
            "3. 逐步调 batch、队列等待时间和实例数。\n"
            "4. 用真实输入长度/图片尺寸压测,不要只看空请求。\n\n"
            "注意:dynamic batching 会在吞吐和尾延迟之间取舍;实时低延迟接口不要盲目加大等待时间。"
        ),
        source_refs=["nvidia-triton:overview"],
    ),
    chunk(
        chunk_id="ext-tensorrt-llm-triton-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="TensorRT-LLM 与 Triton 适合做高性能 LLM 推理",
        question_patterns=[
            "tensorrt llm 是什么",
            "triton 怎么跑 tensorrt llm",
            "llm 推理想优化吞吐延迟",
            "genai perf 怎么压测",
            "trt llm backend 怎么用",
        ],
        content=(
            "适用场景:需要在 NVIDIA GPU 上优化 LLM 推理性能,并通过 Triton 管理服务。"
            "TensorRT-LLM 用于构建/优化 LLM 的 TensorRT engine;Triton TensorRT-LLM backend 用于服务这些模型。\n\n"
            "典型流程:\n"
            "1. 使用 TensorRT-LLM 工具准备模型和 engine。\n"
            "2. 把模型放入 Triton 可识别的仓库结构。\n"
            "3. 启动 Triton 容器和 TensorRT-LLM backend。\n"
            "4. 用 GenAI-Perf 或真实客户端测吞吐、首 token 延迟和输出 token 延迟。\n\n"
            "注意:TensorRT-LLM 路线通常比 vLLM/SGLang 更强调构建和调优步骤;客户只想快速起服务时,先评估 vLLM/SGLang 是否够用。"
        ),
        source_refs=["nvidia-triton:tensorrt-llm"],
    ),
    chunk(
        chunk_id="ext-triton-genai-perf-metrics-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="Triton / TensorRT-LLM 用 GenAI-Perf 和 metrics 做压测",
        question_patterns=[
            "triton 怎么压测 llm",
            "genai perf 怎么看吞吐延迟",
            "triton metrics endpoint 怎么用",
            "llm 首 token 延迟怎么测",
            "tensorrt llm 服务怎么评估性能",
        ],
        content=(
            "适用场景:TensorRT-LLM 或 Triton 服务已经启动,需要量化吞吐、延迟和 GPU/请求指标,用于容量评估或上线前验收。"
            "NVIDIA 文档推荐使用 GenAI-Perf 测 LLM 吞吐和延迟,同时结合 Triton metrics 端点看服务侧指标。\n\n"
            "压测建议:\n"
            "1. 使用真实输入长度、输出长度和并发度,不要只测短 prompt。\n"
            "2. 分别记录 TTFT、输出 token 延迟、吞吐和错误率。\n"
            "3. 同时查看 Triton metrics 与 DCGM/GPU 指标,判断瓶颈在模型、服务队列还是 GPU。\n"
            "4. 每次只改一个参数,例如 batch、并发、engine 配置或实例数。\n\n"
            "注意:压测结果与模型、engine、输入长度和硬件强相关;不要把一个模型的结论直接套到另一个模型。"
        ),
        source_refs=["nvidia-triton:tensorrt-llm", "nvidia-triton:overview"],
    ),
    chunk(
        chunk_id="ext-kserve-inferenceservice-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="KServe 用 InferenceService 管理生产模型服务",
        question_patterns=[
            "kserve 是什么",
            "inferenceservice 怎么部署模型",
            "kubernetes 上怎么生产部署模型",
            "模型服务怎么自动伸缩",
            "kserve 支持哪些推理后端",
        ],
        content=(
            "适用场景:客户已经有 Kubernetes,希望用统一 CRD 管理模型服务生命周期、伸缩、灰度和监控。"
            "KServe 通过 InferenceService、ServingRuntime 等资源把模型服务声明化,支持生成式和传统预测模型。\n\n"
            "基本理解:\n"
            "1. Predictor 负责模型预测服务。\n"
            "2. Transformer 可做前后处理。\n"
            "3. ServingRuntime 决定底层运行时,例如 vLLM、Triton、TorchServe 或自定义容器。\n"
            "4. 生产环境还要配置网关、鉴权、日志、监控和资源限制。\n\n"
            "注意:KServe 是 K8s 生产服务层,不是单机启动脚本;排障要同时看 KServe CRD 状态、Pod 事件和底层推理服务日志。"
        ),
        source_refs=["kserve-docs:intro"],
    ),
    chunk(
        chunk_id="ext-kserve-multinode-vllm-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="KServe 多节点/多 GPU vLLM 推理的限制",
        question_patterns=[
            "kserve 多机多卡 vllm 怎么配",
            "kserve multinode inference 需要 pvc 吗",
            "tensorParallelSize pipelineParallelSize 怎么理解",
            "kserve 多节点推理不能自动扩缩容吗",
            "vllm 分布式推理 kserve 注意事项",
        ],
        content=(
            "适用场景:在 KServe 上部署超大 LLM,单节点/单卡放不下,需要多节点或多 GPU 推理。"
            "KServe 的 vLLM 多节点方案需要满足若干限制,不能当作普通自动扩缩容来理解。\n\n"
            "关键点:\n"
            "1. 需要 Kubernetes 集群和已安装 KServe。\n"
            "2. 模型文件通常需要 PVC,并且多节点场景要求 ReadWriteMany 访问能力。\n"
            "3. `workerSpec.tensorParallelSize` 和 `workerSpec.pipelineParallelSize` 要通过 workerSpec 配置。\n"
            "4. 多节点功能只支持 Standard 模式,Autoscaler 需要配置为 none。\n"
            "5. 大模型启动慢时要调大 startup/readiness/liveness probe 等待时间。\n\n"
            "注意:修改 tensor/pipeline parallel size 会影响稳定性;生产环境变更前应新建服务或按变更流程操作。"
        ),
        source_refs=["kserve-docs:multinode-vllm"],
    ),
    chunk(
        chunk_id="ext-tgi-maintenance-migration-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="Hugging Face TGI 已偏维护状态时如何选型",
        question_patterns=[
            "tgi 还建议用吗",
            "text generation inference 和 vllm 怎么选",
            "huggingface tgi 维护状态",
            "tgi 迁移到 vllm",
            "部署 llm 应该用 tgi 还是 sglang",
        ],
        content=(
            "适用场景:客户看到旧教程使用 Hugging Face Text Generation Inference(TGI),想知道是否仍适合作为新项目主推方案。"
            "TGI README 已说明项目进入维护模式,后续更推荐 vLLM、SGLang 等推理引擎路线。\n\n"
            "建议口径:\n"
            "1. 旧系统仍在 TGI 上可继续按现状维护和小修。\n"
            "2. 新建服务优先评估 vLLM/SGLang;需要 NVIDIA 深度优化时再评估 TensorRT-LLM/Triton。\n"
            "3. 迁移时重点对齐 OpenAI 兼容接口、模型格式、并发参数、量化方式和压测指标。\n\n"
            "注意:不要简单说 TGI 不能用;应区分存量维护和新项目选型。"
        ),
        source_refs=["tgi-repo:README"],
    ),
    chunk(
        chunk_id="ext-ray-gpu-tasks-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="Ray 任务/Actor 按 GPU 资源调度",
        question_patterns=[
            "ray 怎么给任务分配 gpu",
            "ray remote num_gpus 怎么用",
            "多个 python 任务怎么抢 gpu",
            "ray actor 只用一张卡",
            "ray 集群 gpu 资源怎么看",
        ],
        content=(
            "适用场景:多个 Python 任务或服务需要按 GPU 数量调度,不想手工维护每个进程的 `CUDA_VISIBLE_DEVICES`。"
            "Ray 可以在 task 或 actor 上声明 `num_gpus`,由 Ray 调度器把它放到有足够 GPU 资源的节点上。\n\n"
            "示例:\n"
            "```\nimport ray\nray.init()\n\n@ray.remote(num_gpus=1)\nclass Worker:\n    def run(self):\n        return \"ok\"\n\nworkers = [Worker.remote() for _ in range(2)]\n```\n"
            "调度前确认 Ray 集群识别到了 GPU 资源;容器/K8s 场景还要先保证底层 GPU 已暴露。\n\n"
            "注意:Ray 的 `num_gpus` 是调度资源声明,不是显存上限;任务内部仍可能因为 batch 太大而 OOM。"
        ),
        source_refs=["ray-docs:gpu-resources"],
    ),
    chunk(
        chunk_id="ext-ray-train-gpu-scaling-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="Ray Train 用 ScalingConfig 启动多 GPU 训练",
        question_patterns=[
            "ray train 怎么多卡训练",
            "ray torchtrainer use_gpu 怎么配",
            "ray train num_workers 是 gpu 数吗",
            "多节点 pytorch 训练 ray 怎么写",
            "ray train 每个 worker 一张卡",
        ],
        content=(
            "适用场景:训练代码已经能用 PyTorch 跑,想用 Ray Train 扩到多 GPU 或多节点。"
            "Ray Train 的 `ScalingConfig(num_workers=N, use_gpu=True)` 用来声明 worker 数和是否使用 GPU;默认通常按每个 worker 一张 GPU 理解。\n\n"
            "示例结构:\n"
            "```\nfrom ray.train import ScalingConfig\nfrom ray.train.torch import TorchTrainer\n\ntrainer = TorchTrainer(\n    train_loop_per_worker=train_func,\n    scaling_config=ScalingConfig(num_workers=4, use_gpu=True),\n)\nresult = trainer.fit()\n```\n"
            "先用小模型/小数据验证分布式启动,再扩到真实任务;同时确认数据路径所有 worker 都能访问。\n\n"
            "注意:Ray 负责分布式调度和启动,不自动修复模型显存不足、数据不均衡或 NCCL 网络问题。"
        ),
        source_refs=["ray-docs:train-serve-resources"],
    ),
    chunk(
        chunk_id="ext-ray-serve-gpu-replicas-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="Ray Serve 给推理副本分配 GPU",
        question_patterns=[
            "ray serve 每个 replica 一张 gpu",
            "ray_actor_options num_gpus 怎么写",
            "ray serve gpu 推理怎么部署",
            "ray serve 模型副本抢 gpu",
            "ray serve 扩容 gpu 服务",
        ],
        content=(
            "适用场景:用 Ray Serve 部署模型服务,希望每个副本固定占用一张或部分 GPU。"
            "Ray Serve 可通过 deployment 的 `ray_actor_options` 给每个 replica 声明 GPU 资源。\n\n"
            "示例:\n"
            "```\nfrom ray import serve\n\n@serve.deployment(ray_actor_options={\"num_gpus\": 1})\nclass ModelService:\n    def __call__(self, request):\n        return \"ok\"\n```\n"
            "扩副本前先看集群总 GPU 数和每个模型显存占用;如果副本数大于可用 GPU,部署会等待资源。\n\n"
            "注意:Ray Serve 解决服务副本调度,模型加载、量化、batching 和端口暴露仍需按应用框架配置。"
        ),
        source_refs=["ray-docs:train-serve-resources"],
    ),
    chunk(
        chunk_id="ext-slurm-gpu-gres-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="Slurm 通过 GRES 申请 GPU 作业",
        question_patterns=[
            "slurm 怎么申请 gpu",
            "sbatch 申请一张卡怎么写",
            "srun --gpus 和 gres 区别",
            "slurm 里 cuda_visible_devices 为什么变了",
            "gpu 集群作业怎么指定卡数",
        ],
        content=(
            "适用场景:高校/实验室/HPC 集群用 Slurm 管理 GPU,客户需要提交训练或仿真作业。"
            "Slurm 用 GRES/TRES 管理 GPU,提交作业时应通过 Slurm 申请 GPU,不要绕过调度器直接占卡。\n\n"
            "示例:\n"
            "```\nsrun --gpus=1 --mem=32G nvidia-smi\n"
            "srun --tres-per-task=gres/gpu:1 -n2 --gpus=2 --mem=2G <command>\n```\n"
            "作业内部看到的 GPU 编号可能被 Slurm 映射过,以 `CUDA_VISIBLE_DEVICES` 为准,不一定等于物理卡编号。\n\n"
            "注意:如果 `nvidia-smi` 能看到卡但 Slurm 作业申请不到,要找集群管理员确认 `slurm.conf/gres.conf` 和节点状态。"
        ),
        source_refs=["slurm-docs:gres"],
    ),
    chunk(
        chunk_id="ext-slurm-gpu-accounting-mig-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="Slurm GPU 计量、MPS/MIG 和共享资源限制",
        question_patterns=[
            "slurm 能统计 gpu 利用率吗",
            "slurm mig 怎么配置",
            "slurm mps 和 gpu gres 区别",
            "slurm 多个作业共享 gpu",
            "gres gpumem gpuutil 没数据",
        ],
        content=(
            "适用场景:集群希望统计 GPU 显存/利用率,或在 Slurm 里配置 MIG、MPS、共享 GPU。"
            "Slurm 可通过 GRES/TRES 记录 `gres/gpumem`、`gres/gpuutil` 等信息,但依赖 NVML/rsmi 等后端和配置。\n\n"
            "关键点:\n"
            "1. `AccountingStorageTRES=gres/gpu` 后可收集 GPU 相关 TRES。\n"
            "2. MIG 在 Slurm 中可作为特定 GRES 类型配置,但 Slurm 不负责动态切分 MIG。\n"
            "3. MPS/shard 可让多作业共享 GPU,但配置和隔离语义不同于整卡 GRES。\n\n"
            "注意:这些通常是管理员侧配置;普通用户只应按集群文档申请资源,不要自行绕过。"
        ),
        source_refs=["slurm-docs:gres"],
    ),
    chunk(
        chunk_id="ext-trl-sfttrainer-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="TRL SFTTrainer 做监督微调",
        question_patterns=[
            "trl sfttrainer 怎么用",
            "sft 微调 qwen 怎么跑",
            "trl 监督微调数据集怎么传",
            "sfttrainer 和 transformers trainer 区别",
            "sft 想配 lora 省显存",
        ],
        content=(
            "适用场景:客户想用 Hugging Face TRL 对语言模型做监督微调(SFT),而不是只用基础 Transformers Trainer。"
            "SFTTrainer 是 TRL 的后训练入口之一,可结合 datasets 和 PEFT/LoRA。\n\n"
            "最小结构:\n"
            "```\nfrom datasets import load_dataset\nfrom trl import SFTTrainer\n\ntrainer = SFTTrainer(\n    model=\"Qwen/Qwen3-0.6B\",\n    train_dataset=load_dataset(\"trl-lib/Capybara\", split=\"train\"),\n)\ntrainer.train()\n```\n"
            "显存紧张时先考虑 LoRA/QLoRA、减小 batch、开启 gradient accumulation 和合适的精度配置。\n\n"
            "注意:SFT 数据格式、chat template 和 tokenizer 处理会直接影响训练质量;不要只看脚本能跑。"
        ),
        source_refs=["hf-trl:sft"],
    ),
    chunk(
        chunk_id="ext-trl-dpotrainer-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="TRL DPOTrainer 用偏好数据做对齐训练",
        question_patterns=[
            "dpo trainer 怎么用",
            "偏好数据怎么训练模型",
            "trl dpo 需要什么数据格式",
            "dpo 和 sft 有什么区别",
            "dpo 能不能配 lora",
        ],
        content=(
            "适用场景:模型已经经过 SFT,客户有 chosen/rejected 偏好样本,希望做 DPO 对齐训练。"
            "TRL 的 DPOTrainer 可加载偏好数据集,也能结合 PEFT/LoRA 降低显存需求。\n\n"
            "最小结构:\n"
            "```\nfrom datasets import load_dataset\nfrom trl import DPOTrainer\n\ntrainer = DPOTrainer(\n    model=\"Qwen/Qwen3-0.6B\",\n    train_dataset=load_dataset(\"trl-lib/ultrafeedback_binarized\", split=\"train\"),\n)\ntrainer.train()\n```\n"
            "如果只训练 adapter,可传入 PEFT 配置。训练前确认数据里 prompt/chosen/rejected 或等价字段符合当前 TRL 版本要求。\n\n"
            "注意:DPO 是偏好优化,不是补齐基础指令能力的万能办法;数据质量和参考模型/adapter 设置很关键。"
        ),
        source_refs=["hf-trl:dpo"],
    ),
    chunk(
        chunk_id="ext-hf-datasets-streaming-cache-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="Hugging Face Datasets 大数据集 streaming 和缓存目录",
        question_patterns=[
            "datasets 数据集太大下载不下",
            "huggingface datasets streaming 怎么用",
            "hf datasets 缓存占满系统盘",
            "HF_DATASETS_CACHE 怎么设置",
            "训练数据集怎么边读边训",
        ],
        content=(
            "适用场景:数据集很大,本地磁盘不够,或只想先抽样验证训练流程。"
            "Hugging Face Datasets 支持 `streaming=True`,可以边迭代边读取;缓存目录可通过环境变量切到数据盘。\n\n"
            "示例:\n"
            "```\nfrom datasets import load_dataset\n"
            "ds = load_dataset(\"HuggingFaceFW/fineweb\", split=\"train\", streaming=True)\n"
            "print(next(iter(ds)))\n```\n"
            "缓存目录:\n"
            "```\nexport HF_HOME=/data/huggingface\nexport HF_DATASETS_CACHE=/data/hf_datasets_cache\n```\n"
            "本地已有数据时,可以先加载成 Dataset 再 `to_iterable_dataset(num_shards=...)`,配合 DataLoader worker 分片。\n\n"
            "注意:streaming 方便启动和抽样,但随机 shuffle、过滤和多 epoch 训练的语义要单独确认。"
        ),
        source_refs=["hf-datasets:stream", "hf-datasets:cache"],
    ),
    chunk(
        chunk_id="ext-zarr-science-array-storage-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="Zarr 适合大型科学数组和对象存储数据",
        question_patterns=[
            "zarr 是什么",
            "大规模科学数组怎么存",
            "hdf5 太大并发读慢怎么办",
            "对象存储上怎么放多维数组",
            "ai4science 数据集 zarr 怎么理解",
        ],
        content=(
            "适用场景:气象、遥感、生物医学、仿真等任务处理大规模 N 维数组,希望支持分块、压缩、并发读取和对象存储。"
            "Zarr 是 chunked、compressed N-dimensional arrays 的 Python 实现之一,常用于科学数据和云原生数据集。\n\n"
            "使用建议:\n"
            "1. 先确认数据维度和常见访问模式,再设计 chunk shape。\n"
            "2. 大对象存储场景避免过小 chunk 造成对象数量过多。\n"
            "3. 训练前可用小范围切片验证 I/O 吞吐,再接 Dask/Xarray/DataLoader。\n\n"
            "注意:Zarr 解决存储布局和并发 I/O,不自动把计算放到 GPU;GPU 加速还要结合 CuPy、Dask-CUDA 或具体训练框架。"
        ),
        source_refs=["zarr-docs:index"],
    ),
    chunk(
        chunk_id="ext-dask-cudf-multigpu-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="Dask-cuDF / Dask-CUDA 做多 GPU 表格和机器学习任务",
        question_patterns=[
            "pandas 数据太大想用多张 gpu",
            "dask cudf 多 gpu 怎么启动",
            "dask cuda localcudacluster 怎么用",
            "rapids 多 gpu 数据处理",
            "cuml 多 gpu 训练怎么跑",
        ],
        content=(
            "适用场景:单卡 cuDF 处理不了更大的表格数据,或希望把 RAPIDS/cuML 任务扩到多 GPU。"
            "Dask-cuDF 让 Dask DataFrame 使用 cuDF 后端;多 GPU 需要 dask.distributed 集群,通常用 Dask-CUDA 简化本机多卡启动。\n\n"
            "示例:\n"
            "```\nfrom dask_cuda import LocalCUDACluster\nfrom distributed import Client\n\ncluster = LocalCUDACluster(CUDA_VISIBLE_DEVICES=\"0,1\", rmm_pool_size=0.9)\nclient = Client(cluster)\n```\n"
            "读 parquet/csv 后先看分区数量和每个分区大小;不要对超大集合直接 `.compute()` 到单卡内存。"
            "cuML 的 Dask 版本也需要 Dask 依赖和 cluster。\n\n"
            "注意:多 GPU 数据处理的瓶颈常在磁盘/对象存储、网络和分区设计,不是只增加 GPU 数就会线性加速。"
        ),
        source_refs=["dask-docs:gpu", "rapids-docs:dask-cudf", "rapids-docs:cuml-dask"],
    ),
]

COMMUNITY_IMAGE_TARGETED_CHUNKS = [
    chunk(
        chunk_id="ext-community-ltx-video-comfyui-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="LTX-2 / ComfyUI-LTXVideo 视频生成镜像的模型和显存检查",
        question_patterns=[
            "ltx 2.3 comfyui 视频生成模型放哪里",
            "ltxvideo 节点找不到模型",
            "comfyui ltx 需要多大显存",
            "ltx 文生视频图生视频怎么排查",
            "ltx lipdub 工作流缺模型",
        ],
        content=(
            "适用场景:客户使用 LTX-2/LTX-Video 的 ComfyUI 视频生成镜像,遇到节点不可用、模型缺失、显存不足或工作流加载失败。"
            "ComfyUI-LTXVideo README 要求先有 ComfyUI,并提示需要 CUDA GPU、32GB+ 显存和 100GB+ 空闲磁盘用于模型与缓存。\n\n"
            "排查顺序:\n"
            "1. 确认 ComfyUI 已更新到能加载该节点包的版本,安装节点后重启 ComfyUI。\n"
            "2. 看节点菜单里是否出现 `LTXVideo` 分类;没有出现时先看 custom_nodes 安装目录和 Python 依赖。\n"
            "3. 模型路径按 README 放置:LTX-2.3 checkpoint 到 `ComfyUI/models/checkpoints`,空间/时间 upscaler 到 `ComfyUI/models/latent_upscale_models`,LoRA 到 `ComfyUI/models/loras`,Gemma text encoder 到 `ComfyUI/models/text_encoders/...`。\n"
            "4. 两阶段放大、IC-LoRA、Lipdub、Text-to-Audio 工作流依赖的模型不同,不要只下载主 checkpoint。\n"
            "5. OOM 时先降低分辨率、帧数和两阶段放大,再考虑更大显存卡或分离工作流。\n\n"
            "注意:LTX-2 已在 ComfyUI core 中集成,但这个仓库仍提供额外节点和示例工作流;客户镜像里的节点版本和工作流版本要匹配。"
        ),
        source_refs=["ltx-video-repo:README", "comfyui-ltxvideo-repo:README"],
    ),
    chunk(
        chunk_id="ext-community-digital-human-livetalking-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="LiveTalking 实时数字人镜像的启动和实时性判断",
        question_patterns=[
            "livetalking 数字人怎么启动",
            "livetalking wav2lip 模型放哪里",
            "数字人推流卡顿怎么判断是 cpu 还是 gpu",
            "livetalking webrtc 页面打不开",
            "inferfps finalfps 是什么意思",
        ],
        content=(
            "适用场景:客户用 LiveTalking 做实时交互数字人,需要启动 WebRTC 服务、放置 Wav2Lip 模型,或判断实时推理是否跟得上。"
            "LiveTalking README 说明项目已在 Ubuntu 22.04、Python 3.12、PyTorch 2.9.1、CUDA 13.0 下测试,如果 CUDA 不同,应按 PyTorch 官网选择匹配版本。\n\n"
            "典型动作:\n"
            "1. 若从源码重建,先创建 conda 环境并安装 PyTorch 与 `requirements.txt`;镜像已内置时优先使用镜像自带环境。\n"
            "2. 将 `wav2lip256.pth` 放到项目 `models/` 目录,并按 README 重命名为 `wav2lip.pth`。\n"
            "3. 参考启动命令:`python app.py --transport webrtc --model wav2lip --avatar_id wav2lip256_avatar1`。\n"
            "4. 浏览器打不开时先确认程序是否监听、端口是否开放、访问地址是否用了实例公网/转发地址。\n"
            "5. 卡顿时看后端日志:`inferfps` 是 GPU 推理帧率,`finalfps` 是最终推流帧率;README 建议两者都达到 25 以上才算实时。\n\n"
            "注意:每路视频压缩主要消耗 CPU,每路口型推理消耗 GPU;多人同时说话时 GPU 会先成为瓶颈。"
        ),
        source_refs=["livetalking-repo:README"],
    ),
    chunk(
        chunk_id="ext-community-digital-human-infinitetalk-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="InfiniteTalk / MultiTalk 数字人视频的依赖和模型准备",
        question_patterns=[
            "infinitetalk 数字人需要哪些模型",
            "multitalk 480p 720p 需要怎么装",
            "infinitetalk flash attn 安装失败",
            "音频驱动视频模型缺 wan2.1",
            "数字人视频低显存怎么跑",
        ],
        content=(
            "适用场景:客户用 InfiniteTalk 或 MultiTalk 做音频驱动的视频配音、图片数字人或多人对话视频,遇到依赖安装、模型下载或低显存问题。"
            "InfiniteTalk README 把它定义为 audio-driven video generation,支持 audio-driven video-to-video 和 image-to-video;MultiTalk 面向多人对话视频,支持 480P/720P。\n\n"
            "准备重点:\n"
            "1. 源码环境通常是 Python 3.10,安装 PyTorch、xformers、flash-attn、requirements,并安装 FFmpeg。\n"
            "2. InfiniteTalk 需要 Wan2.1-I2V-14B-480P、chinese-wav2vec2-base、MeiGen-InfiniteTalk 等权重。\n"
            "3. MultiTalk 需要 Wan2.1-I2V-14B-480P、chinese-wav2vec2-base、MeiGen-MultiTalk 等权重。\n"
            "4. `flash_attn` 安装失败时,先确认 PyTorch/CUDA 版本匹配,再按 README 顺序补 ninja、psutil、packaging、wheel。\n"
            "5. OOM 时优先选 480P、低显存推理配置或多 GPU 推理;不要把 720P、多阶段、长视频同时打开。\n\n"
            "注意:这类工作流同时吃模型盘、系统依赖、显存和视频编码链路;只看 GPU 显存不足以定位全部问题。"
        ),
        source_refs=["infinitetalk-repo:README", "multitalk-repo:README"],
    ),
    chunk(
        chunk_id="ext-community-voice-conversion-svc-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="SVC / so-vits-svc 变声镜像不是开箱即用 TTS",
        question_patterns=[
            "svc fusion 是不是直接文字转语音",
            "so vits svc 训练数据怎么准备",
            "唱歌变声模型要不要自己训练",
            "svc f0 hubert 预处理怎么理解",
            "语音克隆和 svc 有什么区别",
        ],
        content=(
            "适用场景:客户把 SVC/so-vits-svc 类镜像当成 TTS 或通用语音克隆工具,需要解释它更偏向歌声/声音转换训练。"
            "so-vits-svc README 明确说明项目关注 Singing Voice Conversion,不是 TTS;框架本身不带可直接合成任意声音的能力,功能需要用户训练模型。Amphion 也把 TTS、VC、SVC 分成不同任务。\n\n"
            "处理建议:\n"
            "1. 先确认客户目标:文字转语音选 TTS;参考音色说话选 voice cloning;已有源音频换音色才是 VC/SVC。\n"
            "2. 训练数据用 WAV;README 建议先切音频,一般 5-15 秒更适合,过长容易在预处理或训练时爆显存。\n"
            "3. 根据配置准备 ContentVec/Hubert、F0 predictor、NSF-HIFIGAN 等预训练文件。\n"
            "4. 训练前运行 resample、生成 filelist/config、生成 hubert 和 f0。\n"
            "5. 显存不够时调小 batch size、音频片段时长和训练配置。\n\n"
            "注意:变声/克隆涉及授权和署名;公开发布转换作品前要确认音频来源和目标声音的使用权限。"
        ),
        source_refs=["so-vits-svc-repo:README", "amphion-repo:README"],
    ),
    chunk(
        chunk_id="ext-community-dots-tts-voice-cloning-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="dots.tts 语音克隆镜像的模型选择和 Web Demo",
        question_patterns=[
            "dots tts 语音克隆怎么跑",
            "dots.tts prompt audio 和 prompt text 怎么填",
            "dots tts base soar mf 选哪个",
            "dots tts gradio 端口是多少",
            "语音克隆参考音频没有转录文本可以吗",
        ],
        content=(
            "适用场景:客户使用 dots.tts 做文本转语音、参考音频克隆或 Gradio Web 演示。"
            "dots.tts README 将其描述为 2B 参数的连续自回归 TTS 系统,提供 base、soar、mf 三类 checkpoint:base 是预训练,soar 偏高质量语音克隆,mf 偏推理速度。\n\n"
            "操作要点:\n"
            "1. 从源码安装时用 Python 3.10-3.12 的干净 conda 环境,再 `pip install -e .`。\n"
            "2. 命令行可直接把 Hugging Face repo id 传给 `--model-name-or-path`,也可传本地模型目录。\n"
            "3. 最推荐的克隆方式是 reference audio + transcript:同时传 `--prompt-audio` 和 `--prompt-text`。\n"
            "4. 只有 reference audio 也可走 x-vector-only cloning,但效果和可控性通常不如带转录文本。\n"
            "5. Web Demo 用 `python apps/gradio/app.py --model-name-or-path ...`,README 默认监听 `0.0.0.0:7860`。\n"
            "6. `--num-steps` 越高通常质量越好但更慢;mf checkpoint README 推荐更少步数。\n\n"
            "注意:参考音频要尽量干净、无背景噪声、与转录文本一致;否则常见问题是音色不像、发音错或杂音明显。"
        ),
        source_refs=["dots-tts-repo:README"],
    ),
    chunk(
        chunk_id="ext-community-cosyvoice-voxcpm-tts-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="CosyVoice / VoxCPM 语音合成镜像的选型差异",
        question_patterns=[
            "cosyvoice 和 voxcpm 哪个适合语音克隆",
            "voxcpm2 支持哪些语言",
            "cosyvoice 模型放 pretrained_models 哪里",
            "tts 需要参考音频还是可以文字描述音色",
            "语音合成镜像怎么选模型",
        ],
        content=(
            "适用场景:客户在 CosyVoice、VoxCPM、语音克隆镜像之间选型,或问模型下载、语言覆盖和实时性。"
            "CosyVoice README 介绍 Fun-CosyVoice 3.0/CosyVoice 2.0/1.0,并提供 clone、conda、requirements、sox 和 `pretrained_models/` 下载路径。VoxCPM2 README 说明它是 2B TTS,支持多语言、voice design、controllable cloning 和 48kHz 输出。\n\n"
            "选型提示:\n"
            "1. 需要成熟 WebUI、中文生态和多模型权重时,优先看 CosyVoice 镜像是否已带好 `pretrained_models/`。\n"
            "2. 需要自然语言描述音色或带风格控制的克隆时,可关注 VoxCPM2 的 Voice Design / Controllable Voice Cloning。\n"
            "3. VoxCPM README 要求 Python >=3.10 且 <3.13、PyTorch >=2.5.0、CUDA >=12.0;镜像重装环境时要匹配。\n"
            "4. CosyVoice 从源码安装需要 `git clone --recursive`,否则子模块缺失会导致依赖或模型代码不完整。\n"
            "5. 语音质量问题优先检查参考音频质量、语言标签、文本标点、切句长度和采样率。\n\n"
            "注意:语音克隆不等于声音分离或视频配音全流程;视频配音还需要 ASR、翻译、对齐、混音和视频封装。"
        ),
        source_refs=["cosyvoice-repo:README", "voxcpm-repo:README"],
    ),
    chunk(
        chunk_id="ext-community-seed-tts-eval-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="Seed-TTS-Eval 是语音评测集和指标脚本",
        question_patterns=[
            "seed tts eval 是不是语音合成模型",
            "怎么评估 tts 的 wer 和 sim",
            "seed tts eval 需要准备什么输入",
            "语音克隆结果怎么客观打分",
            "cal wer cal sim 怎么用",
        ],
        content=(
            "适用场景:客户看到 Seed-TTS-Eval 镜像,以为它能直接生成语音,或希望评测 TTS/语音克隆结果。"
            "Seed-TTS-Eval README 说明仓库包含 seed-TTS 项目的客观测试集和指标计算脚本,用于 zero-shot speech generation 能力评估,不是一个 TTS 推理模型。\n\n"
            "使用方式:\n"
            "1. 先 `pip3 install -r requirements.txt` 安装依赖。\n"
            "2. 准备 meta file 和模型生成好的音频目录;meta 每行包含文件名、prompt 文本、prompt 音频、待合成文本和真值等字段。\n"
            "3. WER 使用 ASR 引擎计算识别错误率,README 提到英文用 Whisper-large-v3、中文用 Paraformer-zh。\n"
            "4. SIM 使用 WavLM-large 说话人验证模型抽 embedding 后算 cosine similarity。\n"
            "5. 脚本形式:`bash cal_wer.sh <meta> <synth_audio_dir> <zh|en>` 和 `bash cal_sim.sh <meta> <synth_audio_dir> <wavlm_ckpt>`。\n\n"
            "注意:评测结果依赖测试集、ASR、说话人模型和音频预处理;不要把不同配置下的分数直接横向比较。"
        ),
        source_refs=["seed-tts-eval-repo:README"],
    ),
    chunk(
        chunk_id="ext-community-ai-toolkit-lora-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="AI-Toolkit 做 FLUX/Qwen/Wan LoRA 训练",
        question_patterns=[
            "ai toolkit 怎么训练 qwen image lora",
            "ai-toolkit 支持 flux wan 吗",
            "boogu image lora 训练数据怎么准备",
            "ai toolkit webui 怎么启动训练",
            "lora 训练显存不够怎么办",
        ],
        content=(
            "适用场景:客户使用 AI-Toolkit 或基于它的镜像训练 FLUX、Qwen-Image、Wan 等图像/视频模型 LoRA。"
            "AI Toolkit README 将其定义为 diffusion models 的 all-in-one training suite,支持消费级硬件上的图像和视频模型,支持列表包含 FLUX、Qwen-Image/Qwen-Image-Edit、Wan 2.1/2.2 等。\n\n"
            "处理建议:\n"
            "1. 从源码安装时使用 Python >=3.10,准备 venv,先安装与 CUDA 匹配的 PyTorch,再装 requirements。\n"
            "2. 先用 UI 或示例配置跑一个小数据 smoke,确认模型、数据、输出目录都能读写。\n"
            "3. 数据集要检查图片/视频路径、caption、触发词、分辨率和重复样本;LoRA 质量通常先受数据影响。\n"
            "4. 训练 OOM 时先降分辨率、batch、缓存策略和训练目标,再考虑量化/低显存配置。\n"
            "5. 如果镜像是汉化版,排查时仍要回到原始 AI-Toolkit 配置字段和日志。\n\n"
            "注意:AI-Toolkit 支持很多模型,但不同模型的配置不可互换;Qwen-Image、FLUX、Wan 的底模、caption 和输出 adapter 都要分开管理。"
        ),
        source_refs=["ai-toolkit-repo:README"],
    ),
    chunk(
        chunk_id="ext-community-qwen-image-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="Qwen-Image 镜像适合文字渲染和精确图片编辑",
        question_patterns=[
            "qwen image 适合生成带文字的图片吗",
            "qwen image edit 效果不对怎么办",
            "qwen image comfyui 支持吗",
            "qwen image 需要哪个 diffusers 版本",
            "boogu image gguf 和 qwen image 什么关系",
        ],
        content=(
            "适用场景:客户使用 Qwen-Image 或基于 Qwen-Image 的 ComfyUI/编辑镜像,需要理解能力边界和常见依赖问题。"
            "Qwen-Image README 将其描述为 20B MMDiT 图像基础模型,重点能力是复杂文字渲染和精确图片编辑;README 也提到 Qwen-Image 已原生支持 ComfyUI,并支持多种 LoRA。\n\n"
            "排查建议:\n"
            "1. 文生图和图像编辑要区分模型权重,不要把 Qwen-Image 和 Qwen-Image-Edit 的工作流混用。\n"
            "2. README 要求 transformers >=4.51.3,并建议安装最新 diffusers;编辑效果不对时先升级 diffusers 和节点。\n"
            "3. ComfyUI 找不到模型时,先按工作流检查 checkpoint、text encoder、VAE/AE、LoRA 等具体节点路径。\n"
            "4. 文字渲染失败时先减少画面元素和文字长度,明确字体/位置/语言,再调 seed 和分辨率。\n"
            "5. GGUF 镜像通常是低显存运行方案,不是官方原始权重的完整等价替代。\n\n"
            "注意:Qwen-Image 擅长图片文字和编辑,但长段落、小字号、复杂排版仍需要多轮提示词和后期修正。"
        ),
        source_refs=["qwen-image-repo:README"],
    ),
    chunk(
        chunk_id="ext-community-wan-video-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="Wan2.2 视频生成镜像的模型、分辨率和低显存参数",
        question_patterns=[
            "wan2.2 文生视频图生视频模型怎么选",
            "wan 720p 视频生成显存不够",
            "wan2.2 flash attn 安装失败",
            "wan speech to video 需要 cosyvoice 吗",
            "wan2.2 多 gpu 怎么跑",
        ],
        content=(
            "适用场景:客户使用 Wan2.2 做文生视频、图生视频、文图生视频、语音到视频或角色动画,遇到模型下载、720P 显存和依赖问题。"
            "Wan2.2 README 列出 T2V-A14B、I2V-A14B、TI2V-5B、S2V-14B、Animate-14B 等模型;T2V/I2V 支持 480P/720P,TI2V-5B 支持 720P 视频生成。\n\n"
            "处理步骤:\n"
            "1. 先按任务选模型:T2V 文生视频,I2V 图生视频,TI2V 文图生视频,S2V 语音到视频,Animate 角色动画。\n"
            "2. 安装依赖时若 `flash_attn` 失败,README 建议先安装其他包,最后再装 `flash_attn`。\n"
            "3. 下载权重可用 `huggingface-cli download ... --local-dir` 或 modelscope download。\n"
            "4. README 示例提示 720P A14B 单卡命令至少需要 80GB 显存;OOM 时可用 `--offload_model True`、`--convert_model_dtype`、`--t5_cpu`。\n"
            "5. 多 GPU 推理使用 torchrun,并结合 FSDP / DeepSpeed Ulysses 参数。\n"
            "6. 语音到视频如果需要用 CosyVoice 生成语音,还要额外安装 `requirements_s2v.txt`。\n\n"
            "注意:720P、长视频、语音驱动和角色动画会叠加显存与磁盘压力;先用短 prompt、低分辨率、小步数做连通性验证。"
        ),
        source_refs=["wan22-repo:README", "cosyvoice-repo:README"],
    ),
    chunk(
        chunk_id="ext-community-comfyui-gguf-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="ComfyUI-GGUF 低显存图像镜像的模型放置和节点选择",
        question_patterns=[
            "comfyui gguf 模型放哪个目录",
            "unet loader gguf 节点找不到",
            "flux gguf 低显存怎么用",
            "gguf lora 在 comfyui 能用吗",
            "Force CLIP Device 导致 OOM 怎么办",
        ],
        content=(
            "适用场景:客户使用 Flux/Qwen/Ideogram 等 GGUF ComfyUI 镜像,想在较小显存上运行 DiT/Transformer 类图像模型。"
            "ComfyUI-GGUF README 说明该节点包为原生 ComfyUI 模型提供 GGUF 量化支持,当前仍是 WIP;Transformer/DiT 模型如 flux 更适合量化低位运行。\n\n"
            "排查顺序:\n"
            "1. 先确认 ComfyUI 版本足够新,能在加载 UNET-only 时支持 custom ops。\n"
            "2. 节点包应放在 `ComfyUI/custom_nodes/ComfyUI-GGUF`,并安装 requirements。\n"
            "3. `.gguf` 模型文件放到 `ComfyUI/models/unet`。\n"
            "4. 工作流里使用 `GGUF Unet loader`,README 说明它在 `bootleg` 分类下。\n"
            "5. T5 的 GGUF 量化需要使用对应的 `*CLIPLoader (gguf)` 节点替代普通 CLIP loader。\n"
            "6. LoRA 加载是 experimental,优先用内置 LoRA loader 节点验证最小工作流。\n\n"
            "注意:README 特别提醒 Force/Set CLIP Device 不是该节点包的一部分;单 GPU 用户不要乱设到 cuda:0 后再把 OOM 归因给 GGUF。"
        ),
        source_refs=["comfyui-gguf-repo:README"],
    ),
    chunk(
        chunk_id="ext-community-triposplat-3d-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="TripoSplat 单图生成 3D Gaussian 文件",
        question_patterns=[
            "triposplat 单张图片生成 3d 怎么跑",
            "triposplat 输出 ply splat 怎么看",
            "单图 3d 高斯模型权重放哪里",
            "comfyui triposplat 工作流怎么用",
            "3d gaussian splatting 生成文件打不开",
        ],
        content=(
            "适用场景:客户用 TripoSplat 镜像从单张 2D 图片生成 3D Gaussian,用于资产预览、AR/VR 或游戏素材初稿。"
            "TripoSplat README 说明它能把单张 2D 图转换为可变数量的 3D Gaussians,并支持官方 ComfyUI workflow template。\n\n"
            "使用要点:\n"
            "1. 模型权重下载到 `ckpts/`,可用 Hugging Face CLI、huggingface_hub、ModelScope CLI 或 SDK。\n"
            "2. 源码最小依赖包含 torch/torchvision 以及 numpy、safetensors、pillow、tqdm。\n"
            "3. 命令行可先运行 `python run_example.py` 验证权重和环境。\n"
            "4. 需要 Web 演示时安装 gradio,运行 `python run_gradio.py`。\n"
            "5. 输出 `.ply` / `.splat` 可用 3D Gaussian viewer 查看,例如 SparkJS 或 SuperSplat。\n\n"
            "注意:输入图片质量会直接影响 3D 结果;透明/遮挡/单色背景、主体不完整或视角极端时,生成结果需要人工修正。"
        ),
        source_refs=["triposplat-repo:README"],
    ),
    chunk(
        chunk_id="ext-community-video-dubbing-tts-webui-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="视频配音和语音克隆 WebUI 需要拆成 ASR/TTS/对齐链路",
        question_patterns=[
            "视频配音镜像是不是只需要 tts",
            "gpt sovits 和 f5 tts 适合做视频配音吗",
            "语音克隆 webui 缺 ffmpeg 怎么办",
            "f5 tts 参考音频没有文本会怎样",
            "gpt-sovits 预训练模型放哪里",
        ],
        content=(
            "适用场景:客户使用视频配音、语音克隆或 GPT-SoVITS/F5-TTS 类 WebUI 镜像,希望把一段视频变成另一种语言或另一种音色。"
            "GPT-SoVITS README 说明它提供 few-shot voice conversion 和 TTS WebUI,集成伴奏分离、训练集切分、中文 ASR、文本标注等工具;F5-TTS README 提供 CLI/Gradio 入口和参考音频推理方式。\n\n"
            "解释方式:\n"
            "1. 视频配音不是单一 TTS:通常需要音频提取、ASR、翻译或改写、TTS/克隆、时长对齐、混音和重新封装视频。\n"
            "2. GPT-SoVITS 的预训练模型放到 `GPT_SoVITS/pretrained_models`;UVR5、ASR 等额外模型分别放到 README 指定目录。\n"
            "3. GPT-SoVITS 和 F5-TTS 都依赖 FFmpeg;视频/音频读写失败先查 FFmpeg 是否在环境里。\n"
            "4. F5-TTS 的 Gradio 可用 `f5-tts_infer-gradio --port 7860 --host 0.0.0.0` 对外监听。\n"
            "5. F5-TTS 如果 `--ref_text` 留空,README 说明会让 ASR 模型转写,这会额外占用 GPU 显存。\n\n"
            "注意:客户问“声音不像”时,先检查参考音频、转录文本、语言、噪声和授权,不要直接归因于 GPU 或镜像故障。"
        ),
        source_refs=["gpt-sovits-repo:README", "f5-tts-repo:README"],
    ),
]

SCENE_SIGNAL_TARGETED_CHUNKS = [
    chunk(
        chunk_id="ext-scene-video-comfyui-models-vram-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="视频生成工作流的模型路径和显存检查",
        question_patterns=[
            "comfyui 视频生成缺模型怎么查",
            "视频工作流节点找不到 checkpoint",
            "图生视频显存不够先降什么",
            "视频生成工作流加载失败怎么办",
            "视频模型目录怎么排查",
        ],
        content=(
            "适用场景:视频生成工作流打开后缺模型、节点不可用、工作流无法加载或推理爆显存。\n\n"
            "排查顺序:\n"
            "1. 先确认基础 WebUI 能启动,再看自定义节点是否安装完整并重启过服务。\n"
            "2. 按节点实际要求检查 checkpoint、text encoder、VAE、LoRA、upscaler 等目录,不要只下载主模型。\n"
            "3. 用最小工作流先跑 1 个短片段,确认模型路径和依赖能连通。\n"
            "4. OOM 时优先降低分辨率、帧数、batch、放大阶段和同时加载的模型数量。\n"
            "5. 长视频、补帧、放大、音频驱动会叠加显存和磁盘压力,应分阶段验证。\n\n"
            "注意:外部语料只保留通用排查方法;具体项目预置了哪些权重或节点,应查内部语料或项目文档。"
        ),
        source_refs=["comfyui-docs:getting_started", "diffusers-docs:pipelines"],
    ),
    chunk(
        chunk_id="ext-scene-realtime-digital-human-latency-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="实时数字人的延迟和帧率定位",
        question_patterns=[
            "实时数字人推流卡顿怎么查",
            "口型同步延迟高怎么办",
            "数字人 webrtc 页面能开但很卡",
            "数字人推理帧率和播放帧率不一致",
            "实时交互数字人需要看哪些指标",
        ],
        content=(
            "适用场景:数字人页面能打开,但口型延迟、画面卡顿、声音不同步或多人访问后变慢。\n\n"
            "定位顺序:\n"
            "1. 先把链路拆成音频输入、ASR/文本、口型或头像推理、视频编码、传输、浏览器播放。\n"
            "2. 看后端日志里的推理耗时、编码耗时和队列长度,确认慢在 GPU 推理还是 CPU 编码。\n"
            "3. 用单用户、短音频、低分辨率验证基线,再增加并发和画质。\n"
            "4. WebRTC 页面异常时检查服务监听地址、端口转发、反向代理和浏览器控制台错误。\n"
            "5. 多路并发通常同时消耗 GPU、CPU 和带宽,需要分别监控。\n\n"
            "注意:实时性不是只看 GPU 利用率;浏览器、编码器和网络也会让最终画面变慢。"
        ),
        source_refs=["webrtc-docs:overview", "ffmpeg-docs:documentation"],
    ),
    chunk(
        chunk_id="ext-scene-audio-driven-avatar-video-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="音频驱动头像视频的依赖、模型和低显存处理",
        question_patterns=[
            "音频驱动数字人视频缺依赖怎么查",
            "头像视频生成需要准备哪些模型",
            "audio driven video 低显存怎么跑",
            "数字人视频生成 flash attention 安装失败",
            "音频转口型视频跑不起来",
        ],
        content=(
            "适用场景:用音频驱动图片或视频生成说话人视频时,遇到依赖安装、权重缺失、显存不足或视频编码失败。\n\n"
            "排查顺序:\n"
            "1. 先确认 Python、PyTorch、CUDA、xformers 或 flash-attn 等依赖和项目要求一致。\n"
            "2. 按项目文档分别准备基础视频模型、音频编码器、口型或头像模型,不要把它们混在一个目录里。\n"
            "3. FFmpeg 是音视频读写和封装的常见依赖,视频读写失败时先验证 `ffmpeg -version`。\n"
            "4. 低显存先用短音频、低分辨率、少帧数或分段推理,确认链路通后再提高质量。\n"
            "5. 多人对话或长视频应分片处理,再做拼接和音画对齐。\n\n"
            "注意:音频驱动视频同时依赖语音质量、图像质量、模型权重、显存和编码链路,不要只按一个报错判断。"
        ),
        source_refs=["pytorch-docs:get-started", "ffmpeg-docs:documentation"],
    ),
    chunk(
        chunk_id="ext-scene-voice-conversion-boundary-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="变声、语音克隆和文字转语音的边界",
        question_patterns=[
            "变声模型是不是直接文字转语音",
            "语音克隆和 voice conversion 有什么区别",
            "已有音频换音色应该用什么任务",
            "唱歌变声需要自己训练吗",
            "声音转换数据怎么准备",
        ],
        content=(
            "适用场景:客户把变声、语音克隆和 TTS 混在一起,导致选错工具或预期不一致。\n\n"
            "判断方法:\n"
            "1. 文字输入生成语音是 TTS。\n"
            "2. 给一段参考音频,让模型模仿音色朗读新文本,是语音克隆或可控 TTS。\n"
            "3. 已有源音频换成另一种音色,是 voice conversion 或 singing voice conversion。\n"
            "4. 变声/歌声转换通常需要目标声音数据、预处理、训练和推理,不是只输入文字。\n"
            "5. 质量问题先看训练数据干净程度、音频切片长度、采样率、转录文本和授权。\n\n"
            "注意:声音相关任务涉及肖像声纹和版权授权,对外使用前必须确认声音来源和用途许可。"
        ),
        source_refs=["amphion-repo:README", "so-vits-svc-repo:README"],
    ),
    chunk(
        chunk_id="ext-scene-tts-voice-cloning-reference-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="语音克隆的参考音频、转录文本和效果排查",
        question_patterns=[
            "语音克隆效果不像先查什么",
            "参考音频和转录文本分别有什么用",
            "tts 克隆有噪声怎么办",
            "参考音频没有文本能不能克隆",
            "语音克隆速度慢怎么排查",
        ],
        content=(
            "适用场景:语音克隆能跑通,但音色不像、发音错、噪声大或生成速度慢。\n\n"
            "排查顺序:\n"
            "1. 参考音频应尽量短、干净、单人、无背景音乐,且和转录文本一致。\n"
            "2. 如果项目支持 reference audio + transcript,优先同时提供两者;自动转写会引入额外错误。\n"
            "3. 文本过长时先分句,避免一次生成导致韵律漂移或显存压力上升。\n"
            "4. 质量差先换参考音频和文本,再调整采样步数、语速、温度或模型配置。\n"
            "5. 速度慢时区分模型推理、音频后处理和 Web 服务排队。\n\n"
            "注意:不同 TTS 项目的参数名称不同,但参考音频质量和文本一致性是长期稳定的关键因素。"
        ),
        source_refs=["f5-tts-repo:README", "cosyvoice-repo:README"],
    ),
    chunk(
        chunk_id="ext-scene-tts-model-selection-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="TTS 模型选型看语言、克隆方式和实时性",
        question_patterns=[
            "tts 模型怎么选",
            "语音合成要多语言还是语音克隆",
            "实时 tts 和高质量 tts 怎么取舍",
            "音色描述和参考音频有什么区别",
            "tts 模型下载后怎么验证",
        ],
        content=(
            "适用场景:客户不知道该选哪类 TTS 模型,或下载模型后不确定是否能满足业务。\n\n"
            "选型维度:\n"
            "1. 语言和口音:先确认目标语言、方言、英文夹杂和数字读法是否支持。\n"
            "2. 控制方式:有的模型靠参考音频克隆,有的支持自然语言描述音色,有的只做固定音色。\n"
            "3. 实时性:交互场景优先延迟和稳定性;离线配音可优先质量和长文本一致性。\n"
            "4. 部署成本:看模型大小、显存、CPU 后处理和并发排队。\n"
            "5. 验收方式:准备固定测试文本、参考音频、目标风格和人工听测标准。\n\n"
            "注意:外部语料不维护某个平台可用的模型列表;模型是否已预置应查内部语料或资源实际状态。"
        ),
        source_refs=["cosyvoice-repo:README", "voxcpm-repo:README"],
    ),
    chunk(
        chunk_id="ext-scene-tts-evaluation-metrics-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="TTS 和语音克隆评测看可懂度、相似度和人工听测",
        question_patterns=[
            "语音克隆结果怎么客观评估",
            "tts wer 和 speaker similarity 怎么理解",
            "语音合成交付前怎么验收",
            "asr 评测分数为什么不稳定",
            "声音相似度高但听起来不像怎么办",
        ],
        content=(
            "适用场景:需要判断 TTS 或语音克隆是否可用,不能只凭单条试听决定。\n\n"
            "评测方法:\n"
            "1. 可懂度常用 ASR 后的 WER/CER 做近似,但会受 ASR 模型和文本规范影响。\n"
            "2. 音色相似度可用说话人 embedding 的 cosine similarity,但不能完全替代人耳判断。\n"
            "3. 人工听测要覆盖目标语言、数字、专有名词、长句、情绪和噪声参考音频。\n"
            "4. 每次评测固定测试集、采样参数、后处理和评分表,便于版本间比较。\n"
            "5. 失败样本要分类:读错字、断句差、音色不像、噪声、节奏不稳、长文本漂移。\n\n"
            "注意:不同评测工具输出不可直接横比;重点是同一业务测试集上的稳定改进。"
        ),
        source_refs=["seed-tts-eval-repo:README", "whisper-repo:README"],
    ),
    chunk(
        chunk_id="ext-scene-image-video-lora-training-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="图像和视频 LoRA 训练的数据、显存和验收",
        question_patterns=[
            "图像 lora 训练效果差先查数据吗",
            "视频 lora 训练显存不够怎么办",
            "flux 或 wan lora 数据怎么准备",
            "lora 训练前怎么做小样本验证",
            "训练出来的 lora 怎么验收",
        ],
        content=(
            "适用场景:训练图像或视频生成模型的 LoRA,遇到数据不规范、显存不足、结果不像或无法复现。\n\n"
            "处理建议:\n"
            "1. 先固定底模、训练脚本、配置文件、数据目录和输出目录。\n"
            "2. 数据集要检查路径、caption、触发词、分辨率、重复样本和授权。\n"
            "3. 训练 OOM 时先降低分辨率、batch、帧数、缓存策略和训练模块数量。\n"
            "4. 用很小的数据和短步数先跑 smoke,确认能保存 adapter 并能加载推理。\n"
            "5. 验收时固定 prompt、seed、采样器和对照图,不要只看一张随机结果。\n\n"
            "注意:LoRA 质量通常先受数据影响,再受训练参数影响;显卡更大不能弥补脏数据。"
        ),
        source_refs=["diffusers-docs:lora", "ai-toolkit-repo:README"],
    ),
    chunk(
        chunk_id="ext-scene-image-edit-text-rendering-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="图像编辑和文字渲染模型的能力边界",
        question_patterns=[
            "图片生成中文文字不稳定怎么办",
            "图像编辑模型改错位置怎么排查",
            "海报文字生成效果差怎么判断",
            "图片编辑和文生图模型能混用吗",
            "文字渲染模型验收要看什么",
        ],
        content=(
            "适用场景:用图像生成或编辑模型做海报、商品图、局部修改和带文字图片时,结果不稳定。\n\n"
            "排查建议:\n"
            "1. 区分文生图、图生图、局部重绘和专门的编辑模型,不要混用不兼容工作流。\n"
            "2. 文字渲染失败时先减少文字长度、明确语言、位置、字号和背景复杂度。\n"
            "3. 局部编辑失败时检查 mask、参考图分辨率、提示词是否只描述要改的区域。\n"
            "4. 多轮编辑可能累积画质损失,必要时保留原图和中间版本。\n"
            "5. 验收要用固定版式和业务真实文字,不能只看示例 prompt。\n\n"
            "注意:长段落、小字号和复杂排版仍常需要后期设计工具修正。"
        ),
        source_refs=["diffusers-docs:image-to-image", "comfyui-docs:getting_started"],
    ),
    chunk(
        chunk_id="ext-scene-video-generation-low-vram-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="视频生成低显存运行的取舍",
        question_patterns=[
            "视频生成 oom 先降低哪些参数",
            "720p 视频生成显存不够怎么办",
            "长视频生成特别慢怎么优化",
            "视频模型 offload 会变慢吗",
            "图生视频帧数和分辨率怎么取舍",
        ],
        content=(
            "适用场景:视频生成报 OOM、速度很慢,或想在较小显存上跑通工作流。\n\n"
            "处理顺序:\n"
            "1. 先降分辨率、帧数、视频时长、batch 和采样步数。\n"
            "2. 使用 CPU offload、分块 VAE、低精度或量化方案时,要接受速度下降。\n"
            "3. 长视频优先分段生成,再做拼接、补帧或放大。\n"
            "4. 多阶段工作流要逐段验证,不要一次打开生成、放大、插帧和配音。\n"
            "5. 记录显存峰值、耗时、输入参数和输出质量,作为升级显卡或改参数的依据。\n\n"
            "注意:视频生成的显存通常随分辨率、帧数和并发快速增长,低显存方案主要是换时间。"
        ),
        source_refs=["diffusers-docs:memory", "pytorch-docs:notes/cuda"],
    ),
    chunk(
        chunk_id="ext-scene-quantized-comfyui-loaders-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="量化图像模型在 ComfyUI 中的加载排查",
        question_patterns=[
            "comfyui gguf 模型加载失败怎么办",
            "量化模型低显存运行怎么排查",
            "unet loader 和 clip loader 选错会怎样",
            "gguf lora 不生效怎么查",
            "低显存图像工作流节点不匹配",
        ],
        content=(
            "适用场景:用量化图像模型降低显存,但出现 loader 不匹配、节点缺失、LoRA 不生效或效果异常。\n\n"
            "排查顺序:\n"
            "1. 确认量化模型格式和节点包支持的 loader 类型一致。\n"
            "2. 区分 UNet/DiT、CLIP/text encoder、VAE/AE 和 LoRA,不要把文件放错目录。\n"
            "3. 工作流中普通 loader 与量化 loader 不能随意互换,先跑项目提供的最小示例。\n"
            "4. LoRA 支持可能受模型架构和节点版本限制,先用无 LoRA 的基线验证。\n"
            "5. 低显存成功不代表质量等价,要和未量化或更高精度结果做对照。\n\n"
            "注意:量化方案适合降低显存门槛,但排查时仍要回到模型格式、节点版本和路径三件事。"
        ),
        source_refs=["comfyui-docs:getting_started", "comfyui-gguf-repo:README"],
    ),
    chunk(
        chunk_id="ext-scene-single-image-3d-reconstruction-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="单图生成 3D 资产的输入质量和结果查看",
        question_patterns=[
            "单张图片生成 3d 效果差怎么办",
            "3d gaussian splatting 输出文件怎么查看",
            "图片转 3d 模型权重放哪里",
            "生成的 ply 或 splat 打不开",
            "云端跑 3d 预览黑屏怎么办",
        ],
        content=(
            "适用场景:用单张图片生成 3D Gaussian、mesh 或点云资产,但结果畸形、文件打不开或无法预览。\n\n"
            "排查建议:\n"
            "1. 输入图尽量主体完整、清晰、背景简单,避免强遮挡和极端视角。\n"
            "2. 先用项目最小示例验证权重、依赖和输出目录。\n"
            "3. 输出 `.ply`、`.splat`、mesh 或点云时,用对应 viewer 验证格式是否正确。\n"
            "4. 云端预览黑屏时检查 WebGL、VNC/浏览器 GPU、端口转发和文件路径。\n"
            "5. 生成结果常需要人工修正,不要把它当作最终可直接生产的 3D 模型。\n\n"
            "注意:单图 3D 生成对输入图片非常敏感,多视角或人工后处理通常更可靠。"
        ),
        source_refs=["triposplat-repo:README", "gaussian-splatting-repo:README"],
    ),
    chunk(
        chunk_id="ext-scene-video-dubbing-pipeline-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="视频配音要拆成 ASR、翻译、TTS、对齐和封装",
        question_patterns=[
            "视频配音是不是只需要 tts",
            "视频翻译配音声音不像怎么办",
            "语音克隆 webui 缺 ffmpeg 怎么办",
            "配音音画不同步怎么排查",
            "视频换语言要查哪些环节",
        ],
        content=(
            "适用场景:希望把视频变成另一种语言或音色,但声音不像、字幕不准、时长不匹配或封装失败。\n\n"
            "拆解链路:\n"
            "1. 音频提取和降噪:先确保源音频可读且人声清楚。\n"
            "2. ASR 和文本处理:转写、翻译、断句会影响后续 TTS 质量。\n"
            "3. TTS 或语音克隆:参考音频、转录文本、语言和授权决定效果上限。\n"
            "4. 时长对齐:需要调语速、分句、静音和片段边界。\n"
            "5. 混音和视频封装:FFmpeg 缺失或参数错误会导致输出失败。\n\n"
            "注意:客户说“配音不好”时,先定位是哪一段链路失败,不要直接归因于 GPU 或单个模型。"
        ),
        source_refs=["ffmpeg-docs:documentation", "f5-tts-repo:README", "gpt-sovits-repo:README"],
    ),
]

SECOND_WAVE_EXTERNAL_CHUNKS = [
    chunk(
        chunk_id="ext-sd-scripts-lora-training-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="sd-scripts / kohya 做 Stable Diffusion LoRA 训练",
        question_patterns=[
            "kohya sd-scripts 怎么训练 lora",
            "stable diffusion lora 训练需要装什么",
            "sdxl lora 训练脚本怎么选",
            "flux lora 训练环境怎么准备",
            "kohya 训练 lora 显存不够怎么办",
        ],
        content=(
            "适用场景:客户要训练 Stable Diffusion/SDXL/FLUX 等图像模型 LoRA,但不知道应该用 sd-scripts、kohya GUI 还是普通 diffusers。"
            "sd-scripts README 说明它包含 Stable Diffusion 和其他图像生成模型的训练、生成和工具脚本,支持 LoRA、DreamBooth/finetune、Textual Inversion、模型转换和 LoRA 合并等。\n\n"
            "建议流程:\n"
            "1. 先确认底模类型:SD1.5、SDXL、FLUX 等不同模型对应不同训练脚本和配置。\n"
            "2. 先安装与实例 CUDA/显卡匹配的 PyTorch,再安装 sd-scripts requirements;README 明确 PyTorch 不放在 requirements 里,需要单独安装。\n"
            "3. 训练前准备图片、caption、输出目录和日志目录;路径尽量用绝对路径。\n"
            "4. OOM 时优先降低分辨率、batch、缓存策略和训练网络维度,再考虑 xformers/DeepSpeed/FP8 等高级配置。\n"
            "5. RTX 50 系等新显卡要特别确认 PyTorch/CUDA wheel 是否支持。\n\n"
            "注意:kohya GUI 只是更方便的入口,底层仍要理解 sd-scripts 的模型类型、数据集配置和日志。"
        ),
        source_refs=["sd-scripts-repo:README", "kohya-ss-repo:README"],
    ),
    chunk(
        chunk_id="ext-kohya-ss-gui-paths-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="kohya_ss GUI 训练目录和配置路径",
        question_patterns=[
            "kohya_ss 模型目录怎么填",
            "kohya gui output dataset logging 路径",
            "kohya lora 训练输出在哪里",
            "kohya_ss config toml 怎么用",
            "kohya 训练找不到数据集",
        ],
        content=(
            "适用场景:客户打开 kohya_ss 图形界面后,不知道 base model、dataset、output、log、LoRA 目录怎么填。"
            "kohya_ss README 建议用 `config.toml` 预填常用目录,包括 `model_dir`、`lora_model_dir`、`output_dir`、`dataset_dir`、DreamBooth 和 LoRA 相关路径。\n\n"
            "处理建议:\n"
            "1. 把底模目录、数据集目录、输出目录和日志目录分开,不要把训练输出写回底模目录。\n"
            "2. 使用绝对路径,避免 WebUI 工作目录变化后找不到文件。\n"
            "3. 数据集目录先用少量图片做一次 smoke run,确认 caption 和图片能被识别。\n"
            "4. 训练输出的 LoRA 文件再复制/挂载到 WebUI 或 ComfyUI 的 LoRA 目录中使用。\n"
            "5. 如果 GUI 中 GPU ID 选择无效,回到 accelerate 配置或启动日志确认实际使用的卡。\n\n"
            "注意:GUI 能降低操作门槛,但不会自动判断数据质量;caption 错、触发词混乱、重复样本过多都会直接影响 LoRA 效果。"
        ),
        source_refs=["kohya-ss-repo:README"],
    ),
    chunk(
        chunk_id="ext-diffusers-lora-dreambooth-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="Diffusers LoRA / DreamBooth 训练怎么选",
        question_patterns=[
            "diffusers lora 和 dreambooth 区别",
            "dreambooth 会不会比 lora 更吃显存",
            "diffusers 怎么训练图片 lora",
            "少量图片训练一个角色用什么",
            "lora rank alpha 怎么理解",
        ],
        content=(
            "适用场景:客户想用 Hugging Face Diffusers 训练图像模型,但不知道 LoRA、DreamBooth、全量微调怎么选。"
            "Diffusers 官方文档把 DreamBooth 描述为用少量主体或风格图片个性化文生图模型的训练方法;LoRA 文档则使用 PEFT 的 LoraConfig 设置 rank、alpha 和注入模块。\n\n"
            "选型建议:\n"
            "1. 只想训练小体积 adapter,优先 LoRA;便于分享、合并和在 WebUI/ComfyUI 里加载。\n"
            "2. 想强烈个性化某个主体或风格,可看 DreamBooth,但显存和过拟合风险通常更高。\n"
            "3. 数据很少时要控制训练步数和学习率,并保留验证图定期检查是否过拟合。\n"
            "4. LoRA rank/alpha 不是越大越好;先从小配置验证概念,再加训练预算。\n"
            "5. 训练脚本能跑不代表效果好,caption、触发词和数据清洗更重要。\n\n"
            "注意:Diffusers 脚本版本和底模版本要匹配;老教程里的参数名可能已经变化。"
        ),
        source_refs=["hf-diffusers:lora-training", "hf-diffusers:dreambooth-training"],
    ),
    chunk(
        chunk_id="ext-sd-dataset-caption-buckets-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="图像 LoRA 数据集、caption 和分辨率桶排查",
        question_patterns=[
            "lora 训练图片和 caption 怎么整理",
            "sdxl lora 数据集目录怎么放",
            "kohya bucket resolution 是什么",
            "训练 lora 出图不像是不是 caption 问题",
            "lora 训练重复次数怎么设",
        ],
        content=(
            "适用场景:LoRA 训练能跑完,但结果不像、学偏了、或者训练脚本读不到图片。"
            "sd-scripts 文档把数据集配置作为核心文档之一;实际训练中,图片、caption、分辨率和重复次数比单个命令更影响结果。\n\n"
            "排查顺序:\n"
            "1. 每张图片是否能和 caption 对上;caption 不要混入错误主体、错误风格或无关标签。\n"
            "2. 触发词是否稳定出现,且不会和普通描述词冲突。\n"
            "3. 图片分辨率和长宽比是否过于混乱;开启 bucketing 时仍要避免极端尺寸。\n"
            "4. 重复次数、batch 和 epoch 是否导致小数据集被过度训练。\n"
            "5. 先用 20-50 张样本跑小训练,确认数据读取和输出方向,再放大全量。\n\n"
            "注意:客户问“显卡没有跑满”时,也要检查磁盘读取、图片解码和数据增强,不一定是 GPU 故障。"
        ),
        source_refs=["sd-scripts-repo:README", "kohya-ss-repo:README"],
    ),
    chunk(
        chunk_id="ext-sd-lora-merge-convert-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="LoRA 合并、转换和使用前的兼容性检查",
        question_patterns=[
            "lora 训练完怎么合并",
            "kohya lora 怎么给 comfyui 用",
            "sd-scripts lora merge 是什么",
            "lora 和底模不匹配怎么办",
            "训练出来的 safetensors 加载失败",
        ],
        content=(
            "适用场景:客户训练完 LoRA 后,想合并到模型、转给 WebUI/ComfyUI 使用,或遇到加载失败。"
            "sd-scripts README 明确包含 LoRA merging、model conversion、image tagging 等工具;但合并和转换前必须先确认底模体系一致。\n\n"
            "处理建议:\n"
            "1. 先记录训练用底模、分辨率、网络类型和 LoRA 维度。\n"
            "2. 使用前确认目标工具支持该 LoRA 类型:SD1.5、SDXL、FLUX、LoCon/LoHA 等不可混用。\n"
            "3. 只做推理时优先不合并,直接加载 adapter;需要固定发布或减少加载步骤时再合并。\n"
            "4. 合并前备份原始底模和 LoRA;合并后用少量固定 prompt 回归效果。\n"
            "5. 加载失败先看文件是否完整、格式是否为 safetensors、底模是否匹配、节点/扩展是否过旧。\n\n"
            "注意:合并不会修复训练质量问题;过拟合、caption 错误和触发词污染要回到数据和训练参数处理。"
        ),
        source_refs=["sd-scripts-repo:README", "hf-diffusers:lora-training"],
    ),
    chunk(
        chunk_id="ext-faster-whisper-gpu-asr-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="faster-whisper 用 GPU 做语音转文字",
        question_patterns=[
            "faster whisper 怎么用 gpu",
            "whisper 转录怎么加速",
            "faster-whisper cuda 库缺失",
            "语音识别 int8 float16 怎么选",
            "长音频转文字显存不够怎么办",
        ],
        content=(
            "适用场景:客户做视频配音、会议转写或字幕生成,需要比 openai/whisper 更快的本地 ASR。"
            "faster-whisper README 说明它基于 CTranslate2 重实现 Whisper,可用 GPU FP16 或 INT8,并给出 `WhisperModel(model_size, device=\"cuda\", compute_type=\"float16\")` 示例。\n\n"
            "处理步骤:\n"
            "1. `pip install faster-whisper` 后先用短音频验证。\n"
            "2. GPU 模式需要 cuBLAS 和 cuDNN;README 提醒新版本 CTranslate2 需要 CUDA 12 和 cuDNN 9。\n"
            "3. 显存够时用 `compute_type=\"float16\"`;显存紧张时试 `int8_float16` 或更小模型。\n"
            "4. 批量转写可用 BatchedInferencePipeline 和 `batch_size`,吞吐更高但显存也会增加。\n"
            "5. 长视频先抽音频、切片、再合并字幕,避免一次性把超长音频塞进流程。\n\n"
            "注意:faster-whisper README 说明 PyAV 自带 FFmpeg 库,不一定要求系统安装 FFmpeg;但视频抽音频和封装字幕仍常需要 FFmpeg 命令行。"
        ),
        source_refs=["faster-whisper-repo:README"],
    ),
    chunk(
        chunk_id="ext-whisperx-alignment-diarization-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="WhisperX 做词级时间戳和说话人分离",
        question_patterns=[
            "whisperx 词级时间戳怎么用",
            "字幕对齐不准怎么办",
            "视频里多人说话怎么区分说话人",
            "whisperx diarization 需要什么",
            "语音转字幕时间轴漂移",
        ],
        content=(
            "适用场景:客户已经能转文字,但字幕时间戳不准,或需要区分不同说话人。"
            "WhisperX README 描述其提供 fast ASR、word-level timestamps 和 speaker diarization,通过 forced alignment 提高时间戳质量。\n\n"
            "使用建议:\n"
            "1. 先用基础 ASR 得到片段级转写,再做 alignment 获取词级时间戳。\n"
            "2. 需要说话人分离时再开启 diarization;这一步可能需要额外模型和认证,也会增加耗时。\n"
            "3. 长音频先按静音或固定时长切片,避免单段过长导致对齐失败。\n"
            "4. 背景音乐、人声重叠、噪声和压缩失真会明显降低对齐与分离质量。\n"
            "5. 视频字幕漂移时,同时检查原视频帧率、音频采样率、切片边界和最终封装命令。\n\n"
            "注意:WhisperX 解决的是 ASR 后处理和时间轴问题,不是 TTS 或声音克隆模型。"
        ),
        source_refs=["whisperx-repo:README"],
    ),
    chunk(
        chunk_id="ext-demucs-vocal-separation-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="Demucs 做人声/伴奏分离",
        question_patterns=[
            "demucs 怎么分离人声和伴奏",
            "视频配音怎么先提取人声",
            "伴奏和 vocals 怎么分开",
            "demucs 跑得慢怎么处理",
            "语音克隆前要不要先降噪分离",
        ],
        content=(
            "适用场景:客户做视频配音、翻唱、语音克隆前处理,需要把 vocals 和 accompaniment 分开。"
            "Demucs README 将其描述为 music source separation 模型,可把 drums、bass、vocals 和其他伴奏分离。\n\n"
            "处理建议:\n"
            "1. 先确认目标是人声分离、降噪还是配音替换;Demucs 主要是源分离,不是 ASR/TTS。\n"
            "2. 用短音频先跑一次,确认输出 stem 名称和质量。\n"
            "3. GPU 不够时降低并发、切分音频或改用更轻模型。\n"
            "4. 分离结果再进入 ASR/TTS/混音链路;不要直接把伴奏和新语音简单覆盖,要检查响度和时长。\n"
            "5. 音乐、人声重叠和混响强的素材可能会有残留或伪影。\n\n"
            "注意:源分离输出不是法律授权;克隆、翻唱和公开发布仍要确认素材与声音权限。"
        ),
        source_refs=["demucs-repo:README"],
    ),
    chunk(
        chunk_id="ext-ffmpeg-video-dubbing-chain-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="FFmpeg 处理视频配音链路的抽音频、切片和封装",
        question_patterns=[
            "视频配音怎么用 ffmpeg 抽音频",
            "长视频怎么切片再转字幕",
            "新配音怎么合回原视频",
            "音画不同步 ffmpeg 怎么排查",
            "字幕和配音时间轴怎么处理",
        ],
        content=(
            "适用场景:客户把视频交给 ASR/TTS/配音模型后,需要抽取音频、切片、混音或把新音频封装回视频。"
            "FFmpeg 官方文档说明它是通用媒体转换工具,可读入多种输入、filter、转码并输出;filter 文档覆盖多输入多输出的处理链。\n\n"
            "常见链路:\n"
            "1. 先抽音频到 wav,再送 ASR 或语音处理模型。\n"
            "2. 长视频按时间或静音切段,每段独立转写和合成,最后按时间轴合并。\n"
            "3. 新音频长度不一致时,先处理静音、语速、补空白和响度,不要直接强行覆盖。\n"
            "4. 封装回视频时保留原视频流,替换音频流,再检查总时长和首尾同步。\n"
            "5. 音画漂移时看采样率、帧率、时间基、切片边界和中间文件格式。\n\n"
            "注意:FFmpeg 只处理媒体容器和音视频流;ASR 错字、TTS 音色不像、对齐失败要回到对应模型链路排查。"
        ),
        source_refs=["ffmpeg-docs:ffmpeg", "ffmpeg-docs:filters"],
    ),
    chunk(
        chunk_id="ext-nerfstudio-install-custom-data-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="Nerfstudio / NeRF 场景重建的安装和自定义数据",
        question_patterns=[
            "nerfstudio 怎么安装",
            "nerf 重建需要 cuda 吗",
            "nerfstudio 自定义图片数据怎么准备",
            "colmap 数据怎么给 nerfstudio",
            "nerfstudio viewer 打不开",
        ],
        content=(
            "适用场景:客户从 3D 生成进一步进入 NeRF/三维重建,需要安装 Nerfstudio、处理自定义图片数据或打开 viewer。"
            "Nerfstudio 安装文档建议使用 conda 管理依赖;仓库 README 说明本地运行需要 NVIDIA 显卡和 CUDA。官方自定义数据文档会引导使用 COLMAP 处理相机位姿。\n\n"
            "处理顺序:\n"
            "1. 先确认实例有 NVIDIA GPU、驱动可见、PyTorch CUDA 正常。\n"
            "2. 用官方安装流程创建独立环境,不要混在已有 ComfyUI/训练环境里。\n"
            "3. 自定义图片先用 COLMAP 或 Nerfstudio 数据处理命令生成相机参数。\n"
            "4. 训练前检查图片数量、清晰度、重叠视角和 EXIF/相机信息。\n"
            "5. Viewer 打不开时先查监听地址、端口、SSH 转发或反向代理。\n\n"
            "注意:NeRF/3DGS 对数据采集质量很敏感;GPU 没报错也可能因为照片重叠不足而效果差。"
        ),
        source_refs=["nerfstudio-docs:installation", "nerfstudio-docs:custom-data"],
    ),
    chunk(
        chunk_id="ext-colmap-gpu-reconstruction-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="COLMAP 特征提取、匹配和 GPU/CPU 选择",
        question_patterns=[
            "colmap 为什么没有用 gpu",
            "colmap feature_extractor 怎么用 gpu",
            "nerf 数据预处理 colmap 卡住",
            "colmap 特征匹配很慢",
            "多张图片重建失败怎么办",
        ],
        content=(
            "适用场景:客户用 COLMAP 给 NeRF、Gaussian Splatting 或 Nerfstudio 生成相机位姿,遇到慢、GPU 不工作或重建失败。"
            "COLMAP CLI 文档说明如果没有 CUDA 设备,可用 `--FeatureExtraction.use_gpu 0` 和 `--SiftMatching.use_gpu 0` 手动选择 CPU 路径;FAQ 说明 SIFT 可用 GPU 或 CPU。\n\n"
            "排查建议:\n"
            "1. 先确认 COLMAP 构建/安装版本是否带 CUDA 支持。\n"
            "2. 用小数据集跑 feature extraction 和 matching,确认数据库能生成。\n"
            "3. 如果 GPU 不可用,显式切到 CPU,至少验证数据流程不是卡在 CUDA。\n"
            "4. 图片重叠不足、运动模糊、纯色背景、重复纹理都会导致 sparse reconstruction 失败。\n"
            "5. 大量图片时先分批或降低分辨率,避免特征匹配阶段耗尽内存。\n\n"
            "注意:COLMAP 的问题常是数据采集问题,不是单纯增加 GPU 就能解决。"
        ),
        source_refs=["colmap-docs:cli", "colmap-docs:faq"],
    ),
    chunk(
        chunk_id="ext-nerfstudio-splatfacto-gsplat-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="Nerfstudio Splatfacto / gsplat 训练 3D Gaussian",
        question_patterns=[
            "splatfacto 是什么",
            "nerfstudio 训练 gaussian splatting",
            "gsplat cuda 编译失败",
            "3d gaussian splatting 需要 colmap 数据吗",
            "splatfacto 训练显存不够",
        ],
        content=(
            "适用场景:客户想从多视角图片训练 3D Gaussian Splatting,而不只是用单图 TripoSplat。"
            "Nerfstudio Splatfacto 文档说明可安装 `gsplat`,相关 CUDA 代码会在第一次执行时编译;COLMAP 数据集或等价相机参数仍是常见输入。\n\n"
            "排查顺序:\n"
            "1. 先保证 Nerfstudio 环境和 PyTorch CUDA 正常。\n"
            "2. 第一次运行 gsplat 会编译 CUDA 代码,失败时看 CUDA toolkit、编译器和 PyTorch 版本是否匹配。\n"
            "3. 输入数据要有可靠相机位姿;COLMAP sparse 失败时不要继续训练。\n"
            "4. OOM 时降低图像分辨率、batch/采样配置或模型规模。\n"
            "5. 训练结果查看前先确认导出格式和 viewer 是否支持。\n\n"
            "注意:单图 3D、NeRF、3DGS 是不同链路;客户问“单张图能不能训练 Splatfacto”时要先解释数据需求。"
        ),
        source_refs=["nerfstudio-docs:splatfacto", "nerfstudio-docs:custom-data"],
    ),
    chunk(
        chunk_id="ext-pytorch3d-install-cuda-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="PyTorch3D 安装要匹配 PyTorch、CUDA 和编译环境",
        question_patterns=[
            "pytorch3d 安装失败",
            "pytorch3d cuda 编译不过",
            "3d computer vision 需要 pytorch3d",
            "pytorch3d 和 torch cuda 版本怎么匹配",
            "pytorch3d import 报错",
        ],
        content=(
            "适用场景:客户做 3D 视觉、网格渲染、点云或研究代码,安装 PyTorch3D 时卡在 CUDA/编译依赖。"
            "PyTorch3D README 说明它提供 PyTorch 中可复用的 3D Computer Vision 组件;安装细节在 INSTALL.md,其中强调 CUDA、PyTorch 和源码构建依赖要匹配。\n\n"
            "处理建议:\n"
            "1. 先确认当前 `torch.__version__` 和 `torch.version.cuda`。\n"
            "2. 优先选择与当前 PyTorch/CUDA 匹配的安装方式,不要盲目从源码编译。\n"
            "3. 源码编译失败时检查 CUDA toolkit、编译器、CUB 等依赖。\n"
            "4. 先 `python -c \"import pytorch3d\"` 验证导入,再跑项目完整训练。\n"
            "5. 不需要可微渲染或网格操作时,不要为普通图片任务强行安装 PyTorch3D。\n\n"
            "注意:PyTorch3D 的安装问题通常不是显卡库存问题,而是 Python/PyTorch/CUDA/编译工具链组合问题。"
        ),
        source_refs=["pytorch3d-repo:README", "pytorch3d-repo:INSTALL"],
    ),
    chunk(
        chunk_id="ext-open3d-pointcloud-visualization-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="Open3D 点云、网格和 3D 可视化基础",
        question_patterns=[
            "open3d 怎么查看点云",
            "ply 点云文件怎么处理",
            "open3d gpu 支持吗",
            "3d 模型生成后怎么可视化",
            "open3d 安装哪个包",
        ],
        content=(
            "适用场景:客户拿到 `.ply`、点云、mesh 或 3D 重建结果,需要在 Python 中读取、处理和可视化。"
            "Open3D README 说明它是 3D data processing 库,支持点云处理、mesh 操作、3D 可视化、3D ML,并提供 `pip install open3d` 和 `open3d-cpu` 安装方式。\n\n"
            "处理建议:\n"
            "1. 只做读取、转换和本地可视化时,先安装 `open3d` 或 CPU 版验证数据。\n"
            "2. 远程服务器没有桌面环境时,图形窗口可能打不开;可先导出文件再本地查看,或用无头/网页 viewer。\n"
            "3. GPU 加速不是所有 Open3D 操作默认可用;遇到慢先确认具体 API 是否支持 GPU。\n"
            "4. `.ply`、`.obj`、`.splat` 等格式不是完全等价,导入前确认 viewer 支持。\n"
            "5. 处理大点云前先做采样、裁剪和统计,避免内存占满。\n\n"
            "注意:Open3D 解决 3D 数据处理和可视化,不负责从图片自动生成高质量 3D 模型。"
        ),
        source_refs=["open3d-repo:README"],
    ),
    chunk(
        chunk_id="ext-monai-medical-imaging-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="MONAI 适合医学影像 AI 的 PyTorch 工作流",
        question_patterns=[
            "monai 是什么",
            "医学影像 dicom nifti 怎么训练",
            "3d 医学分割需要什么框架",
            "monai 和 pytorch 什么关系",
            "医疗影像训练显存不够怎么办",
        ],
        content=(
            "适用场景:客户做 CT/MRI/病理等医学影像分割、分类或检测,需要比普通图片训练更专业的数据处理。"
            "MONAI 官方文档将其描述为基于 PyTorch 的 healthcare imaging AI 框架,属于 PyTorch Ecosystem。\n\n"
            "处理建议:\n"
            "1. 先确认数据格式:DICOM、NIfTI、PNG/JPEG 切片的处理方式不同。\n"
            "2. 3D 医学影像常受显存限制,优先使用 patch/ROI crop、spacing/resize 和滑窗推理。\n"
            "3. 数据增强要符合医学语义,不要随意翻转或颜色扰动。\n"
            "4. 训练前记录 voxel spacing、方向、强度归一化和标签编码。\n"
            "5. 模型能跑不代表临床可用;评估指标、数据划分和隐私合规都要单独确认。\n\n"
            "注意:医学影像客户的问题往往不是“怎么装 CUDA”,而是数据格式、3D 显存和评估流程。"
        ),
        source_refs=["monai-docs:index"],
    ),
    chunk(
        chunk_id="ext-pyg-dgl-gnn-install-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="PyTorch Geometric / DGL 图神经网络安装和 GPU 注意事项",
        question_patterns=[
            "pytorch geometric 怎么安装 cuda 版本",
            "dgl 怎么用 gpu",
            "图神经网络训练显存不够",
            "torch_geometric import 报错",
            "gnn 大图训练怎么排查",
        ],
        content=(
            "适用场景:客户做推荐、知识图谱、分子图、交通网络等图神经网络任务,安装 PyG 或 DGL 时遇到 CUDA wheel 和大图显存问题。"
            "PyG 官方安装文档说明基础包可直接 `pip install torch_geometric`,部分扩展需要按 PyTorch/CUDA 版本选择 wheel。DGL README 说明可通过 pip/conda 安装,也提供 GPU enabled Docker 容器。\n\n"
            "处理建议:\n"
            "1. 先确认 PyTorch 和 CUDA 版本,再选 PyG/DGL 对应安装命令。\n"
            "2. 导入失败时看是否缺 `torch_scatter`、`torch_sparse` 等扩展,以及 wheel 是否匹配。\n"
            "3. 大图 OOM 先考虑采样、mini-batch、子图训练和特征压缩。\n"
            "4. 多 GPU 训练要确认框架本身支持的分布式模式,不要把普通 DataParallel 当成万能方案。\n"
            "5. 分子/蛋白图任务还要检查 RDKit/数据预处理链路。\n\n"
            "注意:图任务瓶颈常在邻居采样、CPU 内存和数据加载,不是只看 GPU 利用率。"
        ),
        source_refs=["pyg-docs:install", "dgl-repo:README"],
    ),
    chunk(
        chunk_id="ext-physicsnemo-sciml-gpu-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="NVIDIA PhysicsNeMo / Modulus 做物理机器学习",
        question_patterns=[
            "nvidia modulus physicsnemo 是什么",
            "物理仿真 ai 训练用什么框架",
            "pinn fno meshgraphnet 需要 gpu 吗",
            "physicsnemo 安装和 pytorch 什么关系",
            "ai4science 物理机器学习怎么开始",
        ],
        content=(
            "适用场景:客户做流体、天气、工程仿真、PDE 或 Physics-Informed ML,需要了解 PhysicsNeMo/Modulus 这类框架。"
            "PhysicsNeMo README 说明它是用于构建、训练、微调和推理 Physics AI 模型的开源深度学习框架,提供可扩展 GPU 优化训练库,并与 PyTorch 集成。\n\n"
            "使用建议:\n"
            "1. 先区分任务:PINN、FNO、MeshGraphNet、CFD、天气/气候等模型族的数据格式差别很大。\n"
            "2. 从官方 examples 或 model zoo 跑通最小案例,再替换成客户数据。\n"
            "3. 多 GPU 前先验证单卡数据管线、loss 和边界条件。\n"
            "4. 物理约束、网格、单位和归一化错误会比 CUDA 错误更难发现。\n"
            "5. 大规模训练再考虑分布式、混合精度和 checkpoint 策略。\n\n"
            "注意:PhysicsNeMo 是专业 AI4Science 工具,客户只做普通表格或图像训练时不必引入。"
        ),
        source_refs=["physicsnemo-repo:README"],
    ),
    chunk(
        chunk_id="ext-nvitop-user-gpu-monitor-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="nvitop / nvtop 做用户级 GPU 进程监控",
        question_patterns=[
            "nvitop 怎么看 gpu 占用",
            "比 nvidia-smi 更方便的监控工具",
            "训练到底哪个进程占显存",
            "gpu 利用率历史怎么看",
            "nvitop command not found",
        ],
        content=(
            "适用场景:客户想实时看 GPU 显存、利用率、进程树和历史曲线,`nvidia-smi` 信息不够直观。"
            "nvitop README 将其描述为 interactive NVIDIA-GPU process viewer,支持持续更新设备和进程状态,并可通过 PyPI/conda 安装。\n\n"
            "处理建议:\n"
            "1. 先用 `nvidia-smi` 确认驱动和 GPU 正常,再安装 nvitop。\n"
            "2. 安装方式可选 `pip3 install --upgrade nvitop`、conda 或 `uvx nvitop`。\n"
            "3. `command not found` 时检查 Python console script 路径是否加入 PATH。\n"
            "4. 多卡机器可按 GPU index/UUID 过滤,也可结合 `CUDA_VISIBLE_DEVICES` 看映射。\n"
            "5. 看到 GPU 利用率低时,同时看 CPU、内存、磁盘和 DataLoader,不要只盯显存。\n\n"
            "注意:nvitop 是观测工具,不能替代训练脚本日志和性能剖析工具。"
        ),
        source_refs=["nvitop-repo:README"],
    ),
    chunk(
        chunk_id="ext-tensorboard-pytorch-logging-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="PyTorch 用 TensorBoard 看 loss、图片和训练曲线",
        question_patterns=[
            "pytorch tensorboard 怎么用",
            "训练 loss 曲线怎么看",
            "tensorboard 远程打不开",
            "summarywriter 日志写到哪里",
            "训练图片怎么可视化",
        ],
        content=(
            "适用场景:客户训练模型时想看 loss、指标、图片样本或 embedding,判断训练是否正常。"
            "PyTorch 官方 TensorBoard 文档说明 `torch.utils.tensorboard` 可把模型和指标写入目录,再用 TensorBoard UI 可视化。\n\n"
            "最小流程:\n"
            "```\nfrom torch.utils.tensorboard import SummaryWriter\nwriter = SummaryWriter('runs/exp1')\nwriter.add_scalar('loss/train', loss, step)\nwriter.close()\n```\n"
            "启动查看:\n"
            "```\ntensorboard --logdir runs --host 0.0.0.0 --port 6006\n```\n"
            "远程打不开时先确认监听地址、端口开放或 SSH 转发。\n\n"
            "注意:TensorBoard 只展示脚本写进去的日志;没有调用 SummaryWriter 或日志目录写错时,页面会是空的。"
        ),
        source_refs=["pytorch-docs:tensorboard"],
    ),
    chunk(
        chunk_id="ext-wandb-mlflow-experiment-tracking-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="W&B / MLflow 做实验记录和模型追踪",
        question_patterns=[
            "wandb 怎么记录训练指标",
            "mlflow tracking server 怎么用",
            "训练实验怎么保存参数和模型",
            "wandb api key 在服务器怎么配置",
            "不想上传云端怎么做本地实验追踪",
        ],
        content=(
            "适用场景:客户反复训练模型,需要记录参数、loss、指标、模型文件和运行版本,避免只靠终端日志。"
            "W&B 官方 PyTorch 文档用于跟踪实验、数据和指标;MLflow Tracking 文档说明可记录参数、代码版本、指标和输出文件,PyTorch 集成支持 autolog。\n\n"
            "选择建议:\n"
            "1. 需要团队在线看板、曲线和 artifact 管理,可用 W&B,但要配置账号和 API key。\n"
            "2. 想本地或自托管,可用 MLflow Tracking Server。\n"
            "3. 任何方案都要记录随机种子、数据版本、代码提交、模型权重路径和环境。\n"
            "4. 不要把私密数据、密钥或客户样本直接上传到第三方实验平台。\n"
            "5. 训练崩溃后,先看实验记录是否保存到最后一步,再恢复 checkpoint。\n\n"
            "注意:实验追踪不是训练加速工具;它解决的是可复现和对比问题。"
        ),
        source_refs=["wandb-docs:quickstart", "wandb-docs:pytorch", "mlflow-docs:tracking", "mlflow-docs:pytorch"],
    ),
    chunk(
        chunk_id="ext-caddy-reverse-proxy-webui-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="Caddy 反向代理远程 WebUI",
        question_patterns=[
            "comfyui gradio 用 caddy 反向代理",
            "远程 webui 怎么加 https",
            "caddy reverse_proxy 怎么写",
            "jupyter gradio 通过域名访问",
            "webui 对外暴露安全吗",
        ],
        content=(
            "适用场景:客户已经有 Gradio、ComfyUI、Jupyter 或自写 FastAPI 页面,希望用域名/HTTPS 访问,而不是直接暴露裸端口。"
            "Caddy 官方 reverse proxy quick-start 说明它可快速启动生产可用的反向代理,`reverse_proxy` 指令可把请求代理到后端服务。\n\n"
            "最小 Caddyfile 思路:\n"
            "```\nexample.com {\n  reverse_proxy 127.0.0.1:7860\n}\n```\n"
            "处理步骤:\n"
            "1. 后端服务优先只监听 `127.0.0.1`,让 Caddy 对外。\n"
            "2. 确认域名解析、防火墙/安全组和 Caddy 监听端口。\n"
            "3. WebSocket 页面要确认代理支持升级连接。\n"
            "4. 给 Gradio/Jupyter/ComfyUI 加访问控制,不要把无鉴权页面直接暴露公网。\n\n"
            "注意:Caddy 解决代理和 HTTPS,不解决应用自身鉴权、资源隔离和多用户安全。"
        ),
        source_refs=["caddy-docs:reverse-proxy"],
    ),
    chunk(
        chunk_id="ext-tunnel-frp-tailscale-cloudflare-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="frp / Tailscale / Cloudflare Tunnel 暴露远程服务怎么选",
        question_patterns=[
            "没有公网端口怎么访问远程 webui",
            "frp 和 cloudflare tunnel 怎么选",
            "tailscale funnel 可以公开 gradio 吗",
            "反向隧道和 ssh -R 区别",
            "实例端口不能开怎么分享服务",
        ],
        content=(
            "适用场景:客户实例端口无法直接开放,或想临时把本地/远程 WebUI 分享出去。"
            "frp README 说明它是 fast reverse proxy,可把 NAT/防火墙后的本地服务暴露到公网。Tailscale Funnel 文档说明可把本地资源通过 Funnel URL 暴露到互联网。Cloudflare Tunnel 文档说明可用 cloudflared 连接服务,避免直接暴露公网 IP。\n\n"
            "选择建议:\n"
            "1. 有自有公网服务器且要 TCP/UDP 灵活转发,可用 frp。\n"
            "2. 团队内私有访问优先 Tailscale Serve;需要公开临时访问再看 Funnel。\n"
            "3. 已有 Cloudflare 域名和 Zero Trust 体系,可用 Cloudflare Tunnel。\n"
            "4. 只给自己访问 Jupyter/ComfyUI,SSH 本地端口转发往往更简单。\n"
            "5. 无论哪种方式,都要给 WebUI 加密码、token 或上游访问控制。\n\n"
            "注意:隧道让服务更容易被访问,也会放大未授权访问风险。"
        ),
        source_refs=["frp-repo:README", "tailscale-docs:funnel", "cloudflare-docs:tunnel", "openssh-docs:ssh"],
    ),
    chunk(
        chunk_id="ext-dockerfile-cuda-image-build-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="自制 CUDA 镜像的 Dockerfile 基础和体积控制",
        question_patterns=[
            "cuda dockerfile 怎么写",
            "自制镜像怎么预装模型和环境",
            "docker 镜像太大怎么清理",
            "nvidia cuda base image 怎么选",
            "docker build 缓存怎么优化",
        ],
        content=(
            "适用场景:客户从社区镜像进一步做自制镜像,想预装 CUDA、PyTorch、模型和 WebUI,但镜像很大或运行后看不到 GPU。"
            "Docker 官方构建最佳实践建议使用多阶段构建、缩小 build context、优化 cache。NVIDIA 深度学习容器用户指南介绍拉取、运行和扩展 GPU 优化容器。\n\n"
            "处理建议:\n"
            "1. 基础镜像先选和目标框架匹配的 CUDA/PyTorch 容器,不要在普通 Ubuntu 里手工拼一整套 CUDA。\n"
            "2. Dockerfile 中把系统依赖、Python 依赖、模型下载分层,减少反复重建。\n"
            "3. 使用 `.dockerignore` 排除数据集、输出图、缓存和无关文件。\n"
            "4. 安装后清理 apt cache、pip cache 和临时文件。\n"
            "5. 构建时不需要 GPU,运行时才需要 `--gpus all` 和 NVIDIA Container Toolkit。\n\n"
            "注意:镜像内 CUDA runtime 和宿主机驱动要兼容;镜像能 build 不代表 GPU 运行一定可用。"
        ),
        source_refs=["docker-docs:build-best-practices", "nvidia-docs:framework-containers", "docker-docs:engine-gpu"],
    ),
    chunk(
        chunk_id="ext-python-env-uv-micromamba-image-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="uv / micromamba 在镜像里管理 Python 环境",
        question_patterns=[
            "镜像里用 uv 还是 conda",
            "micromamba docker 怎么用",
            "uv pip sync 是什么",
            "python 依赖怎么固定到镜像",
            "conda 镜像构建太慢怎么办",
        ],
        content=(
            "适用场景:客户自制镜像时 Python 依赖安装慢、环境不可复现,或 conda 镜像体积过大。"
            "uv 官方文档说明它提供 `uv venv`、`uv sync`、`uv pip install/sync` 等能力;micromamba 文档提供更轻量的 conda 兼容环境管理方式。\n\n"
            "选择建议:\n"
            "1. 纯 pip/pyproject 项目可优先 uv,构建速度快,适合锁依赖和创建 venv。\n"
            "2. 依赖 conda-forge、CUDA 相关二进制包或科学计算包时,可考虑 micromamba。\n"
            "3. 镜像中固定 Python 版本、requirements/lock 文件和安装源。\n"
            "4. 不要在容器启动时才安装大依赖;应在 build 阶段完成。\n"
            "5. 运行前用一条最小 import 检查确认核心包可用。\n\n"
            "注意:环境工具不会自动解决 CUDA/PyTorch wheel 不匹配;这仍要按显卡和驱动选择。"
        ),
        source_refs=["uv-docs:features", "micromamba-docs:install", "pytorch-docs:get-started"],
    ),
    chunk(
        chunk_id="ext-hf-snapshot-preload-image-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="把 Hugging Face 模型预下载到镜像或数据盘",
        question_patterns=[
            "镜像里怎么预下载 huggingface 模型",
            "snapshot_download local_dir 怎么用",
            "模型每次启动都重新下载怎么办",
            "huggingface cache 放数据盘",
            "自制镜像预置大模型注意什么",
        ],
        content=(
            "适用场景:客户希望实例启动后不用再等模型下载,或者把模型缓存放到数据盘/镜像中。"
            "Hugging Face Hub 下载文档说明可下载单文件或整个仓库,`snapshot_download` 可把仓库快照下载到本地目录,也可指定 cache/local_dir。\n\n"
            "处理建议:\n"
            "1. 镜像构建阶段可预下载公开模型;受限模型需要 token,不建议把 token 写进镜像层。\n"
            "2. 大模型优先放数据盘或共享缓存,避免系统盘被占满。\n"
            "3. 固定 revision/commit,防止模型仓库更新导致结果不可复现。\n"
            "4. 下载后做文件数量和大小检查,避免只拿到 git-lfs 指针文件。\n"
            "5. 运行时通过模型本地路径启动 vLLM、ComfyUI 或训练脚本。\n\n"
            "注意:预下载能减少启动等待,但会增加镜像或数据盘体积;发布镜像前要确认授权和再分发许可。"
        ),
        source_refs=["hf-hub-docs:download", "hf-docs:environment_variables", "git-lfs:pull"],
    ),
]

SESSION_637_TARGETED_CHUNKS = [
    chunk(
        chunk_id="ext-a1111-webui-listen-port-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="AUTOMATIC1111 SD-WebUI 远程访问与端口排查",
        question_patterns=[
            "sd webui 启动了但是外部打不开",
            "automatic1111 怎么让别人访问",
            "stable diffusion webui 端口怎么改",
            "webui 只能 127.0.0.1 访问怎么办",
            "sd.webui 7860 refused to connect",
        ],
        content=(
            "适用场景:客户在实例里启动 AUTOMATIC1111 Stable Diffusion WebUI 后,本机或浏览器访问不到 7860 页面。"
            "A1111 官方 wiki 说明 `--listen` 会让服务监听网络连接,`--port xxxx` 可指定端口;README 说明该项目是基于 Gradio 的 Stable Diffusion Web UI。\n\n"
            "排查顺序:\n"
            "1. 先看启动日志里最终绑定地址,区分 `127.0.0.1:7860` 和 `0.0.0.0:7860`。\n"
            "2. 远程访问时启动参数应包含 `--listen`;需要固定端口时加 `--port 7860` 或其他未占用端口。\n"
            "3. 端口小于 1024 通常需要更高权限,优先用 7860、7861、3000 这类普通端口。\n"
            "4. 在实例内先 `curl http://127.0.0.1:7860` 验证进程是否真的响应。\n"
            "5. 再用 `ss -ltnp | grep 7860` 确认监听地址和进程。\n\n"
            "示例启动:\n"
            "```bash\n"
            "python launch.py --listen --port 7860\n"
            "```\n\n"
            "注意:外部仍访问不到时,不要只看 WebUI 日志;还要检查实例端口转发、反向代理或安全组/防火墙。公开暴露 WebUI 前应考虑认证、访问范围和数据风险。"
        ),
        source_refs=["a1111-repo:README", "a1111-wiki:command-line-arguments", "linux-man:ss", "curl-docs:manpage"],
    ),
    chunk(
        chunk_id="ext-a1111-model-paths-001",
        product_area="modelverse",
        source_type="faq",
        source_origin="external_official",
        title="AUTOMATIC1111 模型、LoRA 和 VAE 路径排查",
        question_patterns=[
            "sd webui 模型放进去看不到",
            "automatic1111 lora 放哪里",
            "stable diffusion webui checkpoint 目录",
            "vae 放到哪 webui 才能识别",
            "sd webui 想共用数据盘模型目录",
        ],
        content=(
            "适用场景:客户把 checkpoint、LoRA、VAE 或 embedding 放到服务器上,但 A1111 页面里没有出现。"
            "A1111 wiki 提供多种命令行路径参数,用于指定模型相关目录;项目 README 则说明 WebUI 支持 checkpoint、LoRA、Textual Inversion 等能力。\n\n"
            "处理建议:\n"
            "1. 先确认文件类型和用途:checkpoint/safetensors 不是 LoRA,LoRA 也不是 VAE。\n"
            "2. 默认目录通常在 WebUI 项目下的 `models/Stable-diffusion`、`models/Lora`、`models/VAE` 等位置;不同启动脚本和分支可能略有差异。\n"
            "3. 如果模型在数据盘,优先用启动参数指向外部目录,而不是复制多份大文件。\n"
            "4. 放好文件后重启 WebUI,或在页面里刷新对应模型列表。\n"
            "5. 文件只显示几百字节时,优先怀疑 git-lfs 指针文件,需要重新拉取真实权重。\n\n"
            "示例:\n"
            "```bash\n"
            "python launch.py --ckpt-dir /data/models/checkpoints --lora-dir /data/models/lora --vae-dir /data/models/vae\n"
            "```\n\n"
            "注意:模型目录参数只解决文件发现问题;如果模型和底模版本不匹配,仍可能出现加载错误或生成效果异常。"
        ),
        source_refs=["a1111-repo:README", "a1111-wiki:command-line-arguments", "git-lfs:pull"],
    ),
    chunk(
        chunk_id="ext-sd-webui-controlnet-install-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="A1111 ControlNet 扩展安装和模型缺失排查",
        question_patterns=[
            "sd webui controlnet 不显示",
            "automatic1111 controlnet 怎么安装",
            "controlnet 预处理器缺失",
            "controlnet 模型放哪里",
            "openpose controlnet 加载不了",
        ],
        content=(
            "适用场景:客户在 A1111 中找不到 ControlNet 面板、预处理器缺失,或 ControlNet 模型加载失败。"
            "sd-webui-controlnet README 说明它是 AUTOMATIC1111 的扩展,可从 Extensions 的 Install from URL 安装仓库。\n\n"
            "处理顺序:\n"
            "1. 确认基础 A1111 WebUI 能正常启动,再处理 ControlNet 扩展。\n"
            "2. 在 Extensions -> Install from URL 填入 `https://github.com/Mikubill/sd-webui-controlnet.git`,安装后重启 WebUI。\n"
            "3. 扩展目录应出现在 `extensions/sd-webui-controlnet` 下;如果安装中断,删除半成品目录后重新安装。\n"
            "4. 模型文件、预处理器依赖和 WebUI 版本要一起检查;只装扩展但没有对应 ControlNet 权重,页面能显示但无法正常出图。\n"
            "5. SD1.5、SDXL、Flux 等工作流使用的 ControlNet/预处理器并不完全相同,要按底模选择。\n\n"
            "注意:ControlNet 报错不一定是 GPU 问题;常见原因是扩展未重启、依赖未装完、模型目录不对或底模版本不匹配。"
        ),
        source_refs=["sd-webui-controlnet-repo:README", "a1111-repo:README"],
    ),
    chunk(
        chunk_id="ext-webui-refused-connection-debug-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="WebUI、Jupyter、Gradio 页面 refused to connect 的通用排查",
        question_patterns=[
            "网页 refused to connect 怎么排查",
            "webui 启动了浏览器打不开",
            "jupyter 页面打不开",
            "gradio 页面只能本机访问",
            "端口开了还是打不开",
        ],
        content=(
            "适用场景:客户看到 `ERR_CONNECTION_REFUSED`、`This site can't be reached`,或说 WebUI/Jupyter/Gradio 启动了但页面打不开。"
            "`ss` 文档说明它可查看 socket/监听端口;curl 可直接从实例内发起 HTTP 请求验证服务是否响应。\n\n"
            "排查顺序:\n"
            "1. 看进程日志确认服务是否还活着,不是只看曾经打印过 URL。\n"
            "2. 实例内访问本机地址: `curl -v http://127.0.0.1:PORT`。\n"
            "3. 看监听地址: `ss -ltnp | grep PORT`。如果只监听 `127.0.0.1`,外部无法直连。\n"
            "4. 对 WebUI/Gradio/A1111 一类服务,启动参数通常要绑定 `0.0.0.0` 或开启对应的 listen/server_name 参数。\n"
            "5. 如果实例内能访问、外部不能访问,再查端口转发、反向代理和网络安全策略。\n"
            "6. 如果实例内也拒绝连接,说明服务没有监听该端口或已经崩溃。\n\n"
            "常用命令:\n"
            "```bash\n"
            "ss -ltnp | grep 7860\n"
            "curl -v http://127.0.0.1:7860\n"
            "```\n\n"
            "注意:超时和拒绝连接含义不同。拒绝连接通常说明目标地址可达但端口没人接;超时更像网络路径、防火墙或安全策略问题。"
        ),
        source_refs=["linux-man:ss", "curl-docs:manpage", "a1111-wiki:command-line-arguments", "jupyter-server-docs:public-server", "gradio-docs:blocks-launch"],
    ),
    chunk(
        chunk_id="ext-ssh-connection-debug-001",
        product_area="login",
        source_type="runbook",
        source_origin="external_official",
        title="SSH 连不上时区分超时、拒绝连接和认证失败",
        question_patterns=[
            "ssh 连不上怎么办",
            "ssh connection timed out",
            "ssh connection refused",
            "ssh permission denied publickey",
            "vscode remote ssh 连接失败",
        ],
        content=(
            "适用场景:客户无法 SSH 登录实例,或 VS Code Remote SSH 失败。OpenSSH `ssh_config` 文档包含 `ConnectTimeout`、认证、连接参数等配置;`ssh -v` 可输出握手和认证细节。\n\n"
            "排查顺序:\n"
            "1. 先分清错误类型:timeout 多半是网络或端口路径问题;refused 通常是目标端口无 sshd 监听;Permission denied 多半是用户名、密钥或权限问题。\n"
            "2. 用 `ssh -vvv -p PORT user@HOST` 收集详细日志。\n"
            "3. 检查端口、用户名、密钥文件是否和平台给出的登录命令一致。\n"
            "4. 私钥权限过宽时,OpenSSH 可能拒绝使用;Linux/macOS 上可 `chmod 600 key.pem`。\n"
            "5. 连接不稳定时,再考虑 `ServerAliveInterval`、`ServerAliveCountMax` 或 VS Code Remote 的重连配置。\n\n"
            "示例:\n"
            "```bash\n"
            "ssh -vvv -o ConnectTimeout=15 -p 22 root@example.com\n"
            "```\n\n"
            "注意:不要把密码、私钥、完整公网 IP 和登录命令贴进公开环境。"
        ),
        source_refs=["openssh-docs:ssh_config", "openssh-docs:ssh-keygen", "vscode-docs:remote-ssh"],
    ),
    chunk(
        chunk_id="ext-scp-rsync-transfer-debug-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="scp/rsync 传文件失败、断开和权限错误排查",
        question_patterns=[
            "本地文件怎么传到实例",
            "scp 传文件 permission denied",
            "rsync 中断了怎么继续",
            "上传大文件老是断",
            "传输文件打不开",
        ],
        content=(
            "适用场景:客户能 SSH 登录,但上传数据集、模型或结果文件失败。scp 文档说明它通过网络复制文件;rsync 文档支持增量复制、保留属性和断点/部分文件相关选项。\n\n"
            "处理建议:\n"
            "1. 小文件先用 scp 验证链路和路径,大目录优先用 rsync。\n"
            "2. Permission denied 先确认远端目录存在且当前用户有写权限。\n"
            "3. 大文件/大目录中断后,用 rsync 重新执行同一命令,它会按差异继续传。\n"
            "4. 目录路径要注意末尾 `/` 的语义:同步目录本身和同步目录内容不同。\n"
            "5. 传完后用 `ls -lh`、`du -sh`、校验和或文件数量检查确认。\n\n"
            "示例:\n"
            "```bash\n"
            "scp -P 22 local.tar root@example.com:/data/\n"
            "rsync -avP -e \"ssh -p 22\" ./dataset/ root@example.com:/data/dataset/\n"
            "```\n\n"
            "注意:如果 scp/rsync 都超时,问题通常不在文件工具本身,应回到 SSH 连接和网络路径排查。"
        ),
        source_refs=["openssh-docs:scp", "rsync-docs:man", "openssh-docs:ssh_config"],
    ),
    chunk(
        chunk_id="ext-ollama-model-cache-repair-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="Ollama 模型下载、缓存目录和损坏 blob 排查",
        question_patterns=[
            "ollama run 500 unable to load model",
            "ollama blob 文件损坏怎么办",
            "ollama 模型下完了还是找不到",
            "ollama 模型缓存目录在哪",
            "ollama pull 中断后怎么修复",
        ],
        content=(
            "适用场景:客户运行 `ollama run` 时模型加载失败、HTTP 500、提示 blob/sha256 文件相关错误,或模型下载后列表里看不到。"
            "Ollama FAQ 说明模型默认存放路径和 `OLLAMA_MODELS` 可改缓存位置;CLI 文档提供 pull/run/list 等模型管理入口。\n\n"
            "排查顺序:\n"
            "1. 先看 `ollama list` 是否能列出目标模型,再用 `ollama show MODEL` 看模型元信息。\n"
            "2. 确认当前服务使用的 `OLLAMA_MODELS` 目录和你下载模型时使用的是同一个目录。\n"
            "3. 检查磁盘是否满、下载是否中断、模型 tag 是否写错。\n"
            "4. 对疑似损坏或半下载模型,优先用 Ollama 命令删除后重新 `ollama pull MODEL`,不要随手删除未知共享目录。\n"
            "5. 重新下载前可停止正在使用该模型的会话,避免文件被占用。\n\n"
            "示例:\n"
            "```bash\n"
            "ollama list\n"
            "ollama show qwen2.5:7b\n"
            "ollama pull qwen2.5:7b\n"
            "```\n\n"
            "注意:修改 `OLLAMA_MODELS` 后需要确保启动 Ollama 服务的进程也带着同一个环境变量,否则会出现不同命令看到不同模型目录的现象。"
        ),
        source_refs=["ollama-docs:faq", "ollama-docs:cli", "ollama-docs:api/openai-compatibility"],
    ),
    chunk(
        chunk_id="ext-ollama-context-vram-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="Ollama 上下文长度、显存占用和模型常驻排查",
        question_patterns=[
            "ollama 上下文调大后显存不够",
            "ollama num_ctx 怎么设置",
            "ollama 为什么一直占显存",
            "ollama 模型怎么卸载出显存",
            "ollama 长上下文需要多少显存",
        ],
        content=(
            "适用场景:客户使用 Ollama 跑长上下文、代码助手或多轮对话时显存突然上涨,或者模型退出后显存仍被占用。"
            "Ollama 上下文长度文档说明更大的 context length 会增加所需内存/显存;FAQ 说明模型默认会在内存中保留一段时间以加快后续请求。\n\n"
            "处理建议:\n"
            "1. 先确认模型大小、量化等级、上下文长度和并发请求数,不要只看参数量。\n"
            "2. 长上下文会显著增加 KV cache 占用;显存紧张时先降低 context length。\n"
            "3. 用 `ollama ps` 查看当前加载中的模型。\n"
            "4. 不再使用时用 `ollama stop MODEL` 卸载模型,再观察 `nvidia-smi`。\n"
            "5. 如果只需要短问答,不要把 context length 设得过大。\n\n"
            "示例:\n"
            "```bash\n"
            "ollama ps\n"
            "ollama stop qwen2.5:7b\n"
            "```\n\n"
            "注意:上下文长度、并发数、GPU offload 和模型量化会一起影响显存;单独换端口或重启前端并不会降低模型本身的显存需求。"
        ),
        source_refs=["ollama-docs:context-length", "ollama-docs:faq", "nvidia-docs:nvidia-smi"],
    ),
    chunk(
        chunk_id="ext-openwebui-ollama-openai-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="Open WebUI 连接 Ollama 或 OpenAI 兼容接口",
        question_patterns=[
            "open webui 怎么接 ollama",
            "openwebui 怎么接 vllm",
            "open webui 连接 openai compatible api",
            "想给团队一个网页聊天界面",
            "ollama 前端页面怎么部署",
        ],
        content=(
            "适用场景:客户已经有 Ollama、vLLM、SGLang 或其他 OpenAI 兼容服务,想给团队一个可用的 Web 聊天界面。"
            "Open WebUI 官方文档说明它支持 Ollama 和 OpenAI-compatible APIs,Quick Start 推荐 Docker,也支持 Python 和 Kubernetes。\n\n"
            "处理建议:\n"
            "1. 先确认后端模型服务可用:Ollama 默认 11434,vLLM/SGLang 常见为 OpenAI 兼容 `/v1` 接口。\n"
            "2. Open WebUI 容器和模型服务在同一机器时,要注意容器内访问宿主机地址不能总是写 `localhost`。\n"
            "3. 接 vLLM/SGLang 时按 OpenAI-compatible API 填 base URL 和 API key。\n"
            "4. 页面打不开时按 WebUI 端口监听、容器端口映射、反向代理顺序排查。\n"
            "5. 面向团队使用时要配置用户、权限和访问边界,不要裸露内部模型接口。\n\n"
            "注意:Open WebUI 是前端/管理层,不会自动解决底层模型显存不足、模型加载失败或推理服务未启动的问题。"
        ),
        source_refs=["openwebui-docs:quick-start", "openwebui-repo:README", "ollama-docs:api/openai-compatibility"],
    ),
    chunk(
        chunk_id="ext-litellm-proxy-openai-compatible-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="LiteLLM 代理自托管 OpenAI 兼容模型服务",
        question_patterns=[
            "多个模型服务怎么统一成 openai 接口",
            "litellm 怎么代理 vllm",
            "openai compatible endpoint 怎么配置 litellm",
            "想给团队一个统一模型网关",
            "base_url 不同的模型怎么统一调用",
        ],
        content=(
            "适用场景:客户已经有 vLLM、SGLang、Ollama 或其他 OpenAI 兼容服务,希望统一成一个团队可调用的模型网关。"
            "LiteLLM 文档说明 Proxy 是自托管 OpenAI-compatible gateway,OpenAI-compatible provider 文档说明可在 `model_list` 中配置 `openai/` 前缀、`api_base` 和 `api_key`。\n\n"
            "处理建议:\n"
            "1. 先确保后端模型服务自身可用,再接入 LiteLLM。\n"
            "2. 在 `config.yaml` 的 `model_list` 中给每个后端起一个对外模型名。\n"
            "3. OpenAI 兼容后端通常用 `model: openai/<backend-model-name>` 和 `api_base: http://host:port/v1`。\n"
            "4. 启动代理后,用标准 OpenAI SDK 指向 LiteLLM 的 base_url 测试。\n"
            "5. 团队使用时再补鉴权、限流、预算和日志,不要一开始就把网关直接公开。\n\n"
            "示例配置片段:\n"
            "```yaml\n"
            "model_list:\n"
            "  - model_name: qwen-local\n"
            "    litellm_params:\n"
            "      model: openai/qwen-local\n"
            "      api_base: http://127.0.0.1:8000/v1\n"
            "      api_key: sk-local\n"
            "```\n\n"
            "注意:LiteLLM 解决接口统一和治理问题,不负责替后端模型省显存或修复 CUDA 错误。"
        ),
        source_refs=["litellm-docs:getting-started", "litellm-docs:openai-compatible"],
    ),
    chunk(
        chunk_id="ext-docker-gpu-visible-devices-advanced-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="Docker 容器中 GPU 不可见或只看到部分卡",
        question_patterns=[
            "docker 容器里只看到几张 gpu",
            "容器里 nvidia-smi no devices were found",
            "nvidia visible devices 怎么设置",
            "配置 8 张卡容器里只有 4 张",
            "docker --gpus all 还是看不到显卡",
        ],
        content=(
            "适用场景:宿主机能看到 GPU,但容器里 `nvidia-smi` 看不到卡、只看到部分卡,或训练进程报 no device found。"
            "NVIDIA Container Toolkit 文档说明 Docker 可用 `--gpus` 或 `NVIDIA_VISIBLE_DEVICES` 指定容器可见 GPU;安装文档说明需要配置容器运行时。\n\n"
            "排查顺序:\n"
            "1. 宿主机先跑 `nvidia-smi`,确认驱动和 GPU 正常。\n"
            "2. 用最小 CUDA 镜像验证容器运行时: `docker run --rm --gpus all nvidia/cuda:12.9.0-base-ubuntu22.04 nvidia-smi`。\n"
            "3. 检查启动参数是否限制了 `--gpus`、`NVIDIA_VISIBLE_DEVICES` 或 `CUDA_VISIBLE_DEVICES`。\n"
            "4. 如果只看到部分卡,比较宿主机 GPU 编号、容器可见编号和应用内部编号。\n"
            "5. Docker runtime 没配置时,按 NVIDIA Container Toolkit 安装文档配置并重启 Docker。\n\n"
            "注意:容器内 GPU 编号可能会重新从 0 开始;应用日志里的 cuda:0 不一定是宿主机物理 0 号卡。"
        ),
        source_refs=["nvidia-docs:docker-specialized", "nvidia-docs:container-toolkit-sample", "docker-docs:engine-gpu"],
    ),
    chunk(
        chunk_id="ext-gpu-card-count-mismatch-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="多卡机器实际可见卡数和预期不一致",
        question_patterns=[
            "明明是 8 张卡为什么只显示 4 张",
            "torch 只看到部分 gpu",
            "cuda visible devices 限制了卡数",
            "nvidia-smi 和 torch 卡数不一致",
            "程序只用了第一张卡",
        ],
        content=(
            "适用场景:客户购买或启动了多卡环境,但 `nvidia-smi`、PyTorch、容器或应用日志显示的卡数不一致。"
            "PyTorch CUDA 文档说明可用 CUDA 设备接口检查可用设备;NVIDIA 容器文档说明可见设备也可能被容器参数限制。\n\n"
            "处理建议:\n"
            "1. 分层确认:宿主机 `nvidia-smi`、容器内 `nvidia-smi`、Python `torch.cuda.device_count()` 分别看多少。\n"
            "2. 检查 `CUDA_VISIBLE_DEVICES` 是否只暴露了部分卡。\n"
            "3. 容器场景检查 `--gpus`、`NVIDIA_VISIBLE_DEVICES` 和 compose/k8s 资源声明。\n"
            "4. 分布式训练还要检查启动进程数和 rank/world size,不是看到多卡就会自动并行。\n"
            "5. 如果宿主机层就缺卡,再进入驱动、硬件或平台资源状态排查。\n\n"
            "最小检查:\n"
            "```bash\n"
            "nvidia-smi\n"
            "python - <<'PY'\n"
            "import torch\n"
            "print(torch.cuda.is_available(), torch.cuda.device_count())\n"
            "PY\n"
            "```\n\n"
            "注意:不要把宿主机卡号、容器内卡号和框架逻辑卡号混为一谈。"
        ),
        source_refs=["pytorch-docs:notes/cuda", "nvidia-docs:nvidia-smi", "nvidia-docs:docker-specialized"],
    ),
    chunk(
        chunk_id="ext-nvidia-mps-single-node-sharing-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="NVIDIA MPS 单机多进程共享 GPU 的适用边界",
        question_patterns=[
            "一张 gpu 能不能给多个进程同时用",
            "nvidia mps 是什么",
            "多个人共用一张卡怎么减少互相影响",
            "小任务 gpu 利用率低能不能开 mps",
            "mps 和 mig 有什么区别",
        ],
        content=(
            "适用场景:客户希望一张 GPU 同时跑多个小 CUDA 进程,或多人共享单卡但又不想上完整 Kubernetes/MIG。"
            "NVIDIA MPS 文档说明 MPS 是轻量运行时服务,用于让 CUDA 多进程/多应用工作流协同使用 GPU。\n\n"
            "判断方式:\n"
            "1. MPS 更适合单进程占不满 GPU 的小 kernel/MPI 类任务,不是万能的显存隔离工具。\n"
            "2. 多个任务仍共享同一张卡的显存,其中一个进程 OOM 仍可能影响整体体验。\n"
            "3. 需要硬隔离或明确切分资源时,优先看 MIG 或调度系统资源限制。\n"
            "4. 开 MPS 前先用 nvidia-smi/nvitop 证明 GPU 利用率低而不是数据加载、CPU 或 I/O 卡住。\n"
            "5. 生产环境要明确谁启动/停止 MPS control daemon,避免用户互相影响。\n\n"
            "注意:MPS 是 CUDA 运行时层能力,不是 WebUI 或 PyTorch 参数。是否收益明显取决于 workload 形态。"
        ),
        source_refs=["nvidia-docs:mps", "nvidia-docs:mig-user-guide", "nvitop-repo:README"],
    ),
    chunk(
        chunk_id="ext-dvc-data-versioning-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="用 DVC 管理训练数据和模型文件版本",
        question_patterns=[
            "训练数据怎么做版本管理",
            "数据集太大不想放 git",
            "dvc remote 怎么配置",
            "团队怎么共享数据版本",
            "模型文件和代码版本怎么对应",
        ],
        content=(
            "适用场景:客户的训练数据、模型权重或实验产物太大,不适合直接放进 Git,但又希望能复现实验。"
            "DVC 文档说明它可配置 remote storage,支持 S3、NFS、SSH、Google Drive、Azure Blob、HDFS 等远端存储,并通过 push/pull 共享数据和模型。\n\n"
            "处理建议:\n"
            "1. Git 管代码和小的元数据,DVC 管大数据和模型文件。\n"
            "2. 先 `dvc init`,再 `dvc add data/` 生成跟踪文件。\n"
            "3. 配置 remote 后用 `dvc push` 上传,新机器用 `dvc pull` 拉取。\n"
            "4. 训练脚本记录 Git commit、DVC 文件和关键参数,方便复现。\n"
            "5. 对多人团队,remote 权限和容量配额要提前规划。\n\n"
            "示例:\n"
            "```bash\n"
            "dvc init\n"
            "dvc add data/train\n"
            "dvc remote add -d storage s3://bucket/path\n"
            "dvc push\n"
            "```\n\n"
            "注意:DVC 解决版本和共享问题,不等于备份策略;重要数据仍应有独立备份和权限控制。"
        ),
        source_refs=["dvc-docs:start", "dvc-docs:remote-storage"],
    ),
    chunk(
        chunk_id="ext-s5cmd-minio-object-transfer-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="用 s5cmd 或 MinIO mc 处理对象存储大批量传输",
        question_patterns=[
            "对象存储数据怎么同步到实例",
            "s3 大量小文件怎么快点传",
            "s5cmd 怎么批量复制",
            "minio mc mirror 怎么用",
            "训练数据在桶里怎么拉下来",
        ],
        content=(
            "适用场景:客户的数据集在 S3/兼容对象存储里,需要批量复制到 GPU 实例,或把结果同步回桶。"
            "s5cmd README 说明它是快速的 S3/本地文件系统执行工具,支持通配符和多种操作;MinIO mc README 说明 mc 提供类似 Unix 的对象存储工具,mc mirror 文档说明可同步目录和桶。\n\n"
            "处理建议:\n"
            "1. 大量小文件优先考虑对象存储专用工具,不要用低并发脚本逐个下载。\n"
            "2. s5cmd 适合高并发 cp/sync/批处理;mc 适合 MinIO/S3 兼容场景和 mirror 工作流。\n"
            "3. 先小范围 dry run 或列目录确认路径,避免把桶根目录同步错。\n"
            "4. 同步完成后检查文件数量、总大小和关键样本。\n"
            "5. 训练时优先从本地盘读取热点数据,不要每个 batch 都跨网络读对象存储。\n\n"
            "示例:\n"
            "```bash\n"
            "s5cmd cp 's3://bucket/dataset/*' ./dataset/\n"
            "mc mirror myminio/bucket/dataset ./dataset\n"
            "```\n\n"
            "注意:对象存储工具需要凭证和 endpoint 配置;不要把 access key 写进提交的脚本或镜像层。"
        ),
        source_refs=["s5cmd-repo:README", "minio-mc-repo:README", "minio-docs:mc-mirror"],
    ),
    chunk(
        chunk_id="ext-label-studio-dataset-annotation-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="用 Label Studio 做图像、文本、音频和视频标注",
        question_patterns=[
            "训练前数据怎么标注",
            "label studio 支持哪些数据类型",
            "图片视频语音标注工具",
            "标注结果怎么导出训练",
            "想做自己的微调数据集",
        ],
        content=(
            "适用场景:客户准备做微调、视觉训练、ASR/TTS 或多模态任务,需要把原始数据整理成可训练标签。"
            "Label Studio README 说明它是开源数据标注工具,支持音频、文本、图像、视频和时间序列;官方指南覆盖创建项目、导入数据、标注和导出。\n\n"
            "处理建议:\n"
            "1. 先确定任务类型:分类、检测、分割、转录、问答、偏好数据等对应不同标注模板。\n"
            "2. 数据量大时优先把原始文件放对象存储或共享盘,标注任务只引用路径/URL。\n"
            "3. 建项目后先用少量样本试标,确认标签体系和导出格式适合训练脚本。\n"
            "4. 有模型初筛结果时,可导入预标注结果再人工校对。\n"
            "5. 导出后记录数据版本、标注模板和标签含义,避免后续训练不可复现。\n\n"
            "注意:标注工具不会自动提高数据质量;标签定义、抽检和一致性检查仍然是训练效果的关键。"
        ),
        source_refs=["label-studio-repo:README", "label-studio-docs:guide"],
    ),
    chunk(
        chunk_id="ext-model-files-persistence-boundary-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="实例、容器和数据盘中的文件保留边界",
        question_patterns=[
            "重启后下载的模型还在吗",
            "容器重建后文件会不会丢",
            "数据应该放系统盘还是数据盘",
            "停止实例后训练结果保留吗",
            "镜像里装的软件和数据盘文件有什么区别",
        ],
        content=(
            "适用场景:客户担心模型、数据集、训练结果在重启、停止、重建容器或制作镜像后丢失。"
            "外部通用原则是:文件是否保留取决于它写在持久卷/数据盘/对象存储,还是写在一次性容器层或临时目录。Docker 和数据管理工具都遵循这个边界。\n\n"
            "说明方式:\n"
            "1. 先问清楚操作类型:重启进程、重启容器、重启机器、停止实例、删除实例、重建镜像不是一回事。\n"
            "2. 训练数据、模型和结果优先写到明确的持久路径,例如挂载盘、对象存储或 DVC remote。\n"
            "3. 容器内部临时层适合缓存和运行时文件,不适合唯一保存实验结果。\n"
            "4. 制作镜像通常适合固化环境和代码,大数据更适合放外部持久存储。\n"
            "5. 关键结果写完后立刻做 `ls/du` 和远端同步检查。\n\n"
            "注意:这条是通用外部知识,具体某个平台停止/释放资源后的保留规则必须以平台文档和当前资源状态为准。"
        ),
        source_refs=["docker-docs:build-best-practices", "dvc-docs:remote-storage", "s5cmd-repo:README", "minio-mc-repo:README"],
    ),
    chunk(
        chunk_id="ext-systemd-user-service-webui-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="把 WebUI 或模型服务做成可重启的后台服务",
        question_patterns=[
            "ssh 断开后 webui 就停了怎么办",
            "怎么让服务开机自动启动",
            "模型服务崩了能不能自动拉起",
            "后台运行 webui 比 nohup 稳定吗",
            "systemd 管理 gradio 服务",
        ],
        content=(
            "适用场景:客户用 SSH 手动启动 WebUI、Ollama、vLLM 或 Gradio 后,断开连接或进程崩溃导致服务不可用。"
            "Linux 后台任务可用 tmux/nohup 简化处理;更长期的服务化运行通常需要 supervisor 或 systemd 这类进程管理方式。\n\n"
            "处理建议:\n"
            "1. 临时调试用 tmux,方便回到会话看日志。\n"
            "2. 长期运行用 systemd/supervisor 管理,明确 working directory、环境变量、启动命令和日志。\n"
            "3. 服务启动前先把命令在 shell 里跑通,再写成 unit。\n"
            "4. WebUI 仍要绑定正确地址和端口,服务化只保证进程管理,不保证网络可访问。\n"
            "5. 显存不足、模型文件缺失这类错误不会因为 systemd 自动变好,只能从日志继续排查。\n\n"
            "最小思路:\n"
            "```bash\n"
            "tmux new -s webui\n"
            "python launch.py --listen --port 7860\n"
            "```\n\n"
            "注意:用户级服务和系统级服务的权限、环境变量、PATH 不同;同一条命令在 shell 能跑,写进服务后也要重新验证。"
        ),
        source_refs=["tmux-wiki:getting-started", "linux-man:nohup", "a1111-wiki:command-line-arguments"],
    ),
]

CUDA_COMPILE_CLEANUP_CHUNKS = [
    chunk(
        chunk_id="ext-nvidia-smi-safe-cleanup-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="nvidia-smi 显存清理的安全流程",
        question_patterns=[
            "nvidia-smi cleanup 怎么做",
            "显存被占满了怎么安全清理",
            "nvidia-smi 看到进程可以直接 kill 吗",
            "gpu-reset 什么时候能用",
            "怎么确认占显存的进程是不是自己的",
        ],
        content=(
            "适用场景:客户看到 `nvidia-smi` 里有进程占用显存,想释放 GPU,但不确定能不能直接 kill。"
            "NVIDIA nvidia-smi 文档说明 `--gpu-reset` 可重置 GPU 状态,但需要 root,且目标 GPU 上不能有任何应用占用;它通常用于硬件/驱动状态问题,不是常规清显存命令。\n\n"
            "安全流程:\n"
            "1. 用 `nvidia-smi` 找到占显存的 PID、GPU 编号和显存占用。\n"
            "2. 用 `ps -fp PID` 或 `pwdx PID` 确认进程命令、用户和工作目录,不要只凭 PID 判断。\n"
            "3. 如果是自己启动的训练或 WebUI,优先用程序内停止、Ctrl-C、服务停止命令或 `ollama stop MODEL` 等正常方式退出。\n"
            "4. 正常退出无效时,再对自己的无用进程使用 `kill PID`;仍不退出时再考虑 `kill -9 PID`。\n"
            "5. 多用户机器上,不要杀其他用户进程;先联系进程所有者或管理员。\n"
            "6. `nvidia-smi --gpu-reset` 不是清理别人的进程,也不是 OOM 通用解法;有进程占用时它会失败。\n\n"
            "常用命令:\n"
            "```bash\n"
            "nvidia-smi\n"
            "ps -fp <PID>\n"
            "kill <PID>\n"
            "```\n\n"
            "注意:PyTorch 缓存显存、Ollama 常驻模型、vLLM/ComfyUI 服务进程都可能让显存看起来一直占着。先确认进程用途,再决定停止方式。"
        ),
        source_refs=["nvidia-docs:nvidia-smi", "pytorch-docs:notes/cuda", "ollama-docs:faq"],
    ),
    chunk(
        chunk_id="ext-pytorch-torch-compile-basic-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="torch.compile 的适用场景、最小用法和回退方式",
        question_patterns=[
            "torch.compile 有什么用",
            "pytorch 2 compile 怎么开",
            "torch.compile 第一次为什么很慢",
            "torch.compile 怎么回退 eager",
            "训练脚本能不能直接加 torch.compile",
        ],
        content=(
            "适用场景:客户想用 PyTorch 2 的 `torch.compile` 提升模型训练或推理速度,但不知道放在哪里、为什么第一次慢、出错后怎么回退。"
            "PyTorch 官方教程说明可用 `torch.compile(model)` 包装模型;第一次调用会触发编译,所以首轮通常比后续慢。\n\n"
            "处理建议:\n"
            "1. 先在不使用 `torch.compile` 的 eager 模式下跑通脚本,确认结果正确。\n"
            "2. 再只包装模型或热点函数: `model = torch.compile(model)`。\n"
            "3. 第一次迭代包含编译开销,测速时要跳过 warmup。\n"
            "4. 对输入 shape 稳定、重复执行的模型更容易受益;控制流复杂、shape 频繁变化、Python 逻辑多的代码收益可能有限。\n"
            "5. 出现编译错误、结果异常或调试困难时,先去掉 `torch.compile` 回到 eager,证明问题是否由编译引入。\n\n"
            "最小示例:\n"
            "```python\n"
            "import torch\n"
            "model = MyModel().cuda()\n"
            "model = torch.compile(model)\n"
            "out = model(x.cuda())\n"
            "```\n\n"
            "注意:`torch.compile` 不是省显存开关。它可能改变执行图、编译缓存和峰值内存表现,不能替代 batch size、混合精度或模型量化。"
        ),
        source_refs=["pytorch-docs:torch-compile-tutorial", "pytorch-docs:torch-compiler-troubleshooting"],
    ),
    chunk(
        chunk_id="ext-pytorch-torch-compile-debug-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="torch.compile graph break、动态 shape 和反复编译排查",
        question_patterns=[
            "torch.compile graph break 怎么查",
            "torch.compile 一直 recompiling",
            "torch.compile dynamic shape 怎么处理",
            "torch compile 报 torchdynamo unsupported",
            "inductor 编译慢怎么定位",
        ],
        content=(
            "适用场景:`torch.compile` 后运行变慢、反复编译、日志出现 graph break、TorchDynamo unsupported,或输入 shape 变化导致不稳定。"
            "PyTorch troubleshooting 文档说明可用 `TORCH_LOGS=\"graph_breaks\"` 查看 graph break;官方动态 shape 文档说明 shape 变化可能触发重新编译,可考虑 `dynamic=True`。\n\n"
            "排查顺序:\n"
            "1. 用小输入和少量 step 复现问题,先确认 eager 模式正常。\n"
            "2. 开 `TORCH_LOGS=\"graph_breaks\"` 看是否有 Python 控制流、`Tensor.item()`、数据依赖分支等导致图断裂。\n"
            "3. 输入尺寸经常变化时,尝试 `torch.compile(model, dynamic=True)` 或固定 batch/sequence/image shape。\n"
            "4. 用 `fullgraph=True` 可强制暴露 graph break,适合定位,但不一定适合直接生产开启。\n"
            "5. 如果每次请求 shape 都不同,编译开销可能抵消收益;这种服务场景可先关闭 compile 或只编译稳定子模块。\n\n"
            "示例:\n"
            "```bash\n"
            "TORCH_LOGS=\"graph_breaks,recompiles\" python train.py\n"
            "```\n"
            "```python\n"
            "model = torch.compile(model, dynamic=True)\n"
            "```\n\n"
            "注意:不要把所有 `torch.compile` 报错都当成 CUDA 安装问题。先看 graph break/recompile 日志,再决定是否改代码、固定 shape 或回退 eager。"
        ),
        source_refs=[
            "pytorch-docs:torch-compiler-troubleshooting",
            "pytorch-docs:torch-compiler-dynamic-shapes",
            "pytorch-docs:torch-compile-tutorial",
        ],
    ),
    chunk(
        chunk_id="ext-cuda-toolkit-install-linux-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="Linux 安装 CUDA Toolkit: apt/package、runfile、conda 和容器选择",
        question_patterns=[
            "cuda toolkit 怎么安装",
            "需要安装 nvcc 吗",
            "apt install cuda-toolkit 和 runfile 怎么选",
            "conda install cuda -c nvidia 有什么区别",
            "编译 cuda 扩展提示 nvcc not found",
        ],
        content=(
            "适用场景:客户需要编译 CUDA 扩展、安装 PyTorch3D/gsplat/CuPy 源码组件,或报 `nvcc not found`,想安装 CUDA Toolkit。"
            "NVIDIA CUDA Linux 安装指南说明 Toolkit 可通过 package manager、runfile、Conda 等方式安装;Quick Start 指出安装后应验证 CUDA 程序能运行。\n\n"
            "选择建议:\n"
            "1. 只运行 PyTorch/Transformers 推理或训练时,通常不需要系统 CUDA Toolkit;框架 wheel 自带所需 runtime,关键是驱动足够新。\n"
            "2. 需要 `nvcc` 编译自定义 CUDA 扩展时,才需要 Toolkit 或等价的编译组件。\n"
            "3. 系统级长期环境优先用发行版 package manager 安装,例如 Ubuntu/Debian 使用 NVIDIA 配置后的 `apt install cuda-toolkit` 或指定版本包。\n"
            "4. 临时/隔离 Python 环境可考虑 `conda install cuda -c nvidia`,并放在独立环境中。\n"
            "5. 容器里优先选择带 `devel` 的 CUDA/NVIDIA 框架镜像,不要在运行时容器里临时装一堆编译工具。\n"
            "6. runfile 适合少数需要手工控制安装路径的场景;混用 package manager 和 runfile 容易产生冲突,安装前要查清已有方式。\n\n"
            "检查命令:\n"
            "```bash\n"
            "nvidia-smi\n"
            "nvcc --version\n"
            "python -c \"import torch; print(torch.version.cuda, torch.cuda.is_available())\"\n"
            "```\n\n"
            "注意:`nvidia-smi` 显示的是驱动支持的 CUDA 上限,不是 Toolkit 版本;`nvcc --version` 才能说明本机是否有 CUDA 编译器。"
        ),
        source_refs=[
            "nvidia-docs:cuda-install-linux",
            "nvidia-docs:cuda-quick-start",
            "nvidia-docs:cuda-compatibility",
            "pytorch-docs:get-started",
        ],
    ),
]

THIRD_WAVE_EXTERNAL_CHUNKS = [
    chunk(
        chunk_id="ext-accelerate-fsdp-config-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="Accelerate FSDP 训练大模型的基础配置",
        question_patterns=[
            "accelerate fsdp 怎么配置",
            "fsdp 训练大模型怎么启动",
            "多卡训练想用 fsdp 省显存",
            "fsdp_transformer_layer_cls_to_wrap 填什么",
            "accelerate launch fsdp 配置示例",
        ],
        content=(
            "适用场景:客户用 DDP 或普通 Trainer 训练大模型显存不够,想用 FSDP 把参数、梯度和优化器状态切到多张 GPU 上。"
            "Hugging Face Accelerate 支持通过 `accelerate config` 生成 FSDP 配置,再用 `accelerate launch` 启动训练。\n\n"
            "处理建议:\n"
            "1. 先确认单卡或 DDP 版本能跑通,再切 FSDP,避免把脚本 bug 和分布式配置问题混在一起。\n"
            "2. `distributed_type` 选择 FSDP,`fsdp_sharding_strategy` 常见选择是 `FULL_SHARD`。\n"
            "3. Transformer 模型优先用 `TRANSFORMER_BASED_WRAP`,并设置正确的 transformer layer 类名。\n"
            "4. 训练超大模型时可打开 `fsdp_cpu_ram_efficient_loading`,但要配合 `fsdp_sync_module_states`。\n"
            "5. FSDP 能降单卡显存压力,但通信更多,多节点网络和磁盘 checkpoint 也会影响速度。\n\n"
            "典型流程:\n"
            "```bash\n"
            "accelerate config\n"
            "accelerate launch train.py\n"
            "```\n\n"
            "注意:FSDP 配置强依赖模型结构。类名填错、包裹策略不合适或 checkpoint 策略不匹配,都可能导致 OOM、速度慢或保存失败。"
        ),
        source_refs=["hf-accelerate:fsdp", "pytorch-docs:fsdp-advanced"],
    ),
    chunk(
        chunk_id="ext-fsdp-checkpoint-merge-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="FSDP checkpoint 保存、恢复和合并权重",
        question_patterns=[
            "fsdp 保存出来一堆 shard 怎么合并",
            "accelerate fsdp checkpoint 怎么恢复训练",
            "fsdp 训练完怎么 save_pretrained",
            "merge_fsdp_weights 怎么用",
            "fsdp sharded state dict 是什么",
        ],
        content=(
            "适用场景:FSDP 训练后输出多个分片文件,客户不知道如何恢复训练、导出单个模型文件或上传模型。"
            "Accelerate 文档建议 FSDP checkpoint 可使用 `accelerator.save_state()` / `load_state()`,也可用 `merge_fsdp_weights` 合并分片权重。\n\n"
            "处理建议:\n"
            "1. 恢复训练优先保留完整 checkpoint 目录,包括模型、优化器、scheduler 和随机状态。\n"
            "2. `SHARDED_STATE_DICT` 适合训练中保存,每个进程写自己的分片。\n"
            "3. 对外发布或部署前,再用 `accelerate merge-weights` 或 `merge_fsdp_weights` 合并成普通权重。\n"
            "4. 使用 Transformers `save_pretrained` 时,按 Accelerate 文档把 `accelerator.get_state_dict(model)` 传进去。\n"
            "5. 合并权重要在空间充足的磁盘上做,大模型可能需要大量 CPU 内存和临时空间。\n\n"
            "示例:\n"
            "```bash\n"
            "accelerate merge-weights pytorch_model_fsdp_0/ output_path\n"
            "```\n\n"
            "注意:不要只复制其中一个 rank 的分片目录就当成完整模型。分片 checkpoint 缺文件后通常无法正确恢复。"
        ),
        source_refs=["hf-accelerate:fsdp", "pytorch-docs:fsdp-advanced"],
    ),
    chunk(
        chunk_id="ext-fsdp-vs-deepspeed-zero-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="FSDP 和 DeepSpeed ZeRO 怎么选",
        question_patterns=[
            "fsdp 和 deepspeed zero 有什么区别",
            "训练大模型用 fsdp 还是 zero3",
            "fsdp full shard 对应 deepspeed 哪个 stage",
            "zero2 zero3 和 fsdp 怎么选",
            "accelerate 里 fsdp deepspeed 选哪个",
        ],
        content=(
            "适用场景:客户训练大模型时在 PyTorch FSDP 和 DeepSpeed ZeRO 之间选择。二者都能减少单卡显存占用,但配置方式、生态和 checkpoint 处理不同。"
            "Accelerate 文档给出映射关系:`FULL_SHARD` 类似 ZeRO Stage 3,`SHARD_GRAD_OP` 类似 ZeRO Stage 2。\n\n"
            "选择建议:\n"
            "1. 已经在 PyTorch/Transformers/Accelerate 生态里,优先试 FSDP,集成路径更直接。\n"
            "2. 已有 DeepSpeed 配置或需要 ZeRO-Offload、成熟 DeepSpeed 配置项时,继续用 DeepSpeed。\n"
            "3. ZeRO-3/FULL_SHARD 通常更省显存,但通信和 checkpoint 更复杂。\n"
            "4. ZeRO-2/SHARD_GRAD_OP 通常更容易调通,但参数仍有更多副本。\n"
            "5. 不要只看显存,还要看网络、磁盘、恢复训练和最终导出权重流程。\n\n"
            "注意:同一模型在不同节点、网络和 batch 下结果不同。建议用小规模样例先比较能否稳定跑完、吞吐和 checkpoint 成本。"
        ),
        source_refs=["hf-accelerate:fsdp", "hf-accelerate:deepspeed", "deepspeed-docs:config-json"],
    ),
    chunk(
        chunk_id="ext-accelerate-memory-estimator-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="用 Accelerate 估算模型显存和内存需求",
        question_patterns=[
            "模型要多大显存怎么估算",
            "accelerate estimate-memory 怎么用",
            "训练前怎么判断卡够不够",
            "显存不够是不是换更大卡",
            "模型参数量和显存怎么换算",
        ],
        content=(
            "适用场景:客户还没开始训练或推理,想先判断模型能不能放进 GPU,或者该选多大显存的实例。"
            "Accelerate 提供 `estimate-memory` 工具,可按模型名估算加载模型时不同精度的大致内存占用。\n\n"
            "处理建议:\n"
            "1. 用 `accelerate estimate-memory <model_id>` 先估算权重加载需求。\n"
            "2. 推理还要额外考虑 KV cache、并发、上下文长度和 batch。\n"
            "3. 训练还要考虑梯度、优化器状态、激活值和 checkpoint,通常远高于只加载权重。\n"
            "4. 量化、LoRA、FSDP/DeepSpeed、activation checkpointing 都会改变显存需求。\n"
            "5. 估算值只能用于选型起点,最终要用实际脚本和目标 batch 做一次 smoke run。\n\n"
            "示例:\n"
            "```bash\n"
            "accelerate estimate-memory Qwen/Qwen2.5-7B-Instruct\n"
            "```\n\n"
            "注意:客户问“7B 要多大卡”时,要先区分是加载推理、长上下文服务、全量训练还是 LoRA 微调。"
        ),
        source_refs=["hf-accelerate:model-size-estimator", "hf-accelerate:big-model-inference"],
    ),
    chunk(
        chunk_id="ext-hf-big-model-device-map-offload-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="Hugging Face 大模型 device_map、CPU offload 和磁盘 offload 边界",
        question_patterns=[
            "device_map auto 是不是就不会爆显存",
            "大模型能不能一部分放 cpu 一部分放 gpu",
            "transformers offload_folder 怎么用",
            "模型太大加载不了怎么用 accelerate 分层加载",
            "cpu offload 为什么推理很慢",
        ],
        content=(
            "适用场景:客户用 Transformers 加载大模型时显存不足,想用 `device_map=\"auto\"`、CPU offload 或磁盘 offload 勉强运行。"
            "Accelerate 的 big model inference 文档说明可用空权重初始化、device map 和 offload 把模型分布到 GPU/CPU/磁盘。\n\n"
            "处理建议:\n"
            "1. `device_map=\"auto\"` 能帮助放置层,但不是保证不 OOM 的开关;KV cache 和输入长度仍占显存。\n"
            "2. CPU/disk offload 可以让模型跑起来,但速度通常明显下降,适合验证功能,不适合高并发服务。\n"
            "3. `offload_folder` 要放到空间足够且 IO 较好的磁盘。\n"
            "4. 如果只是推理服务,优先评估 vLLM/SGLang 的张量并行、量化和上下文设置。\n"
            "5. 如果频繁切层或磁盘 IO 打满,应换更大显存或多卡方案,不要继续堆 offload。\n\n"
            "注意:offload 能降低显存压力,但会把压力转移到 CPU 内存和磁盘 IO。客户追求吞吐和延迟时要谨慎使用。"
        ),
        source_refs=["hf-accelerate:big-model-inference", "hf-accelerate:model-size-estimator"],
    ),
    chunk(
        chunk_id="ext-megatron-lm-accelerate-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="Megatron-LM / Accelerate 适合更大规模并行训练",
        question_patterns=[
            "megatron lm 是什么",
            "张量并行和 fsdp 有什么区别",
            "accelerate megatron lm 怎么用",
            "训练超大模型只用数据并行够吗",
            "pipeline parallel tensor parallel 什么时候需要",
        ],
        content=(
            "适用场景:客户训练模型规模继续变大,只靠 DDP、FSDP 或 ZeRO 仍不够,开始关注张量并行和流水线并行。"
            "Accelerate 提供 Megatron-LM 集成文档,用于把模型训练扩展到更多并行维度。\n\n"
            "处理建议:\n"
            "1. 普通微调优先用 LoRA/QLoRA、FSDP 或 DeepSpeed,不要一开始就上 Megatron-LM。\n"
            "2. 当单层矩阵本身太大、单卡放不下或吞吐不够时,才考虑 tensor parallel。\n"
            "3. 当模型层数很多、需要跨阶段切分时,再考虑 pipeline parallel。\n"
            "4. 这类训练对网络、启动脚本、数据吞吐和 checkpoint 要求更高,建议先在小模型上验证流程。\n"
            "5. 多节点训练前先确认 NCCL 和节点间带宽,否则并行策略配置正确也可能很慢。\n\n"
            "注意:Megatron-LM 属于高级分布式训练方案。客户只想做 7B/14B LoRA 微调时,通常不需要它。"
        ),
        source_refs=["hf-accelerate:megatron-lm", "pytorch-docs:distributed"],
    ),
    chunk(
        chunk_id="ext-pytorch-activation-checkpointing-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="Activation checkpointing 用计算换训练显存",
        question_patterns=[
            "activation checkpointing 怎么省显存",
            "gradient checkpointing 为什么训练变慢",
            "显存不够能不能重算激活",
            "torch checkpoint 怎么用",
            "transformers gradient_checkpointing_enable 有什么代价",
        ],
        content=(
            "适用场景:训练时显存主要被激活值占用,客户希望用更小显存跑更长序列或更大 batch。"
            "PyTorch checkpoint 文档说明 activation checkpointing 会在前向时少保存中间激活,反向时重新计算,以计算时间换显存。\n\n"
            "处理建议:\n"
            "1. 优先在 Transformer block 或重复的大模块上启用,不要随意包很小的函数。\n"
            "2. 预期训练会变慢,因为反向阶段需要重算一部分前向。\n"
            "3. 与混合精度、LoRA、FSDP/DeepSpeed 可组合,但每次只改一个变量更容易定位效果。\n"
            "4. 用 Transformers 时可优先使用模型提供的 `gradient_checkpointing_enable()`。\n"
            "5. 打开后重新确认 loss 是否正常下降,并比较显存峰值和 step time。\n\n"
            "注意:activation checkpointing 主要解决训练激活显存,不能减少模型权重本身占用,也不解决数据加载慢。"
        ),
        source_refs=["pytorch-docs:activation-checkpointing", "hf-transformers:trainer"],
    ),
    chunk(
        chunk_id="ext-fsdp-mixed-precision-auto-wrap-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="FSDP mixed precision 和 auto wrap 策略排查",
        question_patterns=[
            "fsdp mixed precision 怎么配置",
            "fsdp auto wrap 策略怎么选",
            "fsdp 训练显存没降下来",
            "transformer_based_wrap 为什么没包住模型",
            "fsdp bf16 fp16 训练怎么排查",
        ],
        content=(
            "适用场景:客户已经启用 FSDP,但显存下降不明显、速度很慢、精度异常,或不确定 mixed precision 和 auto wrap 策略怎么配。"
            "Accelerate FSDP 和 PyTorch FSDP 文档都强调自动包裹策略、混合精度和模型结构会共同影响显存、通信和稳定性。\n\n"
            "处理建议:\n"
            "1. 先确认 FSDP 真的包住了目标层,尤其是 Transformer block 类名是否写对。\n"
            "2. mixed precision 优先按硬件选择 bf16 或 fp16;A100/H100 等通常优先考虑 bf16。\n"
            "3. 如果只包了最外层或包裹粒度太粗,显存和通信效果可能不符合预期。\n"
            "4. 如果包裹粒度太细,通信和调度开销会上升,训练可能变慢。\n"
            "5. 每次只改一个变量,记录显存峰值、step time、loss 和 checkpoint 是否正常。\n\n"
            "注意:FSDP 不是单一开关。auto wrap、mixed precision、sharding strategy、checkpoint 和模型加载方式要一起检查。"
        ),
        source_refs=["hf-accelerate:fsdp", "pytorch-docs:fsdp-advanced"],
    ),
    chunk(
        chunk_id="ext-vllm-lora-serving-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="vLLM 用一个底模挂载 LoRA adapter 对外服务",
        question_patterns=[
            "vllm 怎么加载 lora 模型服务",
            "一个 base model 能不能挂多个 lora",
            "vllm --enable-lora 怎么用",
            "lora 微调完怎么部署成 openai 接口",
            "vllm lora adapter 模型名怎么填",
        ],
        content=(
            "适用场景:客户已经有一个基础模型和若干 LoRA adapter,希望不用为每个 LoRA 单独加载完整模型,直接用 vLLM 对外提供 OpenAI 兼容接口。"
            "vLLM 官方 LoRA 文档支持启动时用 `--enable-lora` 和 `--lora-modules` 挂载 adapter。\n\n"
            "启动示例:\n"
            "```bash\n"
            "vllm serve <base-model> --enable-lora --lora-modules sql-lora=/data/lora/sql\n"
            "```\n\n"
            "处理建议:\n"
            "1. 确认 LoRA adapter 与 base model 架构匹配。\n"
            "2. 请求里的 `model` 填暴露出来的 LoRA 名称,而不是一定填 base model。\n"
            "3. 多 LoRA 会增加显存和调度开销,需要关注 vLLM 的 LoRA 相关限制和指标。\n"
            "4. adapter 目录要包含完整 adapter 权重和配置,不要只传训练日志目录。\n\n"
            "注意:LoRA serving 适合多个轻量业务变体共用一个底模;如果 adapter 与底模不匹配,通常会在加载阶段报错。"
        ),
        source_refs=["vllm-docs:features/lora", "vllm-docs:cli/serve"],
    ),
    chunk(
        chunk_id="ext-vllm-dynamic-lora-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="vLLM 动态加载 LoRA adapter 的适用边界",
        question_patterns=[
            "vllm lora 能不能运行中动态加载",
            "不用重启 vllm 可以加 lora 吗",
            "动态 lora adapter 为什么加载失败",
            "vllm 多租户 lora 怎么管理",
            "lora adapter 等待队列怎么看",
        ],
        content=(
            "适用场景:客户希望服务不停机,在运行中为不同客户或不同任务切换 LoRA adapter。"
            "vLLM LoRA 文档说明服务端可支持运行时配置 LoRA adapter,同时指标里也有 running/waiting LoRA adapter 信息。\n\n"
            "处理建议:\n"
            "1. 先用启动时加载方式验证 adapter 与 base model 兼容,再尝试动态加载。\n"
            "2. 动态加载的 adapter 路径要能被服务进程访问,容器内外路径不要混淆。\n"
            "3. 给 adapter 命名时避免和 base model 或其他 adapter 冲突。\n"
            "4. 监控 LoRA 请求等待情况,如果 adapter 太多或显存紧张,请求会排队或失败。\n"
            "5. 多租户场景要加鉴权和配额,不要让用户任意加载不可信权重。\n\n"
            "注意:动态 LoRA 是服务治理能力,不是无限显存能力。adapter 数量、rank、并发和 base model 大小都会影响资源。"
        ),
        source_refs=["vllm-docs:features/lora", "vllm-docs:design/metrics", "vllm-docs:usage/security"],
    ),
    chunk(
        chunk_id="ext-vllm-structured-output-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="vLLM 结构化输出让模型按 JSON/schema 返回",
        question_patterns=[
            "vllm 怎么让模型输出 json",
            "openai 接口 response_format 在 vllm 支持吗",
            "结构化输出和普通 prompt 有什么区别",
            "模型总是不按格式返回怎么办",
            "vllm guided decoding 怎么用",
        ],
        content=(
            "适用场景:客户把模型接到业务系统,希望结果稳定返回 JSON、枚举或指定 schema,而不是只靠 prompt 约束。"
            "vLLM structured outputs 文档说明 OpenAI 兼容接口可使用结构化输出相关参数来约束生成。\n\n"
            "处理建议:\n"
            "1. 简单场景先用明确 prompt 加 JSON 示例;稳定性不够时再用 structured outputs。\n"
            "2. 约束越复杂,解码开销和失败概率可能越高,先从小 schema 验证。\n"
            "3. 请求失败时检查模型是否支持对应解码方式、schema 是否合法、输出长度是否足够。\n"
            "4. 生产接口仍要在服务端解析和校验 JSON,不要完全相信模型输出。\n"
            "5. 对外 API 要说明返回格式版本,避免业务端 schema 改动无法兼容。\n\n"
            "注意:结构化输出解决的是格式约束,不保证事实正确。事实性问题仍需要检索、评测和业务校验。"
        ),
        source_refs=["vllm-docs:features/structured_outputs"],
    ),
    chunk(
        chunk_id="ext-vllm-prometheus-metrics-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="vLLM Prometheus 指标看吞吐、延迟和队列",
        question_patterns=[
            "vllm 怎么接 prometheus",
            "vllm metrics 看哪些指标",
            "模型服务吞吐和延迟怎么监控",
            "vllm 请求排队怎么判断",
            "grafana 怎么看 vllm 服务状态",
        ],
        content=(
            "适用场景:客户把 vLLM 服务用于多人或业务调用,需要持续看请求量、延迟、KV cache、队列和 LoRA adapter 状态。"
            "vLLM metrics 文档说明 OpenAI API server 暴露 Prometheus 风格指标,可接 Prometheus/Grafana。\n\n"
            "处理建议:\n"
            "1. 先确认 vLLM 的 `/metrics` 端点能被 Prometheus 抓取。\n"
            "2. 重点看请求吞吐、首 token 延迟、每 token 延迟、队列等待、KV cache 使用和错误数。\n"
            "3. 使用 LoRA 时关注 running/waiting LoRA adapter 指标。\n"
            "4. GPU 指标要结合 DCGM/nvidia-smi,不要只看应用层指标。\n"
            "5. 做压测时同时保存 prompt 长度、输出长度、并发和模型参数,否则指标不可比较。\n\n"
            "注意:服务变慢可能来自队列、KV cache、长上下文、磁盘、网络或 GPU 利用率,需要把 vLLM 指标和系统指标一起看。"
        ),
        source_refs=["vllm-docs:design/metrics", "vllm-docs:observability-config", "nvidia-dcgm:exporter"],
    ),
    chunk(
        chunk_id="ext-vllm-opentelemetry-tracing-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="vLLM OpenTelemetry tracing 用于请求链路定位",
        question_patterns=[
            "vllm 能不能接 opentelemetry",
            "模型服务请求链路怎么追踪",
            "otlp_traces_endpoint 是什么",
            "vllm 详细 trace 怎么开",
            "为什么模型请求慢但 gpu 不满",
        ],
        content=(
            "适用场景:客户需要定位模型请求从入口到推理引擎的耗时,区分排队、调度、预填充、解码或外部网关开销。"
            "vLLM observability 配置包含 OTLP traces endpoint 和详细 trace 选项,可把 trace 送到 OpenTelemetry 兼容后端。\n\n"
            "处理建议:\n"
            "1. 先用 Prometheus 指标判断整体是否慢,再用 trace 查单个请求链路。\n"
            "2. 配置 OTLP endpoint 前先确认 trace 后端可达,网络和证书没有问题。\n"
            "3. 详细 trace 可能增加开销,建议用于排查或采样,不要无边界长期全量开启。\n"
            "4. 日志、metrics、trace 三者要用同一请求 ID 或时间窗口关联。\n"
            "5. 对外服务要注意不要把敏感 prompt 或响应内容无控制地写入 trace。\n\n"
            "注意:trace 是定位手段,不是性能优化开关。开启后仍需要根据瓶颈选择扩容、限流、缩短上下文或调整并发。"
        ),
        source_refs=["vllm-docs:observability-config", "vllm-docs:design/metrics"],
    ),
    chunk(
        chunk_id="ext-vllm-security-reverse-proxy-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="vLLM 对外服务的鉴权、限流和反向代理安全",
        question_patterns=[
            "vllm 服务直接暴露公网安全吗",
            "openai 兼容接口怎么加鉴权和限流",
            "vllm 前面要不要加 nginx",
            "模型 api 被刷爆怎么防",
            "vllm security 文档建议什么",
        ],
        content=(
            "适用场景:客户把 vLLM OpenAI 兼容接口暴露给团队或公网,担心未授权调用、超长请求、滥用和日志泄露。"
            "vLLM security 文档建议在代理层实现额外鉴权、限流、日志和请求约束。\n\n"
            "处理建议:\n"
            "1. 不要把无鉴权的模型服务直接暴露到公网。\n"
            "2. vLLM 内置 API key 适合基础保护,生产场景建议再加反向代理或 API gateway。\n"
            "3. 在代理层限制请求体大小、上下文长度、并发、速率和来源。\n"
            "4. 日志里避免记录完整敏感 prompt、密钥和用户数据。\n"
            "5. 对不同团队/客户使用独立 key 和配额,便于追踪和停用。\n\n"
            "注意:安全策略要放在服务入口统一执行。只依赖应用脚本里的简单判断,很难覆盖所有调用路径。"
        ),
        source_refs=["vllm-docs:usage/security", "vllm-docs:cli/serve"],
    ),
    chunk(
        chunk_id="ext-litellm-virtual-keys-budget-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="LiteLLM virtual keys 用于模型 API 分租户和预算控制",
        question_patterns=[
            "litellm virtual key 怎么用",
            "一个模型服务给多个团队怎么分 key",
            "怎么限制每个用户调用预算",
            "openai compatible 服务怎么做租户隔离",
            "litellm proxy 能不能控制模型访问权限",
        ],
        content=(
            "适用场景:客户有多个自托管或外部 LLM 接口,希望用统一入口发 key、限制预算、追踪用量和控制可访问模型。"
            "LiteLLM proxy 文档提供 virtual keys,可用于跟踪花费和控制模型访问。\n\n"
            "处理建议:\n"
            "1. 先设置 proxy master key,只给管理员使用。\n"
            "2. 给每个用户、团队或应用创建独立 virtual key。\n"
            "3. 为 key 绑定可访问模型、预算和过期时间,避免一个 key 无限制调用所有模型。\n"
            "4. 下游可对接 vLLM、Ollama、云厂商或其他 OpenAI 兼容服务。\n"
            "5. key 泄露时只吊销单个 virtual key,不要重启所有模型服务。\n\n"
            "注意:virtual key 是治理入口,不是模型本身的安全边界。后端模型服务仍应放在内网或受控网络。"
        ),
        source_refs=["litellm-docs:virtual-keys", "litellm-docs:getting-started", "litellm-docs:openai-compatible"],
    ),
    chunk(
        chunk_id="ext-litellm-rate-limit-teams-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="LiteLLM 团队预算、限流和模型访问排查",
        question_patterns=[
            "litellm 怎么按团队设置预算",
            "某个 key 调不了模型怎么查",
            "litellm rate limit 怎么配置",
            "模型 api 预算用完了会怎样",
            "团队共用额度和个人额度怎么区分",
        ],
        content=(
            "适用场景:团队共享模型服务后,有人调用失败、超预算或被限流,需要区分是 key、团队、模型权限还是后端服务问题。"
            "LiteLLM users/budgets 文档支持个人预算、团队预算、模型访问和 rate limit 等治理能力。\n\n"
            "排查顺序:\n"
            "1. 确认请求使用的是 virtual key,不是 master key 或过期 key。\n"
            "2. 检查 key 是否绑定了目标模型,模型别名是否和配置一致。\n"
            "3. 看个人预算和团队预算是否已达到上限。\n"
            "4. 检查 rate limit,短时间并发过高会被限制。\n"
            "5. 如果治理层通过但后端报错,再查 vLLM/Ollama/云模型服务日志。\n\n"
            "注意:给客户解释失败原因时要区分“没有权限”“额度用完”“限流”“后端模型不可用”,处理动作完全不同。"
        ),
        source_refs=["litellm-docs:users-budgets", "litellm-docs:virtual-keys"],
    ),
    chunk(
        chunk_id="ext-vllm-prefix-cache-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="vLLM prefix caching 适合重复长前缀请求",
        question_patterns=[
            "vllm prefix cache 有什么用",
            "很多请求都有同一段 system prompt 怎么加速",
            "长上下文重复请求为什么还是慢",
            "automatic prefix caching 适合哪些场景",
            "rag 请求能不能复用 kv cache",
        ],
        content=(
            "适用场景:很多请求共享相同系统提示词、工具说明、长文档前缀或模板,客户希望减少重复 prefill 开销。"
            "vLLM automatic prefix caching 文档用于复用相同前缀的 KV cache,提高重复前缀场景效率。\n\n"
            "处理建议:\n"
            "1. 先确认请求确实有大量完全相同的前缀;只相似但不相同的文本不能稳定命中缓存。\n"
            "2. 把稳定系统提示词和模板放在前面,变化内容放在后面。\n"
            "3. 监控首 token 延迟、KV cache 使用和命中效果,不要只看平均吞吐。\n"
            "4. 对 RAG 场景,检索片段每次不同会降低复用率;固定系统提示词仍可能受益。\n"
            "5. 缓存会占用显存或内存资源,并发高时要结合上下文长度一起评估。\n\n"
            "注意:prefix caching 不是通用加速魔法。输入前缀变化大、每次 prompt 都不同的场景收益有限。"
        ),
        source_refs=["vllm-docs:features/automatic_prefix_caching", "vllm-docs:design/metrics"],
    ),
    chunk(
        chunk_id="ext-nccl-tests-allreduce-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="用 nccl-tests 验证多卡 all-reduce 性能和正确性",
        question_patterns=[
            "nccl-tests 怎么跑",
            "多卡 allreduce 带宽怎么测",
            "训练慢是不是 nccl 网络问题",
            "all_reduce_perf 输出怎么看",
            "多节点训练前怎么验证通信",
        ],
        content=(
            "适用场景:客户多卡或多节点训练很慢、卡住或怀疑网络有问题,需要先把模型脚本之外的 NCCL 通信能力测清楚。"
            "NVIDIA nccl-tests 仓库提供性能和正确性测试,常用 `all_reduce_perf` 验证 all-reduce。\n\n"
            "处理建议:\n"
            "1. 先在单机多卡跑 `all_reduce_perf`,确认 PCIe/NVLink 路径正常。\n"
            "2. 再跨节点运行同一测试,确认节点间网络带宽和稳定性。\n"
            "3. 测试环境要和训练环境一致,包括容器、驱动、NCCL、网卡和环境变量。\n"
            "4. 如果 nccl-tests 都慢或报错,先修网络/驱动/拓扑,不要继续调训练代码。\n"
            "5. 如果 nccl-tests 正常但训练慢,再查 DataLoader、checkpoint、模型并行策略和 batch。\n\n"
            "注意:nccl-tests 是通信基线,不能代表完整训练吞吐;但它能快速排除或确认底层通信问题。"
        ),
        source_refs=["nvidia-nccl-tests-repo:README", "nvidia-docs:nccl-troubleshooting"],
    ),
    chunk(
        chunk_id="ext-nccl-gpu-nic-topology-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="NCCL GPU-to-NIC 拓扑和网卡选择排查",
        question_patterns=[
            "nccl 选错网卡怎么办",
            "多节点训练为什么走了慢网卡",
            "gpu 到 nic 拓扑怎么查",
            "NCCL_SOCKET_IFNAME 怎么设置",
            "NCCL_IB_HCA 什么时候用",
        ],
        content=(
            "适用场景:多节点训练能跑但很慢,怀疑 NCCL 没走正确网卡、GPU-to-NIC 路径不理想或拓扑不匹配。"
            "NVIDIA NCCL troubleshooting 文档覆盖网络接口选择、拓扑、GPU-to-NIC 和多节点网络问题。\n\n"
            "排查顺序:\n"
            "1. 用 `nvidia-smi topo -m` 看 GPU、CPU、NIC 的相对拓扑。\n"
            "2. 用 `ip addr` / `ibv_devinfo` 等确认可用网卡和 IB 设备。\n"
            "3. 通过 `NCCL_DEBUG=INFO` 查看 NCCL 实际选择的接口。\n"
            "4. 多网卡环境可设置 `NCCL_SOCKET_IFNAME` 指定以太网接口。\n"
            "5. InfiniBand/RDMA 环境可按实际设备配置 NCCL IB 相关变量,但不要盲目复制其他机器的 HCA 名称。\n\n"
            "注意:接口名、网卡数量和拓扑是机器相关事实。迁移实例或换镜像后要重新检查。"
        ),
        source_refs=["nvidia-docs:nccl-troubleshooting", "nvidia-docs:nccl-env", "nvidia-docs:nvidia-smi"],
    ),
    chunk(
        chunk_id="ext-nccl-acs-iommu-rdma-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="NCCL RDMA、ACS、IOMMU 导致跨节点通信异常",
        question_patterns=[
            "nccl rdma 一开就卡住",
            "gpu direct rdma 为什么不生效",
            "acs iommu 会影响 nccl 吗",
            "跨节点 nccl 性能很差怎么查 bios",
            "rdma 关闭反而能跑是什么原因",
        ],
        content=(
            "适用场景:跨节点 NCCL 在 RDMA/InfiniBand 环境下卡住、带宽很低,或开启 GPU Direct RDMA 后失败。"
            "NVIDIA NCCL troubleshooting 文档提到 GPU troubleshooting 会覆盖 GPU-to-GPU、GPU-to-NIC、ACS、拓扑和多节点网络问题。\n\n"
            "处理建议:\n"
            "1. 先用 nccl-tests 和低层网络测试确认不是训练脚本问题。\n"
            "2. 检查 NCCL 日志里是否启用了 IB/RDMA,以及选择了哪些 HCA。\n"
            "3. 让管理员确认 BIOS/PCIe ACS、IOMMU、驱动和 OFED/rdma-core 状态。\n"
            "4. 不要在不了解硬件环境时随意关闭或强开 RDMA 相关变量。\n"
            "5. 如果平台不提供 RDMA,应按普通 TCP/以太网带宽预期设置训练规模。\n\n"
            "注意:ACS/IOMMU/BIOS 属于节点级配置,普通用户通常无法自行修复。需要把 NCCL 日志、节点型号和测试结果交给运维定位。"
        ),
        source_refs=["nvidia-docs:nccl-troubleshooting", "nvidia-nccl-tests-repo:README"],
    ),
    chunk(
        chunk_id="ext-infiniband-low-level-test-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="跑 NCCL 前先用低层 InfiniBand 测试确认网络",
        question_patterns=[
            "跑 nccl 前怎么确认 ib 网络通",
            "ib_write_bw 和 nccl-tests 有什么关系",
            "多节点训练网络底层怎么测",
            "infiniband 带宽正常但训练慢怎么办",
            "rdma 网络先测什么",
        ],
        content=(
            "适用场景:客户有多节点 GPU 训练环境,但不确定 InfiniBand/RDMA 网络是否正常。"
            "NVIDIA NCCL troubleshooting 文档建议在 NCCL 前运行低层 InfiniBand 测试,特别是带宽类测试,先确认节点间网络能力。\n\n"
            "处理建议:\n"
            "1. 先确认两台节点的 IB 设备和驱动都正常。\n"
            "2. 用低层 IB 带宽/连通性测试确认节点间能通信。\n"
            "3. 再运行 nccl-tests,验证 GPU 通信路径。\n"
            "4. 最后再运行真实训练脚本,比较吞吐和通信占比。\n"
            "5. 如果低层 IB 测试已经异常,不要继续从 PyTorch 代码层面排查。\n\n"
            "注意:低层 IB 工具名和安装包随系统而异。客户没有权限安装或运行时,应请管理员提供同节点同网络的测试结果。"
        ),
        source_refs=["nvidia-docs:nccl-troubleshooting", "nvidia-nccl-tests-repo:README"],
    ),
    chunk(
        chunk_id="ext-multinode-nccl-env-minimal-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="多节点训练 NCCL 环境变量最小排查集",
        question_patterns=[
            "多节点训练 nccl 环境变量怎么设",
            "NCCL_DEBUG INFO 有什么用",
            "torchrun 多机卡住怎么收集日志",
            "nccl 日志太少怎么打开",
            "MASTER_ADDR MASTER_PORT 和 nccl 有关系吗",
        ],
        content=(
            "适用场景:多节点 PyTorch/Accelerate/DeepSpeed 训练启动后卡住或报通信错误,需要先收集可判断的日志。"
            "PyTorch distributed 与 NVIDIA NCCL 文档都建议通过启动参数和 NCCL 日志明确节点、端口、rank、接口和错误位置。\n\n"
            "最小排查集:\n"
            "1. 记录 `MASTER_ADDR`、`MASTER_PORT`、`NNODES`、`NODE_RANK`、每节点 GPU 数。\n"
            "2. 设置 `NCCL_DEBUG=INFO` 和必要时 `TORCH_DISTRIBUTED_DEBUG=DETAIL`。\n"
            "3. 多网卡机器先明确 `NCCL_SOCKET_IFNAME`,避免 NCCL 选到容器网卡或慢网卡。\n"
            "4. 确认所有节点时间、镜像、驱动、NCCL/PyTorch 版本一致。\n"
            "5. 先用 nccl-tests 复现通信,再回到训练脚本。\n\n"
            "注意:环境变量不要无限堆。每次改一到两个关键变量并保存日志,否则很难判断哪个设置起作用。"
        ),
        source_refs=["pytorch-docs:elastic-run", "pytorch-docs:distributed", "nvidia-docs:nccl-env", "nvidia-docs:nccl-troubleshooting"],
    ),
    chunk(
        chunk_id="ext-kueue-pytorchjob-queue-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="Kueue 管理 PyTorchJob 队列和 GPU 资源",
        question_patterns=[
            "kueue pytorchjob 怎么排队",
            "gpu 训练任务怎么进队列",
            "kubeflow pytorchjob 为什么一直 pending",
            "多用户 gpu 资源怎么排队调度",
            "localqueue clusterqueue 是什么",
        ],
        content=(
            "适用场景:客户在 Kubernetes 上提交多用户 GPU 训练任务,希望按队列、配额和优先级管理 PyTorchJob。"
            "Kueue 文档提供运行 Kubeflow PyTorchJob 的任务指南,把训练任务接入 Kueue 的调度和资源管理。\n\n"
            "处理建议:\n"
            "1. 把训练任务提交为 PyTorchJob,并指定 Kueue 需要的队列标签或配置。\n"
            "2. 检查 LocalQueue 是否存在并指向正确 ClusterQueue。\n"
            "3. ClusterQueue 的 resource flavor 和 GPU 配额决定任务是否能被接纳。\n"
            "4. Pending 不一定是报错,可能是在等待 GPU 资源或 gang scheduling 条件满足。\n"
            "5. 排查时看 Job、Workload、LocalQueue、ClusterQueue 的状态和事件。\n\n"
            "注意:Kueue 是集群调度层能力。单机 GPU 实例里直接跑训练脚本时不需要 Kueue。"
        ),
        source_refs=["kueue-docs:pytorchjob", "kubeflow-docs:job-scheduling"],
    ),
    chunk(
        chunk_id="ext-kueue-resource-flavors-quota-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="Kueue resource flavor 和配额导致训练任务排队",
        question_patterns=[
            "kueue 任务为什么一直 admitted false",
            "clusterqueue 配额不够怎么看",
            "gpu resource flavor 是什么",
            "训练任务等不到资源怎么查",
            "kueue 队列里有资源为什么不调度",
        ],
        content=(
            "适用场景:PyTorchJob 已提交但长期排队,客户想知道是 GPU 不够、队列配额不够,还是任务声明不匹配。"
            "Kueue 使用 ClusterQueue、LocalQueue、ResourceFlavor 和配额来决定 Workload 何时被接纳。\n\n"
            "排查顺序:\n"
            "1. 看 Workload 状态,确认是否已 admitted。\n"
            "2. 看 LocalQueue 是否绑定到预期 ClusterQueue。\n"
            "3. 看 ClusterQueue 中对应 GPU flavor 的可用配额。\n"
            "4. 检查任务请求的 GPU 数、CPU、内存是否超过队列上限。\n"
            "5. 如果使用 gang scheduling,所有副本资源都满足后才可能启动。\n\n"
            "注意:客户看到 Pod Pending 时,不要只查 Pod。Kueue 场景下要从 Workload/Queue 这一层看调度原因。"
        ),
        source_refs=["kueue-docs:pytorchjob", "kubeflow-docs:job-scheduling"],
    ),
    chunk(
        chunk_id="ext-kubeflow-trainer-trainjob-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="Kubeflow Trainer 适合 Kubernetes 原生分布式训练",
        question_patterns=[
            "kubeflow trainer 是什么",
            "pytorchjob 和 trainjob 有什么区别",
            "kubernetes 上怎么跑多节点训练",
            "kubeflow trainer 能跑 mpi 吗",
            "训练平台怎么管理分布式任务",
        ],
        content=(
            "适用场景:客户不满足于手写 `torchrun` 和 SSH 脚本,希望在 Kubernetes 上以训练任务对象管理多节点、多 GPU 作业。"
            "Kubeflow Trainer 文档定位为 Kubernetes-native 的分布式 AI 模型训练项目,用于编排多框架训练作业。\n\n"
            "处理建议:\n"
            "1. 单机调试阶段先用普通脚本跑通,再迁移到 Trainer/Kubeflow。\n"
            "2. 把镜像、训练入口、数据路径、GPU 资源和副本数写进任务规格。\n"
            "3. 与 Kueue/Volcano 结合时,关注 gang scheduling 和队列状态。\n"
            "4. 任务失败先看 driver/worker Pod 日志和事件,再看训练框架日志。\n"
            "5. 多节点训练要提前验证镜像一致、数据可见、网络和 NCCL。\n\n"
            "注意:Kubeflow Trainer 是集群平台能力,不是单台云主机里的 Python 包。客户需要有 Kubernetes 和对应控制器。"
        ),
        source_refs=["kubeflow-docs:trainer-overview", "kubeflow-docs:job-scheduling"],
    ),
    chunk(
        chunk_id="ext-volcano-gang-scheduling-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="Volcano gang scheduling 避免分布式训练只起一部分 worker",
        question_patterns=[
            "volcano gang scheduling 是什么",
            "分布式训练只启动部分 worker 会怎样",
            "kubeflow on volcano 怎么用",
            "pytorchjob 为什么需要 gang scheduling",
            "多机训练资源不齐能不能先跑",
        ],
        content=(
            "适用场景:分布式训练需要多个 worker 同时启动,但普通调度可能只启动一部分 Pod,导致任务等待、超时或浪费 GPU。"
            "Kubeflow job scheduling 文档提到可用 Volcano 等调度器支持 gang scheduling。\n\n"
            "处理建议:\n"
            "1. 多 worker 训练任务需要所有关键 Pod 都拿到资源后再运行。\n"
            "2. gang scheduling 可以避免只占住部分 GPU 却无法开始训练。\n"
            "3. 配置时要确认任务的最小可运行副本数和资源请求。\n"
            "4. 如果一直排队,说明集群当前无法一次性满足整组资源,需要降副本数或等待资源。\n"
            "5. 对单 Pod 推理服务或单机脚本,通常不需要 gang scheduling。\n\n"
            "注意:gang scheduling 提升的是分布式作业调度正确性,不是训练性能优化。"
        ),
        source_refs=["kubeflow-docs:job-scheduling", "volcano-docs:kubeflow"],
    ),
    chunk(
        chunk_id="ext-kubeflow-job-failure-logs-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="Kubeflow / Kueue 训练任务失败时先看哪些对象",
        question_patterns=[
            "kubeflow 训练任务失败怎么查",
            "pytorchjob pod 失败看哪里",
            "kueue workload admitted 了但任务没跑",
            "分布式训练 worker 日志怎么收集",
            "训练 job pending failed 怎么定位",
        ],
        content=(
            "适用场景:客户用 Kubeflow/Kueue 提交训练任务后,状态显示 Pending、Failed 或部分 Pod 退出,不知道从哪里查。"
            "这类任务有调度层、控制器层和训练脚本层,需要按层排查。\n\n"
            "排查顺序:\n"
            "1. 看 Workload/Queue 是否已 admitted,排除队列和配额问题。\n"
            "2. 看 PyTorchJob/TrainJob 状态和事件,确认控制器是否创建了 Pod。\n"
            "3. 看 driver/master Pod 日志,通常包含分布式启动错误。\n"
            "4. 看各 worker Pod 日志,确认是否 OOM、镜像拉取失败、数据路径不可见或 NCCL 错误。\n"
            "5. 如果是节点资源问题,再看 Pod events、node 状态和 GPU device plugin。\n\n"
            "注意:不要只看最后一个失败 Pod。分布式任务常常是一个 rank 先报错,其他 rank 被动退出。"
        ),
        source_refs=["kueue-docs:pytorchjob", "kubeflow-docs:trainer-overview", "kubeflow-docs:job-scheduling"],
    ),
    chunk(
        chunk_id="ext-lm-eval-harness-basic-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="用 lm-evaluation-harness 做大模型基准评测",
        question_patterns=[
            "lm evaluation harness 怎么用",
            "模型微调后怎么跑基准测试",
            "怎么验证新模型有没有退化",
            "mmlu gsm8k 这类评测怎么跑",
            "上线前怎么比较两个大模型",
        ],
        content=(
            "适用场景:客户微调或换模型后,想用统一任务集比较模型质量,而不是只凭几条人工提问判断效果。"
            "EleutherAI lm-evaluation-harness 提供统一框架,可在多种生成任务上评测语言模型。\n\n"
            "处理建议:\n"
            "1. 先选和业务相关的任务集,不要只追通用榜单分数。\n"
            "2. 固定模型版本、prompt、采样参数、精度和数据集版本。\n"
            "3. 对自托管模型,确认评测工具支持本地模型或 OpenAI 兼容接口。\n"
            "4. 记录每次评测的 commit、模型路径、量化方式和硬件。\n"
            "5. 评测结果只说明任务集表现,不能替代真实业务验收。\n\n"
            "注意:基准评测可能消耗大量 token 和 GPU 时间。正式跑前先用少量样本验证命令、路径和输出格式。"
        ),
        source_refs=["lm-eval-repo:README", "lm-eval-repo:task-guide"],
    ),
    chunk(
        chunk_id="ext-lm-eval-custom-task-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="lm-evaluation-harness 自定义任务用于业务验收",
        question_patterns=[
            "lm eval 能不能评自己的题库",
            "怎么把业务测试集接到 lm-evaluation-harness",
            "自定义大模型评测任务怎么写",
            "评测题和答案怎么组织",
            "模型上线前业务集怎么自动验收",
        ],
        content=(
            "适用场景:客户已有业务题库、客服问答或内部验收集,希望用统一评测流程判断模型是否可上线。"
            "lm-evaluation-harness task guide 说明可以定义和扩展评测任务。\n\n"
            "处理建议:\n"
            "1. 把业务问题、标准答案、评分方式和数据版本固定下来。\n"
            "2. 区分选择题、短答案、生成式问答和 RAG 问答,评分方式不要混用。\n"
            "3. 自定义任务先用 5-10 条样例跑通,再扩到全量。\n"
            "4. 评测输出要保存到文件,便于回归比较。\n"
            "5. 对生成式业务问答,最好同时保留人工抽检或 LLM judge 复核。\n\n"
            "注意:自定义评测的核心不是工具,而是题库质量和评分标准。题库含糊会导致评测结果不可解释。"
        ),
        source_refs=["lm-eval-repo:task-guide", "lm-eval-repo:README"],
    ),
    chunk(
        chunk_id="ext-mlflow-genai-evaluation-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="MLflow GenAI Evaluation 评测 LLM 和 Agent 应用",
        question_patterns=[
            "mlflow 怎么评测 llm 应用",
            "agent 回答质量怎么持续监控",
            "rag 应用上线前怎么做评测",
            "llm judge 和自定义 scorer 怎么用",
            "模型应用质量怎么记录到 mlflow",
        ],
        content=(
            "适用场景:客户已经有 RAG、Agent 或 LLM 应用,希望系统化记录问题、回答、评分和版本,而不是只手工测试。"
            "MLflow GenAI evaluation 文档提供内置和自定义 scorer,用于测量、改进和监控 LLM/Agent 应用质量。\n\n"
            "处理建议:\n"
            "1. 先定义评测数据集:问题、上下文、期望答案或评分标准。\n"
            "2. 用内置 scorer 覆盖通用质量维度,再按业务补自定义 scorer。\n"
            "3. 每次模型、prompt、检索库或工具变更都记录实验版本。\n"
            "4. 上线前比较新旧版本分数和失败样例,不要只看平均分。\n"
            "5. 线上抽样监控要注意脱敏,避免把敏感用户内容写入日志。\n\n"
            "注意:LLM judge 不是绝对真值。重要业务仍需人工抽检和明确拒答/引用规则。"
        ),
        source_refs=["mlflow-docs:genai-eval", "mlflow-docs:tracking"],
    ),
    chunk(
        chunk_id="ext-mlflow-model-registry-promotion-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="MLflow Model Registry 管理模型版本和上线流转",
        question_patterns=[
            "mlflow model registry 有什么用",
            "训练好多版模型怎么管理",
            "模型从测试到上线怎么标记",
            "怎么回滚到旧模型版本",
            "模型文件和指标怎么关联",
        ],
        content=(
            "适用场景:客户反复训练和微调模型,需要管理不同版本、指标、文件和上线状态。"
            "MLflow Model Registry 用于注册模型版本、管理生命周期和关联实验记录。\n\n"
            "处理建议:\n"
            "1. 每次训练记录参数、指标、代码版本和模型文件。\n"
            "2. 把可候选上线的模型注册到 Model Registry。\n"
            "3. 用别名或阶段标记区分候选、生产和归档版本。\n"
            "4. 上线前保留评测报告和回滚目标版本。\n"
            "5. 模型服务部署时记录具体 registry 版本,避免“用了哪个模型”说不清。\n\n"
            "注意:Registry 管版本,不替你判断模型是否好。上线仍要结合离线评测、业务验收和安全检查。"
        ),
        source_refs=["mlflow-docs:model-registry", "mlflow-docs:tracking", "mlflow-docs:pytorch"],
    ),
    chunk(
        chunk_id="ext-eval-observability-release-gate-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="模型上线前把评测、监控和回滚做成发布门槛",
        question_patterns=[
            "模型上线前要检查哪些指标",
            "怎么判断新模型可以替换旧模型",
            "llm 应用发布门槛怎么设",
            "模型服务上线后怎么监控质量",
            "模型效果变差怎么回滚",
        ],
        content=(
            "适用场景:客户要把微调模型或 RAG/Agent 应用上线,希望有可执行的验收和回滚标准。"
            "可把 lm-evaluation-harness/MLflow 这类离线评测,与线上 vLLM/LiteLLM 指标结合起来做发布门槛。\n\n"
            "建议门槛:\n"
            "1. 离线业务评测集通过,关键分组不能退化。\n"
            "2. 延迟、吞吐、错误率和 GPU 利用率达到目标。\n"
            "3. 日志、metrics、trace 能定位单个请求问题。\n"
            "4. 模型版本、prompt 版本和知识库版本可追溯。\n"
            "5. 保留旧版本和配置,出现质量或稳定性问题能回滚。\n\n"
            "注意:不要只用“几条样例回答不错”作为上线依据。模型、数据、检索、工具和服务指标要一起验收。"
        ),
        source_refs=["lm-eval-repo:README", "mlflow-docs:genai-eval", "vllm-docs:design/metrics", "litellm-docs:users-budgets"],
    ),
    chunk(
        chunk_id="ext-ultralytics-yolo-custom-training-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="Ultralytics YOLO 自定义数据训练目标检测模型",
        question_patterns=[
            "yolo 怎么训练自己的数据集",
            "ultralytics train data yaml 怎么写",
            "目标检测数据集怎么组织",
            "yolo 训练用 gpu 怎么启动",
            "训练检测模型前要准备什么",
        ],
        content=(
            "适用场景:客户想在 GPU 实例上训练自己的目标检测模型,例如工业缺陷、商品、人体或实验图像识别。"
            "Ultralytics YOLO train 文档提供 `yolo train` 入口和数据配置方式。\n\n"
            "处理建议:\n"
            "1. 先确认数据标注格式、类别名和 train/val 划分正确。\n"
            "2. 准备数据配置 YAML,指向图片和标签路径。\n"
            "3. 用小模型、小 epoch 先跑通训练,确认 GPU 可用和 loss 正常。\n"
            "4. 再增加分辨率、batch、epoch 或换更大模型。\n"
            "5. 训练后用验证集和真实样例检查误检、漏检和类别混淆。\n\n"
            "示例:\n"
            "```bash\n"
            "yolo detect train model=yolo11n.pt data=data.yaml imgsz=640 epochs=10 device=0\n"
            "```\n\n"
            "注意:大多数训练问题来自数据路径、标签格式或类别映射错误,不要一上来只看显存。"
        ),
        source_refs=["ultralytics-docs:train", "label-studio-docs:guide"],
    ),
    chunk(
        chunk_id="ext-ultralytics-yolo-oom-performance-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="YOLO 训练 OOM、速度慢和 GPU 利用率低排查",
        question_patterns=[
            "yolo 训练爆显存怎么办",
            "ultralytics 训练很慢怎么查",
            "yolo batch imgsz 怎么调",
            "目标检测 gpu 利用率低",
            "yolo 多卡训练怎么排查",
        ],
        content=(
            "适用场景:YOLO 训练时报 CUDA OOM、训练速度慢或 GPU 利用率上不去。"
            "Ultralytics train 文档暴露了 batch、imgsz、device 等训练参数,排查时要同时看模型大小、分辨率和数据加载。\n\n"
            "排查顺序:\n"
            "1. OOM 时先降低 `batch` 和 `imgsz`,再考虑换小模型。\n"
            "2. 如果 GPU 利用率低,检查数据是否在慢盘、图片是否过大、workers 是否太低。\n"
            "3. 先单卡跑通,再用多卡,避免分布式问题掩盖数据问题。\n"
            "4. 保存训练日志、显存峰值和每 epoch 时间,便于比较不同配置。\n"
            "5. 验证阶段也可能 OOM,需要单独调 batch 或图片尺寸。\n\n"
            "注意:提升 imgsz 通常会显著增加显存和计算量。客户想要更高精度时,要同时评估成本和训练时间。"
        ),
        source_refs=["ultralytics-docs:train", "pytorch-docs:notes/cuda"],
    ),
    chunk(
        chunk_id="ext-sam2-install-checkpoints-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="SAM2 安装、checkpoint 准备和图像/视频分割入口",
        question_patterns=[
            "sam2 怎么安装和下载模型",
            "segment anything 2 checkpoint 放哪里",
            "sam2 能分割视频吗",
            "图像分割模型怎么在 gpu 上跑",
            "sam2 demo 缺权重怎么处理",
        ],
        content=(
            "适用场景:客户想用 SAM2 做图像或视频分割,但不知道代码、checkpoint 和输入输出如何准备。"
            "Meta SAM2 仓库提供安装、模型 checkpoint 和图像/视频预测示例。\n\n"
            "处理建议:\n"
            "1. 按仓库说明安装依赖并确认 PyTorch/CUDA 可用。\n"
            "2. 下载与配置匹配的 SAM2 checkpoint,放到脚本能访问的路径。\n"
            "3. 图像分割和视频分割入口不同,先用官方最小示例跑通。\n"
            "4. 视频任务会占更多显存和磁盘 IO,先用短视频验证。\n"
            "5. 对外服务前要明确输入大小、并发和输出格式。\n\n"
            "注意:SAM2 是视觉基础模型,不是自动标注全流程。具体业务仍需要提示点/框、后处理和人工抽检。"
        ),
        source_refs=["sam2-repo:README", "pytorch-docs:get-started"],
    ),
    chunk(
        chunk_id="ext-sam2-video-segmentation-oom-001",
        product_area="gpu_troubleshooting",
        source_type="runbook",
        source_origin="external_official",
        title="SAM2 视频分割显存、帧数和分辨率排查",
        question_patterns=[
            "sam2 视频分割爆显存怎么办",
            "sam2 处理长视频很慢",
            "segment anything 2 视频帧太多",
            "sam2 分割结果不稳定怎么查",
            "视频分割 gpu 资源怎么估算",
        ],
        content=(
            "适用场景:SAM2 处理视频时 OOM、速度慢、临时文件大或分割结果不稳定。"
            "视频分割比单图更依赖帧数、分辨率、提示质量和显存余量。\n\n"
            "处理建议:\n"
            "1. 先把视频截短、降分辨率,用少量帧验证流程。\n"
            "2. OOM 时降低输入分辨率、分段处理视频或换更小模型配置。\n"
            "3. 把临时帧和输出放到空间足够的数据盘,不要写满系统盘。\n"
            "4. 分割漂移时检查提示点/框是否稳定,必要时增加关键帧提示。\n"
            "5. 服务化时限制上传文件大小、视频时长和并发。\n\n"
            "注意:视频分割的成本不只看模型大小,还要看帧数、分辨率、输出保存和后处理。"
        ),
        source_refs=["sam2-repo:README", "pytorch-docs:notes/cuda"],
    ),
    chunk(
        chunk_id="ext-lammps-gpu-package-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="LAMMPS GPU package 加速分子模拟的前置条件",
        question_patterns=[
            "lammps 怎么用 gpu",
            "lammps gpu package 需要 cuda 吗",
            "分子模拟任务怎么确认跑在显卡上",
            "lammps pair style 没用 gpu 怎么查",
            "ai4science lammps gpu 环境怎么配",
        ],
        content=(
            "适用场景:科研客户要用 LAMMPS 在 GPU 实例上跑分子动力学模拟,但不确定安装包、pair style 和 CUDA 是否支持。"
            "LAMMPS GPU package 文档说明 CUDA 模式需要 NVIDIA GPU 和对应 CUDA Toolkit,并非所有模拟部分都会自动上 GPU。\n\n"
            "处理建议:\n"
            "1. 确认当前 LAMMPS 构建包含 GPU package。\n"
            "2. 确认输入脚本使用的 pair style、kspace 或命令支持 GPU 加速。\n"
            "3. 运行时查看 LAMMPS 输出,确认 GPU package 被启用。\n"
            "4. 对比 CPU 和 GPU 运行时间,不要只看 `nvidia-smi` 有无瞬时波动。\n"
            "5. 多 GPU/多节点时还要关注 MPI、NCCL/网络和任务划分。\n\n"
            "注意:LAMMPS GPU 加速是按功能模块支持的。脚本里大部分计算如果不支持 GPU,整体速度可能提升有限。"
        ),
        source_refs=["lammps-docs:gpu-package", "nvidia-docs:cuda-install-linux"],
    ),
    chunk(
        chunk_id="ext-pyscf-gpu4pyscf-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="PySCF / GPU4PySCF 把量子化学计算迁移到 GPU",
        question_patterns=[
            "pyscf 怎么用 gpu",
            "gpu4pyscf 怎么安装",
            "pyscf to_gpu 是什么",
            "量子化学 dft 能不能用显卡加速",
            "gpu4pyscf 支持哪些显卡",
        ],
        content=(
            "适用场景:科研客户用 PySCF 做量子化学/DFT 计算,希望用 NVIDIA GPU 加速。"
            "PySCF GPU 文档说明 PySCF 对象和 GPU4PySCF 对象可通过 `to_gpu()` / `to_cpu()` 转换;GPU4PySCF 仓库说明二进制包支持一定计算能力以上的 NVIDIA GPU。\n\n"
            "处理建议:\n"
            "1. 先确认 Python、CUDA、CuPy/GPU4PySCF 安装匹配。\n"
            "2. 用最小分子和小基组跑通 `to_gpu()` 示例。\n"
            "3. 检查当前方法和积分是否已有 GPU 实现,不是所有 PySCF 功能都等价加速。\n"
            "4. 大体系要同时关注 GPU 显存和 CPU 内存。\n"
            "5. 结果要和 CPU 版本做小样本对比,确认数值和收敛正常。\n\n"
            "注意:GPU4PySCF 适合特定量子化学工作流。客户问“PySCF 能否全部上 GPU”时,应按具体方法和版本确认。"
        ),
        source_refs=["pyscf-docs:gpu", "gpu4pyscf-repo:README", "cupy-docs:install"],
    ),
    chunk(
        chunk_id="ext-apptainer-gpu-container-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="Apptainer / Singularity 在科研/HPC 场景使用 GPU 容器",
        question_patterns=[
            "apptainer 怎么用 nvidia gpu",
            "singularity 容器里看不到显卡",
            "hpc 不能用 docker 怎么跑 cuda 镜像",
            "apptainer --nv 是什么",
            "科研软件容器怎么带 gpu 运行",
        ],
        content=(
            "适用场景:科研客户来自 HPC 习惯,希望用 Apptainer/Singularity 运行 CUDA 容器,而不是 Docker。"
            "Apptainer GPU 文档说明可通过 NVIDIA CUDA GPU 支持运行加速应用,常见入口是 `--nv`。\n\n"
            "处理建议:\n"
            "1. 先确认宿主机 NVIDIA 驱动和 `nvidia-smi` 正常。\n"
            "2. 运行容器时带上 GPU 支持选项,例如 `apptainer exec --nv image.sif nvidia-smi`。\n"
            "3. 容器内 CUDA runtime 要和宿主驱动兼容。\n"
            "4. 数据目录要显式 bind/mount,不要假设容器能看到宿主所有路径。\n"
            "5. 从 Docker/NGC 镜像转换时,先用最小命令验证 GPU 可见再跑完整软件。\n\n"
            "注意:Apptainer 和 Docker 的权限、挂载和网络模型不同。客户迁移脚本时要逐项检查路径和环境变量。"
        ),
        source_refs=["apptainer-docs:gpu", "nvidia-docs:cuda-compatibility"],
    ),
    chunk(
        chunk_id="ext-webdataset-sharded-tar-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="WebDataset tar shards 适合海量小文件训练数据",
        question_patterns=[
            "webdataset 是什么",
            "训练图片太多小文件读取慢怎么办",
            "tar shards 数据集怎么用",
            "多 gpu 训练数据 io 很慢",
            "huggingface webdataset 怎么组织",
        ],
        content=(
            "适用场景:客户训练图像、视频或多模态模型时有海量小文件,文件系统遍历和随机读取很慢。"
            "Hugging Face WebDataset 文档说明大规模 WebDataset 由多个 tar shard 组成,每个 shard 通常是一个 tar 归档;WebDataset 项目支持 PyTorch 数据加载。\n\n"
            "处理建议:\n"
            "1. 把海量小文件按 shard 打包,减少文件系统元数据压力。\n"
            "2. shard 大小保持相对均衡,便于多 worker 和多 GPU 分配。\n"
            "3. 训练前先用少量 shard 跑通解码、shuffle 和 batch。\n"
            "4. 数据放对象存储或网络盘时,要额外关注流式读取和缓存。\n"
            "5. 记录 shard 生成脚本和数据版本,避免训练集不可复现。\n\n"
            "注意:WebDataset 能改善大规模数据 IO 组织,但不会自动修复坏样本、标签错误或网络带宽不足。"
        ),
        source_refs=["hf-docs:datasets-webdataset", "webdataset-repo:README", "hf-datasets:stream"],
    ),
    chunk(
        chunk_id="ext-onnxruntime-quantization-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="ONNX Runtime 量化适合传统模型和部分 Transformer 推理优化",
        question_patterns=[
            "onnxruntime 怎么量化模型",
            "onnx int8 量化有什么用",
            "pytorch 模型导出 onnx 后怎么变小",
            "onnx 动态量化和静态量化区别",
            "cpu 或 gpu 推理怎么用 onnx 优化",
        ],
        content=(
            "适用场景:客户有 PyTorch/ONNX 模型,希望通过 ONNX Runtime 量化降低模型大小、提升部分推理性能或降低 CPU/GPU 资源。"
            "ONNX Runtime 量化文档提供把 32-bit 浮点模型转换为 8-bit 整数量化模型的 Python API。\n\n"
            "处理建议:\n"
            "1. 先确认模型能稳定导出并用 ONNX Runtime 正确推理。\n"
            "2. 动态量化更容易上手,静态量化通常需要校准数据。\n"
            "3. 量化后必须用业务样例验证精度,不要只看模型文件变小。\n"
            "4. GPU 推理还要确认执行 provider 和算子支持,否则可能回退 CPU。\n"
            "5. LLM 场景常见 AWQ/GPTQ/GGUF/框架量化不等同于 ONNX int8,要按部署引擎选择。\n\n"
            "注意:量化是精度、速度、体积和兼容性的取舍。不同硬件和模型结构收益差异很大。"
        ),
        source_refs=["onnxruntime-docs:quantization", "hf-optimum-onnx:gpu"],
    ),
    chunk(
        chunk_id="ext-optimum-onnx-gpu-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="Hugging Face Optimum ONNX Runtime 在 NVIDIA GPU 上推理",
        question_patterns=[
            "optimum onnx gpu 怎么用",
            "onnxruntime gpu provider 怎么选",
            "transformers 模型能不能导出 onnx 加速",
            "tensorrt execution provider 是什么",
            "onnx 模型为什么没用上 gpu",
        ],
        content=(
            "适用场景:客户想把 Transformers 模型导出到 ONNX,用 ONNX Runtime GPU 或 TensorRT execution provider 做推理优化。"
            "Hugging Face Optimum ONNX 文档提供 NVIDIA GPU 加速推理路径,包括 ONNX Runtime 和 TensorRT provider 相关用法。\n\n"
            "处理建议:\n"
            "1. 先用原始 Transformers 模型确认输出正确。\n"
            "2. 导出 ONNX 后先用 CPU/GPU provider 跑最小样例。\n"
            "3. 检查 ONNX Runtime 是否安装 GPU 版本,CUDA/cuDNN/TensorRT 是否匹配。\n"
            "4. 使用 TensorRT provider 前确认模型算子、shape 和精度支持。\n"
            "5. 比较延迟和吞吐时固定 batch、序列长度和输入 shape。\n\n"
            "注意:ONNX/TensorRT 优化更适合稳定 shape 和生产推理。频繁变化的输入可能需要额外 profile 或回退。"
        ),
        source_refs=["hf-optimum-onnx:gpu", "onnxruntime-docs:quantization", "nvidia-triton:tensorrt-llm"],
    ),
    chunk(
        chunk_id="ext-transformers-awq-gptq-quantization-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="Transformers 量化: AWQ、GPTQ、bitsandbytes 和部署引擎选择",
        question_patterns=[
            "awq gptq bitsandbytes 有什么区别",
            "量化模型怎么在 transformers 里加载",
            "4bit 模型能不能直接 vllm 部署",
            "gptq awq gguf 应该选哪个",
            "量化后为什么速度没变快",
        ],
        content=(
            "适用场景:客户下载到 AWQ/GPTQ/bitsandbytes/GGUF 等量化模型,不知道用哪个框架加载、能否训练或部署。"
            "Transformers quantization 文档覆盖多种量化后端,不同格式和运行引擎兼容性不同。\n\n"
            "处理建议:\n"
            "1. 先看模型卡片说明支持的加载方式和推荐依赖。\n"
            "2. bitsandbytes 常用于 Transformers 中 8bit/4bit 加载和 QLoRA 训练。\n"
            "3. AWQ/GPTQ 多用于推理压缩,要确认 vLLM/Transformers 当前是否支持该模型架构和量化格式。\n"
            "4. GGUF 主要面向 llama.cpp/Ollama 生态,不是普通 PyTorch checkpoint。\n"
            "5. 量化可能省显存,但速度取决于 kernel、硬件、batch、上下文和引擎支持。\n\n"
            "注意:不要把所有 4bit 文件都当成同一种格式。格式不匹配时,常见表现是加载失败、输出异常或完全走不到 GPU 优化路径。"
        ),
        source_refs=["hf-transformers:quantization", "hf-transformers:bitsandbytes", "vllm-docs:configuration/conserving_memory"],
    ),
    chunk(
        chunk_id="ext-gguf-llama-cpp-convert-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="GGUF / llama.cpp 与 Transformers 权重格式的区别",
        question_patterns=[
            "gguf 文件能不能用 transformers 加载",
            "huggingface 模型怎么转 gguf",
            "ollama modelfile 和 gguf 什么关系",
            "llama.cpp 量化模型和 safetensors 区别",
            "下载到 gguf 后应该用什么跑",
        ],
        content=(
            "适用场景:客户下载到 `.gguf` 文件或想把 Hugging Face safetensors 模型转成 llama.cpp/Ollama 可用格式。"
            "llama.cpp 是 GGUF 生态的主要项目;Ollama 也可通过 Modelfile 使用本地 GGUF 文件。\n\n"
            "处理建议:\n"
            "1. `.safetensors` / PyTorch checkpoint 通常给 Transformers、vLLM、SGLang 等使用。\n"
            "2. `.gguf` 通常给 llama.cpp 或 Ollama 生态使用,不能直接当普通 Transformers 权重加载。\n"
            "3. 转换前确认 tokenizer、模型架构和量化目标受支持。\n"
            "4. 转换后用小 prompt 验证输出,再做长上下文或服务化。\n"
            "5. 如果目标是 GPU 高吞吐服务,还要比较 llama.cpp/Ollama 与 vLLM/SGLang 的性能和功能差异。\n\n"
            "注意:GGUF 方便本地和轻量部署,但与训练/微调常用权重格式不同。客户要先明确“跑推理”还是“继续训练”。"
        ),
        source_refs=["llama-cpp-repo:README", "ollama-docs:modelfile"],
    ),
]

STABLE_PLATFORM_EXTERNAL_CHUNKS = [
    chunk(
        chunk_id="ext-api-compatible-base-url-key-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="OpenAI 兼容接口的 base URL、API Key 和模型名排查",
        question_patterns=[
            "openai compatible api base url 怎么填",
            "接口返回 model not found 怎么查",
            "api key 明明填了还是 unauthorized",
            "同一个 sdk 怎么切到自建模型服务",
            "模型接口连不上先检查什么",
        ],
        content=(
            "适用场景:客户使用 OpenAI SDK、兼容网关或自建推理服务时,接口地址、密钥和模型名填错导致无法调用。"
            "这类问题和具体平台无关,先按协议层排查。\n\n"
            "排查顺序:\n"
            "1. 确认 `base_url` 指向 API 根路径,常见形态是以 `/v1` 结尾。\n"
            "2. 确认请求头里有 Bearer API Key,且没有把密钥写进 URL 或日志。\n"
            "3. 用 `/models` 或服务文档确认对外模型名,不要把本地目录路径误填成模型名。\n"
            "4. 用最小 `curl` 请求验证连通性,再接入应用框架。\n"
            "5. 区分 401/403 鉴权失败、404 路径或模型名错误、5xx 服务端异常。\n\n"
            "注意:OpenAI 兼容通常表示接口形状相近,不代表所有参数、模型能力和错误码完全一致。"
        ),
        source_refs=["openai-docs:api-reference", "litellm-docs:openai-compatible", "vllm-docs:cli/serve"],
    ),
    chunk(
        chunk_id="ext-api-compatible-streaming-sse-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="流式输出使用 SSE 时的客户端、代理和超时排查",
        question_patterns=[
            "openai 接口 stream=true 没有逐字返回",
            "流式输出被 nginx 一次性返回",
            "sse 连接过一会儿断开",
            "大模型接口怎么做流式响应",
            "客户端收不到 delta 怎么查",
        ],
        content=(
            "适用场景:聊天或 Agent 应用希望边生成边返回,但客户端看不到流式输出、被代理缓冲,或长连接中途断开。"
            "OpenAI 兼容接口常用 HTTP streaming/SSE 形态传递增量结果。\n\n"
            "排查顺序:\n"
            "1. 确认请求确实打开了 streaming 参数,客户端 SDK 也按迭代流读取。\n"
            "2. 用 `curl -N` 或等价方式绕过业务代码,确认服务端是否逐段返回。\n"
            "3. 反向代理前面要关闭响应缓冲,并调大读超时和空闲超时。\n"
            "4. 前端或网关不要等待完整 JSON 后再渲染,要按事件增量处理。\n"
            "5. 保存 request id、首 token 时间、总耗时和断开位置,便于定位是模型慢、代理断还是客户端读法错。\n\n"
            "注意:流式输出改善用户体感,但不会降低总计算量。服务端仍需要限流和最大输出长度保护。"
        ),
        source_refs=["openai-docs:streaming", "mdn-docs:sse", "nginx-docs:proxy-buffering"],
    ),
    chunk(
        chunk_id="ext-api-compatible-error-retry-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="模型 API 错误码、重试和幂等保护",
        question_patterns=[
            "模型 api 429 500 502 怎么重试",
            "大模型接口偶发超时怎么办",
            "请求失败能不能直接重试",
            "视频或图片任务重复提交怎么避免",
            "api 报 rate limit exceeded 怎么处理",
        ],
        content=(
            "适用场景:业务调用模型 API 时遇到限流、超时或 5xx,需要判断是否重试以及如何避免重复任务。"
            "稳定做法是按错误类型分层处理,不要对所有失败都无脑重试。\n\n"
            "处理建议:\n"
            "1. 401/403 先查密钥和权限,通常不应自动重试。\n"
            "2. 400 类参数错误先修请求体,重试不会解决。\n"
            "3. 429、连接超时、部分 5xx 可做指数退避重试,并设置最大次数。\n"
            "4. 图片、视频、长任务提交要保存本地任务 ID 或请求摘要,避免重复扣费或重复生成。\n"
            "5. 记录错误码、响应体、模型名、输入长度和重试次数,不要只保存一句“调用失败”。\n\n"
            "注意:重试策略要和限流、预算和队列一起设计。过度重试会放大服务压力。"
        ),
        source_refs=["openai-docs:errors", "aws-docs:exponential-backoff", "google-docs:retry-strategy"],
    ),
    chunk(
        chunk_id="ext-api-tool-calling-contract-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="Tool / function calling 的参数契约和执行边界",
        question_patterns=[
            "function calling 返回了参数下一步怎么执行",
            "tool call 为什么参数不符合 schema",
            "agent 工具调用失败怎么定位",
            "模型能不能直接执行函数",
            "工具调用要不要做参数校验",
        ],
        content=(
            "适用场景:客户把模型接入工具或业务函数,以为模型会直接执行操作,或遇到工具参数不合法。"
            "Tool/function calling 本质是让模型输出结构化的工具调用意图,真正执行仍由应用代码完成。\n\n"
            "处理建议:\n"
            "1. 为每个工具定义清晰名称、参数 schema、必填字段和描述。\n"
            "2. 服务端必须重新校验参数,不要因为是模型生成就直接执行。\n"
            "3. 工具执行失败时,把安全可公开的错误摘要返回给模型继续处理。\n"
            "4. 对写操作、付费操作和删除操作增加人工确认或权限检查。\n"
            "5. 记录模型输出、参数校验结果、工具返回和最终回答,便于复盘。\n\n"
            "注意:工具调用不是权限系统。模型只能提出调用建议,应用侧才是安全边界。"
        ),
        source_refs=["openai-docs:function-calling", "anthropic-docs:tool-use", "modelcontextprotocol-docs:concepts"],
    ),
    chunk(
        chunk_id="ext-api-structured-output-schema-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="结构化输出用 JSON Schema 降低格式不稳定",
        question_patterns=[
            "模型回答必须是 json 怎么保证",
            "structured output 和普通 prompt 区别",
            "json schema 输出失败怎么查",
            "模型多返回解释文字导致解析失败",
            "业务系统接大模型结果怎么做格式校验",
        ],
        content=(
            "适用场景:业务系统要求模型稳定返回 JSON、枚举或固定字段,普通 prompt 约束不够稳定。"
            "结构化输出通过 schema 或受约束解码减少格式漂移,但仍要在应用侧校验。\n\n"
            "处理建议:\n"
            "1. schema 先保持简单,字段名、类型和必填项清楚。\n"
            "2. 复杂嵌套、长数组和宽松描述会增加失败或截断风险。\n"
            "3. 给输出预留足够 token,避免 JSON 生成到一半被截断。\n"
            "4. 服务端解析后仍要做类型、枚举、长度和业务规则校验。\n"
            "5. 如果格式正确但事实错误,要另行加入检索、引用和评测,结构化输出本身不保证事实正确。\n\n"
            "注意:结构化输出解决的是格式问题,不是可信性问题。"
        ),
        source_refs=["openai-docs:structured-outputs", "json-schema-docs:overview", "vllm-docs:features/structured_outputs"],
    ),
    chunk(
        chunk_id="ext-api-embedding-rerank-basics-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="Embedding、向量检索和 rerank 的稳定分工",
        question_patterns=[
            "embedding 和 rerank 有什么区别",
            "rag 检索为什么要先向量再重排",
            "向量库搜不到正确文档怎么查",
            "知识库相似度低是不是模型不行",
            "reranker 能解决什么问题",
        ],
        content=(
            "适用场景:客户做 RAG 或语义搜索时,不清楚 embedding、向量库和 reranker 各自负责什么。"
            "稳定分工是:embedding 负责把文本变成可比较的向量,向量库负责粗召回,reranker 负责对候选做更精细排序。\n\n"
            "处理建议:\n"
            "1. 先确认查询、文档标题、正文和元数据都进入了索引。\n"
            "2. 粗召回漏掉时,检查切块大小、关键词缺失、语言混用和过滤条件。\n"
            "3. 粗召回有结果但排序差时,再考虑 rerank。\n"
            "4. 更新文档后必须重建索引或增量写入,否则检索仍用旧内容。\n"
            "5. 用固定问题集评估 Top-K 命中率,不要只凭单次问答判断。\n\n"
            "注意:RAG 质量通常由文档质量、切块、索引、召回、重排和提示词共同决定。"
        ),
        source_refs=["openai-docs:embeddings", "langchain-docs:retrievers", "qdrant-docs:search"],
    ),
    chunk(
        chunk_id="ext-rag-ingestion-index-empty-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="RAG 知识库为空、索引失败和文档未生效排查",
        question_patterns=[
            "知识库上传了但问答还是说不知道",
            "rag 文档索引失败怎么查",
            "dify 知识库没有命中资料",
            "ragflow 上传 pdf 后搜不到",
            "文档更新后模型还是按旧内容回答",
        ],
        content=(
            "适用场景:客户把文档上传到 RAG 工具后,回答没有引用资料、检索为空或仍使用旧内容。"
            "这类问题优先查索引链路,不要先怀疑大模型能力。\n\n"
            "排查顺序:\n"
            "1. 确认文档解析成功,文本不是空白、乱码或只有图片。\n"
            "2. 确认切块、embedding 和写入向量库全部完成。\n"
            "3. 检查应用是否绑定了正确知识库或 collection。\n"
            "4. 更新文档后确认是否触发重建索引,旧索引是否清理。\n"
            "5. 用原文关键词和语义改写各测一次,判断是解析问题还是召回问题。\n\n"
            "注意:扫描版 PDF、表格和图片型文档通常需要 OCR 或专门解析,不能假设普通文本解析能读出内容。"
        ),
        source_refs=["dify-docs:knowledge", "ragflow-docs:document-management", "langchain-docs:document-loaders"],
    ),
    chunk(
        chunk_id="ext-rag-retrieval-quality-debug-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="RAG 回答不准时先拆开看检索、重排和生成",
        question_patterns=[
            "rag 回答不准怎么定位",
            "知识库明明有内容模型就是答错",
            "检索到了资料但回答还是乱编",
            "向量检索 topk 应该怎么调",
            "rag 效果差怎么验收",
        ],
        content=(
            "适用场景:RAG 应用交付前或使用中回答不准,需要判断问题在检索、重排、提示词还是生成。"
            "稳定做法是把链路拆开验证。\n\n"
            "排查顺序:\n"
            "1. 固定一批真实问题,先只看检索 Top-K 是否包含正确片段。\n"
            "2. 如果 Top-K 没有正确片段,优先改文档切块、标题、元数据和查询改写。\n"
            "3. 如果 Top-K 有正确片段但排序靠后,评估 rerank 或调整召回数量。\n"
            "4. 如果上下文正确但回答错,检查提示词是否要求引用、拒答和只基于上下文回答。\n"
            "5. 保存失败样例,按问题类型分组改进,不要只调一个全局参数。\n\n"
            "注意:把生成结果直接当检索质量是不可靠的。检索评测和答案评测要分开做。"
        ),
        source_refs=["langchain-docs:retrieval", "llamaindex-docs:evaluating", "mlflow-docs:genai-eval"],
    ),
    chunk(
        chunk_id="ext-rag-citation-grounding-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="RAG 引用、拒答和只基于资料回答的边界",
        question_patterns=[
            "rag 怎么要求回答带引用",
            "知识库没有答案时怎么让模型别瞎编",
            "模型回答没有引用来源怎么办",
            "怎么判断回答是不是基于资料",
            "rag 需要拒答规则吗",
        ],
        content=(
            "适用场景:客户希望 RAG 应用能说明依据,并在知识库没有答案时拒答。"
            "这通常需要检索结果、提示词和后处理共同约束。\n\n"
            "处理建议:\n"
            "1. 在上下文里保留文档标题、片段 ID 或来源标识。\n"
            "2. 提示词明确要求只基于给定资料回答,无法支持时说明未找到依据。\n"
            "3. 输出后检查引用 ID 是否来自本次检索结果,不要让模型凭空造来源。\n"
            "4. 对高风险问题增加最低相似度、最少命中数或人工复核。\n"
            "5. 评测集中加入“知识库没有答案”的问题,验证拒答行为。\n\n"
            "注意:引用格式正确不等于内容正确。仍需检查引用片段是否真的支持结论。"
        ),
        source_refs=["dify-docs:knowledge", "llamaindex-docs:response-synthesis", "mlflow-docs:genai-eval"],
    ),
    chunk(
        chunk_id="ext-dify-provider-knowledge-troubleshoot-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="Dify 接入兼容模型和知识库时的通用排查顺序",
        question_patterns=[
            "dify 接 openai 兼容接口失败",
            "dify 知识库检索不到",
            "dify 应用调用模型报错",
            "dify provider base url 怎么查",
            "dify rag 回答不引用资料",
        ],
        content=(
            "适用场景:客户在 Dify 中接入模型供应商或知识库应用,出现调用失败、检索不到或回答不引用资料。"
            "不要先查平台按钮,先按模型供应商、知识库、应用编排三层拆开。\n\n"
            "排查顺序:\n"
            "1. 单独测试模型供应商配置:base URL、API Key、模型名和网络连通性。\n"
            "2. 单独测试知识库:文档解析、索引状态和检索命中。\n"
            "3. 检查应用是否绑定了正确模型和知识库。\n"
            "4. 工作流应用要逐个节点看输入输出,定位是模型节点、知识检索节点还是工具节点失败。\n"
            "5. 保存失败请求的错误码和节点日志,不要只截图最终聊天结果。\n\n"
            "注意:Dify 是应用编排层。底层模型服务不可用或知识库未索引时,应用层配置再多也不会正常回答。"
        ),
        source_refs=["dify-docs:models", "dify-docs:knowledge", "dify-docs:workflow"],
    ),
    chunk(
        chunk_id="ext-ragflow-document-parser-index-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="RAGFlow 文档解析、切片和索引问题排查",
        question_patterns=[
            "ragflow 上传文档后问不到内容",
            "ragflow pdf 解析效果不好",
            "ragflow 文档索引一直失败",
            "ragflow 表格图片文档怎么处理",
            "ragflow 检索结果不对怎么查",
        ],
        content=(
            "适用场景:客户用 RAGFlow 做文档问答,上传文档后解析差、索引失败或检索结果不对。"
            "RAGFlow 这类工具重点在文档解析和知识库构建,排查要从原始文档质量开始。\n\n"
            "排查顺序:\n"
            "1. 先查看文档解析后的文本,确认段落、表格和图片内容是否被读出。\n"
            "2. 扫描版 PDF 或图片型资料需要 OCR,纯文本解析可能为空。\n"
            "3. 调整切片策略时,用少量文档验证检索结果,不要直接全量重建。\n"
            "4. 检查 embedding 模型、向量库和索引任务日志是否正常。\n"
            "5. 对表格、合同、论文等不同文档类型分别建立测试问题。\n\n"
            "注意:文档解析质量是 RAG 上限。解析后的文本已经错了,后面的向量检索和大模型很难补救。"
        ),
        source_refs=["ragflow-docs:document-management", "ragflow-docs:knowledge-base", "tesseract-docs:ocr"],
    ),
    chunk(
        chunk_id="ext-n8n-ai-workflow-webhook-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="n8n AI workflow 的模型节点、Webhook 和凭据排查",
        question_patterns=[
            "n8n ai workflow 调模型失败",
            "n8n webhook 收不到请求",
            "n8n 里 api key credential 怎么查",
            "n8n agent 节点执行失败",
            "n8n 工作流怎么定位哪个节点报错",
        ],
        content=(
            "适用场景:客户用 n8n 搭 AI 自动化流程,模型节点、Webhook、凭据或某个节点执行失败。"
            "n8n 是工作流编排工具,排查时要看每个节点的输入输出。\n\n"
            "排查顺序:\n"
            "1. 先手动执行到失败节点,看该节点实际收到的输入。\n"
            "2. 检查模型 API credential、base URL 和模型名是否独立可用。\n"
            "3. Webhook 失败时确认外部服务能访问 n8n 的公开地址,并区分测试 URL 和生产 URL。\n"
            "4. 节点之间传递 JSON 时检查字段路径,避免上游字段为空。\n"
            "5. 长任务要设置超时、重试和错误分支,避免一次失败中断全流程。\n\n"
            "注意:n8n 能连接很多系统,但密钥权限、网络暴露和错误处理仍需要应用侧设计。"
        ),
        source_refs=["n8n-docs:advanced-ai", "n8n-docs:webhooks", "n8n-docs:credentials"],
    ),
    chunk(
        chunk_id="ext-flowise-agent-workflow-tools-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="Flowise 可视化 Agent / LLM workflow 的工具和记忆排查",
        question_patterns=[
            "flowise agent 工具调用失败",
            "flowise 接 openai compatible 怎么查",
            "flowise rag workflow 没命中文档",
            "flowise memory 不生效",
            "可视化 llm workflow 怎么排查节点",
        ],
        content=(
            "适用场景:客户用 Flowise 这类可视化工具构建 Agent、RAG 或 LLM workflow,但模型、工具、记忆或检索节点不工作。"
            "排查方式和代码框架类似:先拆节点,再看输入输出。\n\n"
            "处理建议:\n"
            "1. 先确认模型连接节点独立可用。\n"
            "2. RAG 相关问题先看文档是否解析和索引,再看 retriever 输出。\n"
            "3. 工具调用失败时检查工具描述、参数 schema 和外部服务权限。\n"
            "4. 记忆不生效时确认会话 ID、存储后端和上下文窗口限制。\n"
            "5. 发布成 API 前加鉴权和限流,不要暴露无保护的编排接口。\n\n"
            "注意:可视化编排降低接入门槛,但不会消除模型接口、检索质量和工具安全问题。"
        ),
        source_refs=["flowise-docs:overview", "flowise-docs:agents", "langchain-docs:agents"],
    ),
    chunk(
        chunk_id="ext-langchain-langgraph-agent-state-001",
        product_area="pytorch_basics",
        source_type="faq",
        source_origin="external_official",
        title="LangChain / LangGraph Agent 的状态、工具和可观测性",
        question_patterns=[
            "langgraph agent 为什么循环停不下来",
            "langchain tool 调用参数错怎么查",
            "agent 记不住之前步骤怎么办",
            "langgraph state 怎么设计",
            "agent 执行轨迹怎么调试",
        ],
        content=(
            "适用场景:客户用 LangChain 或 LangGraph 写 Agent,出现循环、状态丢失、工具参数错或难以调试。"
            "稳定做法是把 Agent 当成带状态的流程,而不是一次普通聊天。\n\n"
            "处理建议:\n"
            "1. 明确定义 state 里保存哪些字段,哪些来自用户输入、工具结果或历史步骤。\n"
            "2. 工具参数用 schema 限制,执行前做服务端校验。\n"
            "3. 给循环设置最大步数、停止条件和失败分支。\n"
            "4. 打开 tracing 或保存每一步模型输入、工具调用和输出。\n"
            "5. 对写操作增加确认节点或权限检查。\n\n"
            "注意:Agent 失败常常不是模型单点问题,而是状态、工具、停止条件和错误恢复设计不完整。"
        ),
        source_refs=["langchain-docs:agents", "langgraph-docs:concepts", "langsmith-docs:tracing"],
    ),
    chunk(
        chunk_id="ext-coding-agent-compatible-endpoint-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="AI 编程工具接入兼容模型接口的通用检查",
        question_patterns=[
            "claude code cursor opencode 接自定义模型失败",
            "ai 编程工具 base url 怎么填",
            "coding agent 提示模型不可用",
            "代码助手调用 openai compatible 报错",
            "编程工具连模型接口先查什么",
        ],
        content=(
            "适用场景:客户把 AI 编程工具接到兼容模型网关或自建模型接口,出现模型不可用、鉴权失败或响应格式不兼容。"
            "不同工具界面不同,但底层检查项稳定。\n\n"
            "排查顺序:\n"
            "1. 先用 curl 或 OpenAI SDK 确认接口、密钥和模型名可用。\n"
            "2. 再把同一组 base URL、API Key、模型名填入编程工具。\n"
            "3. 确认工具期望的是 OpenAI、Anthropic 还是其他协议,不要只看“支持自定义模型”。\n"
            "4. 长上下文、工具调用、图片输入等能力可能不是所有兼容端点都支持。\n"
            "5. 保存工具日志里的 HTTP 状态码和响应体,不要只看弹窗文案。\n\n"
            "注意:AI 编程工具通常对模型能力和响应格式更敏感。普通聊天能通,不代表 Agent 编程流程一定能跑。"
        ),
        source_refs=["opencode-docs:providers", "openai-codex-repo:README", "anthropic-docs:claude-code"],
    ),
    chunk(
        chunk_id="ext-coding-agent-workspace-safety-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="AI 编程 Agent 的工作区、文件权限和安全边界",
        question_patterns=[
            "coding agent 会不会改错我的文件",
            "ai 编程工具工作区权限怎么限制",
            "让 agent 跑命令安全吗",
            "代码助手能不能访问整个机器",
            "ai agent 执行 shell 要注意什么",
        ],
        content=(
            "适用场景:客户用云端或本地 AI 编程 Agent 修改代码、运行命令或读取文件,担心误改、泄露或越权。"
            "稳定安全边界是:限制工作区、保留版本控制、审查命令和密钥。\n\n"
            "处理建议:\n"
            "1. 在独立分支或独立工作区运行 Agent,避免直接改生产目录。\n"
            "2. 用 git 查看 diff,确认变更范围再提交。\n"
            "3. 对删除、迁移、发布、付费和外部发送类动作增加确认。\n"
            "4. 不把 API Key、私钥和生产配置写进 prompt 或可提交文件。\n"
            "5. 长任务保存日志,失败后按命令和输出复盘。\n\n"
            "注意:Agent 可以提高效率,但不是权限边界。文件系统、密钥和命令执行仍要按最小权限管理。"
        ),
        source_refs=["openai-codex-repo:README", "anthropic-docs:claude-code-security", "git-docs:worktree"],
    ),
    chunk(
        chunk_id="ext-coding-agent-long-run-recovery-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="AI 编程 Agent 长任务中断后的恢复和验收",
        question_patterns=[
            "coding agent 跑到一半断了怎么办",
            "ai agent 改代码后怎么知道完成没",
            "长时间代码任务失败怎么恢复",
            "agent 生成了一堆改动怎么验收",
            "代码助手中断后如何继续",
        ],
        content=(
            "适用场景:AI 编程 Agent 执行较长任务时网络断开、进程退出或生成了大量改动,客户需要恢复和验收。"
            "稳定做法是把任务拆成可验证批次。\n\n"
            "处理顺序:\n"
            "1. 先看 git 状态和最近日志,确认哪些文件已经改动。\n"
            "2. 用测试、构建或最小复现命令验证现有改动,不要只读 Agent 总结。\n"
            "3. 如果任务中断,基于已有 diff 继续,避免从头生成另一套冲突改动。\n"
            "4. 大变更按功能拆提交,每个提交有对应验证证据。\n"
            "5. 合并前检查是否误改配置、密钥、生成物或无关文件。\n\n"
            "注意:Agent 的“完成”需要用测试和 diff 证明。没有验证输出就不能认为结果可用。"
        ),
        source_refs=["git-docs:status", "git-docs:diff", "openai-codex-repo:README"],
    ),
    chunk(
        chunk_id="ext-mcp-tool-server-basics-001",
        product_area="inference_serving",
        source_type="faq",
        source_origin="external_official",
        title="MCP 工具服务器接入 Agent 时的通用排查",
        question_patterns=[
            "mcp server 连不上 agent 怎么查",
            "mcp 工具没有出现在客户端",
            "agent 调 mcp 工具参数错误",
            "mcp stdio 和 http 有什么区别",
            "mcp 工具权限要怎么控制",
        ],
        content=(
            "适用场景:客户给 Agent 接 MCP 工具服务器,工具不出现、连接失败或调用参数错误。"
            "MCP 解决的是工具暴露和上下文连接协议,不替代业务权限控制。\n\n"
            "排查顺序:\n"
            "1. 确认客户端配置里的启动命令、环境变量或远程地址正确。\n"
            "2. 单独启动 MCP server,看标准输出/错误日志是否正常。\n"
            "3. 工具不出现时检查工具注册名称、描述和 schema 是否有效。\n"
            "4. 调用失败时保存模型给出的参数和 server 返回的错误。\n"
            "5. 对能改数据或访问外部系统的工具做权限、确认和日志审计。\n\n"
            "注意:MCP 让 Agent 更容易使用工具,也会扩大风险面。不要把高权限工具无保护地暴露给任意会话。"
        ),
        source_refs=["modelcontextprotocol-docs:quickstart", "modelcontextprotocol-docs:servers", "anthropic-docs:mcp"],
    ),
    chunk(
        chunk_id="ext-vnc-webrtc-remote-desktop-debug-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="VNC / noVNC / WebRTC 远程桌面打不开或黑屏排查",
        question_patterns=[
            "远程桌面黑屏怎么查",
            "vnc 能连但没有画面",
            "novnc 页面打不开",
            "webrtc 远程桌面连接失败",
            "云端图形界面没声音或剪贴板不通",
        ],
        content=(
            "适用场景:客户在 GPU 实例或云端 Agent 环境里使用远程桌面,遇到页面打不开、黑屏、卡顿或输入输出异常。"
            "远程桌面问题通常由进程、端口、桌面会话、显卡渲染和代理共同决定。\n\n"
            "排查顺序:\n"
            "1. 确认 VNC/noVNC/WebRTC 服务进程仍在运行,并监听预期端口。\n"
            "2. 检查桌面会话是否启动,黑屏时看 X server、窗口管理器和权限日志。\n"
            "3. 经反向代理访问时确认 WebSocket/WebRTC 相关连接未被阻断。\n"
            "4. 卡顿时区分网络带宽、CPU 编码、GPU 渲染和浏览器端性能。\n"
            "5. 剪贴板、音频和文件传输通常需要额外通道支持,不要默认可用。\n\n"
            "注意:远程桌面适合可视化操作,但长期服务仍建议用命令行、API 或受控 WebUI。"
        ),
        source_refs=["novnc-repo:README", "tigervnc-docs:man", "webrtc-docs:overview"],
    ),
    chunk(
        chunk_id="ext-browser-automation-gpu-server-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="服务器上跑 Playwright / Selenium 浏览器自动化的稳定检查",
        question_patterns=[
            "服务器上 playwright 打不开浏览器",
            "selenium 在无桌面环境怎么跑",
            "浏览器自动化截图是空白",
            "headless chrome 缺依赖怎么办",
            "agent 要操作网页浏览器怎么部署",
        ],
        content=(
            "适用场景:客户在云端实例或 Agent 环境里运行浏览器自动化,用于网页操作、截图、测试或数据处理。"
            "无桌面服务器和本地电脑不同,要先确认浏览器依赖、显示模式和沙箱限制。\n\n"
            "处理建议:\n"
            "1. 优先用 headless 模式跑最小页面打开和截图。\n"
            "2. 缺系统库时按 Playwright/Selenium 文档安装浏览器依赖。\n"
            "3. 需要可视化调试时再用 VNC、Xvfb 或远程桌面。\n"
            "4. 容器内运行要关注 sandbox、共享内存和字体依赖。\n"
            "5. 自动化登录和文件上传要保护 cookies、账号和下载目录。\n\n"
            "注意:浏览器自动化是外部系统交互能力,要限制目标站点、凭据和执行范围。"
        ),
        source_refs=["playwright-docs:browsers", "selenium-docs:drivers", "chrome-docs:headless"],
    ),
    chunk(
        chunk_id="ext-s3-object-storage-multipart-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="对象存储大文件上传、分片和校验的通用原则",
        question_patterns=[
            "s3 大文件上传中断怎么恢复",
            "对象存储 multipart upload 是什么",
            "上传数据集后怎么确认没坏",
            "rclone 同步对象存储要不要校验",
            "很多训练文件放对象存储要注意什么",
        ],
        content=(
            "适用场景:客户把模型、数据集或训练结果放到 S3 兼容对象存储,遇到大文件上传慢、中断或担心文件损坏。"
            "对象存储通常适合大对象和归档,大量小文件需要额外组织。\n\n"
            "处理建议:\n"
            "1. 大文件优先使用支持 multipart upload 的工具。\n"
            "2. 上传后记录文件大小、对象列表和校验信息,不要只看命令退出。\n"
            "3. 大量小文件可先打包成 shard 或归档,减少请求和元数据开销。\n"
            "4. 跨地域或公网传输要设置重试、并发和限速,避免失败后从头来。\n"
            "5. 训练前抽样读取数据,确认路径、权限和内容格式正确。\n\n"
            "注意:对象存储不是本地 POSIX 文件系统。随机小文件读写和频繁改写通常不是它的强项。"
        ),
        source_refs=["aws-docs:s3-multipart-upload", "rclone-docs:copy", "webdataset-repo:README"],
    ),
    chunk(
        chunk_id="ext-rsync-checksum-partial-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="rsync 断点续传、校验和目录同步排查",
        question_patterns=[
            "rsync 传大数据中断后怎么继续",
            "rsync 怎么确认两边文件一致",
            "scp 传一半断了能不能续传",
            "同步数据集后文件数量不一致",
            "rsync partial checksum 怎么用",
        ],
        content=(
            "适用场景:客户在本地和实例之间传大模型或数据集,中断后需要续传并确认两边一致。"
            "rsync 适合目录级同步和断点续传,比重复 scp 更稳。\n\n"
            "处理建议:\n"
            "1. 首次同步用归档模式保留目录和时间戳。\n"
            "2. 大文件中断后用 `--partial` 或 `--partial-dir` 保留未完成片段。\n"
            "3. 怀疑内容不一致时增加 `--checksum`,但会更慢。\n"
            "4. 用 `--dry-run` 先看将要同步哪些文件。\n"
            "5. 记录源目录、目标目录和结尾斜杠,避免多套一层目录。\n\n"
            "注意:rsync 能减少重复传输,但两端权限、磁盘空间和网络稳定性仍要单独检查。"
        ),
        source_refs=["rsync-docs:man", "openssh-docs:ssh_config"],
    ),
    chunk(
        chunk_id="ext-package-manager-proxy-dns-001",
        product_area="linux_ops",
        source_type="runbook",
        source_origin="external_official",
        title="pip / conda / npm / Docker / Hugging Face 下载慢的通用排查",
        question_patterns=[
            "pip conda npm docker 下载都很慢怎么查",
            "huggingface 下载模型老是 timeout",
            "docker pull 连接失败是 dns 还是代理",
            "包管理器代理怎么配置",
            "服务器能 ping 通但 pip 还是失败",
        ],
        content=(
            "适用场景:客户在实例里安装依赖或下载模型时,遇到超时、DNS 失败、TLS 错误或速度很慢。"
            "不要只换一个镜像源,先判断是 DNS、代理、证书、目标站点还是工具配置问题。\n\n"
            "排查顺序:\n"
            "1. 用 `curl -I` 或工具自带 verbose 模式测试目标地址。\n"
            "2. 区分 DNS 解析失败、TCP 连接失败、TLS 证书失败和 HTTP 状态码失败。\n"
            "3. pip、conda、npm、Docker、git-lfs、Hugging Face CLI 都有各自代理/镜像配置。\n"
            "4. 避免把临时代理写进全局配置后忘记清理。\n"
            "5. 下载大模型时优先使用支持续传和缓存的工具。\n\n"
            "注意:平台级网络加速域名范围属于内部语料维护;外部语料只保留通用排查方法。"
        ),
        source_refs=["pip-docs:user-guide", "conda-docs:user-guide", "npm-docs:config", "docker-docs:daemon-proxy", "hf-docs:environment_variables"],
    ),
    chunk(
        chunk_id="ext-secret-key-management-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="API Key、令牌和配置文件的最小安全做法",
        question_patterns=[
            "api key 应该放在哪里",
            "模型接口密钥泄露了怎么办",
            "环境变量和配置文件哪个更安全",
            "代码里不小心提交了 token 怎么处理",
            "给团队发模型 key 要注意什么",
        ],
        content=(
            "适用场景:客户在模型 API、RAG 工具、Agent 或自动化脚本里使用密钥,担心泄露和权限过大。"
            "稳定原则是最小权限、可轮换、可追踪、不入库。\n\n"
            "处理建议:\n"
            "1. 不把 API Key 写进代码、截图、聊天记录或可提交配置。\n"
            "2. 用环境变量、密钥管理服务或受保护的本地配置注入。\n"
            "3. 给不同应用、团队和环境使用不同 key,便于限流和吊销。\n"
            "4. 发现泄露后立即吊销或轮换,不要只从仓库删掉历史提交。\n"
            "5. 日志脱敏,避免把 Authorization、Cookie 和完整请求体写入排障日志。\n\n"
            "注意:密钥管理属于应用安全基础。模型服务、Agent 工具和自动化工作流都应按同一原则处理。"
        ),
        source_refs=["owasp-docs:secrets-management", "github-docs:secret-scanning", "twelve-factor:config"],
    ),
    chunk(
        chunk_id="ext-public-webui-auth-security-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="远程 WebUI 对外开放前的鉴权、端口和日志检查",
        question_patterns=[
            "gradio comfyui jupyter 对外开放安全吗",
            "webui 直接暴露公网要注意什么",
            "远程服务怎么加密码和 https",
            "模型接口被别人访问怎么办",
            "反向代理 webui 前要检查什么",
        ],
        content=(
            "适用场景:客户把 Jupyter、Gradio、ComfyUI、Open WebUI 或模型接口开放给外部访问。"
            "远程访问要先考虑鉴权和最小暴露面,再考虑方便。\n\n"
            "处理建议:\n"
            "1. 开发阶段优先用 SSH 本地端口转发,不要直接暴露裸端口。\n"
            "2. 长期服务放在反向代理后面,开启 HTTPS、鉴权和访问日志。\n"
            "3. 应用自身也要有 token、密码或用户权限,不要只靠端口隐蔽。\n"
            "4. 限制上传文件大小、请求体大小、并发和来源。\n"
            "5. 日志里不要记录完整 prompt、密钥或用户隐私内容。\n\n"
            "注意:WebUI 方便演示,但一旦公网可访问,就应按正式服务的安全标准处理。"
        ),
        source_refs=["jupyter-server-docs:public-server", "gradio-docs:sharing", "caddy-docs:reverse-proxy", "owasp-docs:api-security"],
    ),
    chunk(
        chunk_id="ext-audio-tts-asr-quality-debug-001",
        product_area="inference_serving",
        source_type="runbook",
        source_origin="external_official",
        title="TTS / ASR / 语音克隆效果差时先查音频质量和采样链路",
        question_patterns=[
            "tts 声音不像怎么查",
            "语音克隆参考音频要注意什么",
            "asr 转写错字很多怎么办",
            "音频模型输出有杂音",
            "配音字幕不同步怎么定位",
        ],
        content=(
            "适用场景:客户做语音合成、语音克隆、ASR 或视频配音时,效果差、杂音多、识别错或不同步。"
            "语音任务先查输入音频和处理链路,不要只换模型。\n\n"
            "排查顺序:\n"
            "1. 参考音频要清晰、少噪声、音量稳定,并尽量匹配目标语言和风格。\n"
            "2. 检查采样率、声道、响度和格式转换,避免重复压缩。\n"
            "3. ASR 错字多时先分段、降噪,再看模型语言和热词支持。\n"
            "4. 配音不同步时分别检查 ASR 时间戳、TTS 语速和 ffmpeg 合成链路。\n"
            "5. 保存输入音频、转写文本、合成音频和最终视频,分段对比定位。\n\n"
            "注意:语音质量很依赖素材。低质量参考音频通常无法靠显卡或参数完全弥补。"
        ),
        source_refs=["ffmpeg-docs:ffmpeg", "faster-whisper-repo:README", "gpt-sovits-repo:README", "cosyvoice-repo:README"],
    ),
    chunk(
        chunk_id="ext-digital-human-lipsync-latency-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="实时数字人口型同步的延迟、帧率和资源瓶颈",
        question_patterns=[
            "数字人口型不同步怎么查",
            "实时数字人延迟很高",
            "wav2lip 推理帧率低怎么办",
            "webrtc 数字人卡顿",
            "数字人服务 cpu gpu 哪个是瓶颈",
        ],
        content=(
            "适用场景:客户做实时数字人、口型同步或视频驱动头像,遇到延迟高、帧率低、声音和嘴型不同步。"
            "这类链路通常同时消耗 CPU、GPU、网络和浏览器资源。\n\n"
            "排查顺序:\n"
            "1. 分开测音频输入、TTS/ASR、口型模型推理、视频编码和推流。\n"
            "2. GPU 推理慢时看显存、batch、分辨率和模型大小。\n"
            "3. CPU 编码慢时降低分辨率、帧率或换编码配置。\n"
            "4. WebRTC 卡顿时看网络抖动、浏览器控制台和服务端日志。\n"
            "5. 端到端延迟要分段记录,不要只看最终页面体感。\n\n"
            "注意:实时数字人不是单模型推理问题。音频、视频、编码和网络任一环节慢都会影响体验。"
        ),
        source_refs=["livetalking-repo:README", "webrtc-docs:overview", "ffmpeg-docs:ffmpeg"],
    ),
    chunk(
        chunk_id="ext-video-generation-resource-tradeoff-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="视频生成的分辨率、帧数、时长和显存取舍",
        question_patterns=[
            "视频生成为什么特别吃显存",
            "文生视频 oom 怎么调",
            "视频生成时长越长越慢吗",
            "图生视频分辨率怎么选",
            "生成视频卡住是显存还是磁盘问题",
        ],
        content=(
            "适用场景:客户跑文生视频、图生视频或视频编辑模型,遇到 OOM、很慢、临时文件大或任务卡住。"
            "视频生成的资源消耗通常随分辨率、帧数、时长和模型规模快速上升。\n\n"
            "处理建议:\n"
            "1. 先用低分辨率、短时长、小步数跑通流程。\n"
            "2. OOM 时优先降低分辨率、帧数、batch 或启用模型支持的 offload。\n"
            "3. 磁盘写满会让任务看起来像卡住,要检查临时目录和输出目录。\n"
            "4. 多任务并发会叠加显存和 CPU 编码压力,先单任务测基线。\n"
            "5. 对客户验收要固定提示词、随机种子、输入素材和参数,否则结果不可比较。\n\n"
            "注意:视频生成属于高资源工作负载。不要用图片生成的显存经验直接估算视频任务。"
        ),
        source_refs=["hf-diffusers:optimization", "ffmpeg-docs:ffmpeg", "pytorch-docs:notes/cuda"],
    ),
    chunk(
        chunk_id="ext-cv-dataset-format-label-debug-001",
        product_area="pytorch_basics",
        source_type="runbook",
        source_origin="external_official",
        title="CV 训练数据格式、标签和类别映射错误排查",
        question_patterns=[
            "目标检测训练一直效果很差怎么查数据",
            "yolo 标签格式是不是错了",
            "图像分割 mask 和类别对不上",
            "训练集路径正确但读不到标签",
            "cv 数据集怎么验收",
        ],
        content=(
            "适用场景:客户训练目标检测、分割或分类模型时 loss 异常、效果差、类别混淆或数据读取失败。"
            "CV 训练问题很大比例来自数据和标签,不是 GPU 本身。\n\n"
            "排查顺序:\n"
            "1. 抽样可视化图片、框、mask 和类别名,确认人眼看起来正确。\n"
            "2. 检查 train/val 路径、文件后缀、空标签和坏图。\n"
            "3. 检查类别编号是否从框架要求的起点开始,类别名顺序是否一致。\n"
            "4. 分割任务确认 mask 尺寸、像素值和图片一一对应。\n"
            "5. 先用很小数据集过拟合,验证训练代码能学到东西。\n\n"
            "注意:数据格式错时,增加显卡、训练轮数或模型大小通常只会浪费时间。"
        ),
        source_refs=["ultralytics-docs:train", "label-studio-docs:guide", "monai-docs:index"],
    ),
    chunk(
        chunk_id="ext-robotics-sim-gpu-display-001",
        product_area="gpu_troubleshooting",
        source_type="faq",
        source_origin="external_official",
        title="机器人仿真和强化学习的 GPU、显示和无头运行检查",
        question_patterns=[
            "isaac sim 服务器上黑屏怎么办",
            "机器人仿真需要显示器吗",
            "headless 仿真怎么跑",
            "强化学习仿真 gpu 利用率低",
            "仿真环境远程桌面打不开",
        ],
        content=(
            "适用场景:客户跑机器人仿真、强化学习或具身智能环境,遇到显示失败、远程桌面黑屏、GPU 没用上或仿真很慢。"
            "仿真任务同时涉及渲染、物理、Python 环境和远程显示。\n\n"
            "处理建议:\n"
            "1. 先确认任务需要图形界面还是支持 headless 模式。\n"
            "2. 检查 NVIDIA 驱动、OpenGL/Vulkan/EGL 等渲染依赖是否可用。\n"
            "3. 远程桌面只负责显示,不等于仿真已经用上 GPU。\n"
            "4. 强化学习吞吐低时同时看环境步进、CPU、GPU 和数据拷贝。\n"
            "5. 保存最小 demo、日志和渲染模式,便于复现。\n\n"
            "注意:机器人仿真的瓶颈不一定在模型训练,也可能在渲染、物理模拟或环境并行度。"
        ),
        source_refs=["nvidia-isaac-sim-docs:workstation-setup", "nvidia-isaac-lab-docs:workflows", "pytorch-docs:notes/cuda"],
    ),
    chunk(
        chunk_id="ext-ai4science-repro-env-data-001",
        product_area="linux_ops",
        source_type="faq",
        source_origin="external_official",
        title="AI4Science 任务的环境、数据和结果可复现检查",
        question_patterns=[
            "科研任务换机器后结果不一致怎么查",
            "ai4science 实验怎么保证可复现",
            "分子模拟或蛋白预测结果要记录哪些信息",
            "科研 gpu 环境迁移要注意什么",
            "大规模科学数据怎么管理版本",
        ],
        content=(
            "适用场景:科研客户把分子模拟、蛋白结构、物理机器学习或大规模数组计算迁到 GPU 环境,需要保证可复现。"
            "稳定做法是同时记录环境、数据、代码、参数和硬件。\n\n"
            "处理建议:\n"
            "1. 固定代码 commit、容器/conda 环境、核心依赖和 CUDA/驱动信息。\n"
            "2. 数据集、预处理脚本和随机种子要版本化。\n"
            "3. 大文件用对象存储、DVC、Zarr 或 shard 方案时记录文件列表和校验。\n"
            "4. 保存完整命令行、配置文件、日志和输出摘要。\n"
            "5. 迁移机器后先用小样本复现,再跑长任务。\n\n"
            "注意:科研任务的“能跑”不等于“可复现”。结果交付前要能说明环境和数据来源。"
        ),
        source_refs=["dvc-docs:start", "zarr-docs:index", "mlflow-docs:tracking", "apptainer-docs:gpu"],
    ),
]

CHUNKS = (
    VLLM_CHUNKS
    + SGLANG_CHUNKS
    + OLLAMA_CHUNKS
    + COMFYUI_CHUNKS
    + GPU_OPS_CHUNKS
    + EXTRA_CHUNKS
    + LINUX_OPS_CHUNKS
    + PYTORCH_BASICS_CHUNKS
    + MODEL_DOWNLOAD_CHUNKS
    + CHAT_SEEDED_EXTERNAL_CHUNKS
    + AI4SCIENCE_GPU_CHUNKS
    + PRO_GPU_SUPPORT_CHUNKS
    + PRODUCTION_GPU_SUPPORT_CHUNKS
    + SCENE_SIGNAL_TARGETED_CHUNKS
    + SECOND_WAVE_EXTERNAL_CHUNKS
    + SESSION_637_TARGETED_CHUNKS
    + CUDA_COMPILE_CLEANUP_CHUNKS
    + THIRD_WAVE_EXTERNAL_CHUNKS
    + STABLE_PLATFORM_EXTERNAL_CHUNKS
)


def main() -> None:
    out_path = Path(__file__).resolve().parent / "external_candidate_w0.jsonl"
    seen: set[str] = set()
    with out_path.open("w", encoding="utf-8") as fh:
        for c in CHUNKS:
            if c["chunk_id"] in seen:
                raise SystemExit(f"duplicate chunk_id {c['chunk_id']}")
            seen.add(c["chunk_id"])
            fh.write(json.dumps(c, ensure_ascii=False) + "\n")
    print(f"wrote {len(CHUNKS)} chunks -> {out_path}")


if __name__ == "__main__":
    main()
