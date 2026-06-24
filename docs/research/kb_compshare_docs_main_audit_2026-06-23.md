# Compshare Docs vs Knowledge Base Audit (2026-06-23)

## Scope

- Agent repo branch: `codex/kb-compshare-docs-main-audit`
- Agent repo head: `76984cc74f7715e45d9302e049a8fe95f941be63`
- Internal docs remote: `https://git.ucloudadmin.com/www/compshare-docs.git`
- Internal docs head: `3fb1386c16b5038dec1f736b0231966ad6cb47cb`
- Audited docs: 235 Markdown/MDX files
- Audited KB chunks: 752 chunks across stage2b_w0.jsonl=687, external_w0.jsonl=55, curated_faq.jsonl=10

## Result

- Direct source-reference coverage: 176 docs.
- Covered, but through a source-ref alias or rewritten content: 9 docs.
- Likely covered by content but source refs did not map cleanly: 43 docs.
- Partially overlapped with existing KB content: 4 docs.
- Ref matched but current text overlap is low: 1 docs.
- Not found / weak overlap: 2 docs.
- Stale KB source refs not mapped to current doc paths: 55.

Interpretation: the deployed KB already includes the main Agent community docs, but the full docs tree is not completely represented. The main actionable gaps are new or expanded customer-facing docs around Codex Agent Plan setup, GPU region/storage guidance, and the SwitchChargeType API.

## High-Priority Review List

| Path | Status | Best KB Match | Ref Chunks | Text Hit |
| --- | --- | --- | ---: | ---: |
| pages/modelverse/best_practice/codexagent.md | not_found | LangBot接入优云智算API模型供应商 (0.23) | 0 | 0% |
| pages/operation/gpu/networkvolume.md | partial_content_overlap | minimax-speech 同步合成 API - 接口与输入参数 (part 1/3) (0.14) | 0 | 0% |
| pages/operation/introduce/region.md | partial_content_overlap | 登录GPU实例 - 镜像类型与登录方式概览 (0.46) | 0 | 0% |

## Medium-Priority Review List

| Path | Status | Best KB Match | Ref Chunks | Text Hit |
| --- | --- | --- | ---: | ---: |
| pages/gpus/instance/switchchargetype.md | partial_content_overlap | AttachUS3 接口说明与参数 (0.45) | 0 | 0% |
| pages/modelverse/models/text_api/deepseek-ocr.md | partial_content_overlap | DeepSeek-OCR 模型介绍与非流式请求示例 (0.84) | 0 | 0% |
| pages/serviceagreement/overview.md | ref_match_low_content_overlap | 优云智算用户协议（一）- 账户注册、实名认证与多账户管理 (0.50) | 3 | 0% |

## Source-Reference Cleanup Candidates

These KB refs did not map to a current docs path candidate. Some are expected because older pipeline refs used stems or alternate path slugs, but they are worth cleaning up in the next rebuild.

| Source Ref | Chunks | Product Areas | Sample Title |
| --- | ---: | --- | --- |
| gitlab-compshare-docs__action-createcompshareinstance | 1 | resource_purchase | 创建轻量级算力平台主机资源 API - CreateCompShareInstance |
| gitlab-compshare-docs__action-describecompshareinstance | 3 | resource_purchase | DescribeCompShareInstance API 概述 - 请求 / 响应字段 |
| gitlab-compshare-docs__action-rebootcompshareinstance | 1 | resource_purchase | 重启轻量算力平台实例 API (RebootCompShareInstance) |
| gitlab-compshare-docs__action-reinstallcompshareinstance | 1 | image | 重装算力平台实例 ReinstallCompShareInstance API 参考 |
| gitlab-compshare-docs__action-resetcompshareinstancepassword | 1 | login | ResetCompShareInstancePassword API Reference |
| gitlab-compshare-docs__action-startcompshareinstance | 1 | resource_purchase | 启动算力平台实例 API - StartCompShareInstance |
| gitlab-compshare-docs__action-stopcompshareinstance | 1 | resource_purchase | 关闭算力平台实例 API - StopCompShareInstance |
| gitlab-compshare-docs__action-terminatecompshareinstance | 1 | resource_purchase | 删除轻量算力共享平台虚机实例 API (TerminateCompShareInstance) |
| gitlab-compshare-docs__modelverse-models-audio-api-custom-voice-api | 3 | modelverse | 自定义音色管理 API - 概述与上传接口 |
| gitlab-compshare-docs__modelverse-models-audio-api-indexteam-indextts-extend | 2 | modelverse | IndexTeam/IndexTTS 系列模型扩展参数说明（字段一览） |
| gitlab-compshare-docs__modelverse-models-audio-api-minimax-speech | 5 | modelverse | minimax-speech 同步合成 API - 接口与输入参数 (part 1/3) |
| gitlab-compshare-docs__modelverse-models-audio-api-suno | 2 | modelverse | Suno 音乐生成模型 - 异步提交任务 API |
| gitlab-compshare-docs__modelverse-models-audio-api-ttts | 4 | modelverse | IndexTTS-2 /v1/audio/infer 接口参数与请求示例 |
| gitlab-compshare-docs__modelverse-models-image-api-black-forest-labs-flux-kontext-max | 2 | modelverse | black-forest-labs/flux-kontext-max API 参考 (part 1/2) |
| gitlab-compshare-docs__modelverse-models-image-api-black-forest-labs-flux-kontext-max-multi | 2 | modelverse | black-forest-labs/flux-kontext-max/multi API 接口参数与示例 (part 1/2) |
| gitlab-compshare-docs__modelverse-models-image-api-black-forest-labs-flux-kontext-max-text-to-image | 2 | modelverse | black-forest-labs/flux-kontext-max/text-to-image API 参数与示例 (part 1/2) |
| gitlab-compshare-docs__modelverse-models-image-api-black-forest-labs-flux-kontext-pro | 2 | modelverse | flux-kontext-pro API 请求参数说明 |
| gitlab-compshare-docs__modelverse-models-image-api-black-forest-labs-flux-kontext-pro-multi | 2 | modelverse | black-forest-labs/flux-kontext-pro/multi 多图编辑 API 参数与调用示例 (part 1/2) |
| gitlab-compshare-docs__modelverse-models-image-api-black-forest-labs-flux-kontext-pro-text-to-image | 2 | modelverse | flux-kontext-pro 文生图 API 参考（请求/响应/示例） (part 1/2) |
| gitlab-compshare-docs__modelverse-models-image-api-black-forest-labs-flux_1-dev | 2 | modelverse | black-forest-labs/flux.1-dev 图像生成 API 参考（请求/响应参数与调用示例） (part 1/2) |
| gitlab-compshare-docs__modelverse-models-image-api-gemini-2_5-flash-image | 3 | modelverse | gemini-2.5-flash-image API 请求与响应参数说明 |
| gitlab-compshare-docs__modelverse-models-image-api-gemini-3-pro-image | 2 | modelverse | gemini-3-pro-image (nano banana pro) 请求与响应参数说明 |
| gitlab-compshare-docs__modelverse-models-image-api-gpt-image-1 | 2 | image | gpt-image-1 API 请求与响应参数说明 |
| gitlab-compshare-docs__modelverse-models-image-api-gpt-image-1_5 | 3 | image | gpt-image-1.5 API 请求参数、响应参数与图片生成示例 (part 1/2) |
| gitlab-compshare-docs__modelverse-models-image-api-qwen-qwen-image | 2 | modelverse | Qwen/Qwen-Image API 参数与调用示例 (part 1/2) |
| gitlab-compshare-docs__modelverse-models-image-api-qwen-qwen-image-edit | 2 | modelverse | Qwen/Qwen-Image-Edit API 参数与调用示例 (part 1/2) |
| gitlab-compshare-docs__modelverse-models-image-api-stepfun-ai-step1x-edit | 2 | modelverse | stepfun-ai/step1x-edit API 请求/响应参数与调用示例 (part 1/2) |
| gitlab-compshare-docs__modelverse-models-text-api-api-expand | 1 | modelverse | 优云智算 ModelVerse OpenAPI 支持的协议入口与自定义扩展字段 |
| gitlab-compshare-docs__modelverse-models-text-api-deepseek-ocr | 2 | modelverse | DeepSeek-OCR 模型介绍与非流式请求示例 |
| gitlab-compshare-docs__modelverse-models-text-api-embeddings | 6 | modelverse | Embedding 向量嵌入 - 概述与快速开始 |
| gitlab-compshare-docs__modelverse-models-text-api-response-api | 1 | modelverse | /v1/responses 接口文档（ModelVerse 兼容 OpenAI Responses API） |
| gitlab-compshare-docs__modelverse-models-text-api-thinking-deepseek | 1 | modelverse | DeepSeek V3.1模型开启关闭思考(thinking)参数说明与API示例 |
| gitlab-compshare-docs__modelverse-models-text-api-thinking-doubao | 1 | modelverse | Doubao豆包模型思考功能(thinking)参数说明与API示例 |
| gitlab-compshare-docs__modelverse-models-video-api-doubao-seedance-1-5-pro-251215 | 3 | modelverse | doubao-seedance-1-5-pro-251215 异步提交视频生成任务接口 (part 1/2) |
| gitlab-compshare-docs__modelverse-models-video-api-minimax-hailuo-2_3-i2v | 2 | modelverse | MiniMax/Hailuo-2.3-I2V 异步提交任务接口 |
| gitlab-compshare-docs__modelverse-models-video-api-minimax-hailuo-2_3-t2v | 2 | modelverse | MiniMax/Hailuo-2.3-T2V 异步提交任务 API |
| gitlab-compshare-docs__modelverse-models-video-api-openai-sora2-i2v | 2 | modelverse | OpenAI/Sora2-I2V 异步提交任务接口 |
| gitlab-compshare-docs__modelverse-models-video-api-openai-sora2-i2v-pro | 2 | modelverse | OpenAI/Sora2-I2V-Pro 异步提交任务接口 |
| gitlab-compshare-docs__modelverse-models-video-api-openai-sora2-t2v | 2 | modelverse | OpenAI Sora2-T2V 异步提交文生视频任务 API |
| gitlab-compshare-docs__modelverse-models-video-api-openai-sora2-t2v-pro | 2 | modelverse | OpenAI/Sora2-T2V-Pro 异步提交任务 |

## Evidence Files

- Summary JSON: `F:\compshare-agent-runs\kb-docs-main-audit-20260623\analysis\summary.json`
- Per-document coverage CSV: `F:\compshare-agent-runs\kb-docs-main-audit-20260623\analysis\doc_coverage.csv`
- Stale source refs JSON: `F:\compshare-agent-runs\kb-docs-main-audit-20260623\analysis\stale_source_refs.json`

## Recommendation

Update the KB in the next governed corpus rebuild for the high-priority gaps and the SwitchChargeType API. Do not hand-edit the deployed JSONL alone: regenerate the corpus, rebuild the qwen3 sidecar, and update digest pins together.

## Notes

- 43 docs appear in KB content but lack a clean source-ref match; this points to source-ref naming drift, not necessarily missing content.
- 1 docs have source-ref matches but low exact-line overlap; this can mean either doc drift or expected rewriting/cleaning. Review before classifying as stale.
