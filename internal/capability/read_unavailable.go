package capability

import (
	"context"

	"github.com/compshare-agent/internal/platform"
)

// UnavailableCapabilitySpec declares a capability the platform does not currently
// support in real time — e.g. account balance, total bill, spending ledger or
// invoice status. It is exposed to the model as a read tool (same surface as a
// real read capability) so a finance question reaches a deterministic, non-
// fabricated answer instead of the model inventing numbers. Invoking it never
// calls a real upstream API and never depends on keyword routing or an online
// verifier: it returns a structured Unavailable status plus the supported
// alternatives the model should redirect to.
type UnavailableCapabilitySpec struct {
	// Name is the tool-name suffix + observation label (e.g. "account_finance_status").
	Name string
	// Description is the model-facing tool description; it states the capability
	// is not available for real-time query so the model can decline up front.
	Description string
	// Reply is the deterministic user-facing explanation.
	Reply string
	// Alternatives are the supported capabilities to redirect the user to.
	Alternatives []string
}

// unavailableRequest is the (empty) request type of every Unavailable capability:
// the tool takes no parameters — asking about it is the whole input.
type unavailableRequest struct{}

func (unavailableRequest) MissingFields() []platform.MissingField { return nil }

// NewUnavailableCapability erases an UnavailableCapabilitySpec into the same
// RegisteredRead shape as a read capability, so tool exposure, decode and
// dispatch reuse the read plumbing. Its handler ignores the runtime and returns
// a terminal ReadUnavailable result — Render is never reached.
func NewUnavailableCapability(spec UnavailableCapabilitySpec) RegisteredRead {
	return NewReadCapability(ReadCapabilitySpec[unavailableRequest, struct{}]{
		Label:       spec.Name,
		Description: spec.Description,
		Params:      objectParam(nil),
		Handle: func(context.Context, unavailableRequest, ReadRuntime) (struct{}, ReadResult) {
			return struct{}{}, ReadUnavailable(spec.Reply, spec.Alternatives)
		},
		Render: func(struct{}) ReadResult { return ReadResult{} },
	})
}

// accountFinanceStatusCapability is the tool/observation identity of the
// account-finance Unavailable capability.
const accountFinanceStatusCapability = "account_finance_status"

// accountFinanceUnavailableSpec is the account-level financial data capability:
// balance / bill / ledger / invoice real-time queries are not supported, and the
// answer redirects to the supported per-instance price / cost / refund reads.
func accountFinanceUnavailableSpec() UnavailableCapabilitySpec {
	return UnavailableCapabilitySpec{
		Name:        accountFinanceStatusCapability,
		Description: "查询账号余额、账号总账单、消费流水或发票状态——这类账号级实时财务数据当前不支持查询，调用会返回不可用说明与可替代能力。",
		Reply:       "当前不支持直接查询账号余额、账号总账单、消费流水、发票状态、余额提现等账号级财务数据。你可以在控制台费用中心查看这些信息，或联系人工客服确认。\n\n我可以继续帮你查询实例价格、实例费用诊断、资源退费估算等已支持内容。",
		Alternatives: []string{
			"实例价格查询（pricing_query）",
			"实例费用诊断（billing 诊断）",
			"资源退费估算（refund_estimate）",
		},
	}
}
