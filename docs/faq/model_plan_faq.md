# 优云智算模型套餐 FAQ

> 来源：compshare.feishu.cn/wiki/T9GWwVtj4iXoTJkGuj8cRrtYnog
> 抓取日期：2026-04-13

## 1. 目前套餐内支持哪些模型
目前套餐内支持的模型列表：
- MiniMax-M2.1 / MiniMax-M2.5
- moonshotai/kimi-k2.5
- zai-org/glm-5
- gpt-5.1 / gpt-5.1-codex-max / gpt-5.1-codex-mini
- gpt-5.2 / gpt-5.2-codex
- gpt-5.3-codex / gpt-5.4
- claude-opus-4-6 / claude-sonnet-4-6 / claude-haiku-4-5-20251001
- claude-opus-4-5-20251101 / claude-sonnet-4-5-20250929
- deepseek-ai/DeepSeek-V3.2

## 2. 积分和 Token 的换算关系
积分额度 = 输入 Token × 输入倍率 + 缓存创建 Token × 缓存创建倍率 + 缓存命中倍率 × 缓存命中 Token + 输出倍率 × 输出 Token。

注意：Claude 等部分模型倍率较高，在 OpenClaw 等 Agent 工具中请谨慎使用。

## 3. 多个包的抵扣顺序
- 每次 API 调用消耗积分时，扣除顺序为：包月套餐包 > 按量套餐包（按量包/积分包）。
- 如果同时购买了多个按量包：按最先到期的按量包优先扣除。
- 包月套餐：如果购买了多个包月套餐包，优先扣价格低的包月套餐。每多买 1 个同套餐的包月，有效期叠加 30 天。包月为每日固定上限的积分额度，每天 0 点刷新当日额度。

## 4. 如果需要的模型不在套餐内怎么办
请求不在套餐内的模型会正常按照使用量扣费，不走套餐逻辑。
注意：配置时请仔细检查调用模型名称，以防调用套餐外模型产生大额扣费。

## 5. 目前能在哪些工具中使用
支持 Claude Code、OpenCode、OpenClaw、Cline、Kilo Code、Codex CLI 等主流编程工具（TRAE 暂不支持），同时支持 CherryStudio 等 Chatbot 客户端及 API 调用场景。

## 6. OpenClaw、CC 中积分消耗较快
OpenClaw 和 CC 等客户端中会嵌套较多的工具或 Agent，Token 用量相对较多，小套餐请谨慎使用。

## 7. OpenClaw、Claude Code 及 CodeX 接入指南
- OpenClaw：https://www.compshare.cn/docs/modelverse/best_practice/openclaw
- Claude Code：https://www.compshare.cn/docs/modelverse/best_practice/claudecode
- OpenCode：https://www.compshare.cn/docs/modelverse/best_practice/opencode
- CodeX：https://www.compshare.cn/docs/modelverse/best_practice/codex

## 8. GPT 在部分工具中无法使用
GPT 请求需要使用 responses 协议。在工具中将协议类型修改为 openai-responses，API 地址填写 `https://api.modelverse.cn/v1`（确保拼接后为 `https://api.modelverse.cn/v1/responses`）。

## 9. Claude 在部分工具中无法使用
Claude 请求需要选择 Anthropic 协议。API 地址填写 `https://api.modelverse.cn/`（确保拼接后为 `https://api.modelverse.cn/v1/messages`）。

## 10. 在 OpenClaw 中切换模型不生效
需要更换默认模型，或修改 `.openclaw/openclaw.json` 中的默认模型。
