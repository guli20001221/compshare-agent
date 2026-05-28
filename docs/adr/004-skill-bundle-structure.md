# ADR-004: Skill Bundle Structure and Progressive Disclosure

**Status**: Proposed (2026-05-29)
**Depends on**: ADR-003(Skill ⊥ Tool 正交)

## Context

ADR-003 决定把 playbook 拆为独立的 Skill 维度,采用 Claude Code-style markdown bundle + progressive disclosure。本 ADR 细化 Skill 的目录布局、文件契约、加载机制、body cap 策略。

当前 `internal/intent/capabilities/*.md` 是单文件 + 全量 frontmatter 注入 prompt(`CapabilityPromptFragments()` 每 turn 全量灌入),违反 progressive disclosure。

## Decision

### 目录布局

```
internal/skills/
├── deploy_model/
│   ├── skill.md                # 必需,frontmatter + body
│   ├── examples.jsonl          # 可选,few-shot 例子
│   └── runbook.md              # 可选,详细 step-by-step(HolmesGPT 借鉴)
├── safety_warning/
│   └── skill.md                # 跨 tier 复用,无 runbook
├── citation/
│   └── skill.md
├── gpu_specs_query/
│   ├── skill.md
│   └── examples.jsonl
└── ...
```

一个 skill = 一个目录,scope-limited bundle。新增 skill = 新建目录 + 一份 `skill.md`,不动 Go 代码(codegen 自动产 binding)。

### Frontmatter Schema

```yaml
---
name: deploy_model                              # 唯一,snake_case(下划线,见下方"name 字符集决策")
description: 用户想在 CompShare 上部署 LLM         # planner 看到的一句话
triggers:                                       # planner LLM 用作分类锚点
  - "部署 [模型名]"
  - "我想跑 [模型名]"
  - "怎么把 [模型名] 跑起来"
applicable_tiers: [agent]                       # ADR-001 三 tier 名
required_tools:                                 # 仅引用名字,不绑定
  - DescribeAvailableCompShareInstanceTypes
  - CreateUInstance
related_skills:                                 # 可选,planner 时一同 fetch
  - safety_warning
body_cap_lines: 100                             # 可选,override 默认
verification_status: production_validated       # 必需,见下方"未验证内容 provenance"
field_refs_verified: true                       # 必需,skill body 内引用的 API 字段名(如 GpuCount/SshLoginCommand)是否经过 grep 对照真实 API schema 验证;diagnose_* 类 skill 从未验证 Chain 提炼时初值为 false
---
```

`triggers` 不是 deterministic 路由(memory `planner-hint-advisory-only`),只是 planner LLM 的分类锚点;planner 仍按整体语义决定 skill 选择。

### name 字符集决策

Anthropic SKILL.md 官方规范用 kebab-case(`[a-z0-9-]`),本项目**显式选择 snake_case**(`[a-z0-9_]`),理由:

1. **Go 包路径对齐**: `internal/skills/diagnose_gpu_not_detected/skill.md` 跟 Go 内部包命名(`my_package`)风格一致;Anthropic SDK 用 TS/Python 没这个约束
2. **跟现有 capability_registry.go entry 同风格**: `IntentGpuSpecsQuery` / `gpu_specs_query` 全 snake-case
3. **跟 ADR-004 "body cap 100 行 ≠ Anthropic 500 行"同源**: 我们明确允许 + 文档化对 Anthropic 规范的有意偏离

**规则**:
- skill `name` 字段: `[a-z][a-z0-9_]*[a-z0-9]`,1-64 char
- skill 目录名: **必须跟 `name` 字段一致**(避免 ADR-004 amendment 引入的 directory-name mismatch 风险)
- CI 校验:`scripts/check_skill_names.sh` 在 pre-commit 跑

### 未验证内容 provenance(2026-05-29 加)

并非所有 skill 都经过真实流量验证。Skill markdown 是给 LLM 当方法论的,如果方法论本身是某人写当初的 best guess,把它当 ground truth 注入 prompt 会让 LLM 把未验证假设当事实输出 → 跟 RAG corpus 进未验证内容同款 fab 风险(memory `corpus-input-source-tiering` 的相邻应用)。

**verification_status 枚举**:

| 值 | 含义 | Loader 行为 |
|---|---|---|
| `production_validated` | 真实流量跑过 ≥ N 轮,SOP 已校准 | 正常 fetch body |
| `spike_validated` | spike(真机或 mock 故障)跑通,未上 production | 正常 fetch body |
| `unverified` | 设计意图,未真实跑过 | **fetch body 时前置 caution 行**:`[CAUTION: this methodology is unverified, treat steps as suggestions not facts]` |

升级条件:
- `unverified → spike_validated`: ≥1 个真实故障 case 跑通 + reviewer sign-off
- `spike_validated → production_validated`: ≥10 个真实流量场景验证 + 误判率 < 10%
- 降级:任何 verified skill 在 production 反馈中暴露重大方法论错误,可手动降回 `unverified` 并 amend

**强制**: 新加 skill 必须填 `verification_status` + `field_refs_verified`,默认值不允许;CI 检查存在性。

### 命令引用约定(skill body 内)

Skill body 内提到的 shell 命令必须区分谁来跑:

| 形式 | 含义 |
|---|---|
| 行内代码 `command-here` 在"call X"/"agent runs"语境 | **agent 通过 SSH sandbox 跑**(命令必须在 ADR-006 SSH 白名单内) |
| `> 在实例终端跑:command-here` 或 `用户自查 command-here` | **用户自己跑**(可超出 sandbox 白名单,例如 `ss -lntp` / `top` 等) |

防止 LLM 误以为可调 sandbox 外命令。

### field_refs_verified 升级路径

- `field_refs_verified: false` → 跑 grep `host\["FieldName"\]` 在 internal/tools / Go SDK 对照真实 API schema → 字段名一致后改 `true` + commit message 引用 SDK 文件
- Loader 看到 `field_refs_verified: false` 时在 body 前置 caution 行追加 `[FIELD REFS NOT VERIFIED — confirm field names against actual API response before action]`

### Progressive Disclosure Loader

```go
// internal/skills/loader.go
type Skill struct {
    Name             string
    Description      string
    Triggers         []string
    ApplicableTiers  []string
    RequiredTools    []string
    RelatedSkills    []string
    BodyCapLines     int
    Path             string
    // Body is lazy — never eager-loaded. Call Body() to fetch.
    bodyOnce sync.Once
    body     string
}

func (s *Skill) Body() string { ... }   // 第一次调用时 read + cache;cap 行数检查在这里

type Loader struct {
    skills map[string]*Skill
}

func NewLoader(root string) (*Loader, error) {
    // 扫描 root/*/skill.md,解析 frontmatter,build map
    // body 不读
}

func (l *Loader) Metadata() []SkillMeta { ... }  // 入 planner system prompt
func (l *Loader) Fetch(name string) (*Skill, bool) { ... }  // 触发后才拉
```

**生命周期**:
1. Boot 时 loader 扫描目录,build `Metadata()`(每条 ~80 tokens)
2. Planner system prompt 注入 metadata 列表(50 skills × 80 = 4k tokens,可控)
3. Planner emit `skills: ["deploy_model", "safety_warning"]`
4. Engine 按 skill 名调 `Fetch(name).Body()`,inject 到 agent path system prompt
5. Fast / Knowledge path **不 fetch body**,只看 metadata 路由

### Body Cap 策略

- **默认硬上限**: 100 行(约 500-800 tokens);**比 Anthropic 官方建议的 500 行 / 5000 tokens 严 8 倍**,因为 ds-v4-flash 在 input_tok 5K→11K 已观测雪崩(memory `priortext-avalanche-invalidates-planner`),我们的 prompt budget 比 Anthropic 紧
- **per-skill override**: `body_cap_lines` 允许特殊 skill 提升(例如复杂部署 skill 可设 150),**hard ceiling 200 行**,超出 build fail(防 `body_cap_lines: 1000` 绕开整个 cap 机制)
- **CI 检查**: `scripts/check_skill_caps.sh` 在 pre-commit 跑;**跳过 frontmatter**(`---` 到下一个 `---` 之间不数)只数 body 行,否则 frontmatter 15-20 行会吃掉 effective body cap 变 80 行
- **超 cap 行为**: build 失败(`go generate` 报错),不允许静默截断 — memory `chunk-oversize-anchor-text-split` 教训:静默截断比 build 失败更危险

### Skill 触发并发 cap(token budget,不是 count)

业界共识(2026-05-29 调研):**用 token budget 不用 count cap**。Anthropic Claude Code 25K-token re-attached budget,OpenAI Codex Skills 8K-char catalog cap,均不限 N 而限总量;OpenAI Swarm + Copilot Studio 走 N=1 sequential handoff,极保守。

compshare-agent 采用三件套:

| 限制 | 值 | 来源 |
|---|---|---|
| 默认触发 skill 数 N | **2** | 经验值;memory `priortext-avalanche-invalidates-planner` 雪崩防护;典型组合 `safety_warning + domain_skill` |
| 硬上限 N | **3** | 极端情况 `safety + citation + domain` 三件套,超过即 planner schema 校验 reject |
| 并行 token budget cap | **~2K tokens**(包括所有触发 skill body 总和) | 业界 escape hatch;先到先得,后续 skill body 被拒,planner 收到 warning |

**触发顺序**: planner emit 的 `skills` 数组按 priority 排序,顺序 fetch body + 累加 token,超 budget 即停止 + 报 warning。**不做 Claude Code 风格的 most-recent eviction**,因为我们的 turn 短,简化为 first-fit。

### Codegen 消除双源数据

```go
//go:generate go run ./cmd/skillgen --root internal/skills --out internal/skills/registry_gen.go

// 产物 registry_gen.go:
var generatedSkills = map[string]*Skill{
    "deploy_model": {Name: "deploy_model", Path: "internal/skills/deploy_model/skill.md", ...},
    ...
}
```

`go generate ./...` 在 `make build` 钩子里跑,提交时检查 `registry_gen.go` 跟 skill 目录同步(`git status` 不能脏)。

### 跨 tier 复用例

具体清单见 ADR-003 表;复用机制:同一个 `safety_warning` skill 在 fast/agent 两 tier 的 system prompt 里都引用其 metadata + 触发后 fetch body。无重复维护。

## Consequences

**Positive**
- Planner system prompt 从全量灌入 5.9k 降到 ~2-3k(只入 metadata),memory `priortext-avalanche-invalidates-planner` 的雪崩根因部分缓解
- Skill 内容跟 Go 代码完全解耦,加 skill 无需编译
- Codegen 消除 `capability_registry.go` 双源数据漂移
- Bundle 内可放 examples / runbook,长内容不污染 prompt

**Negative**
- 现存 6 个 active capability 拆为目录(`gpu_specs_query/` 等 6 个)+ frontmatter schema 重写
- 新增 `internal/skills/loader.go` ~300 行 + `cmd/skillgen/` ~150 行 codegen 工具
- `go generate` 进入开发流程,新人需要文档

**Risks**
- **触发多 skill 时 body cap 总和超 prompt 预算**: 见上方 "Skill 触发并发 cap" 三件套(N=2 默认 / N=3 硬上限 / 2K token budget),业界共识 token-budget-first
- **Lazy load + concurrency**: `sync.Once` 处理初次 load 竞态;但 hot reload 不支持(改 skill.md 需重启)— 接受,因为生产不需要 hot reload
- **Codegen 增加构建复杂度**: 真实工程债;提供 `make skills` 单独命令 + CI 验证

## 业界对照

| 项目 | Skill bundle 形态 | Progressive disclosure |
|---|---|---|
| Claude Code | SKILL.md + 同目录资源 | ✅ metadata first + body on trigger |
| Anthropic Agent Skills SDK | Same | ✅ |
| OpenAI Assistants | instructions(单字符串扁平) | ❌ 全量 |
| Bedrock Agent | instruction(单字符串扁平) | ❌ 全量 |
| HolmesGPT | toolset.yaml + runbook.md | ⚠️ 部分(toolset 全量,runbook 触发后拉) |
| **本项目** | skill.md + examples.jsonl + runbook.md | ✅ |

最接近:Claude Code。

## Acceptance

- [ ] `internal/skills/` 目录建立,6 个 active capability 全部迁移为 `<name>/skill.md` 形式
- [ ] `internal/skills/loader.go` 实现 lazy `Body()` + `sync.Once` 并发安全 + 单测覆盖
- [ ] `cmd/skillgen/` codegen 工具上线,`go generate ./...` 产 `internal/skills/registry_gen.go`
- [ ] `scripts/check_skill_caps.sh` 加入 pre-commit
- [ ] 至少 1 个跨 tier 复用 skill 投产(`safety_warning`,同时被 fast 的 mutating workflow + agent 的 deploy_model 引用)
- [ ] Planner system prompt 总 tokens 量级从 ~5.9k 降到 ≤3k(N=20 抽样验证)

## References

- ADR-001 / ADR-003: 前置依赖
- memory `priortext-avalanche-invalidates-planner`: prompt 雪崩根因
- memory `chunk-oversize-anchor-text-split`: 静默截断教训(本 ADR 选择 build fail 不截断)
- Claude Code Skill docs: https://docs.claude.com/agent-skills(对照参考,不抄 SDK)
