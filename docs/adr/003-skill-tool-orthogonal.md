# ADR-003: Skills and Tools as Orthogonal Dimensions

**Status**: Proposed (2026-05-28)
**Depends on**: ADR-001(三路分层)
**Supersedes**: 当前 `internal/intent/capabilities/*.md` 把 skill 和 tool 揉在一个 frontmatter 的设计

## Context

当前 `internal/intent/capabilities/*.md` 把两件本质不同的事揉在单文件 frontmatter 里:

1. **Playbook**(教 LLM 怎么思考):`planner_directives` / `planner_examples`
2. **Tool 绑定**(LLM 调什么 API):`required_tool`

这造成 3 个具体问题:
- **跨 tier 复用阻塞**:同一个 "destructive op 前 warn 用户" 的 playbook,fast path 的 stop_uinstance 用、agent path 的 deploy 也用,但当前结构只能绑一个 intent
- **双源数据漂移**:`internal/intent/capability_registry.go:41-48` 的 6 个 Go struct entry vs `internal/intent/capabilities/` 下 6 个 active `.md` frontmatter(另有 1 个 `.disabled`),编译期断言查得到不一致,语义漂移查不出(指令更新但 tool 映射没改)。当前 capability 仅覆盖 catalog + pricing 两类查询,其余 14 个 Intent(monitor / billing / diagnosis / knowledge_qa / operation_lifecycle 等)走 ReAct;ADR-003 范围是这 6 个,扩到全 Intent 是 B2-后续
- **概念混淆**:Claude Code skill / OpenAI instructions / Bedrock instruction 都是 **playbook**,跟 tool spec(OpenAI Function Calling / Anthropic Tool Use / MCP)是两个独立维度;本项目 capabilities 单文件设计跟业界正交模型对不上

## Decision

拆分为两个正交维度,均跨 tier 复用:

### Skill(playbook,指导 LLM 思考)

- **形态**:markdown bundle + YAML frontmatter
- **加载**:progressive disclosure — metadata 默认入 prompt,body 在 planner 触发后才拉
- **内容**:steps / decision points / pitfalls / required_tools 引用(只引用名字,不绑定)
- **目录**:`internal/skills/<skill_name>/skill.md`(必要时同目录加 `examples.jsonl` / `runbook.md`)
- **Body cap**:100 行(收紧自 Anthropic Claude Code 官方建议的 500 行 / 5000 tokens,详见 ADR-004 §Body Cap 策略,ds-v4-flash 雪崩防护)
- **加载机制**:`internal/skills/loader.go`,扫描目录 + 解析 frontmatter

### Tool(typed callable,LLM 实际调用)

- **形态**:OpenAI Function Calling JSON spec + `x-compshare` 扩展
- **加载**:schema 全量暴露给 LLM,body 不存在
- **扩展字段**:
  ```json
  "x-compshare": {
    "class": "read | mutating",
    "requires_acceptance": true,
    "idempotent": true,
    "tier_eligible": ["fast", "agent"]
  }
  ```
- **目录**:`internal/tools/<tool_name>.json` spec + `<tool_name>.go` handler
- **注册**:`go generate` 从目录扫描产 binding,消除双源数据漂移

### 跨 tier 复用例

| Skill / Tool | Fast | Knowledge | Agent |
|---|---|---|---|
| `safety_warning_skill` | ✅ destructive op 前 | — | ✅ deploy / SSH 前 |
| `citation_skill` | — | ✅ 必用 | ✅ 文档查询时 |
| `error_recovery_skill` | ✅ | ✅ | ✅ |
| `DescribeCompShareInstance` tool | ✅ | — | ✅ |
| `OpenSSHSession` tool | — | — | ✅ |
| `RAGRetrieve` tool | — | ✅ | ✅ |

## Consequences

**Positive**
- 双源数据漂移消除(codegen 单源)
- Skill 跨 tier 复用,减少重复 prompt 工程
- Tool spec 业界标准,可未来通过 MCP gateway 暴露(ADR 待写)
- 加新 capability 不再触碰 Go 代码(纯 markdown + JSON)

**Negative**
- 现有 6 个 active `capabilities/*.md` 拆为 `internal/skills/` + `internal/tools/` 两目录(B2 工作,3 周);剩余 14 个 Intent 后续逐步从 ReAct 迁过来
- `internal/intent/capability_registry.go` 重写为 codegen(~200 行新增 + 旧 binding 删)
- 需新增 `internal/skills/loader.go`(~300 行)

**Risks**
- Skill body 入 prompt 又涨 input_tok:触发 2-3 skill 各 100 行 = 又一次 5k+ system prompt → 缓解:planner 触发 cap 最多 2 skill;skill body 内容只写 steps + decision + pitfalls,不写 narrative

## 业界对照

| 维度 | 本项目 | Claude Code | OpenAI Assistants | Bedrock | MS Copilot |
|---|---|---|---|---|---|
| Skill / Playbook | markdown + progressive disclosure | SKILL.md(同构) | instructions(扁平) | instruction field | Plugins prompt |
| Tool / Callable | OpenAI Function + x-compshare | Tool Use | Function Calling | OpenAPI Action Group | Connector |
| Skill ⊥ Tool | ✅ 正交 | ✅ 正交 | ✅ 正交 | 部分 | ✅ 正交 |
| 跨 tier 复用 | ✅ | ✅ | ✅ | 部分 | ✅ |

**对齐最近**:Claude Code(SKILL.md + Tool Use,二者正交且 progressive disclosure)。

## Acceptance

- [ ] `internal/skills/` 目录建立,现存 6 个 active capability 的 playbook 部分迁移完成(其余 14 个 Intent 走 ReAct 路径,后续 ADR 决定迁移节奏)
- [ ] `internal/tools/` 目录建立,工具 spec 改为 OpenAI Function Calling 标准 + `x-compshare` 扩展
- [ ] `internal/intent/capability_registry.go` 改为 codegen(`go generate ./...` 产 binding)
- [ ] Progressive disclosure 实现:planner system prompt 只含 skill metadata,body 在触发后拉
- [ ] 至少 1 个跨 tier 复用 skill 投产(推荐 `safety_warning_skill`)

## Skill 文件示范(`internal/skills/deploy_model/skill.md`)

```markdown
---
name: deploy_model
description: 用户想在 CompShare 上部署 LLM(Qwen / DeepSeek / Llama 等)
triggers:
  - "部署 [模型名]"
  - "我想跑 [模型名]"
  - "怎么把 [模型名] 跑起来"
applicable_tiers: [agent]
required_tools:
  - DescribeAvailableCompShareInstanceTypes
  - GetCompShareInstancePrice
  - CreateUInstance
  - DescribeCompShareInstance
---

# Deploy Model Skill

## 步骤
1. 推理 VRAM 需求:根据用户提供的模型名/参数量推算
2. 查可用 GPU:call `DescribeAvailableCompShareInstanceTypes` + filter
3. 询价对比:call `GetCompShareInstancePrice` for top 3 候选
4. 推荐 + 用户 confirm(HITL)
5. 创建实例:call `CreateUInstance`,**必走 ConfirmFunc**
6. 验证启动:轮询 `DescribeCompShareInstance` 直到 state=Running
7. 返回登录信息

## 失败处理
- 步骤 5 失败 → 不重试,告知用户
- 步骤 6 超时 → 提示用户去控制台,不删实例

## Pitfalls
- "Qwen32B" 不一定是 32B 参数量,可能 Qwen2.5-32B-Instruct,需 clarify
- VRAM 估算保守,加 20-30% buffer(KV cache)
```

## Tool 文件示范(`internal/tools/describe_compshare_instance.json`)

```json
{
  "name": "DescribeCompShareInstance",
  "description": "查询用户在 CompShare 平台上的实例信息",
  "parameters": {
    "type": "object",
    "properties": {
      "InstanceId": {"type": "string", "description": "实例 ID,可选"},
      "ProjectId": {"type": "string"}
    }
  },
  "x-compshare": {
    "class": "read",
    "requires_acceptance": false,
    "idempotent": true,
    "tier_eligible": ["fast", "agent"]
  }
}
```

## References

- ADR-001: Task-Complexity Tiered Architecture
- Anthropic Claude Code Skill SDK: https://docs.claude.com/agent-skills(对照参考)
- OpenAI Function Calling spec(tool 格式来源)
- HolmesGPT toolset YAML(transformer / prerequisites 借鉴,见 ADR-005)

---

## Amendment 1: MCP Compatibility(2026-05-29)

OpenAI Function Calling spec 跟 MCP Tool spec 字段同构(`parameters` ↔ `inputSchema` 仅命名差异,JSON Schema 内核一致,`x-*` 扩展字段两侧都允许),~30 行 Go adapter 即可双向转换,**不构成架构层 lock-in**。但有 2 件契约语义不是字段名能 cover 的,提前定锁避免外部 client 既定后重命名:

### A. Tool naming namespace

| 场景 | 命名 |
|---|---|
| 内部 Go API 调用 | `DescribeCompShareInstance`(保留现有 CamelCase 习惯,改名 breaking 量过大) |
| MCP 外部暴露 | `compshare/instance/describe`(`provider/category/action`,对齐 MCP 生态惯例) |

映射规则锁在 `internal/mcp/naming.go`(B7 时建,30 行;现在不需要实现,但命名转换规则锁死):
- `CamelCase` → `snake_case` 分词
- 第一段 `Describe / Create / Stop / Reboot ...` → 拆出为 `action`
- 中间段含 `Compshare` 前缀 → 拆出为 namespace `compshare`
- 剩余段拼为 `category`(`Instance` / `Image` / `Disk` ...)

**V1 scope**:仅锁单 word category(`Instance` / `Image` / `Disk` / `Pricing` 等)。**Multi-word category 歧义**(例 `DescribeCompShareAvailableInstanceTypes` → category 取 `available_instance_types` 还是 `instance_types`,`available` 是 modifier 还是独立段?)留到 B7 实施时遇第一个真实 multi-word case 由 **sample-driven 决策并 amend 本 §A**,不预先发明规则(memory `task-complexity-tiering` "加抽象 ≠ 简化" 同源)。

### B. `x-compshare` 扩展字段内外可见性

ADR-003 决定 tool spec 用 OpenAI Function + `x-compshare` 扩展。本 amendment 决定每个扩展字段在 MCP 外部暴露时的可见性,这是**安全决策**,不只是命名:

| `x-compshare` 字段 | MCP 外部 | 决策依据 |
|---|---|---|
| `class` (read / mutating) | ✅ 暴露 | 外部 LLM 客户端必须知道是 mutating 才能正确触发 HITL |
| `requires_acceptance` | ✅ 暴露 | HITL 契约必需,否则 confirm 路径漂移 |
| `idempotent` | ✅ 暴露 | 外部客户端重试语义必需 |
| `tier_eligible` | ❌ 隐藏 | 内部 router 概念,对外无意义,暴露反而误导 |
| `api_action` | ❌ 隐藏 | 内部 CompShare OpenAPI action 名,暴露增加攻击面(外部攻击者拿到直接调底层 API 的字符串) |

MCP gateway 层做 projection:外部 spec 只包含 ✅ 字段,内部使用时拿原始 spec。projection 函数锁在 `internal/mcp/projection.go`(B7 时建)。

### C. 不在本 amendment 范围

下列议题留到 B7 临近实施时由真实 client 需求驱动,不预先 ADR 化(避免空中楼阁,memory `task-complexity-tiering` "加抽象 ≠ 简化" 同源)。**"B7 临近" 触发条件**:B7 正式启动前 1 周 OR 首个真实外部 MCP client 接入需求(以先到为准),届时本 amendment 内容迁出为独立后续 ADR(编号落盘时分配 —— ADR-008 已被「skill 格式与进化契约」占用)。
- MCP transport 选择(stdio / HTTP / SSE)
- 外部 client 认证模型
- versioning(我们 spec change 时 MCP server 怎么 announce)
- streaming + progress notification 协议
- resources / prompts / sampling 是否暴露

### Acceptance(本 amendment 范围)

- [ ] 后续 tool spec 新增时,作者明确填 `x-compshare` 5 项字段中每项的可见性(必填 review checklist 一项)
- [ ] B7 临近时,本 amendment 内容迁出为独立后续 ADR(编号落盘时分配;届时 + transport + auth + versioning 等),本 amendment 退役为历史档
