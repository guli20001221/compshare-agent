# CompShare Platform Prior v0.1 Draft

Generated: 2026-06-08

Status: draft. Official docs, in-repo KB, and a logged-in console UI pass were reviewed. Sensitive account, instance, billing, credential, token, and API key values were intentionally not retained; screenshots are omitted for the same reason.

## Evidence Sources

- official_doc: https://www.compshare.cn/docs/operation/introduce/basicconcepts
- official_doc: https://www.compshare.cn/docs/operation/gpu/community
- official_doc: https://www.compshare.cn/docs/operation/gpu/createresources
- official_doc: https://www.compshare.cn/docs/gpus/instance/createcompshareinstance
- official_doc: https://www.compshare.cn/docs/operation/gpu/logininstance
- official_doc: https://www.compshare.cn/docs/operation/gpuspot/gpuspot
- official_doc: https://www.compshare.cn/docs/operation/charge/bill
- official_doc: https://www.compshare.cn/docs/operation/charge/billdescribe
- repo_kb: docs/faq/gpu_instance_faq.md
- repo_kb: docs/faq/model_plan_faq.md
- repo_kb: deploy/kb/curated_faq.jsonl
- repo_code: internal/tools/registry.go
- repo_code: internal/prompt/builder.go
- repo_code: internal/workflow/create_instance.go
- repo_code: internal/intent/capabilities/*.md
- console_ui: docs/platform_prior_v0.1.console_pages.json

The full logged-in console page map is captured in `docs/platform_prior_v0.1.console_pages.json`. The cards below are the higher-signal cards to seed Platform Prior v0.1.

## PageCards

```json
[
  {
    "type": "page_card",
    "page": "部署GPU实例",
    "url_pattern": "/light-gpu/resources/create",
    "visible_concepts": ["平台镜像", "社区镜像", "基础镜像", "系统镜像", "私有镜像", "GPU型号", "GPU数量", "CPU/内存配比", "磁盘", "防火墙", "付款方式", "预估费用"],
    "actions_visible": ["选择镜像", "切换镜像版本", "选择GPU型号", "选择GPU数量", "选择计费方式", "立即部署"],
    "read_only_observations": [
      "官方文档将创建入口描述为 GPU算力服务 -> 部署GPU实例。",
      "创建流程需选择镜像、规格、付费模式后提交。",
      "社区镜像详情页也可跳转到创建配置页。",
      "控制台实测实例类型包含独占式/抢占式；付款方式包含按量计费、包日、包月；提交入口会在当前配置无容量时显示暂无资源。"
    ],
    "unknowns": ["订单确认弹窗未触发", "价格拆分字段需在有资源配置下补样本", "社区镜像版本下拉未逐项展开"],
    "screenshots": []
  },
  {
    "type": "page_card",
    "page": "实例列表",
    "url_pattern": "/light-gpu/console/resources",
    "visible_concepts": ["实例", "实例状态", "GPU规格", "镜像", "系统盘", "数据盘", "外网IP", "内网IP", "SSH登录命令", "登录密码", "定时关机", "计费方式"],
    "actions_visible": ["JupyterLab", "ComfyUI", "登录", "关闭", "启动", "更多操作", "制作镜像"],
    "read_only_observations": [
      "官方图说显示实例卡片含规格、镜像、磁盘、网络、SSH命令和密码。",
      "顶部操作区包含 JupyterLab、ComfyUI、登录、关闭、更多操作。",
      "运行中实例可进入登录弹窗；关机实例需要先启动后登录。",
      "控制台实测运行中实例更多操作含关闭、重启、制作镜像、配置防火墙、删除、更改配置。",
      "控制台实测关机实例更多操作含无卡模式启动、重启、制作镜像、配置防火墙、删除、更改配置。",
      "监控按钮在当前页打开弹窗，含基础监控/进程监控、1小时时间窗、关闭/刷新。"
    ],
    "unknowns": ["抢占式已中断、初始化失败等状态的按钮差异仍需补样本", "JupyterLab/文件管理跳转目标未点击验证"],
    "screenshots": []
  },
  {
    "type": "page_card",
    "page": "镜像列表",
    "url_pattern": "/light-gpu/console/images",
    "visible_concepts": ["平台镜像", "自制镜像", "私有镜像", "共享镜像", "社区镜像", "镜像状态", "镜像大小", "镜像版本"],
    "actions_visible": ["制作镜像", "共享镜像", "删除镜像", "发布社区镜像", "更新镜像信息"],
    "read_only_observations": [
      "FAQ 描述私有镜像分享入口为 控制台 -> 镜像列表 -> 选择平台镜像 -> 更多 -> 共享镜像。",
      "发布社区镜像流程中，制作出的自制镜像可在控制台镜像列表查看。"
    ],
    "unknowns": ["完整筛选器和分页字段未逐项展开", "共享镜像/删除镜像等更多菜单需在对应镜像状态下补样本"],
    "screenshots": []
  },
  {
    "type": "page_card",
    "page": "镜像社区",
    "url_pattern": "https://www.compshare.cn/image-community",
    "visible_concepts": ["社区镜像", "镜像作者", "标签", "版本", "镜像价格", "发布社区镜像"],
    "actions_visible": ["搜索镜像", "选择标签", "查看详情", "使用该镜像创建实例", "发布社区镜像"],
    "read_only_observations": [
      "官方文档称镜像社区支持按镜像名称、作者名称或场景标签发现镜像。",
      "社区镜像详情页提供使用该镜像创建实例入口。"
    ],
    "unknowns": ["详情页字段未逐项展开", "发布入口的审核失败/草稿态未补样本"],
    "screenshots": []
  },
  {
    "type": "page_card",
    "page": "网络加速",
    "url_pattern": "/light-gpu/console/accelerator",
    "visible_concepts": ["网络加速", "Github", "HuggingFace", "Civitai", "应用仓库加速", "DNS配置"],
    "actions_visible": ["开通加速", "查看状态"],
    "read_only_observations": [
      "FAQ 给出控制台开通路径。",
      "文档说明社区镜像默认配置加速，虚机和基础镜像需修改DNS才生效。"
    ],
    "unknowns": ["开通按钮流程未触发", "是否展示费用或授权确认需补样本"],
    "screenshots": []
  },
  {
    "type": "page_card",
    "page": "财务中心",
    "url_pattern": "/uaccount/costcenter",
    "visible_concepts": ["余额", "账单", "订单", "待支付订单", "发票管理", "充值与提现", "退款"],
    "actions_visible": ["充值", "支付订单", "申请发票", "查看账单", "查看退款"],
    "read_only_observations": [
      "账号级实时财务数据不由 agent 查询，应引导用户到财务中心。",
      "欠费订单会限制创建或使用新资源。",
      "发票通常在财务中心的发票管理中申请。"
    ],
    "unknowns": ["账单/发票详情字段未记录敏感值", "支付/充值/开票流程未触发"],
    "screenshots": []
  },
  {
    "type": "page_card",
    "page": "API密钥",
    "url_pattern": "/uaccount/api_manage",
    "visible_concepts": ["API密钥", "PublicKey", "PrivateKey", "签名", "ProjectId"],
    "actions_visible": ["创建密钥", "查看密钥", "禁用密钥"],
    "read_only_observations": [
      "CreateCompShareInstance API 示例指向控制台账户中心 API 密钥页面。"
    ],
    "unknowns": ["密钥创建未触发", "密钥是否仅显示一次需人工验证"],
    "screenshots": []
  },
  {
    "type": "page_card",
    "page": "云存储",
    "url_pattern": "/ufile/ufile/detail",
    "visible_concepts": ["云存储", "对象存储", "空间", "上传下载", "挂载"],
    "actions_visible": ["创建存储空间", "上传文件", "下载文件", "挂载到实例"],
    "read_only_observations": [
      "本地数据上传文档指向云存储控制台。",
      "计费概览说明云存储按天出日账单。"
    ],
    "unknowns": ["挂载流程未触发", "UFile 详情页字段未记录敏感值"],
    "screenshots": []
  }
]
```

## FlowCards

```json
[
  {
    "type": "flow_card",
    "flow": "创建 GPU 实例",
    "steps_observed": ["选择镜像来源", "选择镜像或镜像版本", "选择 GPU 型号和数量", "选择 CPU/内存合法配比", "配置系统盘/数据盘", "选择防火墙", "选择计费方式", "查看价格", "确认订单", "创建后查看实例状态"],
    "risk_level": "high",
    "requires_approval": true,
    "dynamic_facts_needed": ["库存", "价格", "账户余额", "欠费订单", "账户配额", "镜像状态", "合法 CPU/内存/GPU 组合"],
    "must_use_tools": ["DescribeCompShareImages", "DescribeCommunityImages", "DescribeAvailableCompShareInstanceTypes", "CheckCompShareResourceCapacity", "GetCompShareInstanceUserPrice", "CreateInstanceWorkflow"],
    "notes": ["直接创建必须走 CreateInstanceWorkflow，不应裸调 CreateCompShareInstance。", "明确只问公开价格时用 GetCompShareInstancePrice。"]
  },
  {
    "type": "flow_card",
    "flow": "查询 GPU 价格",
    "steps_observed": ["识别 GPU 型号", "识别卡数/CPU/内存/可用区/计费方式", "缺少规格时使用默认或追问", "调用价格工具", "展示目录价或折后价"],
    "risk_level": "low",
    "requires_approval": false,
    "dynamic_facts_needed": ["实时目录价", "用户折后价", "镜像价格", "磁盘价格"],
    "must_use_tools": ["GetCompShareInstancePrice", "GetCompShareInstanceUserPrice"],
    "notes": ["产品公开定价与用户已有账单投诉需要分流。"]
  },
  {
    "type": "flow_card",
    "flow": "查询库存/是否可创建",
    "steps_observed": ["查询可用机型列表", "读取 Status 和 Zone", "明确型号时选择镜像与具体规格", "做容量预检", "回答是否当前可创建"],
    "risk_level": "low",
    "requires_approval": false,
    "dynamic_facts_needed": ["机型 Status", "容量预检结果", "镜像 ID", "计费方式"],
    "must_use_tools": ["DescribeAvailableCompShareInstanceTypes", "DescribeCompShareImages", "CheckCompShareResourceCapacity"],
    "notes": ["Status=Normal 不等于一定能创建；精确剩余数量不公开。"]
  },
  {
    "type": "flow_card",
    "flow": "登录实例",
    "steps_observed": ["选择运行中的实例", "点击登录", "选择 JupyterLab/SSH/VNC 等方式", "使用控制台显示的账号、端口、密码或 token"],
    "risk_level": "medium",
    "requires_approval": false,
    "dynamic_facts_needed": ["实例状态", "SshLoginCommand", "JupyterToken", "软件端口", "公网IP"],
    "must_use_tools": ["DescribeCompShareInstance", "DescribeCompShareJupyterToken", "DescribeCompShareSoftwarePort"],
    "notes": ["token、密码、私钥均为敏感信息；回答中不要要求用户粘贴。"]
  },
  {
    "type": "flow_card",
    "flow": "开机/关机/重启实例",
    "steps_observed": ["刷新实例状态", "确认目标实例", "展示影响和费用提示", "用户确认", "执行操作", "再次查询状态"],
    "risk_level": "high",
    "requires_approval": true,
    "dynamic_facts_needed": ["最新实例状态", "计费方式", "是否抢占式", "是否运行任务", "磁盘保留费用"],
    "must_use_tools": ["DescribeCompShareInstance", "StartInstanceWorkflow", "StopInstanceWorkflow", "RebootInstanceWorkflow"],
    "notes": ["所有变更前必须重新查状态；重启会中断任务；关机会涉及保留资源费用。"]
  },
  {
    "type": "flow_card",
    "flow": "制作自制/私有镜像",
    "steps_observed": ["选择实例", "从实例列表或镜像列表进入制作镜像", "填写镜像名称和信息", "确认制作", "在镜像列表验证", "可用自制镜像再部署实例"],
    "risk_level": "high",
    "requires_approval": true,
    "dynamic_facts_needed": ["实例类型", "实例状态", "系统盘大小", "镜像容量", "镜像存储费用", "敏感信息清理情况"],
    "must_use_tools": [],
    "notes": ["基础镜像实例需要开机制作；系统镜像实例制作与开关机状态无关。当前 agent 工具注册表未暴露制作镜像变更工具。"]
  },
  {
    "type": "flow_card",
    "flow": "发布社区镜像",
    "steps_observed": ["设置平台昵称", "用基础镜像部署实例", "配置并清理实例", "保存为私有镜像", "验证自制镜像", "填写社区镜像信息", "提交审核"],
    "risk_level": "high",
    "requires_approval": true,
    "dynamic_facts_needed": ["昵称/实名认证", "镜像来源", "镜像定价", "审核状态", "镜像内是否含敏感信息"],
    "must_use_tools": [],
    "notes": ["系统镜像制作的镜像不能发布到社区；社区镜像再次制作不能再次公开发布。"]
  },
  {
    "type": "flow_card",
    "flow": "账号级财务查询",
    "steps_observed": ["用户询问余额/总账单/流水/发票状态/退款进度/欠费金额", "agent 拒绝查询实时账号财务", "引导到财务中心对应页面"],
    "risk_level": "medium",
    "requires_approval": false,
    "dynamic_facts_needed": ["账号余额", "账单流水", "发票状态", "退款进度", "欠费金额"],
    "must_use_tools": [],
    "notes": ["实例计费原因可诊断；账号级实时财务当前不由 agent 查询。"]
  }
]
```

## ConceptCards

```json
[
  {
    "type": "concept_card",
    "concept": "GPU实例",
    "definition_candidate": "云上的虚拟 GPU 服务器，包含 vCPU、GPU、内存、操作系统、网络和磁盘，支持创建、远程连接、关机、删除、升降配、磁盘扩容、制作镜像和外网服务。",
    "evidence": ["official_doc:basicconcepts", "repo_tool:DescribeCompShareInstance"],
    "verification_status": "verified_doc",
    "common_misconceptions": ["用户把平台是否有库存误认为自己账号下实例列表。"]
  },
  {
    "type": "concept_card",
    "concept": "地域/可用区",
    "definition_candidate": "地域由地理区域内多个隔离可用区组成；平台 GPU 计算实例默认地域为华北二（乌兰察布），API 中常见 Zone 如 cn-wlcb-01。",
    "evidence": ["official_doc:basicconcepts", "official_doc:create_api"],
    "verification_status": "verified_doc",
    "common_misconceptions": ["用户只说地域时，创建/库存/价格仍可能需要可用区。"]
  },
  {
    "type": "concept_card",
    "concept": "GPU型号与合法配比",
    "definition_candidate": "GPU 型号、卡数、CPU、内存不是任意搭配，合法组合应从 DescribeAvailableCompShareInstanceTypes 获取；该接口 Memory 为 GB，创建/价格接口 Memory 为 MB。",
    "evidence": ["official_doc:create_api", "repo_tool:DescribeAvailableCompShareInstanceTypes", "repo_workflow:create_instance"],
    "verification_status": "verified_doc_and_code",
    "common_misconceptions": ["用户以为 4090 可任意搭配 CPU/内存。", "开发者容易把 GB/MB 单位传错。"]
  },
  {
    "type": "concept_card",
    "concept": "平台镜像",
    "definition_candidate": "平台官方镜像，当前工具层按 System（系统镜像）和 App（应用基础镜像）查询；创建页文档中用户可通过平台镜像选择基础镜像或系统镜像。",
    "evidence": ["official_doc:image_choice", "repo_tool:DescribeCompShareImages"],
    "verification_status": "verified_doc_and_code",
    "common_misconceptions": ["用户把平台镜像、私有镜像、社区镜像混为同一类。"]
  },
  {
    "type": "concept_card",
    "concept": "系统镜像",
    "definition_candidate": "平台官方提供的 Windows/Ubuntu 等裸系统镜像，底层为虚机环境；Ubuntu 默认 ubuntu/22，Windows 使用 administrator/远程桌面；系统镜像制作的自制镜像不支持发布至社区。",
    "evidence": ["official_doc:basicconcepts", "official_doc:image_choice", "official_doc:login"],
    "verification_status": "verified_doc",
    "common_misconceptions": ["用户以为系统镜像也能像基础镜像一样发布到社区。"]
  },
  {
    "type": "concept_card",
    "concept": "基础镜像",
    "definition_candidate": "平台官方提供的 Docker/框架镜像，如 PyTorch、TensorFlow、CUDA、Miniconda 等，底层为容器环境；默认 root/23；基础镜像打包应用后支持发布到社区。",
    "evidence": ["official_doc:basicconcepts", "official_doc:image_choice"],
    "verification_status": "verified_doc",
    "common_misconceptions": ["用户用 ubuntu/22 登录基础镜像导致 SSH 失败。"]
  },
  {
    "type": "concept_card",
    "concept": "社区镜像",
    "definition_candidate": "用户公开发布到镜像社区的容器镜像，可按名称、作者、标签发现和使用，支持定价；默认 root/23；由社区镜像再次制作的镜像只能自用，不能再次公开发布。",
    "evidence": ["official_doc:basicconcepts", "official_doc:image_choice", "repo_tool:DescribeCommunityImages"],
    "verification_status": "verified_doc_and_code",
    "common_misconceptions": ["用户以为付费社区镜像保存后再次使用不再付费。", "用户以为社区镜像可二次发布。"]
  },
  {
    "type": "concept_card",
    "concept": "自制/私有镜像",
    "definition_candidate": "用户从已有实例制作并保存在自己账号下的镜像，可用于后续部署和验证；私有镜像占用存储空间并可能产生费用，当前 FAQ 记录自制镜像最大容量为 1000GB。",
    "evidence": ["official_doc:basicconcepts", "official_doc:image_choice", "repo_faq:gpu_instance_faq"],
    "verification_status": "verified_doc_plus_faq",
    "common_misconceptions": ["用户以为自制镜像免费且无限大。", "用户把共享能力叫私有镜像时易和部署页分类混淆。"]
  },
  {
    "type": "concept_card",
    "concept": "容器实例 vs 虚机实例",
    "definition_candidate": "基础镜像和社区镜像创建容器实例，系统镜像创建虚机实例；两者默认登录名、端口、默认挂载路径和镜像制作/发布规则不同。",
    "evidence": ["official_doc:image_choice", "official_doc:disk", "repo_faq:gpu_instance_faq"],
    "verification_status": "verified_doc_plus_faq",
    "common_misconceptions": ["用户把容器实例当虚机登录。", "用户误以为磁盘扩容后的系统内操作完全一样。"]
  },
  {
    "type": "concept_card",
    "concept": "按量/Postpay/Dynamic",
    "definition_candidate": "按小时后付费，秒级计费；关机后 CPU、GPU 和内存被回收并停止收费，但云盘和镜像资源会保留并继续收费。代码层创建工作流把 Dynamic 映射到用户价格接口的 Postpay。",
    "evidence": ["official_doc:billdescribe", "repo_workflow:create_instance", "repo_kb:curated_faq"],
    "verification_status": "verified_doc_and_code",
    "common_misconceptions": ["用户以为关机后所有费用都停止。", "开发者混用 Dynamic/Postpay 枚举。"]
  },
  {
    "type": "concept_card",
    "concept": "包日/包月",
    "definition_candidate": "预付费计费方式，实例默认自动续费；账户余额不足且未及时续费时资源可能过期并回收。关机通常不释放实例资源，仍按预付费周期计费。",
    "evidence": ["official_doc:billdescribe", "repo_faq:gpu_instance_faq"],
    "verification_status": "verified_doc_plus_faq",
    "common_misconceptions": ["用户以为包月机器关机后像按量一样不收费。"]
  },
  {
    "type": "concept_card",
    "concept": "抢占式/Spot",
    "definition_candidate": "低价但可能被平台回收的实例类型，适合可中断任务；回收后数据保留 7 天，可在有资源时手动启动；不支持手动关机/关机免收费，不支持转为独占式。",
    "evidence": ["official_doc:gpuspot", "official_doc:create_api"],
    "verification_status": "verified_doc",
    "common_misconceptions": ["用户以为抢占式可以手动关机省钱。", "用户以为 Spot 可转为独占式。"]
  },
  {
    "type": "concept_card",
    "concept": "独占式",
    "definition_candidate": "候选定义：非抢占式的常规实例形态，运行期间资源独享；不等于关机后一定保留 GPU，也不保证后续启动不会遇到资源不足。官方文档仅明确独占式按量支持转包月，需 SME 补充精确定义。",
    "evidence": ["official_doc:bill", "official_doc:gpuspot", "repo_kb:billing"],
    "verification_status": "needs_sme_review",
    "common_misconceptions": ["用户以为独占式不会遇到资源不足。", "用户以为独占式关机后仍锁定 GPU。"]
  },
  {
    "type": "concept_card",
    "concept": "无卡模式",
    "definition_candidate": "实例关机后可选择无卡启动，不挂载 GPU，仅收取基础实例费；适用于写代码、调试、上传下载等。FAQ 记录同一账号仅允许 1 台实例开启，无卡开机不能制作镜像。",
    "evidence": ["repo_faq:gpu_instance_faq", "repo_tool:StartCompShareInstance.WithoutGpu"],
    "verification_status": "repo_faq_verified",
    "common_misconceptions": ["用户以为无卡模式可训练或制作镜像。"]
  },
  {
    "type": "concept_card",
    "concept": "系统盘/数据盘",
    "definition_candidate": "系统盘与实例强绑定，跟随实例创建和释放，CLOUD_SSD 系统盘默认 100GB 免费，RSSD 无免费额度且仅部分机型可选；数据盘创建即收费，与是否挂载无关。",
    "evidence": ["official_doc:disk", "official_doc:bill"],
    "verification_status": "verified_doc",
    "common_misconceptions": ["用户以为关机或卸载数据盘就停止数据盘费用。"]
  },
  {
    "type": "concept_card",
    "concept": "公共模型库",
    "definition_candidate": "平台预下载的公共模型库，可挂载/调用；仅支持容器实例，模型位于 /models 目录。",
    "evidence": ["official_doc:basicconcepts", "repo_faq:gpu_instance_faq"],
    "verification_status": "verified_doc_plus_faq",
    "common_misconceptions": ["用户以为虚机系统镜像也可直接使用公共模型库挂载。"]
  },
  {
    "type": "concept_card",
    "concept": "防火墙/软件端口",
    "definition_candidate": "防火墙通过规则绑定实例来控制公网访问；软件端口映射用于 JupyterLab、FileBrowser、应用服务等访问诊断，但 SSH 登录命令应以 DescribeCompShareInstance.SshLoginCommand 为准。",
    "evidence": ["official_doc:basicconcepts", "repo_tool:DescribeCompShareSoftwarePort", "repo_convention:CLAUDE.md"],
    "verification_status": "verified_doc_and_code",
    "common_misconceptions": ["开发者用软件端口接口推断 SSH，导致端口/账号错误。"]
  },
  {
    "type": "concept_card",
    "concept": "账号级财务数据",
    "definition_candidate": "余额、总账单、消费流水、发票状态、退款进度、欠费金额等属于账号级实时财务信息，当前 agent 不查询，应引导用户到控制台财务中心。",
    "evidence": ["repo_prompt:builder", "repo_kb:curated_faq", "repo_tests:account_billing_hardblock"],
    "verification_status": "verified_code_policy",
    "common_misconceptions": ["用户以为 agent 能直接查询余额或发票进度。"]
  }
]
```

## MisconceptionCards

```json
[
  {
    "type": "misconception_card",
    "misconception": "Status=Normal 就表示某个 GPU 一定有库存、一定能创建。",
    "correction": "Normal 只表示机型可售；明确型号/规格的库存问题还要用 CheckCompShareResourceCapacity 做容量预检。",
    "evidence": ["repo_capability:stock_availability", "official_doc:create_api"],
    "agent_rule": "库存回答必须区分可售状态和真实可创建。"
  },
  {
    "type": "misconception_card",
    "misconception": "按量实例关机后不会再产生任何费用。",
    "correction": "按量关机后 CPU/GPU/内存停止计费，但云盘、镜像等保留资源仍可能继续收费。",
    "evidence": ["official_doc:billdescribe", "repo_kb:curated_faq"],
    "agent_rule": "关机/费用回答必须列出可能继续计费的资源。"
  },
  {
    "type": "misconception_card",
    "misconception": "包日/包月实例关机后也会停止计费。",
    "correction": "包日/包月属于预付费，关机不等同退款或停止周期计费。",
    "evidence": ["official_doc:billdescribe", "repo_faq:gpu_instance_faq"],
    "agent_rule": "解释关机费用时先查或确认计费方式。"
  },
  {
    "type": "misconception_card",
    "misconception": "基础镜像、社区镜像、系统镜像都用同一个 SSH 用户和端口。",
    "correction": "容器实例通常 root/23，Ubuntu 系统镜像 ubuntu/22，Windows 走远程桌面/administrator。",
    "evidence": ["official_doc:image_choice", "official_doc:login"],
    "agent_rule": "登录失败诊断必须基于镜像/实例类型。"
  },
  {
    "type": "misconception_card",
    "misconception": "系统镜像制作的自制镜像可以发布到社区。",
    "correction": "系统镜像底层为虚机类型，制作后的镜像不支持发布至镜像社区。",
    "evidence": ["official_doc:image_choice", "repo_faq:gpu_instance_faq"],
    "agent_rule": "发布社区镜像建议必须要求基础镜像路径。"
  },
  {
    "type": "misconception_card",
    "misconception": "从社区镜像再制作镜像后，可以再次公开发布。",
    "correction": "社区镜像再次制作后只能自己使用，无法再次公开发布。",
    "evidence": ["official_doc:basicconcepts", "repo_faq:gpu_instance_faq"],
    "agent_rule": "社区镜像二次发布要明确拒绝并解释原因。"
  },
  {
    "type": "misconception_card",
    "misconception": "自制镜像没有容量限制或存储费用。",
    "correction": "FAQ 记录自制镜像最大容量 1000GB；官方计费说明自制镜像提供 30GB 免费容量，超出按量计费。",
    "evidence": ["repo_faq:gpu_instance_faq", "official_doc:bill"],
    "agent_rule": "制作镜像前提示容量、费用和敏感信息清理。"
  },
  {
    "type": "misconception_card",
    "misconception": "抢占式实例可以手动关机省钱或转成独占式。",
    "correction": "抢占式不支持手动关机/关机免收费，也不支持转为独占式；不用时应释放。",
    "evidence": ["official_doc:gpuspot"],
    "agent_rule": "Spot 操作建议必须提示可中断和限制。"
  },
  {
    "type": "misconception_card",
    "misconception": "JupyterLab 页面关了，实例里的任务也会停止。",
    "correction": "FAQ 说明 JupyterLab 关闭页面后服务器仍会保持运算。",
    "evidence": ["repo_faq:gpu_instance_faq"],
    "agent_rule": "区分网页会话关闭和实例/进程状态。"
  },
  {
    "type": "misconception_card",
    "misconception": "账号余额、发票状态、退款进度可以由 agent 直接查。",
    "correction": "这些是账号级实时财务数据，当前 agent 应引导用户到财务中心，不承诺查询。",
    "evidence": ["repo_prompt:builder", "repo_kb:curated_faq"],
    "agent_rule": "账号级财务实时查询 hard-block 到财务中心。"
  },
  {
    "type": "misconception_card",
    "misconception": "SSH 登录事实应从软件端口列表推断。",
    "correction": "SSH 登录命令以 DescribeCompShareInstance.SshLoginCommand 为准；软件端口接口主要返回应用端口映射。",
    "evidence": ["repo_convention:CLAUDE.md", "repo_tool:DescribeCompShareInstance"],
    "agent_rule": "SSH 诊断先读实例详情，不能仅凭端口表。"
  },
  {
    "type": "misconception_card",
    "misconception": "Memory 在所有 API 里单位相同。",
    "correction": "机型列表里的 Memory 为 GB；创建和价格接口使用 MB。",
    "evidence": ["repo_tool:DescribeAvailableCompShareInstanceTypes", "repo_workflow:create_instance"],
    "agent_rule": "工具参数构造必须显式做 GB/MB 转换。"
  },
  {
    "type": "misconception_card",
    "misconception": "删除/销毁实例可以由 agent 代操作。",
    "correction": "当前 agent 安全规则拒绝删除/销毁操作，引导用户到控制台手动完成。",
    "evidence": ["repo_prompt:builder", "repo_security:levels"],
    "agent_rule": "删除类请求拒绝执行，并提供控制台路径。"
  }
]
```

## Platform Prior Draft

### Product Model

CompShare is a GPU compute and model API platform. For the console agent, the primary bounded domain is GPU resources: instance lifecycle, GPU model/spec/stock/price, image selection, login/connectivity, storage, firewall/ports, billing rules, and read-only diagnostics. ModelVerse/model package questions are in scope only when they concern platform usage, credits, endpoints, and model package concepts.

Core object graph:

- Account owns instances, custom images, API keys, billing records, storage, and possibly teams/projects.
- GPU instance is created from an image and a legal GPU/CPU/memory/disk configuration in a zone.
- Images split into platform images, system images, base/app images, custom/private images, shared images, and community images.
- Billing attaches to instance resources, disks, custom image storage, cloud storage, and paid community images.
- Login/access depends on instance state and image-derived instance type: container vs VM.

### Agent Routing Prior

- Current instance state, instance list, login command, IP, billing mode, and monitor values are dynamic; use tools, not memory.
- Product price and stock are dynamic; use price/capacity tools. Do not answer from static price tables unless explicitly framed as doc examples.
- Account-level financial facts are unsupported by tools; direct users to 财务中心.
- Mutating actions require target disambiguation, state refresh, user-facing parameter summary, and confirmation.
- Delete/destroy remains outside agent execution and should be routed to manual console guidance.
- Knowledge QA should cite docs/KB when available and avoid inventing platform rules.

### Tool Requirements

- `DescribeCompShareInstance`: user's own instances, state, image, IP, billing, SSH login command.
- `GetCompShareInstanceMonitor`: current CPU/memory/GPU/VRAM utilization; no historical window in this stage.
- `DescribeAvailableCompShareInstanceTypes`: GPU model list, legal CPU/memory/GPU combos, Status Normal/SoldOut.
- `CheckCompShareResourceCapacity`: true capacity check for a concrete create config.
- `GetCompShareInstancePrice`: public/list price for product pricing questions.
- `GetCompShareInstanceUserPrice`: actual user price/discount for creation workflow.
- `DescribeCompShareImages`: platform official images, System/App.
- `DescribeCompShareCustomImages`: current user's self-made images.
- `DescribeCommunityImages`: community image groups by name/author/version.
- `DescribeCompShareJupyterToken`: sensitive Jupyter token retrieval.
- Workflows: create/start/stop/reboot/rename/reset-password/scheduler flows wrap confirmation and state refresh.

### Safety Prior

- Treat passwords, tokens, private keys, SSH commands containing credentials, API keys, billing details, and account identifiers as sensitive.
- Do not ask users to paste passwords, Jupyter tokens, private keys, or API secrets into chat.
- Before restart/stop/create/reset-password/rename/scheduler changes, summarize exact target and impact.
- Publishing community images is public-facing and should always include sensitive-data cleanup warnings.
- Financial transactions, recharge, payment, invoice submission, and refund operations should remain manual console actions unless a dedicated approved workflow exists.

### Remaining Gaps After Console Pass

- Instance card buttons were observed for Running and Stopped states; Spot interrupted, install-failed, expired, and reclaiming variants still need samples.
- Create page route and core controls were observed; order confirmation, payment failure, account-balance blocking, and price-breakdown labels still need safe samples.
- Image list and image community routes were observed; detail-state variants for private/shared/community images still need samples.
- Team management team-info menu selection was observed, but the route did not change in the sampled session; permission-dependent behavior needs another account or sample.
- Screenshots remain intentionally empty in this draft because the logged-in console pages expose credentials, instance IDs, IPs, account data, and billing/account surfaces.
