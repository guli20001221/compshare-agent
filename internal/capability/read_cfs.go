package capability

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/cfsbilling"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/zones"
)

// CFS list, create-price, upgrade-price and refund-estimate are independent
// typed read capabilities with a shared observation label.

const (
	// cfsFailureLabel is shared by all four CFS capabilities.
	cfsFailureLabel = string(intent.IntentCFSInfo)

	cfsDescribeAction     = "DescribeCFS"
	cfsCreatePriceAction  = "GetCompShareCFSPrice"
	cfsUpgradePriceAction = "GetCompShareCFSUpgradePrice"
	cfsRefundPriceAction  = "GetCompShareCFSRefundPrice"
)

// CFSResponse is the shared successful CFS outcome — a rendered reply and the
// action that produced it. Terminal outcomes (clarification, zone failure,
// upstream failure) are returned by the handler as a ReadResult, not here.
type CFSResponse struct {
	Reply  string
	Action string
}

func cfsRender(resp CFSResponse) ReadResult {
	r := ReadHandled(resp.Reply)
	r.ToolAction = resp.Action
	return r
}

// cfsClarify returns a deterministic needs-more-input observation.
func cfsClarify(action, reply string) ReadResult {
	r := ReadClarification(reply)
	r.ToolAction = action
	return r
}

// --- CFS list -------------------------------------------------------------------

type CFSListRequest struct {
	CFS *platform.CFSRef `json:"cfs,omitempty"`
}

func (CFSListRequest) MissingFields() []platform.MissingField { return nil }

func cfsListReadSpec() ReadCapabilitySpec[CFSListRequest, CFSResponse] {
	return ReadCapabilitySpec[CFSListRequest, CFSResponse]{
		Label:       readCFSList,
		Description: "查询当前账号 CFS 列表或指定 CFS 的状态、容量和位置。价格、扩容价和退费估算使用对应 CFS 能力。",
		Params:      objectParam(map[string]schemaNode{"cfs": cfsRefParam()}),
		Handle:      cfsListHandle,
		Render:      cfsRender,
	}
}

func cfsListHandle(ctx context.Context, req CFSListRequest, rt ReadRuntime) (CFSResponse, ReadResult) {
	args := map[string]any{}
	if req.CFS != nil {
		cfsID := extractCFSID(req.CFS.ID)
		if cfsID == "" {
			result := ReadFallbackBeforeTool(platform.ReadFallbackValidation)
			result.Reply = "该能力只查询 CFS；完整 CFS ID 应以 cfs- 开头，当前资源 ID 不能作为 CFS 查询。"
			return CFSResponse{}, result
		}
		args["CfsId"] = cfsID
	}
	raw, err := rt.Executor.Execute(ctx, cfsDescribeAction, args)
	if err != nil {
		return CFSResponse{}, ReadFailureAfterTool(cfsDescribeAction, cfsFailureLabel, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return CFSResponse{Reply: renderCFSInfoReply(raw), Action: cfsDescribeAction}, ReadResult{}
}

// --- CFS create price -----------------------------------------------------------

type CFSCreatePriceRequest struct {
	Zone         string `json:"zone"`
	TargetSizeGB int    `json:"target_size_gb"`
	ChargeType   string `json:"charge_type,omitempty"`
}

func (r CFSCreatePriceRequest) MissingFields() []platform.MissingField {
	var out []platform.MissingField
	if r.Zone == "" {
		out = append(out, platform.Missing("zone"))
	}
	if r.TargetSizeGB <= 0 {
		out = append(out, platform.Missing("target_size_gb"))
	}
	return out
}

func cfsCreatePriceReadSpec() ReadCapabilitySpec[CFSCreatePriceRequest, CFSResponse] {
	return ReadCapabilitySpec[CFSCreatePriceRequest, CFSResponse]{
		Label:       readCFSCreatePrice,
		Description: "查询拟创建云存储 Pro（CFS 共享文件存储）在指定可用区和容量下的实时账号净报价。不用于普通实例数据盘；上游接口不返回免费额度字段。",
		Params: objectParam(map[string]schemaNode{
			"zone":           stringParam().described("精确可用区 ID。当前只有上游标记为 Pod 的区域支持该询价。"),
			"target_size_gb": integerParam(1).described("目标容量 GB。"),
			"charge_type":    enumParam(cfsbilling.NewPurchaseTypes()...).described("计费周期：包月、包年或包日。CFS 当前不支持新购按量/后付费；省略时默认为包月。"),
		}, "zone", "target_size_gb"),
		Handle: cfsCreatePriceHandle,
		Render: cfsRender,
	}
}

func cfsCreatePriceHandle(ctx context.Context, req CFSCreatePriceRequest, rt ReadRuntime) (CFSResponse, ReadResult) {
	size := req.TargetSizeGB
	if size <= 0 {
		return CFSResponse{}, cfsClarify(cfsCreatePriceAction, "请补充要创建的 CFS 容量，单位 GB，例如 50GB。CFS 询价只读，不会创建资源。")
	}
	chargeType, supported := cfsCreateChargeTypeFromSlot(req.ChargeType)
	if !supported {
		r := ReadHandled("CFS 当前新购仅支持包月、包年或包日，不支持按量或后付费。询价不会创建资源。")
		r.ToolAction = cfsCreatePriceAction
		return CFSResponse{}, r
	}
	zone, ok := resolveCFSZoneFromSlot(ctx, rt, req.Zone)
	if !ok || zone.Zone == "" || zone.ZoneID == 0 {
		return CFSResponse{}, cfsClarify(cfsCreatePriceAction, "请补充要创建 CFS 的可用区。CFS 当前只支持 Pod/容器可用区，询价只读，不会创建资源。")
	}
	if !zone.IsPod {
		r := ReadHandled(fmt.Sprintf("CFS 当前只支持 Pod/容器可用区，%s 不是 Pod 区，无法询价或创建 CFS。", zone.Zone))
		r.ToolAction = cfsCreatePriceAction
		return CFSResponse{}, r
	}
	args := map[string]any{
		"Size":       size,
		"ChargeType": chargeType,
		"Quantity":   1,
		"zone_id":    zone.ZoneID,
		"az_group":   zone.RegionID,
	}
	raw, err := rt.Executor.ExecuteInternal(ctx, cfsCreatePriceAction, args)
	if err != nil {
		return CFSResponse{}, ReadFailureAfterTool(cfsCreatePriceAction, cfsFailureLabel, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return CFSResponse{Reply: renderCFSCreatePriceReply(raw, size, zone.Zone), Action: cfsCreatePriceAction}, ReadResult{}
}

// --- CFS upgrade price -----------------------------------------------------------

type CFSUpgradePriceRequest struct {
	CFS          platform.CFSRef `json:"cfs"`
	TargetSizeGB int             `json:"target_size_gb"`
}

func (r CFSUpgradePriceRequest) MissingFields() []platform.MissingField {
	var out []platform.MissingField
	if r.CFS.ID == "" {
		out = append(out, platform.Missing("cfs"))
	}
	if r.TargetSizeGB <= 0 {
		out = append(out, platform.Missing("target_size_gb"))
	}
	return out
}

func cfsUpgradePriceReadSpec() ReadCapabilitySpec[CFSUpgradePriceRequest, CFSResponse] {
	return ReadCapabilitySpec[CFSUpgradePriceRequest, CFSResponse]{
		Label:       readCFSUpgradePrice,
		Description: "估算指定已有 CFS 扩容到目标总容量的价格差额。只读，不执行扩容；新建 CFS 报价使用 CFS 创建报价能力。",
		Params:      objectParam(map[string]schemaNode{"cfs": cfsRefParam(), "target_size_gb": integerParam(1)}, "cfs", "target_size_gb"),
		Handle:      cfsUpgradePriceHandle,
		Render:      cfsRender,
	}
}

func cfsUpgradePriceHandle(ctx context.Context, req CFSUpgradePriceRequest, rt ReadRuntime) (CFSResponse, ReadResult) {
	cfsID := extractCFSID(req.CFS.ID)
	if cfsID == "" {
		return CFSResponse{}, cfsClarify(cfsUpgradePriceAction, "请补充要扩容的 CFS ID。CFS 扩容询价只读，不会直接扩容。")
	}
	size := req.TargetSizeGB
	if size <= 0 {
		return CFSResponse{}, cfsClarify(cfsUpgradePriceAction, "请补充 CFS 扩容后的目标容量，单位 GB，例如 200GB。Size 是目标容量，不是新增容量。")
	}
	zoneID, terminal := resolveCFSZoneIDFromDescribe(ctx, rt, cfsID)
	if terminal.Status != "" {
		return CFSResponse{}, terminal
	}
	args := map[string]any{"CfsId": cfsID, "Size": size, "zone_id": zoneID}
	raw, err := rt.Executor.ExecuteInternal(ctx, cfsUpgradePriceAction, args)
	if err != nil {
		return CFSResponse{}, ReadFailureAfterTool(cfsUpgradePriceAction, cfsFailureLabel, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return CFSResponse{Reply: renderCFSUpgradePriceReply(raw, cfsID, size), Action: cfsUpgradePriceAction}, ReadResult{}
}

// --- CFS refund estimate ---------------------------------------------------------

type CFSRefundEstimateRequest struct {
	CFS platform.CFSRef `json:"cfs"`
}

func (r CFSRefundEstimateRequest) MissingFields() []platform.MissingField {
	if r.CFS.ID == "" {
		return []platform.MissingField{platform.Missing("cfs")}
	}
	return nil
}

func cfsRefundEstimateReadSpec() ReadCapabilitySpec[CFSRefundEstimateRequest, CFSResponse] {
	return ReadCapabilitySpec[CFSRefundEstimateRequest, CFSResponse]{
		Label:       readCFSRefundEstimate,
		Description: "估算指定已有 CFS 当前可退金额。只读，不删除或释放 CFS。",
		Params:      objectParam(map[string]schemaNode{"cfs": cfsRefParam()}, "cfs"),
		Handle:      cfsRefundEstimateHandle,
		Render:      cfsRender,
	}
}

func cfsRefundEstimateHandle(ctx context.Context, req CFSRefundEstimateRequest, rt ReadRuntime) (CFSResponse, ReadResult) {
	cfsID := extractCFSID(req.CFS.ID)
	if cfsID == "" {
		return CFSResponse{}, cfsClarify(cfsRefundPriceAction, "请补充要估算退费的 CFS ID。这个查询只做估算，不会删除或释放 CFS。")
	}
	zoneID, terminal := resolveCFSZoneIDFromDescribe(ctx, rt, cfsID)
	if terminal.Status != "" {
		return CFSResponse{}, terminal
	}
	args := map[string]any{"CFSId": cfsID, "zone_id": zoneID}
	raw, err := rt.Executor.ExecuteInternal(ctx, cfsRefundPriceAction, args)
	if err != nil {
		return CFSResponse{}, ReadFailureAfterTool(cfsRefundPriceAction, cfsFailureLabel, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return CFSResponse{Reply: renderCFSRefundReply(raw, cfsID), Action: cfsRefundPriceAction}, ReadResult{}
}

// extractCFSID accepts only a non-empty CFS resource ID.
func extractCFSID(id string) string {
	value := strings.TrimSpace(id)
	if suffix, ok := strings.CutPrefix(strings.ToLower(value), "cfs-"); ok && suffix != "" {
		return value
	}
	return ""
}

func resolveCFSZoneFromSlot(ctx context.Context, rt ReadRuntime, zoneText string) (zones.ZoneInfo, bool) {
	list, err := zones.FetchSupportZones(ctx, rt.Executor, 0, 0)
	if err != nil {
		return zones.ZoneInfo{}, false
	}
	zoneText = strings.TrimSpace(zoneText)
	if zoneText == "" {
		return zones.ZoneInfo{}, false
	}
	if zone, ok := zones.ExactZone(list, zoneText); ok {
		return findCFSZoneInfo(list, zone)
	}
	return zones.ZoneInfo{}, false
}

func findCFSZoneInfo(list []zones.ZoneInfo, zone string) (zones.ZoneInfo, bool) {
	for _, item := range list {
		if strings.EqualFold(item.Zone, zone) {
			return item, true
		}
	}
	return zones.ZoneInfo{}, false
}

func resolveCFSZoneIDFromDescribe(ctx context.Context, rt ReadRuntime, cfsID string) (uint32, ReadResult) {
	raw, err := rt.Executor.Execute(ctx, cfsDescribeAction, map[string]any{"CfsId": cfsID})
	if err != nil {
		return 0, ReadFailureAfterTool(cfsDescribeAction, cfsFailureLabel, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	zoneID := cfsZoneIDFromDescribe(raw)
	if zoneID == 0 {
		r := ReadHandled(fmt.Sprintf("未获取到 %s 所在可用区，暂不能做 CFS 价格或退费估算。这个查询不会修改资源。", cfsID))
		r.ToolAction = cfsDescribeAction
		return 0, r
	}
	return zoneID, ReadResult{}
}

func cfsZoneIDFromDescribe(raw map[string]any) uint32 {
	if raw == nil {
		return 0
	}
	if id := uint32Field(raw, "ZoneId", "ZoneID", "zone_id"); id != 0 {
		return id
	}
	rows := mapSliceAt(raw, "CFSSet")
	for _, rowAny := range rows {
		row, ok := rowAny.(map[string]any)
		if !ok {
			continue
		}
		if id := uint32Field(row, "ZoneId", "ZoneID", "zone_id"); id != 0 {
			return id
		}
	}
	return 0
}

func cfsCreateChargeTypeFromSlot(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return cfsbilling.Month, true
	}
	return value, cfsbilling.SupportsNewPurchase(value)
}

// cfsMountStatusLabel translates known wire values and preserves unknown ones.
func cfsMountStatusLabel(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "mounted":
		return "已挂载"
	case "unmounted":
		return "未挂载"
	case "mounting":
		return "挂载中"
	case "unmounting":
		return "卸载中"
	default:
		return "挂载状态 " + status
	}
}

func renderCFSInfoReply(raw map[string]any) string {
	rows := mapSliceAt(raw, "CFSSet")
	if len(rows) == 0 {
		if id := stringField(raw, "CfsId"); id != "" {
			rows = []any{raw}
		}
	}
	if len(rows) == 0 {
		return "未查询到 CFS 共享文件存储。这个查询是只读操作，不会创建、扩容或删除 CFS。"
	}
	// Listings answer directly. Price queries retain their explicit read-only note
	// because a quote can otherwise be mistaken for an order.
	lines := []string{"CFS 共享文件存储："}
	shown := 0
	for _, rowAny := range rows {
		if shown >= 10 {
			break
		}
		row, ok := rowAny.(map[string]any)
		if !ok {
			continue
		}
		id := stringField(row, "CfsId", "CFSId")
		name := stringField(row, "Name")
		if name == "" {
			name = id
		}
		sizeText := ""
		if size, ok := numericField(row, "Size"); ok {
			sizeText = fmt.Sprintf("，容量 %.0fGB", size)
		}
		charge := stringField(row, "ChargeType")
		if charge != "" {
			charge = "，计费 " + cfsbilling.DisplayLabel(charge)
		}
		mountStatus := stringField(row, "MountStatus")
		if mountStatus != "" {
			mountStatus = "，" + cfsMountStatusLabel(mountStatus)
		}
		lines = append(lines, fmt.Sprintf("- %s（%s）%s%s%s", name, id, sizeText, charge, mountStatus))
		shown++
	}
	if len(rows) > shown {
		lines = append(lines, fmt.Sprintf("仅展示前 %d 个；如需精确查询请提供 CFS ID。", shown))
	}
	return strings.Join(lines, "\n")
}

func renderCFSCreatePriceReply(raw map[string]any, size int, zone string) string {
	price, chargeType := cfsPriceFromDetails(raw)
	if price == "" {
		return fmt.Sprintf("未获取到 %s 创建 %dGB CFS 的价格。CFS 询价是只读操作，不会创建资源。", zone, size)
	}
	period := cfsbilling.DisplayLabel(chargeType)
	// 「询价不会创建资源」 earns its place here — this is the one CFS reply a user
	// could mistake for having placed an order. The 「真正创建需要走确认流程」 that
	// followed it said the same thing a second time and is gone.
	return fmt.Sprintf("%s 创建 %dGB 云存储 Pro（CFS）的当前账号预估净报价：%s %s。上游接口不返回免费额度字段，不能从价格反推额度；询价不会创建资源。", zone, size, period, price)
}

func renderCFSUpgradePriceReply(raw map[string]any, cfsID string, size int) string {
	if price, ok := numericField(raw, "Price"); ok {
		return fmt.Sprintf("%s 扩容到 %dGB 的预估差价：¥%.2f。这个查询只做估算，不会直接扩容。", cfsID, size, price)
	}
	return fmt.Sprintf("未获取到 %s 扩容到 %dGB 的价格。这个查询只做估算，不会直接扩容。", cfsID, size)
}

func renderCFSRefundReply(raw map[string]any, cfsID string) string {
	if price, ok := numericField(raw, "RefundPrice"); ok {
		return fmt.Sprintf("%s 当前预计可退 ¥%.2f。这个查询只做估算，不会删除或释放 CFS。", cfsID, price)
	}
	return fmt.Sprintf("未获取到 %s 的退费估算结果。这个查询只做估算，不会删除或释放 CFS。", cfsID)
}

func cfsPriceFromDetails(raw map[string]any) (string, string) {
	details := mapSliceAt(raw, "PriceDetails")
	if len(details) == 0 {
		return "", ""
	}
	row, ok := details[0].(map[string]any)
	if !ok {
		return "", ""
	}
	chargeType := stringField(row, "ChargeType")
	for _, key := range []string{"Disks", "Price", "TotalPrice"} {
		if price, ok := numericField(row, key); ok {
			return fmt.Sprintf("¥%.2f", price), chargeType
		}
	}
	return "", chargeType
}

func uint32Field(m map[string]any, keys ...string) uint32 {
	for _, key := range keys {
		switch v := m[key].(type) {
		case int:
			if v > 0 {
				return uint32(v)
			}
		case int32:
			if v > 0 {
				return uint32(v)
			}
		case int64:
			if v > 0 {
				return uint32(v)
			}
		case uint:
			if v > 0 {
				return uint32(v)
			}
		case uint32:
			if v > 0 {
				return v
			}
		case uint64:
			if v > 0 {
				return uint32(v)
			}
		case float32:
			if v > 0 {
				return uint32(v)
			}
		case float64:
			if v > 0 {
				return uint32(v)
			}
		}
	}
	return 0
}
