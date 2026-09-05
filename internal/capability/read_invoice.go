package capability

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
)

// Invoice status is an account-scoped, read-only list. Tenant identity is
// injected by the request context; it is never a model argument.
const (
	invoiceStatusCapabilityLabel = string(intent.IntentInvoiceStatus)
	invoiceStatusAction          = "GetCompShareInvoiceIssued"
)

type InvoiceStatusRequest struct {
	Offset int `json:"offset,omitempty"`
}

func (InvoiceStatusRequest) MissingFields() []platform.MissingField { return nil }

type InvoiceStatusItem struct {
	ID                 int
	AmountCents        int
	AmountKnown        bool
	State              string
	Mode               string
	Type               string
	RequestTime        int64
	RejectReason       string
	RefundRejectReason string
}

type InvoiceStatusResponse struct {
	Items      []InvoiceStatusItem
	TotalCount int
	Offset     int
}

func invoiceStatusReadSpec() ReadCapabilitySpec[InvoiceStatusRequest, InvoiceStatusResponse] {
	return ReadCapabilitySpec[InvoiceStatusRequest, InvoiceStatusResponse]{
		Label:       invoiceStatusCapabilityLabel,
		Description: "查询当前账号的发票记录、开具状态、申请时间和金额。只读，不申请或修改发票。",
		Params: objectParam(map[string]schemaNode{
			"offset": integerParam(0).described("分页偏移，默认 0；每次最多 100 条。查询更早的记录时使用结果给出的下一页偏移。"),
		}),
		Handle: invoiceStatusHandle,
		Render: invoiceStatusRender,
	}
}

func invoiceStatusHandle(ctx context.Context, req InvoiceStatusRequest, rt ReadRuntime) (InvoiceStatusResponse, ReadResult) {
	raw, err := rt.Executor.Execute(ctx, invoiceStatusAction, map[string]any{"Limit": 100, "Offset": req.Offset})
	if err != nil {
		return InvoiceStatusResponse{}, ReadFailureAfterTool(invoiceStatusAction, invoiceStatusCapabilityLabel, err)
	}
	rows := mapSliceAt(raw, "InvoiceSet")
	items := invoiceStatusItems(raw)
	totalCount := len(items)
	if value, ok := numericField(raw, "TotalCount"); ok && value >= 0 {
		totalCount = int(value)
	}
	if len(items) == 0 {
		if len(rows) > 0 || totalCount > req.Offset {
			return InvoiceStatusResponse{}, ReadFailureAfterTool(
				invoiceStatusAction,
				invoiceStatusCapabilityLabel,
				fmt.Errorf("upstream reported %d invoice records but none could be projected", totalCount),
			)
		}
		reply := "当前账号没有查询到发票记录。"
		if req.Offset > 0 {
			reply = "本页没有查询到发票记录。"
		}
		r := ReadEmpty(reply)
		r.ToolAction = invoiceStatusAction
		env := invoiceStatusEnvelope(nil, totalCount)
		r.Envelope = &env
		return InvoiceStatusResponse{}, r
	}
	return InvoiceStatusResponse{Items: items, TotalCount: totalCount, Offset: req.Offset}, ReadResult{}
}

func invoiceStatusRender(resp InvoiceStatusResponse) ReadResult {
	r := ReadHandled(renderInvoiceStatusReply(resp))
	r.ToolAction = invoiceStatusAction
	env := invoiceStatusEnvelope(resp.Items, resp.TotalCount)
	if next := resp.Offset + len(resp.Items); next < resp.TotalCount {
		env.Computed = append(env.Computed, envelope.Fact{Key: "next_offset", Label: "下一页偏移", Value: next, Source: envelope.FactSourceComputed})
	}
	r.Envelope = &env
	return r
}

func invoiceStatusItems(raw map[string]any) []InvoiceStatusItem {
	rows := mapSliceAt(raw, "InvoiceSet")
	items := make([]InvoiceStatusItem, 0, len(rows))
	for _, rowAny := range rows {
		row, ok := rowAny.(map[string]any)
		if !ok {
			continue
		}
		id, ok := numericField(row, "InvoiceID")
		if !ok || id <= 0 {
			continue
		}
		amount, amountKnown := numericField(row, "InvoiceAmount")
		requestedAt, _ := numericField(row, "RequestTime")
		items = append(items, InvoiceStatusItem{
			ID:                 int(id),
			AmountCents:        int(amount),
			AmountKnown:        amountKnown,
			State:              stringField(row, "InvoiceState"),
			Mode:               stringField(row, "InvoiceMode"),
			Type:               stringField(row, "InvoiceType"),
			RequestTime:        int64(requestedAt),
			RejectReason:       stringField(row, "RejectReason"),
			RefundRejectReason: stringField(row, "RefundRejectReason"),
		})
	}
	return items
}

func invoiceStatusEnvelope(items []InvoiceStatusItem, totalCount int) envelope.Envelope {
	env := envelope.Envelope{
		Kind:          envelope.KindInvoiceStatus,
		SourceActions: []string{invoiceStatusAction},
		Subjects:      make([]envelope.Subject, 0, len(items)),
		Facts:         make([]envelope.Fact, 0, len(items)*5+1),
		Computed:      make([]envelope.Fact, 0, len(items)),
		Constraints:   envelope.Constraints{DoNotAnswerAccountBill: true},
	}
	env.Facts = append(env.Facts, envelope.Fact{
		Key: "invoice_total_count", Label: "发票记录总数", Value: totalCount, Source: envelope.FactSourceAPI,
	})
	for _, item := range items {
		id := strconv.Itoa(item.ID)
		env.Subjects = append(env.Subjects, envelope.Subject{ID: id, Name: "发票 " + id, Type: envelope.SubjectInvoice})
		addInvoiceFact(&env, id, "state", "发票状态", item.State, "")
		if item.AmountKnown {
			addInvoiceFact(&env, id, "amount", "发票金额", item.AmountCents, "分")
		}
		if item.RequestTime > 0 {
			addInvoiceFact(&env, id, "request_time", "申请时间", item.RequestTime, "Unix 秒")
		}
		if item.Mode != "" {
			addInvoiceFact(&env, id, "mode", "发票模式", item.Mode, "")
		}
		if item.Type != "" {
			addInvoiceFact(&env, id, "type", "发票类型", item.Type, "")
		}
		if item.RejectReason != "" {
			addInvoiceFact(&env, id, "reject_reason", "驳回原因", item.RejectReason, "")
		}
		if item.RefundRejectReason != "" {
			addInvoiceFact(&env, id, "refund_reject_reason", "退票驳回原因", item.RefundRejectReason, "")
		}
		if label := invoiceStateLabel(item.State); label != "" {
			env.Computed = append(env.Computed, envelope.Fact{
				SubjectID: id, Key: "state_label", Label: "状态含义", Value: label, Source: envelope.FactSourceComputed,
			})
		}
	}
	return env
}

func addInvoiceFact(env *envelope.Envelope, subjectID, key, label string, value any, unit string) {
	env.Facts = append(env.Facts, envelope.Fact{
		SubjectID: subjectID, Key: key, Label: label, Value: value, Unit: unit, Source: envelope.FactSourceAPI,
	})
}

func renderInvoiceStatusReply(resp InvoiceStatusResponse) string {
	lines := []string{"发票状态："}
	for _, item := range resp.Items {
		parts := []string{invoiceStateDisplay(item.State)}
		if item.RequestTime > 0 {
			parts = append(parts, "申请时间 "+formatInvoiceTime(item.RequestTime))
		}
		if item.AmountKnown {
			parts = append(parts, fmt.Sprintf("金额 ¥%.2f", float64(item.AmountCents)/100))
		}
		lines = append(lines, fmt.Sprintf("- 发票 %d：%s。", item.ID, strings.Join(parts, "，")))
	}
	if resp.TotalCount > len(resp.Items) || resp.Offset > 0 {
		lines = append(lines, fmt.Sprintf("本次显示第 %d–%d 条，共 %d 条。", resp.Offset+1, resp.Offset+len(resp.Items), resp.TotalCount))
		if next := resp.Offset + len(resp.Items); next < resp.TotalCount {
			lines = append(lines, fmt.Sprintf("后续记录可继续查询，下一页 offset=%d。", next))
		}
	}
	return strings.Join(lines, "\n")
}

func invoiceStateDisplay(state string) string {
	label := invoiceStateLabel(state)
	if label == "" {
		if strings.TrimSpace(state) == "" {
			return "上游未返回状态"
		}
		return state
	}
	return label + "（" + state + "）"
}

func invoiceStateLabel(state string) string {
	switch strings.TrimSpace(state) {
	case "New":
		return "新建"
	case "Accept":
		return "自动开票中"
	case "Sended":
		return "已邮寄"
	case "Finished":
		return "已完成"
	case "Canceled":
		return "已取消"
	case "Reviewing":
		return "人工审核中"
	case "Rejected":
		return "已驳回"
	case "InvoiceIssued":
		return "开具完成，待邮寄"
	case "RefundNew":
		return "退票申请中"
	case "RefundReviewing":
		return "退票人工审核中"
	case "RefundAccept":
		return "人工退票审核中"
	case "RefundFinished":
		return "退票已完成"
	case "RefundReject":
		return "退票已驳回"
	default:
		return ""
	}
}

func formatInvoiceTime(unixSeconds int64) string {
	china := time.FixedZone("UTC+8", 8*60*60)
	return time.Unix(unixSeconds, 0).In(china).Format("2006-01-02 15:04")
}
