# compshare-agent — 本地测试 / 生产部署指南

优云算力共享 AI 助手（CompShare GPU 平台）。Go 1.22 单二进制（`server` 子命令 = HTTP 服务）。
**目标：克隆 → 填密钥 → 直接部署。**

> 配置不入库：`.env`、`deploy/conf/agent.yaml` 都在 `.gitignore` 里（含密钥），仓库里只有 `*.example` 模板。密钥永远只在 `.env`，绝不提交。

---

## 0. 前置

- **Go 1.22+**（`go version`）、**git**
- **PowerShell**（`pwsh`/`powershell` 在 PATH）— 仅 pre-commit 密钥扫描钩子需要
- **Docker** — 本地测试用它起临时 MySQL（§3）
- 一个 **ModelVerse `LLM_API_KEY`** + 一套 **CompShare 凭证**（§2）

---

## 1. 克隆后三步

```bash
git clone <repo-url> compshare-agent && cd compshare-agent
git config core.hooksPath .githooks                         # 启用 pre-commit 密钥扫描（fresh clone 必做一次）
cp .env.example .env                                         # 填 §2 的密钥
cp deploy/conf/agent.yaml.example deploy/conf/agent.yaml     # 只含 ${ENV_VAR} 占位，值从 .env 注入
```

---

## 2. 必填密钥（**全部填进同一个文件：`.env`**）

`agent.yaml` 只有 `${ENV_VAR}` 占位符，**不放明文**；它从环境读值，而 `.env` 就是那个环境。`.env.example` 已列全部变量，照行填即可：

| `.env` 里的键 | 说明 / 从哪拿 |
|---|---|
| `LLM_API_KEY` | ModelVerse key（答复模型 `deepseek-v4-flash`，`https://api.modelverse.cn/v1`）。**必填。** |
| **CompShare 凭证（二选一）** | |
| ① STS 模式：`COMPSHARE_SERVICE_PUBLIC_KEY` + `COMPSHARE_SERVICE_PRIVATE_KEY` + `COMPSHARE_DEFAULT_ROLE_URN` | 服务 AK/SK 走 STS AssumeRole。**用此模式时把 `agent.yaml` 的 `public_key`/`private_key` 两行置空**，否则卡在 `COMPSHARE_PUBLIC_KEY is required`。STS 的 role 自动开通走内网 `iam_url`。 |
| ② 直连模式（本地最简单）：`COMPSHARE_PUBLIC_KEY` + `COMPSHARE_PRIVATE_KEY` | legacy 直连 AK/SK，`agent.yaml` 无需改。 |
| `MYSQL_DSN` | 见 §3（本地测试用 Docker 那条；生产填真实库）。 |

要点：
- **密钥不入库**：`.env` / `agent.yaml` 都 gitignored + pre-commit 有密钥扫描——别提交（GitHub 是外网，提交即外泄）。真实值通过安全渠道（内部密钥库/加密 DM）拿到后填进本地 `.env`。
- **config loader 严格**（实测）：`agent.yaml` 引用的每个 `${ENV_VAR}` 必须**已设置**，其中必填密钥（`LLM_API_KEY`、直连模式的 `COMPSHARE_PUBLIC_KEY`/`PRIVATE_KEY`）必须**非空**，否则报 `environment variable X is required`。所以用 `just run`（自动加载 `.env`）或直接跑二进制前先 `export` 好 `.env`。

---

## 3. 本地测试部署（Docker MySQL + server，实测 `/healthz` → `{"status":"ok"}`）

```bash
# (1) 构建
go build -o agent.exe ./cmd           # Windows；Linux/mac 用 go build -o agent ./cmd

# (2) 起临时 MySQL（测试用；端口 3306，被占就换个端口并同步改下面 DSN）
docker run -d --name cs-mysql -p 3306:3306 \
  -e MYSQL_ROOT_PASSWORD=root -e MYSQL_DATABASE=compshare_agent mysql:8

# (3) 等 MySQL 真正 auth-ready 再建表（MySQL 8 首次初始化要 ~20-40s；过早会 Access denied）
#     直到这条返回成功为止：
docker exec cs-mysql mysql -uroot -proot -e "SELECT 1"

# (4) 建表：按顺序执行【全部】迁移（0003 给 sessions 加 context_version，是 server 启动 schema 校验必需，缺了起不来）
for f in deploy/migrations/000*.sql; do docker exec -i cs-mysql mysql -uroot -proot compshare_agent < "$f"; done
#     建出：sessions / messages / message_feedback / agent_traces

# (5) .env 里把 MYSQL_DSN 设成这条（与上面 docker 的账号/端口一致）：
# MYSQL_DSN=root:root@tcp(127.0.0.1:3306)/compshare_agent?parseTime=true&loc=Local&charset=utf8mb4

# (6) 跑 server（任选其一）
just run                                          # 装了 just 时最省事：自动加载 .env，默认 :8236
#  —— 或者，没装 just：先把 .env 导入环境再跑二进制
set -a; . ./.env; set +a                          # 注意：DSN 含特殊字符，bash source 可能报错；没装 just 就逐个 export
./agent.exe server --addr :8080 -c deploy/conf/agent.yaml

# (7) 验证
curl http://127.0.0.1:8236/healthz                # 期望 {"status":"ok"}
```

填完密钥 + 起好 MySQL 后，server 即可启动；对话功能需要真实 `LLM_API_KEY`（`/healthz` 不调模型，dummy key 也能起）。

---

## 4. 生产部署（pack → tarball → invite.sh）

生产密钥同样只放 `.env`——`just pack` 会把 `.env` 打进 tarball 上传内网服务器，**全程不经 git**。

```bash
# (1) .env 填生产值：STS 服务 AK/SK + COMPSHARE_DEFAULT_ROLE_URN、真实 MYSQL_DSN、ADDR（监听地址）。
#     生产 flag 形态已是 .env.example 默认（写操作开、trace 写库、agentic-RAG 栈等），一般不用改。

# (2) 打包（需装 just；交叉编译 linux 二进制 + kb + 真实 env + invite.sh）
just pack                              # → dist/compshare-agent-<version>.tar.gz

# (3) 上传内网服务器并启动
scp dist/compshare-agent-*.tar.gz ucloud@<host>:/data/yuanpeng.wei/compshare-agent/
ssh ucloud@<host> 'cd /data/yuanpeng.wei/compshare-agent && tar -xzf compshare-agent-*.tar.gz && ./invite.sh'
#     invite.sh 会转发 .env 里的开关并以 server --addr $ADDR 启动。
```

生产 vs 本地差异：
- **凭证**：生产用 STS（`iam_url` 内网自动开通 role）；本地测试直连模式更省事。
- **数据库**：生产填真实 `MYSQL_DSN`，并需 ops 先在目标库按顺序跑 `deploy/migrations/0001`–`0004`（同 §3 第 4 步）。
- **trace**：`COMPSHARE_TRACE_ENABLED=1`/`SINK=mysql` 默认开，依赖上面的 `agent_traces` 表（`0002`+`0004`）。
- 全部运行时开关说明见 `CLAUDE.md` 的「Runtime feature flags」表。
