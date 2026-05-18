package engine

import "testing"

// TestIsAccountBillingUnsupportedNormalized_ThirdPartyServiceExempt covers
// the #34b false-positive scoping rule (2026-05-18): when a normalized user
// message references a known third-party service by name, the
// account-billing hard-block must NOT fire. The corpus covers these
// services (e.g. modelverse 18 chunks in w0); the planner+RAG path should
// answer instead of the canned account-financial-center redirect.
func TestIsAccountBillingUnsupportedNormalized_ThirdPartyServiceExempt(t *testing.T) {
	// Inputs here are already in normalizeMsg output form: ASCII lowercased,
	// internal whitespace collapsed, CJK preserved.
	cases := []string{
		"modelverse 的余额怎么查",
		"在哪里看 modelverse 余额",
		"怎么充 modelverse 余额",
		"openai 余额怎么查",
		"open ai 账单查询",
		"deepseek api 余额",
		"deep seek 总账单",
		"anthropic 退款进度",
		"火山引擎余额",
		"豆包余额还剩多少",
		"volcano 账单",
	}
	for _, n := range cases {
		t.Run(n, func(t *testing.T) {
			if isAccountBillingUnsupportedNormalized(n) {
				t.Errorf("third-party-service query %q wrongly classified as account-billing hard-block; should reach planner", n)
			}
		})
	}
}

// TestIsAccountBillingUnsupportedNormalized_CompShareAccountStillBlocks
// guards the inverse: queries about the user's CompShare account that
// hit the engine pre-planner keyword guard (not planner-classified ones)
// must still hard-block. The exemption must not over-fire and break the
// legitimate account-financial-center path.
//
// NOTE: queries like "我账户里还有多少钱" do NOT hit the engine guard
// (no 余额/总账单/etc keyword match); they are classified as
// billing_account_unsupported by the planner LLM. Those cases are
// covered by planner integration tests, not this file.
func TestIsAccountBillingUnsupportedNormalized_CompShareAccountStillBlocks(t *testing.T) {
	cases := []string{
		"账户余额还剩多少",
		"查本月总账单",
		"本月消费明细",
		"扣费记录",
		"交易记录",
		"待支付账单",
		"订单状态",
	}
	for _, n := range cases {
		t.Run(n, func(t *testing.T) {
			if !isAccountBillingUnsupportedNormalized(n) {
				t.Errorf("CompShare account query %q should be account-billing hard-block but was not", n)
			}
		})
	}
}

// TestIsAccountBillingUnsupportedNormalized_MixedScopeFavorsPlanner: when
// a query mentions both a third-party service AND a CompShare-only keyword,
// the scoping rule favors letting the planner disambiguate. Planner can
// then ask the user to clarify which account they mean, rather than the
// engine making a unilateral hard-block decision.
func TestIsAccountBillingUnsupportedNormalized_MixedScopeFavorsPlanner(t *testing.T) {
	cases := []string{
		"modelverse 余额和我账户余额都查一下",
		"openai 账单和 compshare 账单",
	}
	for _, n := range cases {
		t.Run(n, func(t *testing.T) {
			if isAccountBillingUnsupportedNormalized(n) {
				t.Errorf("mixed-scope query %q should let planner disambiguate, not hard-block", n)
			}
		})
	}
}
