# Real Account Shadow QA - Create

**Summary**: 0/1 PASS

| Case | Result | Notes |
|------|:------:|-------|
| shadow_01_create_test_instance | FAIL | [step 1] expected one of ['CreateInstanceWorkflow'], got []; [step 1] first tool = (none), want CreateInstanceWorkflow |

## shadow_01_create_test_instance

**Input**: `帮我开一台 4090，实例名 qa-shadow-20260416-01，平台 Ubuntu 系统镜像，按量`

**Result**: FAIL

- [step 1] expected one of ['CreateInstanceWorkflow'], got []
- [step 1] first tool = (none), want CreateInstanceWorkflow

```text

Assistant> 您即将创建以下配置的算力共享实例：
- GPU类型：4090
- 实例名称：qa-shadow-20260416-01
- 计费方式：按量（Dynamic）
- 镜像：平台Ubuntu系统镜像
- 其他配置：CPU/内存使用平台默认值，可用区默认cn-wlcb-01

请确认是否执行创建操作？

You>
```
