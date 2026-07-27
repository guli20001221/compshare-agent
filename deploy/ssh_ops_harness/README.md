# 实例内只读排查（SSH-ops）部署

平台 API 看不到实例内部。开启这条通道后，助手在**用户点授权卡同意**之后，SSH 进用户实例执行
**只读**命令（会改环境的命令一律拒绝），实时把每条命令的执行情况推给前端，最后给一段中文根因结论。
修复步骤只作为建议返回，**从不自动执行**。

**默认关闭。** 关闭时这个工具根本不在模型的可见工具列表里，不是"拒绝"，是不存在。

## 谁填哪些字段

| 字段 | 谁填 | 说明 |
|---|---|---|
| `harness_path` | **部署方** | 该机器上 `deploy/ssh_ops_harness/harness.py` 的绝对路径 |
| `gateway_url` | **部署方** | 该机器可达的 claude-code-router 地址（见下文「运行时依赖」） |
| `python` | **部署方** | 装好依赖的解释器绝对路径（venv，**不要用系统 python3**） |
| `model` | **研发** | **`deploy/conf/config.yaml` 里已经填好，不要改** —— 填错不会报错，只会静默降级，见下面的警告 |
| `timeout` | **研发** | 已填好（`12m`），不要改回默认的 `5m` |
| `enabled` | **研发** | 发布开关 |

所以部署方要填的只有三个：`harness_path`、`gateway_url`、`python`；`enabled` 等我们确认后再改成 `true`。

## 配置

**部署方式不变**，仍然是根 README 里那套：把编译好的 `./compshare-agent` 放到项目根，然后
`make deploy`（`deploy/scripts/deploy.sh` 用 `ally invite` 注册服务，工作目录就是这个 checkout）。
这条通道只是在 `deploy/conf/config.yaml` 的 `agent.ssh_ops` 下多填几个字段。

harness 就在这个 checkout 里，和 `config.yaml` 同一棵树。先在部署目录下取到绝对路径：

```bash
pwd    # 例如 /home/ubuntu/compshare-agent —— 下面用 <部署目录> 代指它
```

```yaml
  ssh_ops:
    enabled: true                                                   # 改
    harness_path: "<部署目录>/deploy/ssh_ops_harness/harness.py"      # 填
    gateway_url: "http://127.0.0.1:3456"                            # 填
    python: "<venv>/bin/python"                                     # 填
    model: "modelverse,deepseek-v4-pro"                             # 已填好，别动
    timeout: "12m"                                                  # 已填好，别动
```

`harness_path` 必须写绝对路径（其余像 `deploy/kb/` 那些是相对工作目录的，这个不是）。
每次上线换了 checkout 目录，这个字段要跟着改。

## ⚠️ `model` 必须写成 `provider,model` 两段式

网关（claude-code-router）的路由表里**没有裸模型名的映射**，写裸名会**静默回退到默认模型**，
不报错、不告警，只是诊断质量悄悄变差。实测：

| 配置里写的 | 实际跑的 |
|---|---|
| `deepseek-v4-pro` | ❌ `deepseek-v4-flash` |
| `gpt-5.6-terra` | ❌ `deepseek-v4-flash` |
| `modelverse,deepseek-v4-pro` | ✅ `deepseek-v4-pro` |

**留空更糟**：留空会回落到 `agent.llm.model`（当前是裸名 `gpt-5.6-terra`），过网关同样变成 flash。
所以**留空 ≠ 用主模型，留空 = 偷偷跑 flash**。

`timeout` 默认值 `5m` 偏短，重诊断会被腰斩，现象看起来像模型失败 —— 用 `12m`。

## 运行时依赖（每台跑 server 的机器都要有）

1. **Python 依赖**，装在独立 venv 里，**不要用系统 python3**（系统解释器上没有 `claude_agent_sdk`）。
   venv 建在 checkout **外面**，否则每次 `git status` 都会多出一堆未跟踪文件：

   ```bash
   python3 -m venv ~/.venv/compshare-agent-sshops
   ~/.venv/compshare-agent-sshops/bin/pip install -r <部署目录>/deploy/ssh_ops_harness/requirements.txt
   ```

   然后把 `python:` 填成 `~/.venv/compshare-agent-sshops/bin/python` 的绝对路径（不要用 `~`，展开成完整路径）。

2. **`claude` CLI**（Agent SDK 驱动它跑循环）。

3. **claude-code-router 常驻**。它是**独立于 `config.yaml` 的部署件**：harness 说 Anthropic 协议，
   模型服务说 OpenAI 协议，靠它翻译。它自己的 provider 列表和密钥在 `~/.claude-code-router/config.json`，
   需要单独维护和纳入进程守护。`gateway_url` 指向它。

> ⚠️ **依赖缺失不会在启动时被发现。** 启动只校验 `harness_path` / `gateway_url` 非空；Python 依赖、
> `claude` CLI、网关是否活着，都要到**第一次真实诊断**才暴露，用户侧表现为这次排查失败。
> 上线后请手动跑一次真实诊断确认（见下文「怎么确认真的通了」）。

## 数据库

审计是 **fail-closed** 的：写不进审计就不进用户机器。需要 `ssh_ops_audit` 表：

```bash
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 < deploy/migrations/0011_create_ssh_ops_audit.sql
```

表里只记 who / which instance / 何时 / 结果 / 输出字节数，**没有任何存放凭据的列**。

## 两个硬前提（不满足会怎样）

| 情况 | 行为 |
|---|---|
| 用的是静态 AK/SK（没配 `agent.sts.service_ak/service_sk`） | **通道静默关闭**，日志写明原因，服务正常启动。静态密钥没有按租户限定目标实例，必须拒绝 |
| 没有数据库 | **通道静默关闭**，日志写明原因，服务正常启动 |
| `enabled: true` 但 `harness_path` 或 `gateway_url` 为空 | **启动直接失败**并打印缺哪个字段 |

前两种是"配置没到位就别开"，最后一种是"你说要开却没配全"，所以一个静默降级、一个直接拒绝启动。

## 怎么确认真的通了

1. 启动日志里应有这一行：

   ```
   ssh-ops enabled: consent-gated read-only in-instance diagnosis (per-tenant STS, fail-closed audit, ...)
   ```

   没有这行就是没启用，日志里同一位置会写明是哪个前提没满足。

2. 在控制台对一台**自己的实例**问一句实例内才能回答的问题（例如「实例 uhost-xxx 上的服务打不开了」），
   出现授权卡 → 同意 → 应看到命令逐条流出，最后给出结论。**授权卡 60 秒不点会自动消失。**

3. 查审计表确认落了库：

   ```sql
   SELECT instance_id, phase, disposition, output_bytes, started_at, finished_at
   FROM ssh_ops_audit ORDER BY started_at DESC LIMIT 5;
   ```

   正常一行是 `phase=read_only`、`disposition=ok`、`output_bytes` 非空。

## 关掉

把 `enabled` 改回 `false` 重启即可，其余字段可以留着。关掉后工具不再进入模型的工具列表，
历史审计记录保留。
