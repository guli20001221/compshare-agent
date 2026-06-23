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
just deploy
```

`just deploy` 会交叉编译 Linux amd64 二进制到仓库根目录的 `compshare-agent`，然后调用 `deploy/scripts/deploy.sh` 用 `deploy/conf/config.yaml` 注册服务。

## 本地数据库

后端存储使用 PostgreSQL。首次使用前按顺序执行迁移：

```bash
for f in deploy/migrations/000*.sql; do
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 < "$f"
done
```

`deploy/conf/config.yaml` 中的 `agent.mysql.dsn` 需要填 PostgreSQL libpq URL，例如：

```text
postgresql://user:pass@host:5432/compshare_agent?sslmode=disable
```

## 测试

```bash
go test ./... -count=1
```
