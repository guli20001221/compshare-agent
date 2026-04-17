# Pre-existing Instance Baseline

Captured from control panel instance list before any shadow-QA writes.

Do not modify:

| Name | ID | State | Type | Notes |
| --- | --- | --- | --- | --- |
| `wyp-test-no-delete` | `uhost-1pabqmq2xn2a` | `初始化失败` | `容器 / 3080Ti x1` | Existing account instance; never touch |
| `test-3090` | `uhost-1pa8zsdq560o` | `运行中` | `虚机 / 3090 x1` | Existing account instance; never touch |
| `内网ping勿删` | `uhost-1ozccvmyd3ia` | `无卡模式 / 运行中` | `虚机 / 无卡模式` | Existing account instance; never touch |
| `内网ping勿删除` | `uhost-1oz238rtbtim` | `无卡模式 / 运行中` | `虚机 / 无卡模式` | Existing account instance; never touch |

Allowlist rule for this QA run:
- Only instances created during this run and named with prefix `qa-shadow-20260416-` may receive write actions.
- Any instance not matching the prefix or not explicitly recorded in `qa_allowlist.md` is read-only.
