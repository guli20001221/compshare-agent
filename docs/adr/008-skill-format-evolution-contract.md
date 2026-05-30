# ADR-008: Skill 格式与进化契约

**Status**: Proposed (2026-05-30)
**Depends on**: ADR-003(skill ⊥ tool)、ADR-004(bundle + progressive disclosure)
**Relates to**: B2b spec `docs/plans/2026-05-29-b2b-skill-tool-dir-codegen.md` §3/§12;roadmap B6–B9

> **编号说明**:ADR-003 §C(`003:198,208`)与 ADR-005 §8(`005:195,240`)曾把 "ADR-008"
> 写作各自后续议题(MCP 迁出 / 诊断阈值)的**暂定**槽位。这两处都是软前向引用,均无落盘
> 文件。本 ADR 占用 008 用于 skill 格式与进化契约(lead 2026-05-30 显式指定),并把那两处
> de-pin 为 "后续独立 ADR(编号落盘时分配)" —— 给未出生的 ADR 预订编号正是这次撞号的成因,
> 不再重犯。两处文字同步改成通用措辞(本 PR 一并改)。

## Context

ADR-003/004 把 playbook 拆为独立的 Skill 维度,定了目录布局、frontmatter schema、
progressive disclosure 加载、body cap。本 ADR 在其上补两件 ADR-003/004 未锁的事:

1. **格式对齐 Anthropic 开放 Skill 标准**(2025-12 发布,`agentskills.io/specification`,33+ Agent 采纳)
   —— 我们的 `internal/intent/capabilities/*.md` 已 ~80% 是 skill catalog,B2b 是对齐不是重写,
   需要把"对齐到什么形"写成可 review 的契约,而不是 B2b 实现里临时拍。
2. **skill 自进化**(skill 在使用中变完善)—— lead 的明确目标。这要求 frontmatter **现在就预留**
   进化所需 metadata(否则 B4b/B6 之后再加字段又是一次 `KnownFields(true)` 硬失败重写,
   B2b §11),但**进化 loop 本身不在 B2b**(B2b 只读、只预留字段)。

三份输入(2026-05-30 已读)支撑本 ADR 的取舍:

- **Anthropic Skill 标准(aliyun 文章)**:`SKILL.md` = YAML frontmatter + MD body;`description`
  **只写触发条件不写工作流程**(写了流程,模型会照 description 执行而跳过 body);L1/L2/L3 渐进加载
  = 我们的 progressive disclosure;model-driven activation,无 embedding。
- **Survey 2605.07358**(CUHK-SZ):skill 生命周期 表示/获取/检索/**进化**;进化 = revision →
  **held-out validation** → governance,**held-out 校验是"进化"区别于"乱改"的定义性 gate**(§VI.A)。
  三个风险:**asymmetric revision**(只 add 不 retire/merge,§VII.C)、**PoisonedSkills**
  供应链(self-written body 是可执行指令)、**confounded gains**(涨分可能来自更强 judge 而非更好
  artifact);路由 **body-route vs description-route 是真实分歧,非定论**(§VI.E)。
- **MUSE 2605.27366**(ByteDance):training-free;`SKILL.md`+`scripts/`+`tests/`,**unit-test 不过
  则拒绝注册**;自生成 skill 在 covered task 上 87.94% > 人写 68.40%,~3 次复用回本。但有**回退**实例:
  hvac skill 80%→20%,因为**把单次运行的常数烤进了 skill**。

## Decision

五条决策 A–E。A/C 是格式;B/D/E 是进化契约。**B2b 落地范围 = A + C + B 的字段预留**;
B 的消费者、D 的 loop、E 的执行都在 agent-tier(B6 之后),B2b 不实现。

### A. 采用 Anthropic `SKILL.md` frontmatter 形

frontmatter 用 `name` / `description` / 可选 `metadata{}` 的 Anthropic 形,body 是 markdown。
我们的 snake_case `name`、body cap 100 行(严于官方 500 行)等**有意偏离**已在 ADR-004 §name-字符集 /
§Body-Cap 记录,本 ADR 不复述,只确认"对齐 Anthropic 形 + 文档化偏离"这一条总纲。B2b §3 的统一
superset schema 是这条的落地产物。

### B. 现在预留进化 metadata —— 并定义空值语义

在 ADR-004 已有的 `verification_status` / `field_refs_verified` twin 之上,**reserve 四个**进化字段
(provenance 拆为 `provenance` + `provenance_trace_ref` 两个字段;复用 verification_status 当
validation-status,不再平行加一个):

```yaml
provenance: human_authored            # | distilled_from_trajectory
provenance_trace_ref: ""              # 仅 distilled 时填;必须是 opaque trace id,不内联账号数据
skill_version: 1                      # loop 每次 revision +1
last_validated_against: ""            # 通过校验时所对的 eval 快照 digest(anchors-to-validated-artifact)
```

**空值语义是这条决策的核心**(否则就是 half-wired 的"会撒谎的字段"):

| 字段 | 缺省/空 = | B2b 消费者 |
|---|---|---|
| `provenance` | `human_authored`(人写基线;6 个迁移 capability + 5 个 diagnose_* 全是) | **无** |
| `provenance_trace_ref` | `""`(非 distilled,无来源轨迹) | **无** |
| `skill_version` | `1`(基线版本) | **无** |
| `last_validated_against` | `""`(loop 从未校验过;caution 仍由 `verification_status` 驱动) | **无** |

**B2b 里这三个字段是纯 forward-declared schema,loader 零分支**:caution 行(ADR-004 `004:81,104`)
**只**读 `verification_status` / `field_refs_verified`,**不**读这三个新字段。要有一个测试断言 loader 的
caution 逻辑不消费进化字段 —— 这样字段不"撒谎"(它的 B2b 语义就是"预留、暂无消费者"),也避免
`half-wired-schema-field-grep-whole-chain` 陷阱。

这与 B2b §12.1(`destructive`= future-MCP mirror,defer 消费者到 B7)、§12.2(`idempotent`=
reviewer-checklist-only,defer 校验到 B7)**同一哲学**:schema 稳定性需要现在就声明字段,但 enforcing
consumer 只在有真实 loop/client 消费时才建。B 与 idempotent/destructive defer 自洽,不矛盾。

### C. `description` 只写触发条件,不写工作流程

(Anthropic 标准的硬约束:description 写了流程,model 会照 description 执行而**跳过 body**。)

- **范围 = skill 的 `description` 字段本身**。它**不**触碰、**不**收窄 frontmatter routing block 里的
  `planner_directives` / `planner_examples`(`planner.go` 的 Stage-2C 分类锚点)—— 那些是 planner 的
  路由信号,该详写就详写,B2b §3 明确把它们保留在 routing block。C 只管"`description` 这一行别写成
  workflow"。
- **这是一个 scale 取舍,不是永久结论**。Anthropic 标准 + 我们当前都走 **description-route**(catalog
  里 name+description 路由,body 触发后才拉);survey §VI.E 指出 **body-route**(用 body 内容做更细路由)
  在 skill 规模上去后可能更优,这是真实分歧。约定:**skill 数到 ~100 量级时重新评估 body-aware routing**,
  届时 C 可能被 amend。现在小规模(6+5),description-route 足够且更省 prompt。

### D. 自进化 loop 在 agent 层;loader 永远只读;校验 = held-out gate;接受前 sanitize

- **位置**:进化 loop(revision → validation → governance)在 **agent-tier(B6 之后)**。`internal/skills`
  的 **loader 永远 read-only + digest-pin**(ADR-004 `004:179`),它不写 skill。loop 是独立组件,产出
  候选 skill,经 gate 后由人/CI 落盘 —— loader 仍然只读那份落盘结果。
- **校验 gate = held-out validation,这是"进化"的定义性闸**(survey §VI.A)。具体复用 **B5 诊断 verifier**
  / eval 快照:候选 skill 必须在 held-out task 集上**不劣于**现行 skill 才注册(MUSE 的 unit-test-gate 同
  形)。**confounded-gains 防护**:校验 judge 必须独立于生成 loop 用的 model,且 gate 用 anchor + 分布,
  不用单一均值(memory `llm-acceptance-gate-anchors-not-averages`)。
- **接受候选前必须 sanitize 硬编码标识符**(`project_id` / `instance_id` / `RetCode` / `region` / 具体配额
  数字)。这是 MUSE hvac 80%→20% 回退的直接对策:把单次运行的常数烤进 skill = 把偶发当通则。**本项目
  已有现成的反例**:#3a 的 V100S `RetCode=230 organization_id not available` 是 CLI-缺-project_id 的
  环境产物,绝不能被蒸馏进任何 stock/capacity skill 当"事实"。sanitize 用单一 choke-point 函数,覆盖整条
  派生链(memory `sanitization-covers-all-derived-fields`)。
- **PoisonedSkills 供应链防护**:distilled skill body 是可执行指令,落盘走 digest-pin + reviewer sign-off
  (人 in the loop),不允许 loop 自动 commit;`provenance: distilled_from_trajectory` 的 skill 在升到
  `production_validated` 前一律带 ADR-004 的 unverified caution 行。

### E. v1 写 retire/merge 判据 + 预留设计槽;执行随 loop 落地

asymmetric revision(只 add 不 retire,survey §VII.C)是自进化系统的已知病。本 ADR(设计层)**写下判据**;
**B2b 只预留字段/槽,judge+执行在 loop 落地的那一批(B6 之后)**:

- **Retire**:某 skill 的 `verification_status` 连续 K 个 eval 快照低于阈值,或与另一 skill 的 trigger 语义
  重叠超阈值且 win-rate 更低 → 退役 loser;distilled skill 连续 N 次 revision 都过不了 held-out gate →
  冻结。
- **Merge**:两 skill 的 description/triggers 语义近重复且 `required_tools` 重叠 → 提 merge 候选,**必须
  reviewer sign-off 后才落**(governance gate)。
- **Prune**:distilled skill 连续 M turn 未被检索/命中 → 降级出 catalog(不再注入 metadata),**不硬删**
  (留审计)。

阈值(K/N/M/重叠线)**不预设**,随 loop 落地时用真实数据定(与 ADR-005 §8 "阈值不预设"同纪律)。
B2b 阶段产出 = 上面这组判据写进本 ADR + frontmatter 里 `skill_version`/`last_validated_against` 字段就位,
够 loop 来消费即可。

## Consequences

**Positive**
- frontmatter 一次性把进化字段就位,B6 之后加 loop 不再触发 `KnownFields(true)` 重写(B2b §11 风险消除)。
- 进化的三大风险(asymmetric / poisoned / confounded)各有对策(E / D-sanitize+pin / D-held-out),不是把
  "自进化"当 buzzword 接上。
- C + description-route 维持小 prompt;B5 verifier 复用为进化 gate,不另造校验栈。

**Negative**
- frontmatter 多 4 个 reserved 字段,B2b 的 superset struct + 文档要带上(纯 schema,无逻辑)。
- 进化 loop 是一整批 agent-tier 工作(roadmap 新增 B9),不是"开个开关"。

**Risks**
- **reserved 字段被误当已生效**:正是 lead 点出的 half-wired 风险。对策 = 决策 B 的空值语义表 + loader
  零分支测试;字段文档显式标"B2b 无消费者"。
- **held-out gate 自己被绕**:若 loop 用同一批 task 既生成又校验,gate 失效。对策 = gate 数据集与生成
  trajectory 不相交,且 judge model 与生成 model 不同源(memory `hard-gate-fail-needs-independent-reviewer`)。
- **description-route 在规模上去后变瓶颈**:C 不是永久结论,~100 skill 时重评 body-route(§C)。

## 业界对照

| 项目 | 格式 | 自进化 | held-out gate | retire/merge |
|---|---|---|---|---|
| Anthropic Agent Skills | `SKILL.md` frontmatter + body | skill-creator(ML 式 eval loop) | 有(eval) | 未公开强调 |
| MUSE (ByteDance) | `SKILL.md`+`scripts/`+`tests/` | 5 阶段 training-free | **unit-test 不过则拒注册** | 部分(coverage 蒸馏) |
| Survey 2605.07358 | `S=(M,R,C)` | revision→validation→governance | **定义性 gate(§VI.A)** | 列为缺口(§VII.C) |
| **本项目** | ADR-003/004 bundle + 进化字段预留 | loop 在 agent-tier(B9) | B5 verifier / eval 快照 | 判据本 ADR,执行随 loop |

最接近:MUSE(test-gate + 拒绝注册)+ survey 的生命周期框架;格式对齐 Anthropic 标准。

## Acceptance

> B2b 范围(本 ADR 在 B2b 阶段的可验收产出):
- [ ] B2b §3 superset frontmatter 含 `provenance` / `provenance_trace_ref` / `skill_version` /
      `last_validated_against` 四个 reserved 字段,默认值即决策 B 空值语义表。
- [ ] loader caution 逻辑**只**读 `verification_status` / `field_refs_verified`;一个测试断言它**不**消费
      四个进化字段(half-wired 防护)。
- [ ] 6 个迁移 capability + 5 个 diagnose_* 的 frontmatter 显式填 `provenance: human_authored`(无默认,
      CI 存在性检查,沿用 ADR-004 `004:88` 纪律)。
- [ ] 所有 skill 的 `description` 通过一条 lint:不含 workflow 动词序列(决策 C;reviewer-checklist 起步)。

> 进化 loop 范围(B6 之后,roadmap B9,本 ADR 只立判据,不在 B2b 验收):
- [ ] held-out validation gate 接入 B5 verifier;候选 skill 不劣于现行才注册。
- [ ] 接受候选前的标识符 sanitize choke-point(覆盖 project_id/instance_id/RetCode/region/配额数)。
- [ ] retire/merge/prune 判据的阈值用真实数据定 + reviewer sign-off 落 merge。

## References

- ADR-003(skill ⊥ tool + Amendment-1)、ADR-004(bundle + progressive disclosure + verification_status twin)、ADR-005(5 个 diagnose_* provenance + 阈值不预设纪律)
- B2b spec `docs/plans/2026-05-29-b2b-skill-tool-dir-codegen.md` §3(superset schema)、§12(gate CLOSED)、§12.1/§12.2(destructive/idempotent defer 同源哲学)
- Roadmap `docs/plans/roadmap.md`(B9 = skill self-evolution loop,本 ADR 新增)
- Anthropic 开放 Skill 标准 `agentskills.io/specification`(对照参考,不抄 SDK)
- Survey 2605.07358(skill 生命周期 + 进化 gate + 三风险)、MUSE 2605.27366(test-gate + hvac 回退实例)
- Memory:`unverified-skill-fab-risk-twin-pattern`、`half-wired-schema-field-grep-whole-chain`、`sanitization-covers-all-derived-fields`、`llm-acceptance-gate-anchors-not-averages`、`hard-gate-fail-needs-independent-reviewer`、`constraints-anchor-to-validated-artifact`、`planner-hint-advisory-only`
