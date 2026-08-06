# 飞书话题群知识问答机器人

这套接入实现以下效果：

- 白名单话题群成员点击右下角“＋”创建新话题后，机器人自动回答；外部群还需发布 `im:message.group_msg` 权限和对外共享能力；
- 同一话题里的后续追问需要 `@机器人`，避免机器人插入所有人工讨论；
- 普通群仍然需要 `@机器人`；
- 问题交给当前部署的 Compshare Agent，而不是交给飞书生成答案；
- 机器人始终在原消息所属的话题中回复；
- 同一话题会复用上下文，不同话题互不串线；
- 回答使用飞书原生富文本 Markdown 渲染，标题、加粗、列表、链接和代码块会正常显示；
- 支持文字、富文本以及附带一张图片的问题；图片进入现有 OCR/VL 模块；内部群可直接使用，外部群需一次性授权后使用；
- 长回答自动拆成多条；
- 飞书入口被强制限制为知识库问答，不能查询租户资源、诊断实例或发起任何操作。

## 为什么必须创建飞书应用机器人

自定义 Webhook 机器人只能向群里发消息，不能订阅并接收群成员的提问，因此不适合双向问答。

这里的“飞书应用机器人”只承担消息入口和机器人头像身份。真正理解问题、检索知识库和生成答案的仍然是自己部署的 Compshare Agent。

## 一、配置飞书开放平台

1. 在飞书开放平台创建“企业自建应用”。
2. 为应用开启“机器人”能力，设置机器人名称和头像。
3. 添加应用身份权限：
   - **获取群组中所有消息**（`im:message.group_msg`，敏感权限）：只有开通它，机器人才能在用户创建新话题且没有 `@机器人` 时收到事件；
   - **以应用的身份发消息**（`im:message:send_as_bot`）；
   - **获取单聊、群组消息**（`im:message:readonly`）：用于下载用户消息里的图片并交给 OCR；如果不需要图片问答，可以不申请。不要为了图片读取而申请权限更宽的“获取与发送单聊、群组消息”。
4. 在“事件与回调”中选择“使用长连接接收事件”。
5. 订阅事件 `im.message.receive_v1`（接收消息）。
6. 如果目标群像截图一样带“外部”标记，需要在版本设置中开启“允许机器人被添加到外部群中使用”的对外共享能力；企业或应用所有者需先完成飞书要求的认证。飞书当前的外部群机器人权限列表包含 `im:message.group_msg`，因此申请并发布该权限、订阅 `im.message.receive_v1` 后，外部话题群的新话题也能在没有 `@机器人` 时自动回答。若该敏感权限尚未获批或应用版本尚未生效，机器人只能响应 `@机器人` 的消息。默认的机器人租户令牌下载外部群截图会返回 `234009`；本项目提供了下文的「外部群截图授权」可选回退路径，以群内本企业成员的 `user_access_token` 再次读取图片。
7. 创建并发布应用版本，将机器人添加到目标群。

长连接方式不要求部署公网回调地址，也不需要配置 Webhook 验证地址。

## 二、填写项目配置

编辑 `deploy/conf/config.local.yaml` 中的 `agent.feishu`：

```yaml
feishu:
  app_id: "cli_xxx"
  app_secret: "xxx"
  agent_ws_url: "ws://127.0.0.1:7429/?Action=CreateCSAgentWS"
  company_id: 你的租户编号
  organization_id: 你的组织编号
  project_id: ""
  user_email: "feishu-bot@compshare.local"
  allowed_chat_ids: ["oc_xxx"]
  auto_reply_new_topics: true
  max_concurrent: 4
  max_reply_runes: 3500
  max_image_bytes: 5242880
  external_image_oauth:
    enabled: false
    redirect_url: "http://127.0.0.1:18765/feishu-oauth/callback"
    bootstrap_refresh_token: ""
```

`allowed_chat_ids` 是必须配置的群白名单。第一次联调不知道群 ID 时，可以临时写成 `["*"]`。在群里创建一条测试话题后，启动日志会显示 `chat=oc_xxx`；把该值写进白名单并移除 `"*"`。

开启 `auto_reply_new_topics` 后，所有已加入白名单的**话题根消息**都会被自动回答；飞书在消息事件中将话题群也标记为 `chat_type=group`，由非空 `thread_id` 且为空的 `root_id`、`parent_id` 识别根消息。这同时适用于内部群和已开通 `im:message.group_msg` 的外部群。话题里的评论、普通群消息仍需 `@机器人`。图片最大值应与 `agent.ocr.max_bytes` 保持一致；当前部署均为 5 MB。富文本内有多张图片时，当前版本读取第一张。回答会以飞书 `post` 消息中的 `md` 元素发送，而不是文本消息，因此 CommonMark/GFM 的常用格式会在飞书中渲染。

生产部署读取 `config.prod.yaml`，该文件会继承 `config.local.yaml` 并将此开关覆盖为 `true`，因此不需要把含有 App Secret 的基础配置文件再次提交到公开仓库。

`company_id` 和 `organization_id` 是机器人调用现有 Agent 时使用的固定身份。飞书入口只能使用知识库能力，不会继承网页端的资源操作能力。

### 外部群截图授权

目标外部群需要支持用户直接上传截图提问时，按下面流程一次性完成授权。这个流程不需要连接生产管理机。

1. 在飞书开发者后台为应用申请以下权限：`im:message:readonly`、`im:message.group_msg:get_as_user`、`offline_access`。前两个是最小的消息读取权限，最后一个只用于刷新已授权成员的访问令牌。
2. 在“开发配置 → 安全设置”中添加配置里的重定向 URL：`http://127.0.0.1:18765/feishu-oauth/callback`，并打开“刷新 `user_access_token`”开关（如果后台没有该开关，则飞书默认已开启）。随后发布一个新应用版本使权限和安全设置生效。这是授权时你的本机浏览器回调地址，不暴露生产服务。
3. 将 `0012_create_feishu_oauth_tokens.sql` 应用到现有 PostgreSQL。GitLab `main` 流水线中的 `migrate-feishu-oauth` 手动任务可完成此操作；它只在浏览器中点击触发，不需要 SSH 到生产机。
4. 在**目标外部群内的本企业成员**电脑上，切换到项目目录并执行：

   ```bash
   go run ./cmd feishu-authorize --config deploy/conf/config.local.yaml --enable
   ```

   命令会打开飞书授权页；完成后把首次使用的 refresh token 安静地写入 YAML，不会打印到终端。不要让外部群成员执行这一步：飞书只允许本企业成员的 `user_access_token` 用于此回退路径。
5. 将 YAML 中的 `bootstrap_refresh_token` 保持为空，创建一次 `main` 流水线时仅为该流水线填写变量 `FEISHU_EXTERNAL_IMAGE_OAUTH_BOOTSTRAP_TOKEN`。build 任务会在构建该次镜像时将变量注入空字段；不要将 token 提交到 Git，也不要把它写进部署日志。后续部署会使用数据库中已加密的轮换 token，不需要再次填写该变量。
6. 发布代码与配置到生产。第一次启动时，连接器会消费 bootstrap token、取得 user token，并把轮换后的 token 以 AES-GCM 密文保存到 `feishu_oauth_tokens` 表。以后 token 刷新、Pod 重启和正常部署均不需要重新授权；只有授权被撤销、授权成员退出目标群、飞书 App Secret 轮换，或飞书的 refresh token 到期（当前最长授权期为 365 天）后才需重新执行上面的本机授权命令。

启用后，连接器会先按原方式读取图片；仅当飞书对外部群返回 `234009` 时，才使用该授权成员的 token 重试同一张图片。它不会用该 token 查询群历史或执行任何平台资源操作。

## 三、启动

生产环境沿用项目现有的 `ally` 部署方式。先确认主 Agent 已通过 `make deploy` 正常运行，再执行：

```bash
make deploy-feishu
```

如果主 Agent 和飞书接入服务部署在同一台生产管理机，也可以一次注册两个服务：

```bash
make deploy-all
```

飞书入口会作为 `compshare-agent-feishu` 独立运行，并通过 `agent_ws_url` 访问主 Agent。配置中的 `127.0.0.1:7429` 是生产管理机自身，不是开发电脑。只有两个服务不在同一台机器时，才需要改成飞书接入服务能够访问的主 Agent 内网地址。

本地联调时，可以按下面的方式分别启动两个进程。

先启动 Agent 服务：

```bash
go run ./cmd server -c deploy/conf/config.local.yaml
```

再启动飞书连接器：

```bash
go run ./cmd feishu -c deploy/conf/config.local.yaml
```

生产环境可以从同一份代码构建一个二进制，再分别以 `server` 和 `feishu` 两个进程运行。

## 四、验证

在白名单话题群中点击右下角“＋”创建下面的话题。当前目标群是外部群；在应用版本已经发布 `im:message.group_msg` 的前提下，不需要在正文中 `@机器人`：

```text
外部数据库后期还能连接调用吗？
```

正确结果应满足：

1. 内部群和已开通所需权限的外部群，新话题均无需 `@机器人` 即自动回答；
2. 回答出现在原话题内，而不是创建另一个话题；
3. 在同一话题 `@机器人` 继续追问时能沿用前文；
4. 换一个话题后不会带入上一个话题的内容；
5. 在内部群发送带截图的富文本话题时，截图会直接进入现有 OCR/VL 模块；完成「外部群截图授权」后，外部群截图也会进入同一模块；
6. 要求开机、删资源或查询账号实例时，飞书入口不会获得相应操作能力。

连接器重启后，已存在话题会从新的 Agent 会话开始；飞书中的历史消息不会丢失，但机器人不会自动恢复重启前的上下文。
