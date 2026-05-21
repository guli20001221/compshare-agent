package engine

import (
	"testing"

	"github.com/compshare-agent/internal/refusal"
	"github.com/stretchr/testify/assert"
)

// TestEnginePreBlock_OffTopicRuleFires confirms that the off-topic
// detection rule (registered second in the chain) returns the canonical
// refusal text + category for known off-topic phrases, and that benign
// platform queries fall through unchanged.
func TestEnginePreBlock_OffTopicRuleFires(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantMatch   bool
		wantCat     string
		wantReply   string
	}{
		{"political", "你怎么看现任总统的政策", true, refusal.CategoryOffTopic, refusal.OffTopic},
		{"medical", "我得了糖尿病该怎么办", true, refusal.CategoryOffTopic, refusal.OffTopic},
		{"investment", "推荐买什么股票", true, refusal.CategoryOffTopic, refusal.OffTopic},
		{"selfharm", "我想自杀", true, refusal.CategoryOffTopic, refusal.OffTopic},
		{"benign-pricing", "4090 多少钱一小时", false, "", ""},
		{"benign-debug-hyperbole", "这 bug 我修不动了想死", false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := enginePreBlock.Decide(tc.input)
			if tc.wantMatch {
				assert.True(t, d.Matched, "expected match for %q", tc.input)
				assert.Equal(t, tc.wantCat, d.Category)
				assert.Equal(t, tc.wantReply, d.Reply)
			} else {
				assert.False(t, d.Matched, "expected no match for %q", tc.input)
			}
		})
	}
}
