# Upstream API capability ledger (JIT — read-only-safe subset)

**Purpose:** decide which upstream CompShare APIs become read-only `tool → fast skill`s
next, and which mutating ones go through the saga + confirm-gate write-workflow phase.
**Scope:** deliberately NOT exhaustive (JIT). This batch is the read-only-safe expansion
candidates that map to real user needs (`聊天记录.md`) + high-demand mutating seeds. Source docs:
`F:\uhost-compshare-api-master\docs\api`. Sweep: workflow `wf_74091051` (2026-06-02).

> Wired-status claims ("registered L0 internal") are from a grep of `internal/tools` /
> `internal/security/levels.go` during the sweep — **re-verify at implementation time**.

## Read-only-safe candidates

| Endpoint | Need it serves | Key params | Key returns | Wired status | Notes / gotchas |
|---|---|---|---|---|---|
| `CheckCompShareNetOptimizer` | "网速慢" → 网络加速开了吗 (real pain in 聊天记录.md) | `Region` (opt) | `Optimized:bool`, `Info[]{Region,Optimized}` | L0 internal, **not LLM-exposed** | Just needs LLM exposure + skill + eval. **Strongest first candidate** (maps to a documented user complaint). |
| `DescribeModelRepositoryModels` | 模型仓库有哪些模型 | `Region`; opt `name` (fuzzy), `tags` | `Models[]{Name,Path,Tag,Size,CreateTime}` | L0 in `levels.go:54`, **not a tool** | Clean. Pairs with the tags endpoint below. No paging. |
| `DescribeModelRepositoryTags` | 模型仓库有哪些分类 | `Region` | `Tags[]string` | not registered | Flat tag list; the filter vocabulary for the models endpoint. |
| `DescribeCompShareImageTags` | 镜像有哪些分类/标签 | `Region` | `Tags[]`, `TagsMap{cat→tags}`, `TagIndex[]` | not registered | Hierarchical (框架→[PyTorch,…]); good for an image-catalog skill. |
| `DescribeCompShareSupportZone` | 平台有哪些地域/可用区 | `Region` | `ZoneInfo[]{Region,RegionId,Zone,ZoneId,Describe}` | L0 internal, **not LLM-exposed** | More of an internal sub-query than a standalone user need; low priority as a user-facing skill. |
| `GetCompShareImageCreateProgress` | 我的自制镜像做好了吗 | `CompShareImageId`,`Region`,`Zone` | `Process`,`RemainingDuration`,`TotalDuration` | not registered | Poll-friendly, but needs an image-creation context (mutating-adjacent). Later. |
| `DescribeTeamMemberOrder` / `…UnpaidOrder` | 账单/订单查询 ("扣费规则" in 聊天记录.md) | `TeamId`,`VirtualCompanyId`,`Region` (req) + filters + paging | `Total`, `OrderInfos[]{OrderNo,Amount,…}` | not registered | Read-only but **auth-heavy**: team admin + real-name, needs `TeamId`/`VirtualCompanyId` context plumbing. Medium priority. |
| `ListCompShareTeam` | 我建了哪些团队 | `Region` | `Teams[]{Id,Name,CompanyId,Description,MemberCount}` | not registered | Admin/creator-scoped; precursor for the team-order skills. |
| `DescribeCompShareMachineTypeFamilies` | 全部机型族规格 | `Region` | `MachineTypes[]`, `MachineTypesMap{zone→[]}` | not registered | **Superseded** — data-heavy/slow; the already-wired `DescribeAvailableCompShareInstanceTypes` covers the available subset. **Skip** unless full-family browse is needed. |
| `GetSoftwareURL` | 实例上某软件的访问地址 | `Region`,`UHostId`,`Software` (req) | `URL` | not registered | ⚠️ **Doc says functional, but our real smoke found it end-to-end dead** (memory `reference_tool_claim_verification_2026_06_01`). **Verify-before-build**; do not pick first. |

**Disk list/status: no read API.** The disk dir is entirely Attach/Create/Delete/Resize
(mutating). Disk state is read from `DescribeCompShareInstance.DiskSet[]` (already wired) —
a "我的磁盘" skill needs **no new tool**, just a render over the existing instance call.

**Parked / removed from this console-agent route:** `GetOpenClawModelList` is
OpenClaw / ModelVerse-specific. It is simple, but demand is narrow and it should not
consume an early console-agent expansion slot unless a concrete OpenClaw workflow
resurfaces.

## Mutating seeds → write-workflow phase (saga + confirm gate, never Markdown)

| Endpoint | Class | Confirm | Notes |
|---|---|---|---|
| `AttachCompshareDisk` | mutating | **yes** | Mount an existing disk. Only `CreateAndAttachCompshareDisk` is wired (L1). Plain attach not yet. CLOUD_RSSD only on A800. **High demand; disk attach is still the proposed first write-workflow template** (reversible via detach, lower blast radius than delete/reinstall). |
| `CreateCompShareCustomImage` | mutating | **yes** | Create a custom image from an existing instance. **High demand** for "save/upload my environment as an image" workflows. Needs instance selection, image name, confirm gate, and progress follow-up through `GetCompShareImageCreateProgress`. |
| `PublishCompShareImage` | mutating | **yes** | Publishes a custom image to the community marketplace → review flow. **High demand but higher validation burden**: container images only; `VersionName` semver; ≤3 tags; price/cover/readme/ports need schema validation before execution. |

## First read-only tool→skill candidates (ranked)

1. **`CheckCompShareNetOptimizer`** — maps to a real, documented user complaint ("网速怎么这么慢" → "开下网络加速"); read-only; already L0-internal so it only needs a read-capability vertical. **Recommended first.**
2. **`DescribeModelRepositoryModels` + `DescribeModelRepositoryTags`** — model-repository browse; clean params; pairs naturally into one skill.
3. **`DescribeCompShareImageTags`** — image catalog/tag browse.

Each lands as one `ReadCapabilitySpec` vertical under `internal/capability`: the typed request
struct, its `schemaNode` field contract, its handler and renderer, an entry in `ReadDefinitions()`
(`read_catalog.go`), and **its own `read_<name>_test.go`** driving `NewReadCapability(...).Run`.
There is no separate route manifest or offline skill-eval case: P6 deleted the intent-route stack,
and the per-vertical test is the gate.

After the first read-only expansion, the higher-value branch is likely **write-workflow
templates**, not more narrow read-only browsing: disk attach/create first, then custom-image
create/progress and publish. `DescribeTeamMemberOrder*` stays later because it needs
team-context plumbing (`TeamId` / `VirtualCompanyId`, admin/real-name constraints).
