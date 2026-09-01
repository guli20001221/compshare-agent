// Package refusal owns stable user-facing replies for deterministic policy and
// capability boundaries.
package refusal

// MonitorHistoryUnsupported is returned when the user asks for a historical
// monitor shape the agent cannot safely execute yet (missing target, missing
// concrete time window, or a window beyond the supported limit). Historical
// monitor with an explicit <=30d window is handled by the monitor workflow.
const MonitorHistoryUnsupported = "历史监控需要明确 30 天内的时间范围，单次最多查询 20 台实例。请补充实例和时间段，例如“查询 uhost-xxx 昨天 8 点到 10 点的 CPU 监控”。"

// HumanAgentTransfer is the direct response to an explicit support handoff.
const HumanAgentTransfer = "如需人工客服协助，请扫描下方二维码添加客服微信，会有专人为您服务。\n\n![客服二维码](https://ucompshare-picture.cn-wlcb.ufileos.com/QRCode/qrcode.png)"
