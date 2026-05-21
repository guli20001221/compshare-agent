package guardrails

import (
	"regexp"
)

// Topic-policy detection — recognises clearly off-topic asks at the
// engine input boundary and returns a redirect-style refusal that names
// the appropriate professional channel. Distinct from PII (data
// boundary), output leak (post-LLM), and jailbreak (instruction
// override) — this layer governs *subject matter*, not data or
// instructions.
//
// Design choice — conservative-compound, not topical keyword lists:
//   - A bare keyword ("总统" / "doctor" / "比特币" / "焦虑") appears
//     in many benign platform questions ("总统大选导致 GPU 短缺吗?",
//     "doctor 这个程序为啥跑不起来", "比特币挖矿用什么 GPU?").
//   - Each pattern therefore requires BOTH an advice-seeking verb
//     (推荐 / 评价 / 我得了 / opinion on / should I) AND a clearly
//     off-platform topic noun (总统 / cancer / 股票). One alone never
//     trips.
//
// Why include suicide-ideation here rather than a separate category:
//   - Detection language is structurally identical (verb + topic).
//   - Refusal redirects to professional help, same shape as other
//     off-topic cases. A separate category would justify only if the
//     reply text had to differ materially (e.g. an explicit hotline);
//     we deliberately keep the reply generic to avoid stale hotline
//     numbers in code.
//
// Industry parallels:
//   - AWS Bedrock denied-topic guardrails: enumerate topics + return
//     canned redirect.
//   - OpenAI moderation: structurally similar lexical layer in front
//     of the moderation model.
//   - Anthropic policy: redirect off-topic to "I can help with X, not Y".
//
// Known false-negative surface (acceptable for v1):
//   - Sophisticated phrasing that strips verb-topic adjacency
//     ("the political scene is shifting — what do you think about it")
//     would not match — the noun "political scene" passes but the
//     pattern requires verb adjacency to the canonical political noun.
//   - Suicide ideation phrased indirectly ("I don't want to be here
//     anymore") would not match — explicit phrases only.
//   - Off-topic categories not in this corpus: occult / supernatural /
//     romantic relationship advice / historical political debates.
//   - Phase B could swap to LLM classification if FN/FP economics
//     justify; mirrors the Phase A choice in jailbreak.go.

var offTopicPatterns = []detectionPattern{
	// Chinese political opinion-seeking. Requires opinion verb AND
	// political-figure/topic noun. "国家政策很重要" alone doesn't trip
	// (no opinion verb). "支持哪个 GPU" alone doesn't trip (no
	// political noun).
	{
		name:     "zh_political_opinion",
		verbRe:   regexp.MustCompile(`(评价|你觉得|怎么看|看法|意见|支持|反对|拥护|批评)`),
		domainRe: regexp.MustCompile(`(国家领导人|党中央|总书记|主席|总统|总理|首相|执政党|政党|政府机构|选举|大选|两党|民主党|共和党|共产党|国民党|台独|港独|疆独|藏独|新疆议题|台湾问题|涉港议题|涉疆议题|涉藏议题)`),
	},
	// English political opinion-seeking.
	{
		name:     "en_political_opinion",
		verbRe:   regexp.MustCompile(`(?i)\b(opinion|view|stance|position|support|oppose|endorse|criticize|criticise|think)\b`),
		domainRe: regexp.MustCompile(`(?i)\b(president|prime\s+minister|chancellor|head\s+of\s+state|ruling\s+party|political\s+party|democrat|republican|elections?|impeach|sanctions?\s+on|Xi\s+Jinping|Putin|Trump|Biden|Taiwan\s+(independence|issue|question)|Hong\s+Kong\s+protests?|Tibet\s+issue)\b`),
	},
	// Chinese personal medical advice. Requires self/family subject +
	// illness or symptom + advice-seeking phrase. "怎么治疗 GPU 过热"
	// fails because no self/family subject. "我得了感冒" alone
	// (without 怎么办/吃药) doesn't trip.
	{
		name:     "zh_medical_advice",
		verbRe:   regexp.MustCompile(`(我|我家|我老婆|我老公|我爸|我妈|我儿子|我女儿|我孩子|父母|家人)\s*(得了|患了|有了|发现|查出|确诊|被诊断|生病|发烧|咳嗽|疼痛|心脏|肿瘤|癌症|糖尿病|高血压|新冠|肺炎|感染|过敏)`),
		domainRe: regexp.MustCompile(`(怎么办|该怎么|吃什么药|要不要|要去|去哪|挂号|看医生|去医院|治疗方案|手术|化疗|放疗|住院|开刀)`),
	},
	// English personal medical advice. Tightened (PR #155 review N2):
	//   - Subject ↔ disease adjacency capped at 0-2 intervening words
	//     so "My family member has cancer and I want to run a research
	//     model on 4090" no longer trips (>2 words between subject and
	//     disease).
	//   - Domain "what medicine should I take" qualified with a follow-
	//     up symptom/treatment noun so "what medication should I take
	//     to stay focused" no longer matches.
	{
		name:     "en_medical_advice",
		verbRe:   regexp.MustCompile(`(?i)\b(I|my\s+(family|wife|husband|child|son|daughter|parent|father|mother|kid))\b`),
		domainRe: regexp.MustCompile(`(?i)\b(have|has|got|am\s+sick\s+with|been\s+diagnosed|diagnosed\s+with)\s+(\w+\s+){0,2}(cancer|tumou?r|diabetes|hypertension|covid|pneumonia|infection|allergy)\b|what\s+(medicine|medication|pill|drug)\s+should\s+I\s+take\s+(for|to\s+(treat|cure|relieve|manage))\s+\w+`),
	},
	// Chinese investment recommendation-seeking. "推荐买什么股票" → trip
	// (allows up to 5 non-punct chars between buy-verb and finance noun
	// to accept "买什么股票" / "买点基金"). "推荐用 4090" → no trip
	// because "4090" / "GPU" / "卡" not in finance noun list.
	{
		name:     "zh_investment_advice",
		verbRe:   regexp.MustCompile(`(推荐|建议|该不该|要不要|帮我选|挑一个|哪个好|怎么选)`),
		domainRe: regexp.MustCompile(`(买|入手|持有|抛|卖|清仓|建仓|加仓|减仓|定投)[^。.,;!?]{0,5}(股票|基金|期货|加密货币|比特币|以太|狗狗币|理财产品|外汇|证券)|(股票|基金|币圈|加密货币)\s*(推荐|建议|怎么选|该不该|哪个好)`),
	},
	// English investment recommendation-seeking. domainRe accepts three
	// shapes (PR #155 review N3 extends the third):
	//   1. "(buy|sell|...) ... stock"      ← "should I buy bitcoin"
	//   2. "a stock to (buy|invest|...)"  ← "recommend a stock to invest in"
	//   3. "(hot\s+)?(pick|tip|recommendation)s? + (stocks?|crypto|...)" ←
	//      "recommend hot pick stocks" / "give me a stock tip"
	{
		name:     "en_investment_advice",
		verbRe:   regexp.MustCompile(`(?i)\b(should\s+I|recommend|advise|tip|hot\s+pick|what\s+to|give\s+me)\b`),
		domainRe: regexp.MustCompile(`(?i)\b(buy|sell|short|long|hold|invest\s+in)\s+(\w+\s+)*(stocks?|crypto|bitcoin|ethereum|fund|etf|options|forex|gold|silver|bond)\b|\ba\s+(stocks?|crypto|bitcoin|ethereum|fund|etf|options|forex|bond)\s+to\s+(buy|invest|sell|hold|short|long)\b|\b(hot\s+)?(pick|tip|recommendation)s?\s+(for\s+)?\w*\s*(stocks?|crypto|bitcoin|ethereum|fund|etf)\b|\b(stocks?|crypto|bitcoin|ethereum|fund|etf|options|forex)\s+(pick|tip|recommendation)s?\b`),
	},
	// Chinese severe-emotional-distress (suicide ideation). Strong-
	// signal words only — generic phrases like "想死" / "活不下去"
	// appear frequently as programmer-frustration hyperbole ("这 bug
	// 我修不动了想死" / "实例挂了我活不下去了") and would carry an
	// unacceptable false-positive rate. The remaining strong-signal
	// list covers the explicit-intent vocabulary; a Phase B LLM
	// classifier would be the right place to catch indirect phrasings
	// without burning users.
	{
		name:     "zh_self_harm",
		verbRe:   regexp.MustCompile(`(我|没人|没有人爱|没人在乎)`),
		domainRe: regexp.MustCompile(`(想自杀|想结束生命|想结束自己|寻短|想割腕|想跳楼|想跳河|要自杀|要结束生命|要寻短)`),
	},
	// English severe-emotional-distress. Same conservative-compound
	// shape: strong-signal verbs only, generic "die"/"don't want to
	// live" filtered out.
	{
		name:     "en_self_harm",
		verbRe:   regexp.MustCompile(`(?i)\b(I|me)\b`),
		domainRe: regexp.MustCompile(`(?i)\b(want\s+to\s+(commit\s+suicide|kill\s+myself|end\s+my\s+life)|am\s+suicidal|am\s+going\s+to\s+(kill|harm)\s+myself|planning\s+to\s+(end\s+my\s+life|kill\s+myself))\b|\bsuicide\s+hotline\b|\bself[-\s]harm\s+(plan|intent)\b`),
	},
}

// DetectOffTopic returns true if the message matches one of the
// off-topic categories (political opinion, personal medical advice,
// investment recommendation, severe emotional distress). Empty input
// returns false. Reuses prepareForDetection (jailbreak.go) for trim +
// UTF-8 boundary fix + full-width punctuation normalisation.
func DetectOffTopic(s string) bool {
	s = prepareForDetection(s)
	if s == "" {
		return false
	}
	for _, p := range offTopicPatterns {
		if p.verbRe.MatchString(s) && p.domainRe.MatchString(s) {
			return true
		}
	}
	return false
}
