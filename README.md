# compshare-agent

优云算力共享 AI 助手（CompShare GPU 平台）。Go 1.22 单二进制，`server` 子命令提供 HTTP/WS 服务。

## 配置

唯一部署配置文件是 `deploy/conf/config.yaml`。运行时开关、模型配置、数据库 DSN、STS/直连密钥都直接写在这个文件里，不再使用 `.env` 或 `*.example` 模板。

默认启动路径也是 `deploy/conf/config.yaml`：

```bash
go build -o compshare-agent ./cmd
./compshare-agent server
```

需要覆盖配置路径时：

```bash
./compshare-agent server -c /path/to/config.yaml
```

## 部署

部署入口简化为：

```bash
make deploy
```

`make deploy` 不会编译；它会直接使用项目根目录已有的 `./compshare-agent`，然后调用 `deploy/scripts/deploy.sh` 用 `deploy/conf/config.yaml` 注册服务。管理机上部署前，先在本地编译并把二进制上传到项目根目录。

## 本地数据库

后端存储使用 PostgreSQL。新库和升级都执行同一条命令，迁移可重复执行，已应用过的会跳过：

```bash
for f in deploy/migrations/*.sql; do
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 < "$f"
done
```

`deploy/conf/config.yaml` 中的 `agent.mysql.dsn` 需要填 PostgreSQL libpq URL，例如：

```text
postgresql://user:pass@host:5432/compshare_agent?sslmode=disable
```

## 实例内排查（SSH-ops）

可选功能，**默认关闭**：用户在授权卡上同意后，助手 SSH 进他自己的实例执行只读命令定位问题。
再单独打开 `allow_writes`（也默认关）它才会动手修复并验证；删除数据、格式化、重启关机、改账号密码、
关 SSH/网络这类高危操作两种模式都拒绝。

**比其他功能多一套部署工作**，因为诊断循环是跑在 Claude Agent SDK 里的独立子进程，不在本二进制内：

- 一个装好 `deploy/ssh_ops_harness/requirements.txt` 的 **Python venv**（系统 `python3` 没有 `claude_agent_sdk`）
- **`claude` CLI**（Agent SDK 驱动它跑循环）
- **claude-code-router 常驻进程**：harness 说 Anthropic 协议、模型服务说 OpenAI 协议，靠它翻译。
  它的 provider 与密钥在 `~/.claude-code-router/config.json`，是 `config.yaml` **之外**的部署件
- 一条数据库迁移（审计 fail-closed：写不进审计就不进用户机器）

⚠️ 这些依赖**缺了不会在启动时报错**，要到第一次真实诊断才暴露。上线后手动跑一次确认。

部署步骤、字段填写分工和排错见 [`deploy/ssh_ops_harness/README.md`](deploy/ssh_ops_harness/README.md)。

## 测试

```bash
go test ./... -count=1
```
