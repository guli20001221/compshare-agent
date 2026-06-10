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
