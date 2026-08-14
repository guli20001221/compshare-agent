# 实例内排查与修复（SSH-ops）部署

平台 API 看不到实例内部。用户在授权卡上点同意后，助手 SSH 进他自己的实例执行命令定位问题，
再动手修复并验证，每条改动命令单独弹一次授权卡。删除数据、格式化、重启关机、改账号密码、
关 SSH/网络这类高危操作一律拒绝。审计 fail-closed：写不进审计表就不进用户机器。

## 生产配置

`deploy/conf/config.local.yaml` 已按 ModelVerse Anthropic 协议直连，并被 `config.prod.yaml` 继承到生产镜像内：

```yaml
  ssh_ops:
    harness_path: "/opt/compshare-agent/deploy/ssh_ops_harness/harness.py"
    base_url: "https://api.modelverse.cn"
    api_key: ""                                                  # 空 = 复用 agent.llm.api_key
    python: "/opt/miniforge3/envs/py313/bin/python"              # 生产环境固定解释器
    model: "gpt-5.6-terra"
```

`harness_path` / `base_url` 留空，或 `api_key` 和 `agent.llm.api_key` 同时为空，**服务起不来**。
`python` 留空**不报错**，会悄悄回退到系统 `python3` —— 那上面没有 `claude_agent_sdk`，
症状是用户点完授权卡之后诊断失败。

## 运行环境

生产 Docker 镜像会安装并在构建时验证 Python 依赖与固定版本的 `claude` CLI，宿主机不需要再装。
镜像构建和 Docker 1.12.6 目标机门禁见 [`../docker/README.md`](../docker/README.md)。

只有继续使用旧的宿主机/ally 部署时，才需要手动执行：

```bash
/opt/miniforge3/envs/py313/bin/python -m pip install -r /opt/compshare-agent/deploy/ssh_ops_harness/requirements.txt
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 < deploy/migrations/0011_create_ssh_ops_audit.sql
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 < deploy/migrations/0013_add_ssh_ops_context_observability.sql
```

还需要固定为 `2.1.218` 的 **`claude` CLI**。SDK 自带的 `2.1.185` 会被默认优先选择，所以
harness 显式设置 `cli_path` 使用 PATH 中的受控版本。它通过 `ANTHROPIC_BASE_URL=https://api.modelverse.cn` 和
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
   SELECT instance_id, phase, disposition,
          context_schema_version, context_fact_coverage,
          commands_ran, commands_refused, first_command_class,
          output_bytes, started_at, finished_at
   FROM ssh_ops_audit
   WHERE finished_at IS NOT NULL          -- 只有终态行的 context_* 才是「实际送达」
   ORDER BY started_at DESC LIMIT 5;
   ```

   正常是 `phase=read_write`、`disposition=ok`。表里只有 who / which instance / 何时 / 结果 /
   字节数和聚合指标，**没有任何存放凭据或原始命令的列**。

   **`finished_at IS NOT NULL` 这个条件不是可选的**：`Begin` 写入的 `started` 行记的是本次
   **请求**的 schema/coverage（那时 harness 还没启动），`Finish` 才把它改写成**实际生效**的值——
   harness 没能确认（旧版本、长度上限回退、SDK 在模型出首条消息前就失败）时清零。所以在终态行上，
   `context_schema_version` 非 0 表示模型这一轮确实是带着版本化参考上下文跑的；`0` 表示没有。而
   `started` 行只说明「请求过上下文」，把它当送达结果读会高估覆盖率——`Finish` 本身也可能失败
   （日志里是 `ssh-ops: audit finish failed …`），那种行会永远停在 `started`。

   当前生产值是 **`2`**。`1` 只出现在 Go 二进制回滚到 v2 之前、而 harness 已是新版的混合期：
   新 harness 仍接受 v1，旧 harness 遇到 v2 会安全降级成 task-only（终态行就是 `0`，不是半份
   上下文）。两版的差别全在事实键——v1 用一个 `instance.reported_ports` 同时装 Describe 的
   `Ports` 与 `TcpForwards`，v2 拆成 `platform.instance_port_hints` / `platform.tcp_forwards`，
   并新增 `instance.declared_software`（**只有名字**：同级的 `URL` 里带活的 Jupyter token）和
   `catalog.expected_software_ports`（镜像目录的**预期**端口，状态恒为 `reported`，永远不会是
   `known`）。这四个键任何一个都不证明实例里真的有进程在听——那只有 SSH 查完的
   `guest.listeners` 能说，它的初值是 `not_observed`。

   `context_fact_coverage` 是位掩码，新位只追加不重排（`internal/opscontext/context.go`）。
   注意 `CoveragePortHints`（16）在 v1 行里同时代表了 forwards，所以**跨版本比较这一位之前先按
   `context_schema_version` 分组**。

## 没生效的排查

| 现象 | 原因 |
|---|---|
| 启动失败，提示缺 `harness_path` / `base_url` / API key | 补齐路径、直连地址或 ModelVerse Key |
| 启动正常但日志说通道关闭 | 用的是静态 AK/SK（没配 `agent.sts.service_ak/service_sk`），或没有数据库 |
| 授权卡点了之后诊断失败 | Python 环境 / `claude` CLI / ModelVerse 网络或鉴权不可用 |
| `phase` 还是 `read_only` | `allow_writes` 没生效，改完要重启 |

把 `enabled` 改回 `false` 重启即可关闭，其余字段留着不影响，历史审计保留。
