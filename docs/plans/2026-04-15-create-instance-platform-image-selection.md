# CreateInstance Platform Image Selection Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 修正 `CreateInstanceWorkflow` 的平台镜像选择逻辑，让平台主链路默认覆盖 `System + App`，并避免在返回全集后盲选第一张镜像。

**Architecture:** 保持 BlockA 的产品边界不变，只修平台公共镜像路径，不扩到共享镜像/私有镜像。workflow 平台分支改为默认不传 `ImageType`，允许按 `ImageName` 缩小结果集；同时把平台镜像 helper 从“取第一条”改成“优先匹配用户指定名称，否则退回第一条”，避免去掉过滤后行为变得更随机。

**Tech Stack:** Go, workflow engine, testify, current mock executor tests

---

### Task 1: 为平台镜像路径补失败测试

**Files:**
- Modify: `F:\compshare-agent\internal\workflow\create_instance_test.go`

**Step 1: Write the failing tests**

新增以下测试：

- `TestCreateInstance_PlatformImage_DefaultQueryDoesNotForceSystem`
  - 断言 `DescribeCompShareImages` 调用参数里 **不包含** `ImageType`
- `TestCreateInstance_PlatformImage_WithImageName_UsesNameFilter`
  - 传入 `ImageName: "PyTorch"`，断言 `DescribeCompShareImages` 参数里带 `Name=PyTorch`
- `TestPickPlatformImageId_PrefersNameMatch`
  - 构造 `ImageSet` 同时包含 `Ubuntu 22.04` 和 `PyTorch 2.1`
  - 断言当 `ImageName=PyTorch` 时，helper 选中 `PyTorch 2.1` 对应 ID

**Step 2: Run tests to verify they fail**

Run:

```powershell
go test ./internal/workflow/... -run "TestCreateInstance_PlatformImage_|TestPickPlatformImageId_PrefersNameMatch" -v
```

Expected:
- 现有实现下，平台镜像分支仍会发送 `ImageType=System`
- helper 仍会拿第一条记录

**Step 3: Keep existing coverage intact**

保留现有：
- `TestCreateInstance_HappyPath`
- `TestCreateInstance_Defaults`
- 社区镜像路径相关测试

不要删除社区镜像和价格口径相关断言。

**Step 4: Re-run the full workflow package after implementation**

Run:

```powershell
go test ./internal/workflow/... -v
```

Expected:
- workflow 包全绿

**Step 5: Commit**

```bash
git add internal/workflow/create_instance_test.go
git commit -m "test: cover platform image selection in create workflow"
```

---

### Task 2: 修正平台镜像查询参数

**Files:**
- Modify: `F:\compshare-agent\internal\workflow\create_instance.go:82-103`

**Step 1: Write the minimal implementation**

修改 `stepQueryImages()` 平台分支：

- 当前行为：
  - 平台路径固定返回：
    ```go
    map[string]any{
        "ImageType": "System",
    }
    ```
- 目标行为：
  - 默认不传 `ImageType`
  - 若 `ImageName` 存在，则传 `Name`
  - 可保留 `Limit`

建议实现：

```go
args := map[string]any{
    "Limit": 20,
}
if name := paramStr(wfCtx.Params, "ImageName", ""); name != "" {
    args["Name"] = name
}
return args, nil
```

**Step 2: Keep community path unchanged**

社区镜像分支仍然：
- 走 `DescribeCommunityImages`
- 继续要求 `ImageName`
- 继续使用 `FuzzySearch`

不要在本任务里扩到 `DescribeCompShareSharingImages` 或 `DescribeCompShareCustomImages`。

**Step 3: Run targeted tests**

Run:

```powershell
go test ./internal/workflow/... -run "TestCreateInstance_PlatformImage_|TestCreateInstance_CommunityImage_" -v
```

Expected:
- 平台路径不再强制 `ImageType=System`
- 社区路径不回归

**Step 4: Commit**

```bash
git add internal/workflow/create_instance.go
git commit -m "fix: stop forcing system images in create workflow"
```

---

### Task 3: 把平台镜像 helper 从“取第一条”改成“优先名称匹配”

**Files:**
- Modify: `F:\compshare-agent\internal\workflow\create_instance.go:287-339`
- Test: `F:\compshare-agent\internal\workflow\create_instance_test.go`

**Step 1: Refactor helper signatures**

把平台 helper 改成接收 `params`，这样能读取 `ImageName`：

```go
func pickPlatformImageId(params map[string]any, result map[string]any) string
func pickPlatformImageName(params map[string]any, result map[string]any) string
```

然后把：

```go
return pickPlatformImageId(params, result)
return pickPlatformImageName(params, result)
```

替换成新 helper。

同时显式修改两个 dispatch 函数：

- `pickImageId(params, result)` 平台分支透传 `params`
- `pickImageName(params, result)` 平台分支透传 `params`

这是机械改动，但必须在此任务里一并完成，避免 helper 签名变更后漏改调用点。

**Step 2: Implement deterministic selection**

实现顺序：

1. 若 `ImageName` 为空：
   - 退回第一条结果，保持当前行为兼容
2. 若 `ImageName` 非空：
   - 先做大小写不敏感的精确匹配
   - 再做 `strings.Contains` 模糊匹配
   - 若仍找不到，再退回第一条

建议只匹配 `Name` 字段，不引入更复杂的标签/作者排序。

**Step 3: Add focused tests**

新增：
- 精确匹配命中
- contains 匹配命中
- 无匹配时回退第一条
- 空结果时返回空字符串 / `"未知"`

**Step 4: Run tests**

Run:

```powershell
go test ./internal/workflow/... -run "TestPickPlatformImage|TestCreateInstance_PlatformImage_" -v
```

Expected:
- 新 helper 行为稳定
- 现有创建流程测试继续通过

**Step 5: Commit**

```bash
git add internal/workflow/create_instance.go internal/workflow/create_instance_test.go
git commit -m "fix: prefer named platform images in create workflow"
```

---

### Task 4: 更新工具描述，避免 LLM 继续把 `ImageName` 视为社区专属参数

**Files:**
- Modify: `F:\compshare-agent\internal\tools\registry.go:367-400`
- Modify: `F:\compshare-agent\internal\tools\registry.go:187-210`

**Step 1: Update `CreateInstanceWorkflow` description**

当前描述容易让模型理解为：
- `ImageName` 只在社区镜像路径有效
- 平台镜像默认就是“某张系统镜像”

修改为：
- 平台镜像默认从公共平台镜像中选择，覆盖 `System + App`
- `ImageName` 可用于平台镜像和社区镜像
- 共享/私有镜像仍不在 BlockA 创建主链路

建议描述要点：

```text
支持平台镜像和社区镜像。
平台镜像默认查询公共平台镜像（包含系统镜像和应用镜像）。
传 ImageName 可按镜像名称缩小范围。
传 ImageSource='community' 使用社区镜像创建。
不支持自制/私有镜像。
```

**Step 2: Update parameter description**

把 `ImageName` 参数描述从：
- `仅 ImageSource=community 时有效`

改为：
- `镜像名称关键词。平台镜像路径按 Name 精确/模糊缩小结果；社区镜像路径用于 FuzzySearch。`

**Step 3: Expose `Name` in `DescribeCompShareImages` tool definition**

当前 workflow 直接调 executor 时可以传 `Name`，但 LLM 直调 `DescribeCompShareImages` 仍会被 schema 限制。

在 `DescribeCompShareImages` 的 tool definition 中新增：

```go
"Name": map[string]any{
    "type":        "string",
    "description": "按镜像名称筛选，如 PyTorch / Ubuntu / CUDA",
}
```

必要时也可同步补 `Author` / `Tag`，但本任务最低要求是补 `Name`，保证 workflow 路径和 LLM 直调路径口径一致。

**Step 4: Run package tests**

Run:

```powershell
go test ./internal/tools/... ./internal/engine/... -v
```

Expected:
- tools 注册相关测试和 engine 注册测试全绿

**Step 5: Commit**

```bash
git add internal/tools/registry.go
git commit -m "docs: clarify platform image selection for create workflow"
```

---

### Task 5: 补一条 builder 路由说明，降低 LLM 盲选平台镜像概率

**Files:**
- Modify: `F:\compshare-agent\internal\prompt\builder.go`
- Test: `F:\compshare-agent\internal\prompt\builder_test.go`（如已有相关断言）

**Step 1: Add one routing note**

在 `complex_task` 的创建实例规则附近追加一句：

```text
- 用户提到 Ubuntu/Windows/裸系统/干净环境 → 平台镜像优先选择系统镜像
- 用户提到 PyTorch/CUDA/ComfyUI/Ollama/vLLM/框架环境 → 平台镜像优先选择应用镜像，并尽量带上对应 ImageName
```

目标不是做复杂 prompt engineering，而是让模型在调用 workflow 时更愿意带 `ImageName`。

**Step 2: Keep scope small**

不要在这一任务里：
- 新增平台镜像专用枚举参数
- 新增共享/私有镜像能力
- 重写整段 prompt

**Step 3: Run tests**

Run:

```powershell
go test ./internal/prompt/... -v
```

Expected:
- prompt 包测试全绿

**Step 4: Commit**

```bash
git add internal/prompt/builder.go internal/prompt/builder_test.go
git commit -m "prompt: guide platform image choice in create workflow"
```

---

### Task 6: 做回归验证，确认没有把社区镜像路径打坏

**Files:**
- Test: `F:\compshare-agent\internal\workflow\create_instance_test.go`
- Test: `F:\compshare-agent\internal\engine\scenario_test.go`

**Step 1: Run workflow tests**

```powershell
go test ./internal/workflow/... -v
```

**Step 2: Run engine regression**

```powershell
go test ./internal/engine/... -v
```

**Step 3: Focus on affected scenarios**

重点确认：
- 平台镜像创建
- 平台镜像 + `ImageName=PyTorch` 命中 `App` 镜像
- 社区镜像创建
- Create workflow 确认卡片中的 image / price 字段
- 不指定 `ImageName` 时仍可成功创建

建议额外补一条端到端场景：

- `internal/engine/scenario_test.go`
  - 用户输入类似 `"帮我开一台 4090 跑 PyTorch"`
  - 期望触发 `CreateInstanceWorkflow`
  - 期望 `DescribeCompShareImages` 收到 `Name=PyTorch`
  - 期望最终确认卡片中的 `image` 为 `PyTorch` 类镜像，而不是裸 Ubuntu

**Step 4: Final verification**

```powershell
go test ./internal/workflow/... ./internal/tools/... ./internal/prompt/... ./internal/engine/... -v
```

Expected:
- 相关包全绿

**Step 5: Commit**

```bash
git add internal/workflow/create_instance.go internal/workflow/create_instance_test.go internal/tools/registry.go internal/prompt/builder.go internal/prompt/builder_test.go internal/engine/scenario_test.go
git commit -m "fix: broaden platform image selection in create workflow"
```

---

### Non-Goals

本计划明确不做：

- `DescribeCompShareSharingImages` 接入创建主链路
- `DescribeCompShareCustomImages` 接入创建主链路
- 控制台 page-aware 注入
- 更复杂的平台镜像排序器（按标签、作者、热度、GPU 兼容性排序）
- BlockB 的前端接入和认证态 QA

---

Plan complete and saved to `docs/plans/2026-04-15-create-instance-platform-image-selection.md`. Two execution options:

1. Subagent-Driven (this session) - I dispatch fresh subagent per task, review between tasks, fast iteration
2. Parallel Session (separate) - Open new session with executing-plans, batch execution with checkpoints

Which approach?
