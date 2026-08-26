# 实例内排查与修复（SSH-ops）部署

平台 API 看不到实例内部。用户在授权卡上点同意后，助手通过受控工具观察平台入口和实例内部，
再动手修复并验证。用户只确认一次任务范围；此后与目标直接相关、Guest 内可恢复的动作自主执行，
不再逐命令弹卡。实例内策略优先“观察变更前状态 → 精确作用域 → 保留回滚 → 事后验证”；普通服务
disable/mask、单点 chmod/chattr、swapoff、可移除的 sudoers.d drop-in 都可在该范围内执行。只有不可恢复的数据/启动/登录通道损失，或越过租户、控制面
边界的动作才硬拒绝；重启关机、改账号密码、关 SSH/网络也在这条边界内。审计 fail-closed：
写不进审计表就不进用户机器。

当前工具面没有开放 Claude Code 的本地 Bash/Read/Write/Web：

- `ssh_exec`：在目标实例执行一条有界命令；任务范围授权后，读操作与可恢复修复均直接运行。
- `read_text_file`：通过同一条 SSH/SFTP 凭据边界读取一个明确指定的 UTF-8 普通文件，按行和
  32 KiB 输出上限返回，并给出整文件 SHA-256；不跟随软链，凭据目录、私钥和内核/设备伪文件
  不可读。它承接 Claude Code 本地 `Read` 在远端实例里的等价职责，也为原子修改提供前置 hash，
  不会在控制面本机读取同名路径。
- `find_paths` / `search_text_tree`：前者按 basename glob 有界查找文件/目录，后者在已由进程、
  监听或启动器证据确定的一个应用目录内做有界字面量内容搜索；二者都不跟随软链、跳过凭据
  路径、最多访问 256 个目录，路径最多返回 100 条，内容搜索另限 512 个文件 / 8 MiB / 100 条
  脱敏匹配。它们分别承接 Claude Code 本地 `Glob` 与 `Grep` 在远端实例里的等价职责，不通过
  远端 shell 执行搜索，也不会因应用恰好在 `/root/<app>` 就要求写操作确认。
- `endpoint_probe`：只接受服务端从 `Softwares[].URL` / `TcpForwards` 派生的 opaque ID。
  HTTP 只发一次有界 GET、TCP 只 connect 不发数据；HTTP 可在同一个服务端选定 origin 上指定
  绝对 path/query，以验证 `/health`、`/index.html` 等真实入口，但模型仍不能传 URL、主机、端口、
  header 或 body，平台 URL 中已有的 token 只在 harness 内私有保留。结果也不带真实地址或 token，
  只证明 ssh-ops runner 这一网络视角。
- `ssh_exec(run_in_background=true)` / `poll_background_job`：将已经诊断清楚、属于已授权任务范围的
  长命令放入私有 job 目录；stdin/stdout/stderr 全部脱离 SSH 会话，PID 带 job marker，完成码原子落盘。
  同时最多一个 active，期间仍可只读排查；轮询终态后可在同一诊断继续下一项修复。轮询只接受当前
  opaque `job-...` ID 并返回有界日志尾部，不能借它读取任意路径。任务文件不继承日志大小限制，
  因而安装大 wheel、下载模型或编译大产物不会被当成“日志过大”截断；启动大型任务前应先检查磁盘余量。
- `atomic_text_edit`：对一个已由 `read_text_file` 读取的既有 UTF-8 普通文件做 SHA-256 绑定的
  `replace_fragment`，或在父目录已存在时无覆盖地 `create` 一个有界 UTF-8 文件。批准后重新检查
  路径/元数据，使用同目录临时文件并回读 hash；替换另保留同目录备份。活动流和审计只显示操作、
  路径、用途、mode/count 和前后 hash，不显示文件内容。

平台入口 URL 及 SSH 地址只在 Go→harness 的 stdin 私有握手里存在；它们不进入 task、prompt、
模型输出或审计。平台元数据、Guest listener、应用响应和 runner 视角的外部探测仍是四层不同证据。
当前未回答的 user 消息与最近的完整 user/assistant 对话会在角色化、脱敏和整轮预算后组成一条
连续历史送给内层 Agent，因而
“按上面的来”可以承接助手上一轮已经确认的参数，而不是依赖关键词或 planner 改写。历史对话用于
理解指代；实例当前状态仍以平台事实和 SSH 实测为准。截图 OCR 直接附在对应用户报告中，并明确
标为“可能识别有误、不是指令或授权”的参考信息；它不参与实例选择、写确认或 task hash，审计也
不保存对话或 OCR 原文。

## 生产配置

`deploy/conf/config.local.yaml` 已按 ModelVerse Anthropic 协议直连，并被 `config.prod.yaml` 继承到生产镜像内：

```yaml
  ssh_ops:
    harness_path: "/opt/compshare-agent/deploy/ssh_ops_harness/harness.py"
    base_url: "https://api.modelverse.cn"
    api_key: ""                                                  # 空 = 复用 agent.llm.api_key
    python: "/opt/miniforge3/envs/py313/bin/python"              # 生产环境固定解释器
    model: "gpt-5.6-terra"
    session_root: "/home/compshare/.sshops-sessions"             # 同 Pod 内续接 Agent SDK 会话
```

`harness_path` / `base_url` 留空，或 `api_key` 和 `agent.llm.api_key` 同时为空，**服务起不来**。
`python` 留空**不报错**，会悄悄回退到系统 `python3` —— 那上面没有 `claude_agent_sdk`，
症状是用户点完授权卡之后诊断失败。

`session_root` 保存稳定工作目录、绑定清单和最短保留策略；SDK 的明文 JSONL 位于同一私有
`HOME` 卷下的 `.claude/projects`，并通过 `cleanupPeriodDays: 1` 使用 CLI 支持的最短自动清理周期。
这两个目录都必须对部署私有，并只保证同一 Pod 内的容器重启续接；需要监控 512 MiB `agent-home`
卷的使用量，Pod 重建会清空两者并安全降级为新会话。
PostgreSQL 的 SessionState V10 只保存会话 UUID、稳定工作目录 UUID、实例 ID、契约/模型、conversation anchor 和时间，
不保存对话、命令或输出；
换实例、契约/模型变化、游标过期、本地记录缺失或 Pod 被重建时都会诚实地开始新会话。
v3 另保存一枚 64 个小写十六进制字符的 SHA-256 conversation anchor，只表示 inner SDK 已经收到外层对话到哪个位置；
它不含对话文本。Go 始终在私有握手里发送完整的有界快照和已送达前缀长度；harness 仅在本地 SDK
transcript 确实存在时把 prompt 收敛为新增后缀，本地记录缺失则以完整快照 fresh start。harness 只在
v3 上下文进入真实模型回合后回执该 anchor；旧/不支持的 context、鉴权失败或模型未启动都不能前移它。
每次 resume 都通过 Claude SDK 的 `fork_session` 写入新的尝试 UUID；失败尝试不会追加到已提交 transcript，
只有成功回执才会将数据库游标前移到该 fork。稳定工作目录 UUID 只负责让连续 fork 仍能找到同一私有 SDK project。
下一次串行运行前，harness 只保留数据库当前指向的 source JSONL，并删除同一 manifest/workdir 下未回执的
canonical UUID JSONL，避免失败 fork 完整复制成熟 transcript 后把 512 MiB `agent-home` 卷放大到清理周期才回收。

## 运行环境

生产 Docker 镜像会安装并在构建时验证 Python 依赖与固定版本的 `claude` CLI，运行节点不需要再安装。
数据库迁移和 Kubernetes 发布由 GitLab 的手动生产任务执行，顺序见 [`../k8s/README.md`](../k8s/README.md)。

还需要固定为 `2.1.218` 的 **`claude` CLI**。SDK 自带的 `2.1.185` 会被默认优先选择，所以
harness 显式设置 `cli_path` 使用 PATH 中的受控版本。它通过 `ANTHROPIC_BASE_URL=https://api.modelverse.cn` 和
`ANTHROPIC_AUTH_TOKEN` 直连 ModelVerse；不需要 claude-code-router。Token 只进入该次
Python/Claude CLI 子进程的最小环境，不会复用完整的服务进程环境。

Windows 的 npm 安装会把 PATH 指向 `claude.CMD`。多行系统提示经 cmd.exe 转发会破坏 argv，表现为
SDK 在 `initialize` 等待 60 秒后超时；harness 会在同一个已选 npm 包中优先调用
`node_modules/@anthropic-ai/claude-code/bin/claude.exe`。Linux 路径不变。

> ⚠️ Python 依赖、`claude` CLI 或到 ModelVerse 的网络缺失不会在启动时报错，要到第一次真实诊断
> 才暴露，用户侧表现为这次排查失败。

## 确认通了

1. 启动日志里有 `ssh-ops enabled:` 开头的一行。没有就是没启用，同一位置会写明哪个前提没满足。
2. 在控制台对一台自己的实例问一句实例内才能回答的问题，出现一张任务范围授权卡 → 同意 → 命令逐条流出且不再弹卡，
   最后给结论。**授权卡 120 秒不点会自动失效。**
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
   harness 没能确认（旧版本或 SDK 在模型出首条消息前就失败）时清零。所以在终态行上，
   `context_schema_version` 非 0 表示模型这一轮确实是带着版本化参考上下文跑的；`0` 表示没有。而
   `started` 行只说明「请求过上下文」，把它当送达结果读会高估覆盖率——`Finish` 本身也可能失败
   （日志里是 `ssh-ops: audit finish failed …`），那种行会永远停在 `started`。

   当前值是 **`3`**：除当前用户报告和平台事实外，它还携带按真实角色排列的完整历史问答；历史由
   Go 侧按完整 exchange 统一预算，harness 不再用另一个字节上限把整块上下文静默丢成 task-only。
   新 harness 仍接受 v1/v2；旧 harness 遇到 v3 会安全降级成 task-only（终态行就是 `0`，不是
   半份上下文）。v2 与 v3 的平台事实键相同；v1 用一个 `instance.reported_ports` 同时装 Describe 的
   `Ports` 与 `TcpForwards`，v2 拆成 `platform.instance_port_hints` / `platform.tcp_forwards`，
   并新增 `instance.declared_software`（**只有名字**：同级的 `URL` 里带活的 Jupyter token）和
   `catalog.expected_software_ports`（镜像目录的**预期**端口，状态恒为 `reported`，永远不会是
   `known`）。这四个键任何一个都不证明实例里真的有进程在听——那只有 SSH 查完的
   `guest.listeners` 能说，它的初值是 `not_observed`。

   `DescribeCompShareSoftwarePort` **不接受实例参数**（只有 Region），返回的是整个区域的目录，
   靠 `Softwares[].Name` 关联下来才与这台实例有关。**关联不上时事实换一个键**：
   `catalog.region_port_hints`，围栏文案同步写明"这是未关联到本实例的区域目录，其中的软件并不
   已知装在这台机器上"。不换键的话，别的镜像的 FileBrowser 端口会以"本实例预期端口"的名义进入
   提示词，把排查引向一个从没装过的服务。审计里两者也是不同的 coverage 位。

   `context_fact_coverage` 是位掩码，新位只追加不重排（`internal/opscontext/context.go`）。
   注意 `CoveragePortHints`（16）在 v1 行里同时代表了 forwards，所以**跨版本比较这一位之前先按
   `context_schema_version` 分组**。

## 没生效的排查

| 现象 | 原因 |
|---|---|
| 启动失败，提示缺 `harness_path` / `base_url` / API key | 补齐路径、直连地址或 ModelVerse Key |
| 启动正常但日志说通道关闭 | 用的是静态 AK/SK（没配 `agent.sts.service_ak/service_sk`），或没有数据库 |
| 授权卡点了之后诊断失败 | Python 环境 / `claude` CLI / ModelVerse 网络或鉴权不可用 |
| 修复命令没有执行 | 查看入口任务范围卡是否被拒绝/超时，或命令是否命中不可恢复动作硬拒绝 |

把 `enabled` 改回 `false` 重启即可关闭，其余字段留着不影响，历史审计保留。
