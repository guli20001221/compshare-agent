# compshare-agent — 本地运行 / 同事上手指南

优云算力共享 AI 助手（CompShare GPU 平台）。Go 1.22 单二进制，从 `cmd/` 构建。两个子命令：`cli`（交互终端，只读冒烟最快）和 `server`（HTTP 服务，需 MySQL）。

> 配置不入库：`.env`、`deploy/conf/agent.yaml` 都在 `.gitignore` 里（含密钥）。仓库里只有 `*.example` 模板，克隆后照下面三步从模板复制并填密钥即可。

---

## 0. 前置

- **Go 1.22+**（`go version` 确认）
- **git**
- **PowerShell**（`pwsh` 或 `powershell` 在 PATH 上）— 仅 pre-commit 钩子需要（密钥扫描）。Windows 自带；Linux/mac 装 `pwsh` 或跳过钩子。
- **MySQL 8**（仅 `server` 子命令需要；`cli` 不需要）。本地起法见 §4。
- 一个 **ModelVerse LLM API Key**（`LLM_API_KEY`，必填）+ 一套 **CompShare 凭证**（见 §2）。

---

## 1. 克隆后三步

```bash
git clone <repo-url> compshare-agent && cd compshare-agent

# (1) 启用 pre-commit 密钥扫描钩子（fresh clone 必做一次）
git config core.hooksPath .githooks

# (2) 复制并填写环境变量（密钥都在这里）
cp .env.example .env          # 然后编辑 .env，填 §2 的必填项

# (3) 复制 agent.yaml（里面是 ${ENV_VAR} 占位，真实值从 .env / 进程环境注入）
cp deploy/conf/agent.yaml.example deploy/conf/agent.yaml
```

`agent.yaml` 的占位符 `${LLM_API_KEY}` 等只支持 `${VAR}` 形式，**不支持** `${VAR:-default}` 默认值语法。`justfile` 用 `set dotenv-load`/`set dotenv-required` 自动加载 `.env`——所以跑 `just *` 之前 `.env` 必须存在。

---

## 2. 必填环境变量（填进 `.env`）

| 变量 | `cli` | `server` | 说明 / 从哪拿 |
|---|:--:|:--:|---|
| `LLM_API_KEY` | ✅ | ✅ | ModelVerse key（答复模型 `deepseek-v4-flash` 走 `https://api.modelverse.cn/v1`）。**所有子命令必填。** |
| `MYSQL_DSN` | — | ✅ | MySQL 连接串，仅 `server`。本地见 §4，例：`user:pass@tcp(127.0.0.1:3306)/compshare_agent?parseTime=true&loc=Local&charset=utf8mb4` |
| **CompShare 凭证（二选一）** | | | 用于调 CompShare API |
| ① STS 模式：`COMPSHARE_SERVICE_PUBLIC_KEY` / `COMPSHARE_SERVICE_PRIVATE_KEY` | ✅ | ✅ | 服务自身 AK/SK，用于 STS `AssumeRole`。`cli` STS 模式还需 `COMPSHARE_DEFAULT_ROLE_URN` |
| ② 直连模式（本地 demo 最简单）：`COMPSHARE_PUBLIC_KEY` / `COMPSHARE_PRIVATE_KEY` | ✅ | ✅ | legacy 直连 AK/SK。`agent.sts` 未配置时走这条 |

要点：
- **本地最快路径 = 直连模式**：只填 `LLM_API_KEY` + `COMPSHARE_PUBLIC_KEY` + `COMPSHARE_PRIVATE_KEY`，跑 `cli` 就能起（不碰 MySQL/STS）。
- `project_id` 可留空（只读 API 会回退账号级默认）；mutating 工具（开关机/创建）建议在 `agent.yaml` 里设固定 `project_id`。
- `.env.example` 里每个开关都有逐行注释（写操作开关、上下文优化、trace、agentic-RAG 栈等），默认值即生产部署形态；本地随意，保持默认即可。完整 flag 说明见 `CLAUDE.md` 的「Runtime feature flags」表。

---

## 3. 构建 & 运行

```bash
# 构建
go build -o agent.exe ./cmd      # Windows
go build -o agent     ./cmd      # Linux/mac
#  或：just build  （产物名 compshare-agent）

# —— 最快冒烟：CLI 交互（只读，不需要 MySQL）——
./agent.exe cli -c deploy/conf/agent.yaml
#  起来后直接问，例如「我有哪些实例」「4090 现在有库存吗」「创建实例的操作步骤」

# —— HTTP 服务（需要 MySQL，见 §4）——
just run                          # 用 .env 起，默认 :8236（dotenv-required，故 .env 必须存在）
just run addr=":8080"             # 自定义端口
#  或直接：go run ./cmd server --addr :8080
```

`server` 路由：`POST /`（按 `Action` 分发：CreateSession / Chat(SSE) / GetMeta …）+ `GET /healthz`。

---

## 4. 本地 MySQL（仅 `server` 需要）

```bash
# 起一个本地 MySQL 8（端口映射到 3307 避免和系统库冲突）
docker run -d --name compshare-mysql -p 3307:3306 \
  -e MYSQL_ROOT_PASSWORD=root -e MYSQL_DATABASE=compshare_agent mysql:8

# 建表（DDL 由人工执行，二进制不自动建表）
docker exec -i compshare-mysql mysql -uroot -proot compshare_agent < deploy/migrations/0001_init.sql
# 如需 trace 观测列，再依次执行 deploy/migrations/0002_*.sql、0004_*.sql

# .env 里对应：
# MYSQL_DSN=root:root@tcp(127.0.0.1:3307)/compshare_agent?parseTime=true&loc=Local&charset=utf8mb4
```

---

## 5. 测试

```bash
go test ./... -count=1           #  或 just test
```
- 包含 CLI golden 套件（`eval/golden_test.go`）与离线意图 eval（`eval/evaluate_test.go`）——真模型臂需 `-model` 否则自动跳过，无 `-model` 时只跑离线/确定性部分。
- 集成测试（MySQL/真库往返、OCR e2e、知识库 corpus）会在缺对应 env 时自动 `t.Skip`，裸环境 `go test ./...` 应全绿。
- ⚠️ 跑 CLI/golden 相关测试时从仓库根目录跑（corpus 摘要按 LF-SHA256 钉死，cwd 不对会校验失败）。

---

## 6. 常见坑

- `.env` / `deploy/conf/agent.yaml` **是 gitignored 的**——别 commit，照模板本地复制。
- pre-commit 钩子需要 PowerShell；fresh clone 记得 `git config core.hooksPath .githooks`。
- `just *` 需要 `.env` 存在（`set dotenv-required`），否则报错。
- 知识库 `deploy/kb/*.jsonl` 按 SHA256 钉死（`internal/knowledge/corpus_digest.go`），改语料要同步重生成两个 embedding sidecar + 更新三个 digest 常量，否则加载器拒绝启动。
- 默认 `COMPSHARE_ENABLE_MUTATING_TOOLS=1`（部署模板形态，写操作开）；破坏性动作（删除/L2 终止）无论如何都拒绝。本地想纯只读把它设 0。

---

## 7. 想深入

- 架构 / 子系统边界 / 全部 feature flag：见 `CLAUDE.md`。
- 部署打包：`just pack`（产 `dist/compshare-agent-<ver>.tar.gz`，含 linux 二进制 + kb + env + invite.sh）。
