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

// TestIsAccountBillingUnsupportedNormalized_FAQProcessExempt covers #52
// (2026-05-19): finance FAQ / process questions like "我的发票什么时候开"
// (h03) or "下载速度突然变慢 是欠费了吗 还是网络高峰" (mq05) were 4-mode
// hard-blocked in Lane B.5c because of the bare 发票/欠费 keyword matches.
// The 3-condition AND exemption (topic + process-marker + NOT realtime-
// account) lets these reach the planner/RAG path where the FAQ corpus can
// answer the process/schedule/diagnostic. See engine.go isFinanceFAQ
// ProcessQuestion for the rule structure.
//
// The 3-condition gate is intentionally strict — single markers are too
// wide (e.g. "账户余额怎么查" has 怎么 but should still hard-block because
// 余额 is account-data, not a finance-FAQ topic in this taxonomy).
func TestIsAccountBillingUnsupportedNormalized_FAQProcessExempt(t *testing.T) {
	cases := []string{
		"我的发票什么时候开",                              // h03 — 发票 + 什么时候 + 我的(but 我的 不在 realtime-marker)
		"发票什么时候开",                                       // h03 variant — pure process
		"开票周期多久",                                          // explicit period question
		"退款流程是怎样的",                                    // refund process
		"欠费会影响下载吗",                                    // diagnostic phrasing
		"欠费几天回收",                                          // arrears policy
		"包月到期怎么续费",                                    // expiry rule + process marker
		"下载速度突然变慢 是欠费了吗 还是网络高峰", // mq05 — diagnostic
	}
	for _, n := range cases {
		t.Run(n, func(t *testing.T) {
			if isAccountBillingUnsupportedNormalized(n) {
				t.Errorf("FAQ/process query %q wrongly hardblocked; should reach planner/RAG", n)
			}
		})
	}
}

// TestIsAccountBillingUnsupportedNormalized_FAQExemptDoesNotWeakenBalanceBlock
// is the inverse guard for #52: queries about the user's specific balance /
// flow data must still hard-block even when they contain a process marker.
// The 3-condition gate accomplishes this via two paths:
//   - 余额 / 流水 / 账单明细 / 扣费记录 etc. are accountOnlyDataKeywords
//     (NOT in financeFAQTopicWords) so condition 1 fails and the FAQ
//     exemption never fires.
//   - Even if a query mixes a FAQ topic with a personal-status marker
//     (我的 / 账上 / 还剩 / 开好了吗 / 进度), condition 3 vetoes the
//     exemption.
//
// Without these guards, "账户余额怎么查" would slip through and break
// the financial-data hard-block contract.
func TestIsAccountBillingUnsupportedNormalized_FAQExemptDoesNotWeakenBalanceBlock(t *testing.T) {
	// These cases must still hard-block at the engine layer (i.e., reach
	// accountBillingUnsupportedReply without going through the planner)
	// because each one hits accountOnlyDataKeywords / invoiceRealtime /
	// refundRealtime / arrearsRealtime / etc. The FAQ-process exemption
	// (#52) must NOT swallow them.
	//
	// Note: queries like "账上还剩多少" or "我账户里还有多少钱" do not
	// hit the engine guard (no specific account-data keyword) and rely on
	// the planner LLM for the hard-block route — those are covered by
	// TestBuildSystemPromptPR52FAQProcessVsPersonalStatus in the intent
	// package, not here.
	cases := []string{
		"账户余额怎么查",       // 余额 ∈ accountOnlyDataKeywords; 怎么 doesn't exempt
		"我账户余额多少",       // 余额 ∈ accountOnlyDataKeywords
		"我的发票开好了吗",  // invoice + 开好 + 了吗 → containsInvoiceRealtimeQuestion
		"我现在欠费多少",       // 欠费 + 多少 → containsArrearsRealtimeQuestion
		"退款进度到哪了",       // 退款 + 进度 → containsRefundRealtimeQuestion
		"本月一共花了多少",  // 本月 + 花了 (no instance scope) → monthly summary branch
	}
	for _, n := range cases {
		t.Run(n, func(t *testing.T) {
			if !isAccountBillingUnsupportedNormalized(n) {
				t.Errorf("balance/personal-status query %q must still hard-block; FAQ exemption over-fired", n)
			}
		})
	}
}

// TestIsFinanceFAQProcessQuestion locks the 3-condition AND semantics
// directly on the helper, isolated from the broader hard-block path.
// This makes failure cause-of-error visible (which of topic / marker /
// realtime-veto failed) and protects against silent threshold drift if
// the lists are reordered or extended.
func TestIsFinanceFAQProcessQuestion(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantTrue bool
	}{
		// 3 conditions hit: topic + marker + NO realtime → exempt.
		{"h03 process question", "发票什么时候开", true},
		{"refund process flow", "退款流程是怎样的", true},
		{"arrears diagnostic", "欠费会影响下载吗", true},
		{"package expiry process", "包月到期怎么续费", true},
		// missing topic → not exempt.
		{"no topic just marker", "这怎么办", false},
		{"余额是 account-data 不是 FAQ topic", "账户余额怎么查", false},
		// missing marker → not exempt.
		{"topic but no process marker", "发票相关问题", false},
		// realtime marker veto.
		{"我的 vetoes exemption", "我的发票开好了吗", false},
		{"账上 vetoes exemption", "账上还剩多少欠费", false},
		{"我现在 vetoes exemption", "我现在欠费多少", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isFinanceFAQProcessQuestion(c.input)
			if got != c.wantTrue {
				t.Errorf("isFinanceFAQProcessQuestion(%q) = %v; want %v", c.input, got, c.wantTrue)
			}
		})
	}
}
