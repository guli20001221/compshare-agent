package capability

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/zones"
)

// CFS read capabilities (migrated from the legacy intent route). The four CFS
// protocols — list, create-price, upgrade-price, refund-estimate — each get their
// own typed vertical instead of being funnelled back through one CFSKind Slots
// branch. They share the observation identity (IntentCFSInfo) and the CFS
// renderers, but nothing else. All are read-only price/estimate/list queries with
// no evidence envelope.

const (
	// cfsFailureLabel is the failure-reply + observation label shared by all four
	// CFS capabilities: the legacy route dispatched every CFS protocol under
	// IntentCFSInfo, so failures were labelled with string(IntentCFSInfo).
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

// cfsClarify is the CFS deterministic clarification (a needs-more-input reply
// that is still a Handled/NeedsClarification observation, matching the legacy
// ClarificationResult).
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
		Description: "查询 CFS 列表或指定 CFS 状态。",
		Params:      objectParam(map[string]schemaNode{"cfs": cfsRefParam()}),
		Handle:      cfsListHandle,
		Render:      cfsRender,
	}
}

func cfsListHandle(ctx context.Context, req CFSListRequest, rt ReadRuntime) (CFSResponse, ReadResult) {
	args := map[string]any{}
	if req.CFS != nil {
		if cfsID := extractCFSID(req.CFS.ID); cfsID != "" {
			args["CfsId"] = cfsID
		}
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
		Description: "估算创建 CFS 的价格。",
		Params:      objectParam(map[string]schemaNode{"zone": stringParam(), "target_size_gb": integerParam(1), "charge_type": stringParam()}, "zone", "target_size_gb"),
		Handle:      cfsCreatePriceHandle,
		Render:      cfsRender,
	}
}

func cfsCreatePriceHandle(ctx context.Context, req CFSCreatePriceRequest, rt ReadRuntime) (CFSResponse, ReadResult) {
	size := req.TargetSizeGB
	if size <= 0 {
		return CFSResponse{}, cfsClarify(cfsCreatePriceAction, "请补充要创建的 CFS 容量，单位 GB，例如 50GB。CFS 询价只读，不会创建资源。")
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
		"Zone":       zone.Zone,
		"ChargeType": cfsChargeTypeFromSlot(req.ChargeType),
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
		Description: "估算指定 CFS 扩容到目标容量的价格。",
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
		Description: "估算指定 CFS 当前可退金额。",
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

// --- Relocated verbatim from intent/routing_cfs_refund.go -----------------------

// extractCFSID applies the legacy cfsIDFromTargetRefs contract to a single CFS
// reference: a value is accepted only when it is a non-empty "cfs-"-prefixed id.
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

func cfsChargeTypeFromSlot(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "Month"
	}
	return value
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
	lines := []string{"CFS 共享文件存储（只读查询）："}
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
			charge = "，计费 " + charge
		}
		mountStatus := stringField(row, "MountStatus")
		if mountStatus != "" {
			mountStatus = "，挂载状态 " + mountStatus
		}
		lines = append(lines, fmt.Sprintf("- %s（%s）%s%s%s", name, id, sizeText, charge, mountStatus))
		shown++
	}
	if len(rows) > shown {
		lines = append(lines, fmt.Sprintf("仅展示前 %d 个；如需精确查询请提供 CFS ID。", shown))
	}
	lines = append(lines, "创建或扩容 CFS 需要走确认流程；删除/解绑能力不由 agent 暴露。")
	return strings.Join(lines, "\n")
}

func renderCFSCreatePriceReply(raw map[string]any, size int, zone string) string {
	price := cfsPriceFromDetails(raw)
	if price == "" {
		return fmt.Sprintf("未获取到 %s 创建 %dGB CFS 的价格。CFS 询价是只读操作，不会创建资源。", zone, size)
	}
	return fmt.Sprintf("%s 创建 %dGB CFS 的预估价格：%s。CFS 询价是只读操作，不会创建资源；真正创建需要走确认流程。", zone, size, price)
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

func cfsPriceFromDetails(raw map[string]any) string {
	details := mapSliceAt(raw, "PriceDetails")
	if len(details) == 0 {
		return ""
	}
	row, ok := details[0].(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"Disks", "Price", "TotalPrice"} {
		if price, ok := numericField(row, key); ok {
			return fmt.Sprintf("¥%.2f", price)
		}
	}
	return ""
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
