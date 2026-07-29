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

## 数据库

后端存储使用 PostgreSQL，迁移由运维执行，二进制不会自动建表。新库按顺序执行全部迁移：

```bash
for f in deploy/migrations/*.sql; do
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$f"
done
```

通配符要用 `*.sql`（`000*.sql` 会漏掉 `0010`、`0011`）。迁移不幂等，升级已有库时只执行本库缺的那几个；顺序、两阶段切换和排错见 [`deploy/migrations/README.md`](deploy/migrations/README.md)。

漏迁移的表现是启动失败、且报错指向列而不是迁移，例如 `verify schema messages.turn_protocol: column "turn_id" does not exist`（该列来自 `0005`）。

`deploy/conf/config.yaml` 中的 `agent.mysql.dsn` 需要填 PostgreSQL libpq URL，例如：

```text
postgresql://user:pass@host:5432/compshare_agent?sslmode=disable
```

## 实例内只读排查（SSH-ops）

可选功能，**默认关闭**：用户在授权卡上同意后，助手 SSH 进他自己的实例执行只读命令定位问题。
开启需要额外的配置字段、Python 运行时依赖、一个常驻网关进程和一条数据库迁移。

部署步骤、字段填写分工和排错见 [`deploy/ssh_ops_harness/README.md`](deploy/ssh_ops_harness/README.md)。

## 测试

```bash
go test ./... -count=1
```
