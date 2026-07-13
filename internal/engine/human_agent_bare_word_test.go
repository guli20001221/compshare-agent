package engine

import (
	"testing"

	"github.com/compshare-agent/internal/refusal"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A production turn the lead flagged is two characters long: 「人工」. The user got a
// generic reply, not the transfer QR code — and asked, reasonably, 「人工的处理不是应该
// 已经引入了吗？」 It was: humanAgentTransferKeywords lists 转人工 / 转接人工 / 人工客服 /
// 联系人工 / 找人工 / 叫人工, and the bare word is in none of them.
//
// The omission was deliberate, and the reason is good: 人工 as a SUBSTRING also appears in
// 人工智能 / 人工费 / 人工成本, none of which asks for a human, so matching the substring
// would fire the QR code at users discussing AI. The fix must therefore not widen the
// substring match — it scopes to the WHOLE message, where 人工 has exactly one meaning.
//
// This test drives the real preblock CHAIN, not the predicate. The human-agent rule runs
// LAST (off-topic and jailbreak run first), so a predicate-only test would pass while an
// earlier rule quietly swallowed 「人工」 as off-topic and the user still never saw the QR.
func TestPreBlock_BareHumanAgentWordTransfersToHuman(t *testing.T) {
	t.Run("the bare word reaches the human-agent rule", func(t *testing.T) {
		for _, msg := range []string{"人工", "人工？", "人工!", " 人工 ", "人工。"} {
			d := enginePreBlock.Decide(msg)
			require.True(t, d.Matched, "%q must be intercepted before the LLM", msg)
			assert.Equal(t, refusal.CategoryHumanAgent, d.Category,
				"%q must reach the human-agent rule — if this says off_topic, an EARLIER rule in the chain stole it and the user still never sees the QR code", msg)
			assert.Equal(t, refusal.HumanAgentTransfer, d.Reply, "%q must get the QR-code reply", msg)
		}
	})

	// The guard the narrow whitelist existed to provide, still standing. If this breaks, the
	// fix widened the substring match and every user asking about AI gets a customer-service
	// QR code.
	t.Run("人工 as a substring must NOT transfer", func(t *testing.T) {
		for _, msg := range []string{
			"人工智能现在能做什么",
			"这个要额外收人工费吗",
			"人工成本怎么算",
			"人工智能",
		} {
			d := enginePreBlock.Decide(msg)
			if d.Matched {
				assert.NotEqual(t, refusal.CategoryHumanAgent, d.Category,
					"%q contains 人工 but does not ask for a human — the QR code must not fire", msg)
			}
		}
	})

	// The explicit phrases keep working — this fix is additive, not a replacement.
	t.Run("explicit transfer phrases still transfer", func(t *testing.T) {
		for _, msg := range []string{"转人工", "我要人工客服", "帮我联系人工"} {
			d := enginePreBlock.Decide(msg)
			require.True(t, d.Matched, "%q must still be intercepted", msg)
			assert.Equal(t, refusal.CategoryHumanAgent, d.Category, "%q must still transfer", msg)
		}
	})
}
