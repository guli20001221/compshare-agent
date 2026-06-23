# compshare-agent — 本地测试 / 生产部署指南

优云算力共享 AI 助手（CompShare GPU 平台）。Go 1.22 单二进制（`server` 子命令 = HTTP 服务）。
**目标：克隆 → 填密钥 → 直接部署。**

> 配置不入库：`.env`、`deploy/conf/agent.yaml` 都在 `.gitignore` 里（含密钥），仓库里只有 `*.example` 模板。**运行时开关（写操作 / agentic-RAG / trace / 检索 / 截图等）现在全在 `agent.yaml`**（`agent.features` / `retrieval` / `trace` / `planner`），不再靠 env；密钥放 `.env`（`${ENV_VAR}` 占位）或直接内联进 gitignored 的 `agent.yaml`，绝不提交。优先级：YAML 设了就用 YAML，没设回退 env。

---

## 0. 前置

- **Go 1.22+**（`go version`）、**git**
- **PowerShell**（`pwsh`/`powershell` 在 PATH）— 仅 pre-commit 密钥扫描钩子需要
- **Docker** — 本地测试用它起临时 PostgreSQL（§3）
- 一个 **ModelVerse `LLM_API_KEY`** + 一套 **CompShare 凭证**（§2）

---

## 1. 克隆后三步

```bash
git clone <repo-url> compshare-agent && cd compshare-agent
git config core.hooksPath .githooks                         # 启用 pre-commit 密钥扫描（fresh clone 必做一次）
cp .env.example .env                                         # 填 §2 的密钥
cp deploy/conf/agent.yaml.example deploy/conf/agent.yaml     # 运行时开关都在这里；密钥用 ${ENV_VAR} 占位从 .env 注入（也可内联明文）
```

---

## 2. 必填密钥（**全部填进同一个文件：`.env`**）

`agent.yaml` 的**运行时开关用明文配置**（`agent.features` / `retrieval` / `trace` / `planner`，见 §4 与 `agent.yaml.example`）；**密钥**用 `${ENV_VAR}` 占位从 `.env` 注入（committed example 保持占位），也可直接内联进 gitignored 的 `agent.yaml`。`.env.example` 已列全部密钥变量，照行填即可：

| `.env` 里的键 | 说明 / 从哪拿 |
|---|---|
| `LLM_API_KEY` | ModelVerse key（答复模型 `deepseek-v4-flash`，`https://api.modelverse.cn/v1`）。**必填。** |
| **CompShare 凭证（二选一）** | |
| ① STS 模式：`COMPSHARE_SERVICE_PUBLIC_KEY` + `COMPSHARE_SERVICE_PRIVATE_KEY` + `COMPSHARE_DEFAULT_ROLE_URN` | 服务 AK/SK 走 STS AssumeRole。**用此模式时把 `agent.yaml` 的 `public_key`/`private_key` 两行置空**，否则卡在 `COMPSHARE_PUBLIC_KEY is required`。STS 的 role 自动开通走内网 `iam_url`。 |
| ② 直连模式（本地最简单）：`COMPSHARE_PUBLIC_KEY` + `COMPSHARE_PRIVATE_KEY` | legacy 直连 AK/SK，`agent.yaml` 无需改。 |
| `MYSQL_DSN` | PostgreSQL 连接串（libpq URL，如 `postgresql://user:pass@host:5432/db?sslmode=disable`）。见 §3（本地测试用 Docker 那条；生产填真实库）。变量名沿用 `MYSQL_DSN` 仅为兼容，值已是 PG。 |

要点：
- **密钥不入库**：`.env` / `agent.yaml` 都 gitignored + pre-commit 有密钥扫描——别提交（GitHub 是外网，提交即外泄）。真实值通过安全渠道（内部密钥库/加密 DM）拿到后填进本地 `.env`。
- **config loader 严格**（实测）：`agent.yaml` 引用的每个 `${ENV_VAR}` 必须**已设置**，其中必填密钥（`LLM_API_KEY`、直连模式的 `COMPSHARE_PUBLIC_KEY`/`PRIVATE_KEY`）必须**非空**，否则报 `environment variable X is required`。所以用 `just run`（自动加载 `.env`）或直接跑二进制前先 `export` 好 `.env`。

---

## 3. 本地测试部署（Docker PostgreSQL + server，实测 `/healthz` → `{"status":"ok"}`）

> 后端是 **PostgreSQL**（迁移脚本是 PG 方言：JSONB / TIMESTAMPTZ / 触发器）。**必须用 `psql` 应用迁移，不能用 mysql 客户端**——用 mysql 会直接报错。环境变量名沿用 `MYSQL_DSN` 仅为兼容，值是 libpq URL。

```bash
# (1) 构建
go build -o agent.exe ./cmd           # Windows；Linux/mac 用 go build -o agent ./cmd

# (2) 起临时 PostgreSQL（测试用；端口 5432，被占就换个端口并同步改下面 DSN）
docker run -d --name cs-pg -p 5432:5432 \
  -e POSTGRES_PASSWORD=pg -e POSTGRES_DB=compshare_agent postgres:16   # pg = throwaway 本地密码

# (3) 等 PG auth-ready（首次初始化几秒）；直到这条打印 "accepting connections"：
docker exec cs-pg pg_isready -U postgres

# (4) 建表：按顺序执行【全部】迁移（PG 方言，用 psql；0003 给 sessions 加 context_version，
#     是 server 启动 schema 校验必需，缺了起不来）
for f in deploy/migrations/000*.sql; do
  docker exec -i cs-pg psql -U postgres -d compshare_agent -v ON_ERROR_STOP=1 < "$f"
done
#     建出：sessions / messages / message_feedback / agent_traces

# (5) .env 里把 MYSQL_DSN 设成这条（libpq URL；与上面 docker 的账号/端口一致）：
# MYSQL_DSN=postgresql://postgres:pg@127.0.0.1:5432/compshare_agent?sslmode=disable

# (6) 跑 server（任选其一）
just run                                          # 装了 just 时最省事：自动加载 .env，默认 :8236
#  —— 或者，没装 just：先把 .env 导入环境再跑二进制
set -a; . ./.env; set +a                          # DSN 是 URL，source 一般 OK
./agent.exe server --addr :8236 -c deploy/conf/agent.yaml

# (7) 验证
curl http://127.0.0.1:8236/healthz                # 期望 {"status":"ok"}
```

填完密钥 + 起好 PostgreSQL 后，server 即可启动；对话功能需要真实 `LLM_API_KEY`（`/healthz` 不调模型，dummy key 也能起）。

---

## 4. 生产部署（pack → tarball → invite.sh）

生产密钥放 `.env`、运行时开关放 `agent.yaml`——`just pack` 会把两者打进 tarball 上传内网服务器，**全程不经 git**。

```bash
# (1) .env 填生产密钥：STS 服务 AK/SK + COMPSHARE_DEFAULT_ROLE_URN、真实 MYSQL_DSN、ADDR（监听地址）。
#     运行时开关在 agent.yaml（agent.features/retrieval/trace/planner）——生产形态已是 agent.yaml.example
#     默认（写操作开、trace 写库、agentic-RAG 栈、截图 OCR 等），一般不用改。.env 现在只放密钥 + ADDR。

# (2) 打包（需装 just；交叉编译 linux 二进制 + kb + 真实 env + invite.sh）
just pack                              # → dist/compshare-agent-<version>.tar.gz

# (3) 上传内网服务器并启动
scp dist/compshare-agent-*.tar.gz ucloud@<host>:/data/yuanpeng.wei/compshare-agent/
ssh ucloud@<host> 'cd /data/yuanpeng.wei/compshare-agent && tar -xzf compshare-agent-*.tar.gz && ./invite.sh'
#     invite.sh 转发 .env 里的密钥并以 server -c agent.yaml --addr $ADDR 启动；运行时开关由 agent.yaml 决定。
```

生产 vs 本地差异：
- **凭证**：生产用 STS（`iam_url` 内网自动开通 role）；本地测试直连模式更省事。
- **数据库**：生产填真实 `MYSQL_DSN`（PostgreSQL libpq URL），并需 ops 先在目标库用 `psql` 按顺序跑 `deploy/migrations/0001`–`0004`（PG 方言；同 §3 第 4 步）。
- **trace**：`agent.trace.enabled: true` / `sink: mysql` 默认开（`sink` 值沿用 `mysql`，实际写入上面的 PostgreSQL `agent_traces` 表），依赖该表（`0002`+`0004`）。
- 全部运行时开关说明见 `CLAUDE.md` 的「Runtime feature flags」表。
