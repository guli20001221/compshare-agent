# 2026-04-17 Real Account Shadow QA Round 2

Target coverage:
1. 脏输入 / 口语化输入
2. 外部状态变化
3. 平台失败但 agent 要如实转述

Execution policy:
- 仅对本轮新建实例做写操作
- 只读验证可参考已有实例，但不做修改
- 真实账号 + CLI agent + 控制台 ground truth
