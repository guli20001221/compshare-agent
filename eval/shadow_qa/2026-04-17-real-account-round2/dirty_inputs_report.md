# Real Account Shadow QA Round2 - Dirty Inputs

**Summary**: 0/4 PASS

| Case | Result | Notes |
|------|:------:|-------|
| dirty_01_gpu_ref_stop | FAIL | [step 1] expected one of ['StopInstanceWorkflow'], got ['GetGPUSpecs']; [step 1] first tool = GetGPUSpecs, want StopInstanceWorkflow; [step 1] reply missing '??' |
| dirty_02_crashed_last_night_followup | FAIL | [step 1] reply missing any of ['?', '??', 'ID', 'uhost-']; [step 2] reply missing '???'; [step 2] reply missing '??' |
| dirty_03_correction_other_one | FAIL | [step 1] reply missing any of ['?', '??', 'ID', 'uhost-']; [step 2] reply missing '???'; [step 3] reply missing '???' |
| dirty_04_contextual_just_now_start | FAIL | [step 1] expected one of ['StopInstanceWorkflow'], got ['GetGPUSpecs']; [step 1] first tool = GetGPUSpecs, want StopInstanceWorkflow; [step 1] reply missing '??'; [step 2] expected one of ['StartInstanceWorkflow'], got []; [step 2] first tool = (none), want StartInstanceWorkflow; [step 2] reply missing any of ['??', '??', '??'] |

## dirty_01_gpu_ref_stop

**Input**: `?? 3080Ti ????`

**Result**: FAIL

- [step 1] expected one of ['StopInstanceWorkflow'], got ['GetGPUSpecs']
- [step 1] first tool = GetGPUSpecs, want StopInstanceWorkflow
- [step 1] reply missing '??'

```text
  🔧 调用 GetGPUSpecs ...
  ✅ GetGPUSpecs 查询成功

Assistant> 以下是 RTX 3080Ti 的详细规格参数：

| 参数 | 数值 |
|------|------|
| 显存 | 12 GB |
| FP16 算力 | 34.1 TFLOPS |
| 最大支持 GPU 数量 | 8 卡 |
| 最大 CPU 配置 | 12 核 |
| 最大内存配置 | 125 GB |
| 架构 | Ampere |
| 支持抢占式 | 是 |

**最佳适用场景：**
- 模型推理部署
- Stable Diffusion / ComfyUI 绘图
- 轻量模型微调

这款 GPU 性价比高，适合中小型 AI 任务和日常开发使用。需要帮你创建一台 3080Ti 的实例吗？

You>
```

## dirty_02_crashed_last_night_followup

**Input**: `2-step conversation`

**Result**: FAIL

- [step 1] reply missing any of ['?', '??', 'ID', 'uhost-']
- [step 2] reply missing '???'
- [step 2] reply missing '??'

### Step 1: `???????`

**Step result**: FAIL
- reply missing any of ['?', '??', 'ID', 'uhost-']

```text

Assistant> 请问您具体有什么需求呢？例如查询实例信息、创建GPU实例、诊断实例故障、咨询GPU选型或了解平台使用规则等，可以详细描述一下您的问题。

You>
```

### Step 2: `?? wyptest ??`

**Step result**: FAIL
- reply missing '???'
- reply missing '??'

```text
  🔧 调用 DiagnoseInitFailure ...
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance [诊断 1/1] check_init_state: concluded

Assistant> 实例 **wyptest (uhost-1pampc1jb0mo)** 诊断结果：

**结论**：实例初始化失败，使用镜像为 `cuda130_torch291_py312`。可能原因包括：镜像异常、资源分配冲突、或平台临时问题。

**建议**：删除当前实例后重新创建，建议换用官方平台镜像。

You>
```

## dirty_03_correction_other_one

**Input**: `3-step conversation`

**Result**: FAIL

- [step 1] reply missing any of ['?', '??', 'ID', 'uhost-']
- [step 2] reply missing '???'
- [step 3] reply missing '???'

### Step 1: `???????`

**Step result**: FAIL
- reply missing any of ['?', '??', 'ID', 'uhost-']

```text

Assistant> 您好，我是优云算力共享平台的AI助手，请问您有什么需求吗？比如创建实例、查询GPU信息、诊断实例问题或咨询计费规则等，都可以告诉我。

You>
```

### Step 2: `?? wyptest ??`

**Step result**: FAIL
- reply missing '???'

```text
  🔧 调用 DiagnoseInitFailure ...
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance [诊断 1/1] check_init_state: concluded

Assistant> 查询结果：您的实例 **wyptest (uhost-1pampc1jb0mo)** 初始化失败，使用镜像为 `cuda130_torch291_py312`。

**可能原因**：镜像异常、资源分配冲突或平台临时问题。

**建议**：删除该实例后重新创建，换用官方平台镜像可解决此问题。

You>
```

### Step 3: `?????? uhost-1pan4ajz9whk ??`

**Step result**: FAIL
- reply missing '???'

```text
  🔧 调用 DiagnoseInitFailure ...
  🔧 调用 DescribeCompShareInstance ...
  ✅ DescribeCompShareInstance [诊断 1/1] check_init_state: concluded

Assistant> 查询结果：您的实例 **wyp-test (uhost-1pan4ajz9whk)** 同样初始化失败，使用镜像也是 `cuda130_torch291_py312`。

**问题原因**：该镜像存在异常，或存在资源分配冲突。

**解决建议**：删除此实例后，使用官方平台镜像重新创建即可。

You>
```

## dirty_04_contextual_just_now_start

**Input**: `2-step conversation`

**Result**: FAIL

- [step 1] expected one of ['StopInstanceWorkflow'], got ['GetGPUSpecs']
- [step 1] first tool = GetGPUSpecs, want StopInstanceWorkflow
- [step 1] reply missing '??'
- [step 2] expected one of ['StartInstanceWorkflow'], got []
- [step 2] first tool = (none), want StartInstanceWorkflow
- [step 2] reply missing any of ['??', '??', '??']

### Step 1: `?? 3080Ti ????`

**Step result**: FAIL
- expected one of ['StopInstanceWorkflow'], got ['GetGPUSpecs']
- first tool = GetGPUSpecs, want StopInstanceWorkflow
- reply missing '??'

```text
  🔧 调用 GetGPUSpecs ...
  ✅ GetGPUSpecs 查询成功

Assistant> 以下是RTX 3080 Ti 在优云算力共享平台的规格参数：

| 参数 | 详情 |
|------|------|
| 显存容量 | 12 GB |
| FP16 算力 | 34.1 TFLOPS |
| 单实例最大 GPU 数 | 8 |
| 支持最大 CPU | 12 核 |
| 支持最大内存 | 125 GB |
| 最佳适用场景 | 推理部署、SD/ComfyUI 绘图、轻量微调 |
| 支持抢占式计费 | 是 |
| 架构 | Ampere |

适合预算有限的入门学习、轻量模型推理或 Stable Diffusion 绘图使用。需要帮您推荐具体配置或创建实例吗？

You>
```

### Step 2: `?????????`

**Step result**: FAIL
- expected one of ['StartInstanceWorkflow'], got []
- first tool = (none), want StartInstanceWorkflow
- reply missing any of ['??', '??', '??']

```text

Assistant> 您能具体说明一下您的问题或需求吗？比如查询GPU规格、创建实例、诊断实例问题还是价格咨询等，我会帮您处理。

You>
```
