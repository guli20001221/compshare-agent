# Kubernetes / GitLab CD

本目录的清单和 [`.gitlab-ci.yml`](../../.gitlab-ci.yml) 按照 `compshare-kb` 的交付约定组织：

1. `main` 分支运行 `go test ./...` 和 `go vet ./...`；
2. Kaniko 将同一提交发布为不可变的 `${CI_COMMIT_SHORT_SHA}` 和便利标签 `latest`；
3. GitLab 中的 production job 必须人工确认，才会把提交 SHA 写入 `deployment.yaml` 并应用到
   `prj-ucompshare-prod`。

## 前置条件

- GitLab 项目设置受保护变量 `UHUB_USER` 和 `UHUB_PASS`，并由 `uaek-c5` runner 执行；
- `prj-ucompshare-prod` 已存在可拉取 UHub 镜像的 `regcred`。UHub 账号密码轮换后它会失效，症状是
  任何 deploy（包括回滚）都停在 `ImagePullBackOff`、events 里 `unauthorized: authorization failed`；
  点 `diagnose` 阶段的手动 job `refresh-regcred`，它用 `UHUB_USER` / `UHUB_PASS` 重写这个 Secret；
- 带迁移的提交先点手动 job `migrate-database`：它用本次构建的镜像起一个一次性 Pod
  （[`migration-pod.yaml`](migration-pod.yaml)），在集群内用镜像自带的 `psql` 执行
  [`../migrations/`](../migrations/) 的全部文件，数据库凭据不进入 CI job；
- 配置中的 `agent.retrieval.mcp_url` 指向已经就绪的 `compshare-kb` 服务。

## 运行拓扑

`Deployment` 以单副本 `Recreate` 策略启动 HTTP server 和飞书适配器两个容器。飞书适配器继续通过
`127.0.0.1:7429` 访问同一 Pod 内的服务，避免滚动更新期间同时存在两个飞书长连接。`Service`
在集群内暴露 HTTP 健康检查与 agent WebSocket 入口。

部署 job 在 rollout 完成后，从主容器经 Kubernetes Service 请求 `/healthz`；这会同时确认 Pod
就绪和 Service 到 endpoint 的转发，而不仅是容器进程存活。
