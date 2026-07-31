# 实例内排查与修复（SSH-ops）部署

平台 API 看不到实例内部。用户在授权卡上点同意后，助手 SSH 进他自己的实例执行命令定位问题，
再动手修复并验证，每条改动命令单独弹一次授权卡。删除数据、格式化、重启关机、改账号密码、
关 SSH/网络这类高危操作一律拒绝。审计 fail-closed：写不进审计表就不进用户机器。

## 生产配置

`deploy/conf/config.yaml` 已按 ModelVerse Anthropic 协议直连；部署时只需把 `harness_path`
换成该 checkout 的绝对路径：

```yaml
  ssh_ops:
    harness_path: "<部署目录>/deploy/ssh_ops_harness/harness.py"   # 绝对路径
    base_url: "https://api.modelverse.cn"
    api_key: ""                                                  # 空 = 复用 agent.llm.api_key
    python: "/opt/miniforge3/envs/py313/bin/python"              # 生产环境固定解释器
    model: "gpt-5.6-terra"
```

`harness_path` / `base_url` 留空，或 `api_key` 和 `agent.llm.api_key` 同时为空，**服务起不来**。
`python` 留空**不报错**，会悄悄回退到系统 `python3` —— 那上面没有 `claude_agent_sdk`，
症状是用户点完授权卡之后诊断失败。换了 checkout 目录，`harness_path` 要跟着改。

## 机器上要装的

```bash
/opt/miniforge3/envs/py313/bin/python -m pip install -r <部署目录>/deploy/ssh_ops_harness/requirements.txt
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 < deploy/migrations/0011_create_ssh_ops_audit.sql
```

还需要 **`claude` CLI**。harness 通过 `ANTHROPIC_BASE_URL=https://api.modelverse.cn` 和
`ANTHROPIC_AUTH_TOKEN` 直连 ModelVerse；不需要 claude-code-router。Token 只进入该次
Python/Claude CLI 子进程的最小环境，不会复用完整的服务进程环境。

> ⚠️ Python 依赖、`claude` CLI 或到 ModelVerse 的网络缺失不会在启动时报错，要到第一次真实诊断
> 才暴露，用户侧表现为这次排查失败。

## 确认通了

1. 启动日志里有 `ssh-ops enabled:` 开头的一行。没有就是没启用，同一位置会写明哪个前提没满足。
2. 在控制台对一台自己的实例问一句实例内才能回答的问题，出现授权卡 → 同意 → 命令逐条流出，
   最后给结论。**授权卡 60 秒不点会自动消失。**
3. 审计落库：

   ```sql
   SELECT instance_id, phase, disposition, output_bytes, started_at
   FROM ssh_ops_audit ORDER BY started_at DESC LIMIT 5;
   ```

   正常是 `phase=read_write`、`disposition=ok`。表里只有 who / which instance / 何时 / 结果 /
   字节数，**没有任何存放凭据的列**。

## 没生效的排查

| 现象 | 原因 |
|---|---|
| 启动失败，提示缺 `harness_path` / `base_url` / API key | 补齐路径、直连地址或 ModelVerse Key |
| 启动正常但日志说通道关闭 | 用的是静态 AK/SK（没配 `agent.sts.service_ak/service_sk`），或没有数据库 |
| 授权卡点了之后诊断失败 | Python 环境 / `claude` CLI / ModelVerse 网络或鉴权不可用 |
| `phase` 还是 `read_only` | `allow_writes` 没生效，改完要重启 |

把 `enabled` 改回 `false` 重启即可关闭，其余字段留着不影响，历史审计保留。
