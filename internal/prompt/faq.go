package prompt

// FAQContent contains 11 structured knowledge topics for the CompShare GPU
// platform, injected into the system prompt. Focuses on operational guidance
// rather than raw Q&A. No hardcoded prices — points to API or "以控制台为准".
const FAQContent = `## 平台常见问题

### 1. 镜像选择
控制台镜像入口主要分为平台镜像和社区镜像两大类，不要再理解成“三类镜像”：
- **平台镜像**：控制台内包含 **共享镜像 / 私有镜像 / 基础镜像 / 系统镜像 / 第三方镜像** 五类。
  - 共享镜像：别人分享给你的镜像。
  - 私有镜像：你自己制作或持有的私有镜像；自制镜像最大 1000GB，查询用 DescribeCompShareCustomImages。
  - 基础镜像：容器类型，预装 PyTorch/CUDA/ComfyUI/Ollama 等框架环境，不支持再次安装 Docker。
  - 系统镜像：虚机类型，Windows/Ubuntu 等系统环境，Ubuntu 支持自行安装 Docker。
  - 第三方镜像：虚机类型，如 Isaac Sim/Dify/RAGFlow/Docker Compose 等，自制镜像后不支持发布至社区。
- **社区镜像**：社区作者发布的镜像，控制台内会区分 **付费镜像** 和 **免费镜像**。查询用 DescribeCommunityImages。
- **API 查询说明**：DescribeCompShareImages 用于查询平台公共镜像（ImageType 仅支持 System/App，可理解为系统镜像 / 应用基础镜像），不支持查询 Custom；DescribeCompShareCustomImages 用于查询自制/私有镜像。
注意：社区镜像不可二次发布到社区；共享/私有镜像能否直接用于创建实例，以控制台当前可选项为准。

### 2. 登录实例
四种方式连接实例：
- **SSH**：root 用户，端口见实例详情。容器镜像默认 root。
- **VS Code Remote-SSH**：教程见 https://www.compshare.cn/docs/operation/logininstance#vscode连接
- **JupyterLab**：网页终端，默认工作目录 /workspace。Token 可通过 DescribeCompShareJupyterToken 获取。
- **Windows RDP**：使用 mstsc（远程桌面连接）。
登录教程：https://www.compshare.cn/docs/operation/logininstance

### 3. 防火墙/端口
- 实例分配公网固定 IP，可对外提供服务。带宽 1.5Gb 共享（单 IP 上限 300Mbps）。
- 平台已知服务端口映射可通过 DescribeCompShareSoftwarePort 查询。
- 自定义服务需在 /start.d/ 目录下创建自启动脚本，容器开机自动运行。
- 运行中的容器实例可通过 DescribeCompShareInstance 返回的 Softwares 字段查看已安装应用及访问地址（系统镜像实例或非 Running 状态下该字段可能为空）。

### 4. 云硬盘
- 系统盘有免费额度（具体容量以控制台创建页为准），超出部分按量收费。
- 额外数据盘通过控制台添加。
- 关机后按量模式下额外磁盘仍计费（GPU/CPU/内存停止计费）。
- 自制镜像最大支持 1000GB。

### 5. 公共模型库
主流大模型（LLaMA/ChatGLM/Qwen/DeepSeek 等）已预下载至公共模型库，直接挂载使用，无需自行下载。
文档：https://www.compshare.cn/docs/bestpractices/sharemodel

### 6. 网络加速
GitHub/HuggingFace 学术加速功能已上线：
- 控制台开通：https://console.compshare.cn/light-gpu/console/accelerator
- 社区镜像默认已配置加速。
- 虚机和基础镜像需修改 DNS 方可生效。

### 7. 无卡模式
关机后以无卡模式启动，不挂载 GPU，仅收取基础实例费（远低于正常开机费用，具体以控制台为准）。适合编写代码、上传下载数据等非 GPU 任务。
- 限制：同一账号仅允许 1 台无卡实例。
- 支持机型：4090、4090-48G、3090、5090、A800、H20。
- 无卡模式下不能制作镜像。

### 8. 计费/回收规则
四种计费模式：
- **按量**：按小时后付费。关机后 GPU/CPU/内存停止计费，但额外磁盘继续收费。
- **包时**：按小时预付费，关机仍计费。
- **包日**：按天预付费，关机仍计费。
- **包月**：按月预付费，关机仍计费。
常见计费问题：
- **初始化是否收费**：启动中（Starting）状态不收费；卡初始化超过 5 分钟产生的扣费问题联系客服处理。
- **按量转包月**：需联系客户经理申请，变更后不可再次改回，如需更换计费方式需自制镜像后重开实例。
- **欠费回收**：账号下存在 ≥1 条欠费订单即阻塞所有操作（无法开机、创建），需先到财务中心支付。
常见错误码：8357=资源售罄、8095=配额不足、8429=过期欠费。
价格以 GetCompShareInstanceUserPrice（折后价）或 GetCompShareInstancePrice（目录价）实时查询为准。

### 9. 模型套餐
平台提供 API 模型调用服务，支持 DeepSeek-V3.2、Qwen3-Max、GLM-5、Kimi-K2.5、GPT-5 系列、Claude-4 系列、MiniMax-M2 系列等。
- 积分额度 = 输入Token×输入倍率 + 缓存Token×缓存倍率 + 输出Token×输出倍率（Claude 等模型倍率较高）。
- 套餐外模型按使用量直接扣费，请仔细检查模型名称避免误调。
- 支持工具：Claude Code、OpenCode、OpenClaw、Cline、Kilo Code、Codex CLI 等编程工具，以及 CherryStudio 等客户端。
- Claude 请求需选 Anthropic 协议（API 地址 https://api.modelverse.cn/）；GPT 请求需用 responses 协议。

### 10. 实践部署
- **Docker**：系统镜像 Ubuntu 环境下 sudo apt update && sudo apt install docker.io docker-compose，也可用预装 Docker 的系统镜像。
- **Ollama**：启动命令 export OLLAMA_HOST=外网IP:11434 && ollama serve，客户端 API 地址填 http://外网IP:11434。
- **nvidia-smi 检测不到显卡**：先在控制台重启实例，重启后仍检测不到则为硬件故障，联系技术支持。
- **JupyterLab 关闭页面后**运算会继续保持。Jupyter 默认工作目录 /workspace，只展示该目录下文件。

### 11. 账号管理
- 密码登录：控制台 → 账户管理页面设置。
- 发票：控制台 → 财务中心 → 发票管理。
- 团队管理：支持成员管理和金额分配，面向企业/高校客户定向开通，需联系官方运营。文档：https://www.compshare.cn/docs/uaccount/team`

// ReadOnlyFAQContent is the safe subset injected when mutating tools are
// disabled. It keeps product rules and console navigation, but intentionally
// omits shell commands, startup scripts, and instance-internal procedures.
const ReadOnlyFAQContent = `## 平台常见问题（只读模式）

### 1. 镜像选择
控制台镜像入口主要分为平台镜像和社区镜像：
- 平台镜像包含共享镜像、私有镜像、基础镜像、系统镜像、第三方镜像。
- 社区镜像由社区作者发布，控制台内会区分付费镜像和免费镜像。
- DescribeCompShareImages 查询平台公共镜像；DescribeCompShareCustomImages 查询自制/私有镜像。
注意：社区镜像不可二次发布到社区；共享/私有镜像能否用于创建实例，以控制台当前可选项为准。

### 2. 连接实例
常见入口包括 SSH、VS Code Remote-SSH、JupyterLab 和 Windows RDP。当前助手只提供云侧信息和控制台路径，不登录实例、不执行远程命令。
登录教程：https://www.compshare.cn/docs/operation/logininstance

### 3. 防火墙/端口
- 实例分配公网固定 IP，可对外提供服务。带宽 1.5Gb 共享（单 IP 上限 300Mbps）。
- 平台已知服务端口映射可通过 DescribeCompShareSoftwarePort 查询。
- 运行中的容器实例可通过 DescribeCompShareInstance 返回的 Softwares 字段查看已安装应用及访问地址（系统镜像实例或非 Running 状态下该字段可能为空）。

### 4. 云硬盘
- 系统盘有免费额度，具体容量以控制台创建页为准。
- 额外数据盘通过控制台添加。
- 关机后按量模式下额外磁盘仍计费（GPU/CPU/内存停止计费）。
- 自制镜像最大支持 1000GB。

### 5. 公共模型库
主流大模型已预下载至公共模型库，直接挂载使用，无需自行下载。
文档：https://www.compshare.cn/docs/bestpractices/sharemodel

### 6. 网络加速
GitHub/HuggingFace 学术加速功能可在控制台开通：
https://console.compshare.cn/light-gpu/console/accelerator
社区镜像默认已配置加速；虚机和基础镜像是否生效以控制台和文档为准。

### 7. 无卡模式
关机后以无卡模式启动，不挂载 GPU，仅收取基础实例费（具体以控制台为准）。适合编写代码、上传下载数据等非 GPU 任务。
- 限制：同一账号仅允许 1 台无卡实例。
- 支持机型：4090、4090-48G、3090、5090、A800、H20。
- 无卡模式下不能制作镜像。

### 8. 计费/回收规则
四种计费模式：
- 按量：按小时后付费。关机后 GPU/CPU/内存停止计费，但额外磁盘继续收费。
- 包时：按小时预付费，关机仍计费。
- 包日：按天预付费，关机仍计费。
- 包月：按月预付费，关机仍计费。
常见计费问题：
- 初始化是否收费：启动中（Starting）状态不收费；卡初始化超过 5 分钟产生的扣费问题联系客服处理。
- 按量转包月：需联系客户经理申请，变更后不可再次改回。
- 欠费回收：账号下存在欠费订单时可能阻塞操作，需先到财务中心处理。
常见错误码：8357=资源售罄、8095=配额不足、8429=过期欠费。
价格以 GetCompShareInstanceUserPrice（折后价）或 GetCompShareInstancePrice（目录价）实时查询为准。

### 9. 模型套餐
平台提供 API 模型调用服务，支持 DeepSeek、Qwen、GLM、Kimi、GPT、Claude、MiniMax 等模型。套餐外模型按使用量直接扣费，请仔细检查模型名称避免误调。

### 10. 实践部署
Docker、Ollama、JupyterLab 等环境部署请以控制台文档和镜像说明为准。当前助手不提供实例内命令执行或文件修改，只能说明云侧资源、镜像和监控事实。

### 11. 账号管理
- 密码登录：控制台 → 账户管理页面设置。
- 发票：控制台 → 财务中心 → 发票管理。
- 团队管理：支持成员管理和金额分配，面向企业/高校客户定向开通，需联系官方运营。文档：https://www.compshare.cn/docs/uaccount/team`
