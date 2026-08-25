// Package refusal owns stable user-facing replies for deterministic policy and
// capability boundaries.
package refusal

// MonitorHistoryUnsupported is returned when the user asks for a historical
// monitor shape the agent cannot safely execute yet (missing target, missing
// concrete time window, multiple instances, or a window beyond the supported
// limit). Single-instance historical monitor with an explicit <=24h window is
// handled by the monitor workflow.
const MonitorHistoryUnsupported = "历史监控目前一次只支持查询一台实例，且需要明确 24 小时内的时间范围。请补充实例和时间段，例如“查询 uhost-xxx 昨天 8 点到 10 点的 CPU 监控”。"

// HumanAgentTransfer is the direct response to an explicit support handoff.
const HumanAgentTransfer = "如需人工客服协助，请扫描下方二维码添加客服微信，会有专人为您服务。\n\n![客服二维码](https://ucompshare-picture.cn-wlcb.ufileos.com/QRCode/qrcode.png)"
