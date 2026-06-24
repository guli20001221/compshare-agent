#!/usr/bin/env python3
"""Build the external-corpus retrieval golden set.

Questions are natural user paraphrases, deliberately NOT byte-equal to any
chunk's question_patterns/title (else Top-K becomes a tautology — memory
feedback-eval-questions-not-byte-equal-to-index-terms / -user-tone). product_area
mirrors what engine.inferKnowledgeProductArea would return for the query, so the
eval reflects the runtime +2 affinity boost. Writes scripts/rag_ext/external_golden.jsonl.
"""
from __future__ import annotations

import json
from pathlib import Path

# (question, expected_chunk_ids, product_area, group)
Q = [
    ("我想把模型起成一个接口给别人调,用 vllm 怎么搞", ["ext-vllm-serving-001"], "inference_serving", "vllm"),
    ("vllm 不开服务,能直接在脚本里跑一批输入吗", ["ext-vllm-offline-001"], "inference_serving", "vllm"),
    ("一张卡装不下这个模型,vllm 能拆到两张卡上跑吗", ["ext-vllm-tensor-parallel-001"], "inference_serving", "vllm"),
    ("调用的时候不想写一长串模型路径,能给它起个短名字吗", ["ext-vllm-served-name-001"], "inference_serving", "vllm"),
    ("vllm 加载模型直接 OOM 了,有哪些办法能省显存", ["ext-gpu-oom-vllm-001"], "gpu_troubleshooting", "vllm"),
    ("显存不太够,能不能用量化过的模型在 vllm 上跑", ["ext-vllm-quantization-001"], "inference_serving", "vllm"),
    ("起 vllm 的时候提示端口已经被占用了", ["ext-vllm-port-001"], "inference_serving", "vllm"),
    ("vllm 跑着跑着报 cuda error 崩了,怎么定位是哪出问题", ["ext-vllm-cuda-error-001"], "gpu_troubleshooting", "vllm"),
    ("vllm 启动一直卡着,模型加载不出来", ["ext-vllm-startup-hang-001"], "gpu_troubleshooting", "vllm"),
    ("怎么用 sglang 拉起一个兼容 openai 的服务", ["ext-sglang-serving-001"], "inference_serving", "sglang"),
    ("sglang 跑起来报显存不够,应该调哪个参数", ["ext-sglang-oom-001"], "gpu_troubleshooting", "sglang"),
    ("sglang 想用多张卡一起跑同一个模型", ["ext-sglang-tp-001"], "inference_serving", "sglang"),
    ("sglang 想把上下文长度限制得小一点", ["ext-sglang-context-001"], "gpu_troubleshooting", "sglang"),
    ("ollama 怎么把一个模型下下来再跑起来", ["ext-ollama-run-001"], "inference_serving", "ollama"),
    ("我想用 openai 的库去调本地的 ollama,base_url 填什么", ["ext-ollama-openai-001"], "inference_serving", "ollama"),
    ("ollama 默认只能本机连,怎么让别的机器也能访问", ["ext-ollama-host-001"], "inference_serving", "ollama"),
    ("怎么确认 ollama 到底有没有用上显卡", ["ext-ollama-gpu-001"], "gpu_troubleshooting", "ollama"),
    ("ollama 的模型默认下到系统盘了,能换个目录吗", ["ext-ollama-models-dir-001"], "inference_serving", "ollama"),
    ("想看看现在显卡用了多少显存、是哪个进程在占", ["ext-gpu-nvidia-smi-001"], "gpu_troubleshooting", "gpu_ops"),
    ("怎么查我这台机器的显卡驱动和 cuda 版本", ["ext-gpu-driver-cuda-version-001"], "gpu_troubleshooting", "gpu_ops"),
    ("代码里 torch.cuda.is_available() 返回 false,怎么查", ["ext-gpu-not-detected-001"], "gpu_troubleshooting", "gpu_ops"),
    ("训练的时候老是 cuda out of memory,有什么通用的降显存办法", ["ext-gpu-pytorch-oom-001"], "gpu_troubleshooting", "gpu_ops"),
    ("我调了 empty_cache 为什么显存还是没降下来", ["ext-gpu-empty-cache-001"], "gpu_troubleshooting", "gpu_ops"),
    ("总显存看着还有富余,却报 oom,是不是碎片的问题", ["ext-gpu-fragmentation-001"], "gpu_troubleshooting", "gpu_ops"),
    ("显存被一个进程一直占着不放,怎么清掉", ["ext-gpu-kill-process-001"], "gpu_troubleshooting", "gpu_ops"),
    ("进程莫名其妙被 killed,是显存不够还是内存不够", ["ext-gpu-oom-vs-ram-001"], "gpu_troubleshooting", "gpu_ops"),
    ("多卡机器上怎么让程序只用第 0 和第 1 张卡", ["ext-gpu-visible-devices-001"], "gpu_troubleshooting", "gpu_ops"),
    ("从 huggingface 下载模型特别慢,有没有什么办法", ["ext-gpu-hf-download-001"], "gpu_troubleshooting", "gpu_ops"),
    ("我要跑一个 14b 的模型,大概得多大显存的卡", ["ext-gpu-vram-estimate-001"], "inference_serving", "gpu_ops"),
    # ComfyUI. "comfyui" is not in inferKnowledgeProductArea's keyword sets, so most
    # serving/setup queries infer "" (no +2 boost). Two exceptions, faithfully
    # mirrored here (pinned by TestInferKnowledgeProductArea_LabelsMatchCorpus): the
    # OOM query hits gpu_troubleshooting ("爆显存"); the model-directory query says
    # "模型", a modelverse keyword checked before inference_serving, so it infers
    # "modelverse" — a no-op boost in this external-only eval (no external modelverse
    # chunk); the merged-index mis-boost is checked separately in CLI smoke.
    ("我在实例里把 comfyui 跑起来了,想让别的电脑也能打开它的页面", ["ext-comfyui-start-001"], "", "comfyui"),
    ("comfyui 出图的时候老是爆显存,有什么办法能降一点", ["ext-comfyui-oom-001"], "gpu_troubleshooting", "comfyui"),
    ("下载的大模型放进去了,comfyui 里却选不到,该放哪个文件夹", ["ext-comfyui-models-dir-001"], "modelverse", "comfyui"),
    ("comfyui 想用一个第三方的节点,怎么把它装上", ["ext-comfyui-custom-nodes-001"], "", "comfyui"),
    ("在新开的实例上从头装 comfyui 该怎么弄", ["ext-comfyui-install-001"], "", "comfyui"),
    ("comfyui 启动了日志也没报错,可浏览器就是打不开", ["ext-comfyui-cant-connect-001"], "", "comfyui"),
    ("不想每次手点界面,能不能用脚本自动让 comfyui 跑一个工作流", ["ext-comfyui-api-001"], "", "comfyui"),
    # Linux ops + env management. Areas mirror inferKnowledgeProductArea (pinned in
    # product_area_inference_test.go). Most infer "linux_ops"; the SSH-免密 query
    # carries "ssh"/"密码"/"登录", which the login group claims first (checked
    # before the external sets) — declared faithfully as "login" (a no-op boost on
    # this external-only eval; the merged-index retrieval is covered by CLI smoke).
    ("想让训练在后台跑,人离开后还能重新连回去接着看", ["ext-linux-tmux-001"], "linux_ops", "linux_ops"),
    ("怎么把一条命令丢到后台跑,顺便把输出都存进日志", ["ext-linux-nohup-001"], "linux_ops", "linux_ops"),
    ("实例报 no space left,磁盘满了怎么清出空间", ["ext-linux-disk-cleanup-001"], "linux_ops", "linux_ops"),
    ("free -h 看着内存不多了,想找出是哪个进程占的", ["ext-linux-resource-001"], "linux_ops", "linux_ops"),
    ("想给新项目单独建个 conda 环境,装的包不影响别的", ["ext-linux-conda-001"], "linux_ops", "linux_ops"),
    ("不想装 conda,python 自带的 venv 怎么建环境", ["ext-linux-venv-001"], "linux_ops", "linux_ops"),
    ("pip 下载特别慢,想换成国内的镜像源", ["ext-linux-pip-mirror-001"], "linux_ops", "linux_ops"),
    ("本地的数据集怎么传到实例上去", ["ext-linux-transfer-001"], "linux_ops", "linux_ops"),
    ("每次 ssh 登录都要输密码,怎么搞成免密的", ["ext-linux-ssh-key-001"], "login", "linux_ops"),
    # PyTorch / CUDA basics. "pytorch"/"dataloader"/"混合精度"/"state_dict" reach
    # pytorch_basics (checked after the platform + serving + troubleshooting sets,
    # so an OOM/cuda message still maps to those, not here).
    ("装完 pytorch 发现用不了 gpu,是不是装成 cpu 版了", ["ext-pytorch-install-001"], "pytorch_basics", "pytorch"),
    ("pytorch 写好的代码默认在 cpu 跑,怎么让它上 gpu", ["ext-pytorch-device-001"], "pytorch_basics", "pytorch"),
    ("单机 4 张卡想一起训练,pytorch 怎么搞多卡", ["ext-pytorch-ddp-001"], "pytorch_basics", "pytorch"),
    ("gpu 利用率老是很低,感觉卡在数据加载,dataloader 怎么调快", ["ext-pytorch-dataloader-001"], "pytorch_basics", "pytorch"),
    ("训练想省点显存又快一点,听说混合精度可以,怎么写", ["ext-pytorch-amp-001"], "pytorch_basics", "pytorch"),
    ("pytorch 训练完想把权重保存下来,下次加载接着用,怎么写", ["ext-pytorch-checkpoint-001"], "pytorch_basics", "pytorch"),
    # 模型下载 — product_area declared FAITHFULLY to inferKnowledgeProductArea:
    # "模型" collides into modelverse (checked before inference_serving); the gguf/vllm
    # phrasings (no "模型") fall to inference_serving; the llama-no-permission Q matches
    # no keyword -> "". Distinctive tokens (huggingface/modelscope/gguf/vllm-本地) carry
    # retrieval despite the modelverse mis-boost on the merged index (CLI-smoke verified).
    ("除了 huggingface,国内还能从哪里下载模型", ["ext-modelscope-download-001"], "modelverse", "model_download"),
    ("下载 llama 的时候提示没有权限,需要先申请什么吗", ["ext-hf-token-001"], "", "model_download"),
    ("我有个 gguf 文件,想让 ollama 直接用它", ["ext-ollama-modelfile-001"], "inference_serving", "model_download"),
    ("vllm 怎么加载本地已经下好的目录,不想每次重新下", ["ext-serve-local-path-001"], "inference_serving", "model_download"),
    # Direct coverage for the chat-seeded support-gap chunks. These were mined
    # from customer demand signals, but facts remain grounded in upstream docs.
    ("vscode remote 空闲一会儿就断开,ssh 心跳参数应该怎么设", ["ext-linux-ssh-keepalive-001"], "linux_ops", "chat_seed_direct"),
    ("几十 G 数据传到实例中断了,rsync 怎么保留进度继续传", ["ext-linux-large-transfer-rsync-001"], "linux_ops", "chat_seed_direct"),
    ("gradio 代码在云主机上跑了,外部访问要把监听地址改成什么", ["ext-webapp-gradio-remote-001"], "inference_serving", "chat_seed_direct"),
    ("streamlit 或 fastapi 本机能访问但公网打不开,host 应该怎么绑", ["ext-webapp-streamlit-uvicorn-remote-001"], "inference_serving", "chat_seed_direct"),
    ("huggingface 缓存把系统盘占满了,怎么指定 HF_HOME 并下载到数据盘", ["ext-hf-cache-cli-download-001"], "inference_serving", "chat_seed_direct"),
    ("小显存做 qlora 微调时 peft 的 target_modules 和 4bit 要先检查什么", ["ext-peft-lora-qlora-001"], "pytorch_basics", "chat_seed_direct"),
    ("torchrun 多卡训练一直卡住,nccl 日志和 local_rank 怎么排查", ["ext-pytorch-nccl-ddp-debug-001"], "pytorch_basics", "chat_seed_direct"),
    # AI4Science / professional GPU workloads. These queries intentionally include
    # GPU/显卡/容器 words so they mirror runtime gpu_troubleshooting inference while
    # still testing specialized terms (JAX/CuPy/OpenMM/GROMACS/RAPIDS/ColabFold).
    ("jax 装好后怎么确认是不是跑在 GPU 上", ["ext-jax-gpu-install-001"], "gpu_troubleshooting", "ai4science_gpu"),
    ("cupy 安装后怎么检查数组真的放到显卡上算了", ["ext-cupy-gpu-array-001"], "gpu_troubleshooting", "ai4science_gpu"),
    ("openmm 分子动力学任务怎么强制选 CUDA 显卡", ["ext-openmm-cuda-platform-001"], "gpu_troubleshooting", "ai4science_gpu"),
    ("gromacs mdrun 怎么把 pme 和非键计算放到 gpu 上", ["ext-gromacs-gpu-offload-001"], "gpu_troubleshooting", "ai4science_gpu"),
    ("pandas 数据处理太慢,想用 rapids cudf 在 GPU 上跑", ["ext-rapids-cudf-install-001"], "gpu_troubleshooting", "ai4science_gpu"),
    ("docker 容器里 nvidia-smi 看不到显卡该怎么查", ["ext-docker-gpu-container-001"], "gpu_troubleshooting", "ai4science_gpu"),
    ("colabfold 想在本地 GPU 上预测蛋白结构,要准备什么", ["ext-colabfold-local-gpu-001"], "gpu_troubleshooting", "ai4science_gpu"),
    ("alphafold3 的 docker 怎么确认 GPU 已经传进容器", ["ext-alphafold3-docker-gpu-001"], "gpu_troubleshooting", "ai4science_gpu"),
    # Training / fine-tuning, transfer, remote development, CUDA/NVIDIA, and
    # ComfyUI ecosystem coverage added from official docs and upstream READMEs.
    ("transformers trainer 微调显存不够,应该先调哪些参数", ["ext-transformers-trainer-gpu-001"], "pytorch_basics", "training_finetune"),
    ("accelerate 想用两张 GPU 启动训练脚本", ["ext-accelerate-launch-001"], "pytorch_basics", "training_finetune"),
    ("bitsandbytes 4bit 加载模型后还能训练哪些参数", ["ext-bitsandbytes-quantization-001"], "pytorch_basics", "training_finetune"),
    ("llama factory 怎么跑 qwen 的 lora 微调", ["ext-llamafactory-lora-train-001"], "pytorch_basics", "training_finetune"),
    ("unsloth 安装后怎么确认 gpu 环境没装错", ["ext-unsloth-finetune-001"], "pytorch_basics", "training_finetune"),
    ("deepspeed zero3 offload 怎么省显存", ["ext-deepspeed-zero-001"], "pytorch_basics", "training_finetune"),
    ("git clone 的 safetensors 只有几百字节,怎么拉 lfs 大文件", ["ext-git-lfs-model-download-001"], "linux_ops", "download_transfer"),
    ("aria2 想多线程继续下载大模型文件", ["ext-aria2-parallel-download-001"], "linux_ops", "download_transfer"),
    ("wget 或 curl 下载中断了怎么接着下", ["ext-wget-curl-resume-001"], "linux_ops", "download_transfer"),
    ("rclone 从对象存储同步数据集到实例怎么做", ["ext-rclone-sync-copy-001"], "linux_ops", "download_transfer"),
    ("vscode remote ssh 连上后怎么转发远程端口", ["ext-vscode-remote-server-001"], "linux_ops", "remote_dev"),
    ("jupyter server 在远程实例上怎么安全访问", ["ext-jupyter-server-remote-001"], "linux_ops", "remote_dev"),
    ("ssh -L 和 -R 端口转发怎么选,什么时候用反向代理", ["ext-ssh-tunnel-reverse-proxy-001"], "linux_ops", "remote_dev"),
    ("nccl 多卡 all reduce 卡住怎么开日志和指定网卡", ["ext-nccl-advanced-debug-001"], "gpu_troubleshooting", "cuda_nvidia"),
    ("nvidia-smi 和 torch 显示的 cuda 版本不一样怎么办", ["ext-cuda-driver-compatibility-001"], "gpu_troubleshooting", "cuda_nvidia"),
    ("pytorch 装完 cuda is available false,框架和 cuda 怎么匹配", ["ext-framework-cuda-match-001"], "gpu_troubleshooting", "cuda_nvidia"),
    ("comfyui manager 安装节点后不显示怎么排查", ["ext-comfyui-manager-001"], "inference_serving", "comfyui_ecosystem"),
    ("comfyui flux 工作流缺 t5xxl clip_l ae 模型放哪里", ["ext-comfyui-flux-models-001"], "gpu_troubleshooting", "comfyui_ecosystem"),
    ("comfyui controlnet aux 预处理节点缺失怎么装", ["ext-comfyui-controlnet-001"], "inference_serving", "comfyui_ecosystem"),
    ("comfyui ipadapter 找不到 clip vision 或 flux ipadapter 模型", ["ext-comfyui-ipadapter-001"], "inference_serving", "comfyui_ecosystem"),
    # Production GPU support: monitoring/profiling, containers/Kubernetes,
    # production inference, distributed scheduling, post-training, and large data.
    ("想把 gpu 温度功耗显存指标接到 prometheus grafana", ["ext-dcgm-exporter-prometheus-001"], "gpu_troubleshooting", "monitoring_perf"),
    ("gpu 节点上线前想跑 dcgmi diag 验证硬件和驱动", ["ext-dcgm-diagnostics-run-001"], "gpu_troubleshooting", "monitoring_perf"),
    ("nsys profile pytorch 训练慢,想看 cpu 和 cuda 时间线", ["ext-nsight-systems-profile-001"], "gpu_troubleshooting", "monitoring_perf"),
    ("ncu 想分析某个 cuda kernel 的 occupancy 和 memory 瓶颈", ["ext-nsight-compute-kernel-profile-001"], "gpu_troubleshooting", "monitoring_perf"),
    ("mig 模式下 dcgm exporter 怎么区分每个 gpu instance 指标", ["ext-gpu-telemetry-mig-metrics-001"], "gpu_troubleshooting", "monitoring_perf"),
    ("docker run 容器里怎么用 --gpus all 验证 nvidia-smi", ["ext-docker-run-gpu-access-001"], "linux_ops", "container_k8s"),
    ("docker compose 里怎么给服务声明一张 nvidia gpu", ["ext-docker-compose-gpu-001"], "linux_ops", "container_k8s"),
    ("kubernetes pod 申请 nvidia.com/gpu 资源怎么写", ["ext-k8s-gpu-resource-request-001"], "linux_ops", "container_k8s"),
    ("gpu operator 会帮 k8s 安装哪些 nvidia 组件", ["ext-gpu-operator-stack-001"], "linux_ops", "container_k8s"),
    ("gpu pod 一直 pending 提示 insufficient nvidia.com/gpu 怎么查", ["ext-k8s-gpu-pod-pending-001"], "linux_ops", "container_k8s"),
    ("一张 a100 想用 mig 切成多个隔离实例", ["ext-mig-partition-001"], "gpu_troubleshooting", "container_k8s"),
    ("k8s 里怎么申请 nvidia.com/mig-1g.5gb 这种 mig 资源", ["ext-k8s-mig-device-plugin-001"], "linux_ops", "container_k8s"),
    ("gpu operator time slicing 能不能把一张卡给多个 pod 用", ["ext-k8s-gpu-time-slicing-001"], "linux_ops", "container_k8s"),
    ("triton model repository 怎么组织多个 pytorch onnx 模型", ["ext-triton-model-repository-001"], "inference_serving", "production_inference"),
    ("triton dynamic batching 怎么提高吞吐但不把延迟拉太高", ["ext-triton-dynamic-batching-001"], "inference_serving", "production_inference"),
    ("tensorrt llm 怎么通过 triton backend 部署和优化 llm", ["ext-tensorrt-llm-triton-001"], "inference_serving", "production_inference"),
    ("triton tensorrt llm 怎么用 genai perf 测首 token 延迟", ["ext-triton-genai-perf-metrics-001"], "inference_serving", "production_inference"),
    ("kserve inferenceservice 怎么在 kubernetes 上生产部署模型", ["ext-kserve-inferenceservice-001"], "inference_serving", "production_inference"),
    ("kserve 多节点 vllm 推理为什么需要 rwx pvc 和 workerSpec", ["ext-kserve-multinode-vllm-001"], "inference_serving", "production_inference"),
    ("huggingface tgi 现在还建议新项目使用吗", ["ext-tgi-maintenance-migration-001"], "inference_serving", "production_inference"),
    ("ray remote num_gpus 怎么给任务或 actor 分配 gpu", ["ext-ray-gpu-tasks-001"], "linux_ops", "distributed_scheduler"),
    ("ray train torchtrainer 想用 4 张 gpu 怎么配 scalingconfig", ["ext-ray-train-gpu-scaling-001"], "pytorch_basics", "distributed_scheduler"),
    ("ray serve 每个模型 replica 想占一张 gpu 怎么写", ["ext-ray-serve-gpu-replicas-001"], "inference_serving", "distributed_scheduler"),
    ("slurm sbatch srun 怎么用 gres 申请 gpu 作业", ["ext-slurm-gpu-gres-001"], "linux_ops", "distributed_scheduler"),
    ("slurm 里怎么统计 gpumem gpuutil 以及配置 mig mps", ["ext-slurm-gpu-accounting-mig-001"], "linux_ops", "distributed_scheduler"),
    ("trl sfttrainer 怎么对 qwen 做监督微调并配 lora", ["ext-trl-sfttrainer-001"], "pytorch_basics", "post_training"),
    ("trl dpotrainer 怎么用 chosen rejected 偏好数据训练", ["ext-trl-dpotrainer-001"], "pytorch_basics", "post_training"),
    ("huggingface datasets 数据太大怎么 streaming 并把缓存放数据盘", ["ext-hf-datasets-streaming-cache-001"], "linux_ops", "data_scale"),
    ("ai4science 大型多维数组想用 zarr 放对象存储怎么理解", ["ext-zarr-science-array-storage-001"], "linux_ops", "data_scale"),
    ("dask cudf 和 localcudacluster 怎么用两张 gpu 处理大表", ["ext-dask-cudf-multigpu-001"], "gpu_troubleshooting", "data_scale"),
    # Community-image targeted coverage: sourced from popular community image demand
    # signal, with technical facts from upstream project docs.
    ("ltx 视频工作流既缺 checkpoint 又爆显存,这些模型目录和显存怎么查", ["ext-community-ltx-video-comfyui-001"], "gpu_troubleshooting", "community_images"),
    ("livetalking 数字人推流不流畅,日志里的 inferfps finalfps 该怎么看", ["ext-community-digital-human-livetalking-001"], "", "community_images"),
    ("infinitetalk 数字人视频要准备 wan 和 wav2vec 这些权重吗,低显存怎么跑", ["ext-community-digital-human-infinitetalk-001"], "gpu_troubleshooting", "community_images"),
    ("svc 变声镜像能不能直接文字转语音,还是要先训练自己的声音", ["ext-community-voice-conversion-svc-001"], "pytorch_basics", "community_images"),
    ("dots tts 做声音克隆时参考音频和转录文本分别有什么用", ["ext-community-dots-tts-voice-cloning-001"], "", "community_images"),
    ("cosyvoice 和 voxcpm 做多语言语音合成时该怎么选", ["ext-community-cosyvoice-voxcpm-tts-001"], "", "community_images"),
    ("seed tts eval 只是评测工具吗,wer sim 分数要怎么跑", ["ext-community-seed-tts-eval-001"], "pytorch_basics", "community_images"),
    ("ai toolkit 训练 qwen image 或 wan 的 lora,数据和显存先看什么", ["ext-community-ai-toolkit-lora-001"], "pytorch_basics", "community_images"),
    ("qwen image 生成中文海报文字不稳定,应该升级哪些依赖或节点", ["ext-community-qwen-image-001"], "modelverse", "community_images"),
    ("wan2.2 做 720p 视频生成 oom,offload 和 t5_cpu 这些参数怎么用", ["ext-community-wan-video-001"], "gpu_troubleshooting", "community_images"),
    ("comfyui 低显存 gguf 工作流里 unet loader 和 clip loader 要怎么选", ["ext-community-comfyui-gguf-001"], "gpu_troubleshooting", "community_images"),
    ("triposplat 单张图片转 3d 后导出的 ply splat 应该用什么查看", ["ext-community-triposplat-3d-001"], "", "community_images"),
    ("视频配音镜像声音不像,是 tts 问题还是 asr ffmpeg 对齐链路问题", ["ext-community-video-dubbing-tts-webui-001"], "", "community_images"),
    # Second-wave external expansion: LoRA/SD training, ASR/video pipeline,
    # 3D/CV, AI4Science specialties, user monitoring, proxying, and image build.
    ("kohya sd-scripts 训练 sdxl lora 时 pytorch 和 cuda 环境先怎么配", ["ext-sd-scripts-lora-training-001"], "pytorch_basics", "external_second_wave"),
    ("kohya_ss 图形界面里底模、数据集、输出和日志目录应该怎么填", ["ext-kohya-ss-gui-paths-001"], "linux_ops", "external_second_wave"),
    ("diffusers 训练图片模型时 lora 和 dreambooth 应该怎么选", ["ext-diffusers-lora-dreambooth-001"], "pytorch_basics", "external_second_wave"),
    ("lora 训练结果不像,caption、触发词和 bucket 分辨率要怎么检查", ["ext-sd-dataset-caption-buckets-001"], "pytorch_basics", "external_second_wave"),
    ("训练好的 lora 想放到 comfyui 里用,底模和格式要检查什么", ["ext-sd-lora-merge-convert-001"], "pytorch_basics", "external_second_wave"),
    ("faster-whisper 用 gpu 转录长音频时 cuda 库和 int8 float16 怎么选", ["ext-faster-whisper-gpu-asr-001"], "inference_serving", "external_second_wave"),
    ("whisperx 字幕时间戳不准,想做词级对齐和说话人分离", ["ext-whisperx-alignment-diarization-001"], "inference_serving", "external_second_wave"),
    ("demucs 怎么把视频里的 vocals 和伴奏先分离出来", ["ext-demucs-vocal-separation-001"], "linux_ops", "external_second_wave"),
    ("视频配音前后用 ffmpeg 抽音频、切片、再合回视频怎么排查同步", ["ext-ffmpeg-video-dubbing-chain-001"], "linux_ops", "external_second_wave"),
    ("nerfstudio 用自己的照片训练 nerf,需要 cuda 和 colmap 数据吗", ["ext-nerfstudio-install-custom-data-001"], "gpu_troubleshooting", "external_second_wave"),
    ("colmap feature extraction 没用上 gpu 或重建失败怎么查", ["ext-colmap-gpu-reconstruction-001"], "gpu_troubleshooting", "external_second_wave"),
    ("nerfstudio splatfacto 训练 gaussian splatting 时 gsplat cuda 编译失败", ["ext-nerfstudio-splatfacto-gsplat-001"], "gpu_troubleshooting", "external_second_wave"),
    ("pytorch3d 安装时 cuda 和 torch 版本不匹配怎么办", ["ext-pytorch3d-install-cuda-001"], "pytorch_basics", "external_second_wave"),
    ("open3d 怎么在服务器上查看和处理 ply 点云文件", ["ext-open3d-pointcloud-visualization-001"], "linux_ops", "external_second_wave"),
    ("monai 做 3d 医学影像分割时显存和 dicom nifti 数据要注意什么", ["ext-monai-medical-imaging-001"], "pytorch_basics", "external_second_wave"),
    ("pytorch geometric 或 dgl 做图神经网络时 cuda wheel 怎么选", ["ext-pyg-dgl-gnn-install-001"], "pytorch_basics", "external_second_wave"),
    ("physicsnemo 做 pinn fno meshgraphnet 这类物理机器学习怎么开始", ["ext-physicsnemo-sciml-gpu-001"], "pytorch_basics", "external_second_wave"),
    ("nvitop 想看训练进程的显存、gpu 利用率和进程树", ["ext-nvitop-user-gpu-monitor-001"], "gpu_troubleshooting", "external_second_wave"),
    ("pytorch 训练想用 tensorboard 看 loss 曲线和样例图片", ["ext-tensorboard-pytorch-logging-001"], "pytorch_basics", "external_second_wave"),
    ("wandb 和 mlflow 怎么记录训练参数、指标和模型文件", ["ext-wandb-mlflow-experiment-tracking-001"], "pytorch_basics", "external_second_wave"),
    ("gradio 或 comfyui 想用 caddy 反向代理到域名和 https", ["ext-caddy-reverse-proxy-webui-001"], "linux_ops", "external_second_wave"),
    ("没有公网端口时 frp、tailscale funnel、cloudflare tunnel 怎么选", ["ext-tunnel-frp-tailscale-cloudflare-001"], "linux_ops", "external_second_wave"),
    ("自制 cuda 镜像 dockerfile 怎么写,镜像太大怎么优化", ["ext-dockerfile-cuda-image-build-001"], "linux_ops", "external_second_wave"),
    ("镜像里用 uv 或 micromamba 固定 python 依赖,该怎么选", ["ext-python-env-uv-micromamba-image-001"], "linux_ops", "external_second_wave"),
    ("想把 huggingface 模型提前下载到镜像或数据盘,避免每次启动重下", ["ext-hf-snapshot-preload-image-001"], "linux_ops", "external_second_wave"),
    # 637-session targeted expansion: high-frequency support questions from the
    # real-session routing set, grounded in upstream docs rather than raw chats.
    ("sd webui 在服务器里启动了,外面打开 7860 显示 refused to connect", ["ext-a1111-webui-listen-port-001"], "inference_serving", "session_637_targeted"),
    ("automatic1111 下载的 checkpoint 和 lora 放到数据盘后页面里找不到", ["ext-a1111-model-paths-001"], "modelverse", "session_637_targeted"),
    ("a1111 的 controlnet 扩展装了但面板不出现,模型也加载不了", ["ext-sd-webui-controlnet-install-001"], "inference_serving", "session_637_targeted"),
    ("webui jupyter gradio 日志没报错但浏览器访问端口被拒绝", ["ext-webui-refused-connection-debug-001"], "login", "session_637_targeted"),
    ("ssh 连实例超时和 permission denied 应该分别怎么查", ["ext-ssh-connection-debug-001"], "login", "session_637_targeted"),
    ("scp 上传数据集 permission denied,rsync 大文件中断后怎么继续", ["ext-scp-rsync-transfer-debug-001"], "linux_ops", "session_637_targeted"),
    ("ollama run 报 unable to load model sha256 blob,是不是模型没下完整", ["ext-ollama-model-cache-repair-001"], "inference_serving", "session_637_targeted"),
    ("ollama 调大上下文后显存不够,模型还一直占着显存怎么卸掉", ["ext-ollama-context-vram-001"], "gpu_troubleshooting", "session_637_targeted"),
    ("想用 open webui 接本机 ollama 或 vllm 的 openai 接口", ["ext-openwebui-ollama-openai-001"], "inference_serving", "session_637_targeted"),
    ("团队有多个 openai compatible 模型服务,想用 litellm 统一 base url", ["ext-litellm-proxy-openai-compatible-001"], "inference_serving", "session_637_targeted"),
    ("docker 里 nvidia-smi 看不到卡,或者 --gpus all 只看到部分 gpu", ["ext-docker-gpu-visible-devices-advanced-001"], "gpu_troubleshooting", "session_637_targeted"),
    ("8 卡机器 torch 只显示 4 张,怎么确认是容器还是 cuda_visible_devices 限制", ["ext-gpu-card-count-mismatch-001"], "gpu_troubleshooting", "session_637_targeted"),
    ("一张 gpu 想同时跑几个小任务,nvidia mps 适合吗", ["ext-nvidia-mps-single-node-sharing-001"], "gpu_troubleshooting", "session_637_targeted"),
    ("训练数据太大不能放 git,怎么用 dvc 管数据版本和远端存储", ["ext-dvc-data-versioning-001"], "linux_ops", "session_637_targeted"),
    ("对象存储里的大量训练图片想快速同步到实例,用 s5cmd 还是 minio mc", ["ext-s5cmd-minio-object-transfer-001"], "linux_ops", "session_637_targeted"),
    ("想做自己的图片语音微调数据集,用 label studio 怎么标注导出", ["ext-label-studio-dataset-annotation-001"], "pytorch_basics", "session_637_targeted"),
    ("重启容器或者停止实例后,下载的模型和训练结果到底会不会丢", ["ext-model-files-persistence-boundary-001"], "linux_ops", "session_637_targeted"),
    ("ssh 断了 webui 就停,怎么把模型服务放后台并能看日志", ["ext-systemd-user-service-webui-001"], "linux_ops", "session_637_targeted"),
    # Focused补充: safe GPU cleanup, torch.compile, and CUDA Toolkit install.
    ("nvidia-smi 里显存被别的 pid 占着,怎么安全清理不能误杀任务", ["ext-nvidia-smi-safe-cleanup-001"], "gpu_troubleshooting", "cuda_compile_cleanup"),
    ("pytorch 2 的 torch.compile 怎么加到训练脚本里,第一次慢正常吗", ["ext-pytorch-torch-compile-basic-001"], "pytorch_basics", "cuda_compile_cleanup"),
    ("torch.compile 后一直 graph break 或 recompiling,怎么开日志排查动态 shape", ["ext-pytorch-torch-compile-debug-001"], "pytorch_basics", "cuda_compile_cleanup"),
    ("编译 cuda 扩展提示 nvcc not found,linux 上 cuda toolkit 应该怎么装", ["ext-cuda-toolkit-install-linux-001"], "gpu_troubleshooting", "cuda_compile_cleanup"),
    # Third-wave external expansion: production/research platform questions beyond
    # basic installation, covering advanced training, governed serving, cluster
    # networking, queues, evaluation, CV, AI4Science, and data/model formats.
    ("普通 ddp 微调大模型显存不够,想换 accelerate fsdp 该先配哪些项", ["ext-accelerate-fsdp-config-001"], "pytorch_basics", "advanced_training"),
    ("fsdp 训练完只有分片目录,怎么合成能部署或上传的权重", ["ext-fsdp-checkpoint-merge-001"], "pytorch_basics", "advanced_training"),
    ("多卡训练大模型时 fsdp 和 deepspeed zero3 该怎么取舍", ["ext-fsdp-vs-deepspeed-zero-001"], "pytorch_basics", "advanced_training"),
    ("还没买实例前,怎么先估一下这个模型至少要多少显存", ["ext-accelerate-memory-estimator-001"], "pytorch_basics", "advanced_training"),
    ("transformers device_map auto 能不能把模型放一部分到 cpu 上跑", ["ext-hf-big-model-device-map-offload-001"], "inference_serving", "advanced_training"),
    ("模型再大一点只靠 fsdp 不够,什么时候要 tensor parallel 或 pipeline parallel", ["ext-megatron-lm-accelerate-001"], "pytorch_basics", "advanced_training"),
    ("想省训练激活显存,gradient checkpointing 打开后为什么会慢", ["ext-pytorch-activation-checkpointing-001"], "pytorch_basics", "advanced_training"),
    ("fsdp 已经开了但显存没降多少,auto wrap 和 bf16 要怎么查", ["ext-fsdp-mixed-precision-auto-wrap-001"], "pytorch_basics", "advanced_training"),
    ("一个底座模型挂好几个 lora,想用 vllm 对外提供 openai 接口", ["ext-vllm-lora-serving-001"], "inference_serving", "production_serving_governance"),
    ("vllm 服务不想重启,能不能运行中给不同客户加 lora adapter", ["ext-vllm-dynamic-lora-001"], "inference_serving", "production_serving_governance"),
    ("接口返回必须是 json schema 格式,vllm 结构化输出怎么做", ["ext-vllm-structured-output-001"], "inference_serving", "production_serving_governance"),
    ("vllm 上线后想看 qps、延迟、排队和显存指标接 prometheus", ["ext-vllm-prometheus-metrics-001"], "inference_serving", "production_serving_governance"),
    ("线上大模型接口变慢,想用 tracing 看每个请求卡在哪里", ["ext-vllm-opentelemetry-tracing-001"], "inference_serving", "production_serving_governance"),
    ("vllm 端口要给客户访问,反向代理和鉴权安全上要注意什么", ["ext-vllm-security-reverse-proxy-001"], "inference_serving", "production_serving_governance"),
    ("给团队发不同 api key,希望每个 key 有预算和可用模型限制", ["ext-litellm-virtual-keys-budget-001"], "inference_serving", "production_serving_governance"),
    ("多个团队共用 litellm 代理,想按人或团队限流控成本", ["ext-litellm-rate-limit-teams-001"], "inference_serving", "production_serving_governance"),
    ("很多请求前缀一样,vllm prefix cache 能不能降低首 token 延迟", ["ext-vllm-prefix-cache-001"], "inference_serving", "production_serving_governance"),
    ("两台 gpu 服务器多机训练前,想先测 nccl allreduce 带宽", ["ext-nccl-tests-allreduce-001"], "gpu_troubleshooting", "cluster_network"),
    ("多节点训练特别慢,怎么判断 gpu 到网卡的拓扑是不是绕远了", ["ext-nccl-gpu-nic-topology-001"], "gpu_troubleshooting", "cluster_network"),
    ("rdma 环境 nccl 还是走不起来,acs 或 iommu 会不会影响", ["ext-nccl-acs-iommu-rdma-001"], "gpu_troubleshooting", "cluster_network"),
    ("ib 口看起来正常但训练带宽低,除了 nccl 还要测什么", ["ext-infiniband-low-level-test-001"], "gpu_troubleshooting", "cluster_network"),
    ("torchrun 跨节点启动卡住,MASTER_ADDR 和 nccl 网卡变量怎么最小化检查", ["ext-multinode-nccl-env-minimal-001"], "gpu_troubleshooting", "cluster_network"),
    ("k8s 上多用户训练希望排队等 gpu,PyTorchJob 怎么接 kueue", ["ext-kueue-pytorchjob-queue-001"], "linux_ops", "job_scheduling"),
    ("不同 gpu 规格想分队列配额,避免一个团队把资源全占了", ["ext-kueue-resource-flavors-quota-001"], "linux_ops", "job_scheduling"),
    ("kubeflow trainer 现在提交 pytorch 训练任务的入口是什么", ["ext-kubeflow-trainer-trainjob-001"], "pytorch_basics", "job_scheduling"),
    ("分布式训练必须所有 worker 一起拿到 gpu,gang scheduling 解决什么", ["ext-volcano-gang-scheduling-001"], "linux_ops", "job_scheduling"),
    ("kubeflow 训练任务 pending 或失败,应该先看哪些事件和日志", ["ext-kubeflow-job-failure-logs-001"], "linux_ops", "job_scheduling"),
    ("新模型上线前想跑一套标准题,用 lm eval harness 怎么开始", ["ext-lm-eval-harness-basic-001"], "pytorch_basics", "eval_observability"),
    ("公司自己的题库想接进 lm evaluation harness,要准备什么文件", ["ext-lm-eval-custom-task-001"], "pytorch_basics", "eval_observability"),
    ("rag 或 agent 回答质量想用 mlflow 做评测,需要哪些输入", ["ext-mlflow-genai-evaluation-001"], "inference_serving", "eval_observability"),
    ("模型版本从 staging 到 production,mlflow model registry 怎么管", ["ext-mlflow-model-registry-promotion-001"], "pytorch_basics", "eval_observability"),
    ("客户验收模型时,怎么把评测分数、服务指标和成本放在同一套门槛里", ["ext-eval-observability-release-gate-001"], "inference_serving", "eval_observability"),
    ("想训练自己的目标检测模型,yolo 数据 yaml 和 gpu 命令怎么准备", ["ext-ultralytics-yolo-custom-training-001"], "pytorch_basics", "cv_ai4science_data"),
    ("yolo 训练显存爆了或 gpu 利用率低,batch imgsz workers 先调谁", ["ext-ultralytics-yolo-oom-performance-001"], "gpu_troubleshooting", "cv_ai4science_data"),
    ("sam2 第一次跑图像或视频分割,checkpoint 和 demo 怎么准备", ["ext-sam2-install-checkpoints-001"], "inference_serving", "cv_ai4science_data"),
    ("sam2 分割长视频又慢又爆显存,应该按帧数还是分辨率排查", ["ext-sam2-video-segmentation-oom-001"], "gpu_troubleshooting", "cv_ai4science_data"),
    ("科研用户跑 lammps 分子模拟,怎么确认 gpu package 真的启用了", ["ext-lammps-gpu-package-001"], "gpu_troubleshooting", "cv_ai4science_data"),
    ("pyscf 量子化学计算想迁到 gpu,gpu4pyscf 和 to_gpu 怎么验证", ["ext-pyscf-gpu4pyscf-001"], "gpu_troubleshooting", "cv_ai4science_data"),
    ("hpc 用户不用 docker,apptainer 容器里怎么让 cuda 和显卡可见", ["ext-apptainer-gpu-container-001"], "linux_ops", "cv_ai4science_data"),
    ("几千万张训练图片都是小文件,webdataset tar shard 适合吗", ["ext-webdataset-sharded-tar-001"], "linux_ops", "cv_ai4science_data"),
    ("onnx 模型想 int8 量化,动态量化和静态量化怎么选", ["ext-onnxruntime-quantization-001"], "inference_serving", "cv_ai4science_data"),
    ("transformers 模型导出 onnx 后没用上 gpu,provider 怎么查", ["ext-optimum-onnx-gpu-001"], "inference_serving", "cv_ai4science_data"),
    ("awq gptq bitsandbytes gguf 都是 4bit 吗,部署时怎么分", ["ext-transformers-awq-gptq-quantization-001"], "inference_serving", "cv_ai4science_data"),
    ("下载到 gguf 文件后,为什么不能直接用 transformers 当 safetensors 加载", ["ext-gguf-llama-cpp-convert-001"], "inference_serving", "cv_ai4science_data"),
]


def main() -> None:
    out_path = Path(__file__).resolve().parent / "external_golden.jsonl"
    with out_path.open("w", encoding="utf-8") as fh:
        for i, (question, expected, area, group) in enumerate(Q, start=1):
            row = {
                "question_id": f"ext-eg-{i:03d}",
                "question": question,
                "product_area": area,
                "expected_behavior": "answer",
                "expected_chunk_ids": expected,
                "group": group,
            }
            fh.write(json.dumps(row, ensure_ascii=False) + "\n")
    print(f"wrote {len(Q)} golden questions -> {out_path}")


if __name__ == "__main__":
    main()
