# Kubernetes / GitLab CD

本目录的清单和 [`.gitlab-ci.yml`](../../.gitlab-ci.yml) 按照 `compshare-kb` 的交付约定组织：

1. `main` 分支运行 `go test ./...` 和 `go vet ./...`；
2. Kaniko 将同一提交发布为不可变的 `${CI_COMMIT_SHORT_SHA}` 和便利标签 `latest`；
3. GitLab 中的 production job 必须人工确认，才会把提交 SHA 写入 `deployment.yaml` 并应用到
   `prj-ucompshare-prod`。

## 前置条件

- GitLab 项目设置受保护变量 `UHUB_USER` 和 `UHUB_PASS`，并由 `uaek-c5` runner 执行；
- `prj-ucompshare-prod` 已存在可拉取 UHub 镜像的 `regcred`；
- 先通过受控运维流程执行 [`../migrations/`](../migrations/) 的全部 PostgreSQL 迁移。镜像不带
  `psql`，因此不把数据库凭据或迁移权限复制进 CI job；
- 配置中的 `agent.retrieval.mcp_url` 指向已经就绪的 `compshare-kb` 服务。

## 运行拓扑

`Deployment` 以单副本 `Recreate` 策略启动 HTTP server 和飞书适配器两个容器。飞书适配器继续通过
`127.0.0.1:7429` 访问同一 Pod 内的服务，避免滚动更新期间同时存在两个飞书长连接。`Service`
在集群内暴露 HTTP 健康检查与 agent WebSocket 入口。

部署 job 在 rollout 完成后，从主容器经 Kubernetes Service 请求 `/healthz`；这会同时确认 Pod
就绪和 Service 到 endpoint 的转发，而不仅是容器进程存活。
