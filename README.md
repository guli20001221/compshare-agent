# compshare-agent

优云算力共享 AI 助手（CompShare GPU 平台）。Go 1.25 单二进制，`server` 子命令提供 HTTP/WS 服务。

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

生产交付物是一个自包含 Docker 镜像，内含 Go 服务、生产配置、Python 3.13、Claude Agent SDK、
Claude CLI、知识库和 SSH-ops harness。生产节点是 Docker 1.12.6，必须用专用发布命令生成
`linux/amd64` Docker schema-v2/gzip 镜像：

```bash
make docker-push-legacy IMAGE=registry.example.com/compshare/compshare-agent:<不可变版本>
```

构建、目标机兼容性检查、主服务/飞书启动方式见
[`deploy/docker/README.md`](deploy/docker/README.md)。生产配置会进入镜像层，私有仓库的 pull 权限
按生产凭据权限管理。

旧的宿主机/ally 部署入口仍保留：

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

用户在授权卡上同意后，助手 SSH 进他自己的实例执行命令定位问题，再动手修复并验证，每条改动命令
单独弹一次授权卡；删除数据、格式化、重启关机、改账号密码、关 SSH/网络这类高危操作一律拒绝。

**比其他功能多一套部署工作**：诊断循环跑在 Claude Agent SDK 的独立子进程里，不在本二进制内，
所以机器上还要有一个装了 `deploy/ssh_ops_harness/requirements.txt` 的 Python 环境、`claude` CLI，
以及一条数据库迁移。Claude CLI 通过 Anthropic 协议直连 ModelVerse，不需要额外路由器。

部署步骤与排错见 [`deploy/ssh_ops_harness/README.md`](deploy/ssh_ops_harness/README.md)。

## 测试

```bash
go test ./... -count=1
```
