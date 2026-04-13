package prompt

// FAQContent contains curated FAQ entries from the CompShare Feishu wiki,
// compressed into Q&A pairs for System Prompt injection (~4K tokens).
// Source: compshare.feishu.cn/wiki/GE5vwRiCUiCX2KkgFs5cIJ5Enfc
// Source: compshare.feishu.cn/wiki/T9GWwVtj4iXoTJkGuj8cRrtYnog
const FAQContent = `## 平台常见问题

### 实例相关

Q: 实例卡初始化或卡启动中怎么办？
A: 容器启动失败或容器环境被破坏会导致此问题，请加官方群联系群主处理。

Q: 常见错误码是什么意思？
A: 错误码8357=资源不足（售罄，稍后重试）；错误码8095=配额不足（联系运营增加配额）；错误码8429=资源过期（去订单管理支付欠费）。

Q: 容器实例如何设置服务自启动？
A: 在 /start.d/ 目录下创建可执行的 bash 脚本，容器开机会自动运行。

Q: Windows环境检测不到GPU？
A: Windows环境需要自行安装显卡驱动。下载地址：https://www.nvidia.cn/geforce/drivers/

Q: SSH无法连接、XShell无法连接、VS Code无法连接？
A: 请检查用户名、密码、端口号输入是否正确，不同类型镜像登录信息不同。容器镜像默认用root用户，端口为实例详情中的SSH端口。

Q: IP是公网IP吗？
A: 是公网固定IP，可以对外提供服务。网络带宽为1.5Gb共享带宽，单IP上限300Mbps。

Q: 什么是"无卡启动模式"？
A: 关机后选择无卡模式启动，不挂载GPU，仅收取基础实例费0.15元/小时，适合编写/调试代码、上传下载数据。限制：同一账号仅允许1台无卡实例；支持4090、4090-48G、3090、5090、A800、H20；无卡开机不能制作镜像。

Q: JupyterLab关闭页面后服务器还会保持运算吗？
A: 会继续保持运算。

Q: nvidia-smi检测不到显卡怎么办？
A: 可能为驱动问题，先在控制台重启实例。重启后仍检测不到则为硬件故障，联系技术支持。

Q: 网页Jupyter左侧文件列表不显示？
A: Jupyter默认工作目录在 /workspace 下，只有该目录下的文件才会展示。

### 计费相关

Q: 有哪些计费模式？
A: 四种：①按量（后付费，关机释放GPU/CPU/内存，停止计费，但磁盘和镜像继续收费）；②包时（预付费按小时，关机仍计费）；③包日（预付费按天，关机仍计费）；④包月（预付费按月，关机仍计费）。所有预付费模式已开通自动续费。

Q: 关机后还扣费吗？
A: 取决于计费模式。按量模式关机后GPU/CPU/内存停止计费，但额外扩容的磁盘和镜像资源继续收费。包时/包日/包月模式关机后仍然计费，因为资源不会释放。

Q: 实例卡初始化会扣费吗？
A: 卡初始化超过5分钟可联系客服解决扣费问题。卡启动中状态不会扣费（主机还未开机）。

Q: 按量机器能变更为包月吗？
A: 支持，需向客户经理/官方申请。注意：变更后不支持再次变更计费方式。

Q: 账号欠费无法使用怎么办？
A: 账号下有≥1条欠费订单则无法使用和创建资源，请去"财务中心"手动支付。

### 镜像相关

Q: 自制镜像容量限制是多少？
A: 最大容量1000GB。

Q: 自制镜像大小如何计算？
A: 容器镜像按实际存储大小；虚机镜像按系统盘大小。

Q: 什么镜像不能发布到社区？
A: ①系统镜像（虚机类型）不能发布；②社区镜像再制作的镜像不能发布。

Q: 制作镜像时实例要什么状态？
A: 基础镜像实例需开机状态；系统镜像实例不限。

### 实践部署

Q: 能安装Docker吗？
A: 支持，需使用系统镜像的Ubuntu环境。命令：sudo apt update && sudo apt install docker.io docker-compose。也可用预装Docker的系统镜像。

Q: 如何调用云端Ollama？
A: 启动命令：export OLLAMA_HOST=外网IP:11434 && ollama serve。客户端API地址填 http://外网IP:11434。

Q: VS Code如何连接实例？
A: 教程见：https://www.compshare.cn/docs/operation/logininstance#vscode连接

### 网络加速

Q: 如何加速访问GitHub、HuggingFace？
A: 外网学术加速已上线，控制台开通：https://console.compshare.cn/light-gpu/console/accelerator。社区镜像默认生效，虚机/基础镜像需修改DNS。

Q: 如何快速拉取热门模型？
A: 主流模型已在公共模型库，直接挂载即可。文档：https://www.compshare.cn/docs/bestpractices/sharemodel

### 账号相关

Q: 如何设置密码登录？
A: 控制台 → 账户管理页面设置。

Q: 如何开发票？
A: 控制台 → 财务中心 → 发票管理。

Q: 团队管理功能怎么用？
A: 面向企业、高校客户定向开通，支持成员管理和金额分配。需联系官方运营开通。

### 模型套餐

Q: 套餐支持哪些模型？
A: 包括 DeepSeek-V3.2、Qwen3-Max、GLM-5、Kimi-K2.5、GPT-5系列、Claude-4系列、MiniMax-M2系列等。详见控制台模型列表。

Q: 积分和Token怎么换算？
A: 积分 = 输入Token×输入倍率 + 缓存Token×缓存倍率 + 输出Token×输出倍率。注意Claude等模型倍率较高。

Q: 套餐外的模型怎么计费？
A: 按使用量直接扣费，不走套餐逻辑。请仔细检查模型名称避免误调套餐外模型。

Q: 模型套餐能在哪些工具中使用？
A: Claude Code、OpenCode、OpenClaw、Cline、Kilo Code、Codex CLI等编程工具，以及CherryStudio等Chatbot客户端和API调用。TRAE暂不支持。`
