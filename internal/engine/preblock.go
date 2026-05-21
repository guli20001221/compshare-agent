package engine

import (
	"github.com/compshare-agent/internal/refusal"
	"github.com/compshare-agent/internal/router"
)

// enginePreBlock is the package-level Chat()-head decision chain. Static
// because the rule list is stateless data; per-tenant rule overlays
// (when needed) should construct a fresh router.PreBlock at session
// scope rather than mutating this singleton.
//
// Order = evaluation order. Keyword sets are disjoint by construction
// (余额/账单/财务 vs 226604/资源不足 vs monitor-history regex), so the
// ordering does not affect correctness for any current real input —
// but it is preserved for trace stability and to keep ranking explicit
// if a future rule introduces overlap.
//
// When adding a new rule:
//
//  1. Add the Category + reply text in internal/refusal/templates.go.
//  2. Implement the predicate in this package (engine_*.go helpers).
//  3. Append a router.Rule literal here.
//  4. Update the priority-chain comment block above Engine.Chat().
//
// post-LLM hard-blocks (currently only cited_contract_violation in
// Chat()) are intentionally NOT routed through this chain — they are
// structurally different (run AFTER the LLM produces text, not BEFORE).
// When ≥2 post-LLM rules exist, factor them out to a sibling
// internal/policy/postblock.go using the same router.Rule pattern.
var enginePreBlock = router.New(
	router.Rule{
		Match:    isAccountBillingUnsupported,
		Category: refusal.CategoryAccountBilling,
		Reply:    refusal.AccountBillingUnsupported,
	},
	router.Rule{
		Match:    isResourceShortageQuestion,
		Category: refusal.CategoryResourceShortage,
		Reply:    refusal.ResourceShortage226604,
	},
	router.Rule{
		Match:    isUnsupportedHistoricalMonitorQuestion,
		Category: refusal.CategoryMonitorHistory,
		Reply:    refusal.MonitorHistoryUnsupported,
	},
)
