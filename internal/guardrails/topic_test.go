package guardrails

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDetectOffTopic_True_PoliticalOpinion locks Chinese + English
// political-opinion patterns. Requires opinion verb AND political
// noun; bare keywords don't trip.
func TestDetectOffTopic_True_PoliticalOpinion(t *testing.T) {
	cases := []string{
		"你怎么看现任总统的政策",
		"我支持哪个执政党",
		"你对台湾问题怎么看",
		"评价一下美国总统",
		"What's your opinion on the president",
		"Do you support the ruling party",
		"What do you think about the Taiwan independence issue",
	}
	for _, msg := range cases {
		t.Run(msg[:min(len(msg), 12)], func(t *testing.T) {
			assert.True(t, DetectOffTopic(msg),
				"expected detection on %q", msg)
		})
	}
}

// TestDetectOffTopic_True_MedicalAdvice locks personal medical advice
// patterns. Requires self/family subject + symptom + advice phrase.
func TestDetectOffTopic_True_MedicalAdvice(t *testing.T) {
	cases := []string{
		"我得了糖尿病该怎么办",
		"我家人发烧了去哪挂号",
		"我儿子咳嗽要不要看医生",
		"我老公被诊断为高血压治疗方案是什么",
		"I have cancer what medicine should I take",
		"My family has been diagnosed with diabetes what medication should I take",
	}
	for _, msg := range cases {
		t.Run(msg[:min(len(msg), 12)], func(t *testing.T) {
			assert.True(t, DetectOffTopic(msg),
				"expected detection on %q", msg)
		})
	}
}

// TestDetectOffTopic_True_InvestmentAdvice locks investment-
// recommendation patterns.
func TestDetectOffTopic_True_InvestmentAdvice(t *testing.T) {
	cases := []string{
		"推荐买什么股票",
		"建议入手哪个基金",
		"该不该卖比特币",
		"币圈怎么选",
		"Should I buy bitcoin",
		"Recommend a stock to invest in",
	}
	for _, msg := range cases {
		t.Run(msg[:min(len(msg), 12)], func(t *testing.T) {
			assert.True(t, DetectOffTopic(msg),
				"expected detection on %q", msg)
		})
	}
}

// TestDetectOffTopic_True_SelfHarm locks severe emotional distress
// patterns. Strong-signal phrases only — generic "want to die" /
// "活不下去" filtered out to avoid hyperbole false positives.
func TestDetectOffTopic_True_SelfHarm(t *testing.T) {
	cases := []string{
		"我想自杀",
		"我想结束生命",
		"我要自杀了",
		"我想跳楼",
		"I want to kill myself",
		"I want to commit suicide",
		"I am planning to end my life",
		"I am suicidal",
	}
	for _, msg := range cases {
		t.Run(msg[:min(len(msg), 12)], func(t *testing.T) {
			assert.True(t, DetectOffTopic(msg),
				"expected detection on %q", msg)
		})
	}
}

// TestDetectOffTopic_False_BenignPlatform guards against over-eager
// refusal on legitimate platform usage. Exhaustively covers benign
// phrasings that share keywords with the off-topic patterns.
func TestDetectOffTopic_False_BenignPlatform(t *testing.T) {
	cases := []string{
		"",
		// Political keyword without opinion verb.
		"国家政策影响了 GPU 短缺吗",
		"总统大选会影响算力价格吗",
		// Opinion verb without political noun.
		"你怎么看 4090 和 5090 的差异",
		"我支持的 GPU 是 4090",
		// Medical keywords without self/family + symptom.
		"怎么治疗 GPU 过热",
		"实例感染病毒了怎么办",
		"医生用的 ML 模型推理用哪个 GPU 好",
		// Self subject but unrelated to medical.
		"我的实例咋这么慢",
		"我儿子在用 4090 跑游戏",
		// Investment keywords without recommend verb pointing at finance.
		"4090 怎么买",
		"我买了 4090 卡",
		"推荐用 4090 跑 LoRA",
		"币圈用什么 GPU 挖矿",  // crypto-mining is a legitimate GPU use case
		"比特币挖矿用什么 GPU", // same
		// Self-harm-shaped hyperbole that's actually frustration.
		"这 bug 我修不动了想死",   // contains "想死" but typed in frustration about debugging
		"实例挂了我活不下去了",     // hyperbole
		"再写一行 yaml 我就要死了", // hyperbole
		// Common benign questions.
		"4090 多少钱一小时",
		"4090 显存多大",
		"推荐用什么 GPU 跑 LoRA",
		"我账单怎么这么高",
		"我有哪些实例",
		"What does error code 226601 mean",
	}
	for _, msg := range cases {
		t.Run(strings.ReplaceAll(msg, " ", "_"), func(t *testing.T) {
			assert.False(t, DetectOffTopic(msg),
				"benign platform message must NOT be flagged: %q", msg)
		})
	}
}

// TestDetectOffTopic_KnownFP_EnglishMedicalMixedPlatform locks the
// behavior of mixed-intent English messages where the user mentions a
// medical condition AND a platform topic ("research model on 4090") in
// one turn. Current behavior: trips medical (FP). PR #155 review N2
// flagged this — RE2 cannot do message-level platform-noun lookaround
// so the proper fix is a separate prepareForDetection-style escape
// hatch deferred to a follow-up commit. This test locks current
// behavior so the fix has an explicit assertion to flip.
func TestDetectOffTopic_KnownFP_EnglishMedicalMixedPlatform(t *testing.T) {
	knownFP := []string{
		"My family member has cancer and I want to run a research model on 4090",
		"My mother has diabetes — can I use compshare to run insulin-dosing research",
		"I have diabetes, what GPU do you recommend for ML research",
	}
	for _, msg := range knownFP {
		t.Run(strings.ReplaceAll(msg[:min(len(msg), 30)], " ", "_"), func(t *testing.T) {
			assert.True(t, DetectOffTopic(msg),
				"KNOWN FP — when the platform-noun escape hatch lands, flip this to assert.False and remove the assertion: %q", msg)
		})
	}
}

// TestDetectOffTopic_FullWidthPunctuation locks normalisation against
// full-width-punct evasion (consistent with DetectJailbreakAttempt).
func TestDetectOffTopic_FullWidthPunctuation(t *testing.T) {
	cases := []string{
		"我想自杀。",
		"推荐买什么股票？",
	}
	for _, msg := range cases {
		t.Run(msg[:min(len(msg), 12)], func(t *testing.T) {
			assert.True(t, DetectOffTopic(msg),
				"full-width punctuation must not evade detection: %q", msg)
		})
	}
}

// TestDetectOffTopic_Idempotent guards against accidental state in
// detection patterns.
func TestDetectOffTopic_Idempotent(t *testing.T) {
	msg := "我想自杀"
	assert.Equal(t, DetectOffTopic(msg), DetectOffTopic(msg))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
