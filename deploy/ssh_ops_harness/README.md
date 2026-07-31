# 实例内排查与修复（SSH-ops）部署

平台 API 看不到实例内部。用户在授权卡上点同意后，助手 SSH 进他自己的实例执行命令定位问题，
再动手修复并验证，每条改动命令单独弹一次授权卡。删除数据、格式化、重启关机、改账号密码、
关 SSH/网络这类高危操作一律拒绝。审计 fail-closed：写不进审计表就不进用户机器。

## 要填的三个字段

`deploy/conf/config.yaml` 的 `agent.ssh_ops` 下只有这三个要按机器填，其余已经填好：

```yaml
  ssh_ops:
    harness_path: "<部署目录>/deploy/ssh_ops_harness/harness.py"   # 绝对路径
    gateway_url: "http://127.0.0.1:3456"
    python: "<venv>/bin/python"                                  # 绝对路径，不要用系统 python3
```

`harness_path` / `gateway_url` 留空，**服务起不来**（启动报错并打印缺哪个）。
`python` 留空**不报错**，会悄悄回退到系统 `python3` —— 那上面没有 `claude_agent_sdk`，
症状是用户点完授权卡之后诊断失败。换了 checkout 目录，`harness_path` 要跟着改。

## 机器上要装的

```bash
python3 -m venv ~/.venv/compshare-agent-sshops        # venv 建在 checkout 外面
~/.venv/compshare-agent-sshops/bin/pip install -r <部署目录>/deploy/ssh_ops_harness/requirements.txt
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 < deploy/migrations/0011_create_ssh_ops_audit.sql
```

还需要 **`claude` CLI**，以及常驻的 **claude-code-router**（harness 说 Anthropic 协议、模型服务说
OpenAI 协议，靠它翻译；它的 provider 和密钥在 `~/.claude-code-router/config.json`，是 `config.yaml`
**之外**的部署件，要单独维护和守护）。`gateway_url` 指向它。

> ⚠️ 这些依赖**缺了不会在启动时报错**，要到第一次真实诊断才暴露，用户侧表现为这次排查失败。

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
| 启动失败，提示缺 `harness_path` / `gateway_url` | 这两个必填 |
| 启动正常但日志说通道关闭 | 用的是静态 AK/SK（没配 `agent.sts.service_ak/service_sk`），或没有数据库 |
| 授权卡点了之后诊断失败 | venv / `claude` CLI / 网关三者之一不可用 |
| `phase` 还是 `read_only` | `allow_writes` 没生效，改完要重启 |

把 `enabled` 改回 `false` 重启即可关闭，其余字段留着不影响，历史审计保留。
