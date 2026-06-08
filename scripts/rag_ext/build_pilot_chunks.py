#!/usr/bin/env python3
"""Author the RAG Phase 1 external chunks (vLLM / SGLang / Ollama + GPU ops).

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
  pytorch-docs:notes/cuda                    https://docs.pytorch.org/docs/stable/notes/cuda.html
  nvidia-docs:nvidia-smi                      https://docs.nvidia.com/deploy/nvidia-smi/
  hf-docs:installation                        https://huggingface.co/docs/huggingface_hub/installation
  comfyui-docs:getting_started               https://docs.comfy.org/
  comfyui-repo:README                        https://github.com/comfyanonymous/ComfyUI
  comfyui-repo:comfy/cli_args.py             https://github.com/comfyanonymous/ComfyUI/blob/master/comfy/cli_args.py
  comfyui-repo:extra_model_paths.yaml.example  https://github.com/comfyanonymous/ComfyUI/blob/master/extra_model_paths.yaml.example
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

CHUNKS = (
    VLLM_CHUNKS + SGLANG_CHUNKS + OLLAMA_CHUNKS + COMFYUI_CHUNKS + GPU_OPS_CHUNKS + EXTRA_CHUNKS
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
