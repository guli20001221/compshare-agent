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
