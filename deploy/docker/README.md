# Docker 1.12.6 生产部署

生产节点当前是 Docker `1.12.6` / API `1.24` / `linux/amd64`。它只负责 `pull` 和 `run`；
镜像必须在装有当前版 BuildKit/buildx 的构建机，或 GitLab 的 Kaniko runner 上生成，不能在生产节点
执行本仓库的多阶段 `Dockerfile`。

镜像是自包含交付物：Go 服务、生产 `deploy/conf/config.yaml`、Python 3.13 环境、
Claude Agent SDK、Claude CLI、远程知识库客户端和 SSH-ops harness 全部在镜像内。能拉取镜像的人也能读取
其中的生产配置，因此私有仓库的 pull 权限就是凭据权限；每次更换凭据都要用新 tag 重建镜像。

## 为什么有专用发布命令

Docker 1.12 不应接收 OCI image index、provenance/SBOM attestation 或 zstd 图层。发布脚本固定：

- 单架构 `linux/amd64`；
- Docker schema-v2 media types；
- gzip 图层；
- 关闭 provenance 和 SBOM；
- 推送后读取私有仓库中的原始 manifest，格式不符立即失败。

使用不可变版本号或 Git SHA，不要只发布 `latest`：

```bash
./deploy/docker/build-push-docker-1.12.sh \
  registry.example.com/compshare/compshare-agent:3a3690c
```

也可以通过 Makefile：

```bash
make docker-push-legacy \
  IMAGE=registry.example.com/compshare/compshare-agent:3a3690c
```

## 目标机兼容性门禁

第一次发布必须先跑这三个一次性检查。它们不连接数据库、ModelVerse 或用户实例：

```bash
IMAGE=registry.example.com/compshare/compshare-agent:3a3690c
docker pull "$IMAGE"

docker run --rm --entrypoint /opt/miniforge3/envs/py313/bin/python \
  "$IMAGE" -c 'import claude_agent_sdk, paramiko; print("python-runtime-ok")'

docker run --rm --entrypoint /usr/local/bin/claude "$IMAGE" --version
docker run --rm "$IMAGE" --help
```

如果出现 `Operation not permitted`、线程创建失败或莫名其妙的子进程启动失败，优先升级 Docker/runc。
仅为了确认是否是 2016 年默认 seccomp profile 导致，可以临时追加
`--security-opt seccomp=unconfined` 对照一次；不要把关闭 seccomp 当作长期生产配置。

## 启动

先执行 `deploy/migrations/*.sql`。镜像里包含迁移文件，但没有内置 `psql`；迁移仍由发布流程执行。

主服务：

```bash
docker run -d \
  --name compshare-agent \
  --restart=always \
  -p 7429:7429 \
  "$IMAGE" server

curl --fail http://127.0.0.1:7429/healthz
docker logs --tail 200 compshare-agent
```

同一镜像启动飞书适配器。当前生产配置使用 `127.0.0.1:7429`，所以让它共享主服务的网络命名空间：

```bash
docker run -d \
  --name compshare-agent-feishu \
  --restart=always \
  --net=container:compshare-agent \
  "$IMAGE" feishu
```

如果重建了主服务容器，也要重建飞书容器，避免继续引用旧的网络命名空间。镜像内已经使用 `tini`
作为 PID 1，不依赖新版 Docker 的 `--init`。

容器需要出站访问 PostgreSQL、ModelVerse、STS/IAM、飞书以及用户实例的 SSH 端口。

## Docker API 风险

`tcp://172.18.210.6:2375` 是无 TLS 的 Docker Remote API。能访问它就基本等价于拥有宿主机 root
权限。至少应通过防火墙只允许发布机访问；更稳妥的是关闭 2375，改用 Unix socket/SSH 隧道，
或启用双向 TLS 的 2376。参见 Docker 官方的
[Protect the Docker daemon socket](https://docs.docker.com/engine/security/protect-access/)。
