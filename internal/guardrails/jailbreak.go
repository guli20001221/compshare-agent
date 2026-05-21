package guardrails

import (
	"regexp"
	"strings"
)

// Jailbreak detection — recognises common instruction-override and
// system-prompt-extraction patterns at the engine input boundary so the
// agent can return a canned on-topic refusal instead of forwarding the
// payload to the LLM. Distinct from PII redaction (replaces tokens) and
// output-leak redaction (post-LLM): this layer does NOT mutate the
// message; the caller short-circuits on a positive detection.
//
// Design choice — pattern matching, not LLM classification:
//   - Deterministic, cheap, observable per-pattern hit counts.
//   - Mirrors industry guardrails (AWS Bedrock denied-topic patterns,
//     OpenAI moderation lexical pre-filter, Anthropic abuse heuristics)
//     that gate before a model call.
//   - LLM classification of jailbreak intent is more accurate but adds
//     a round-trip + cost on every user turn; left as a Phase B option
//     if pattern coverage proves insufficient.
//
// Detection is **conservative by composition**: each pattern requires
// BOTH an override verb (ignore/disregard/forget/print/reveal/扮演/忽略/
// 显示/无视) AND an instruction-domain noun (instructions/system prompt/
// rules/指令/系统提示词/规则). A single noun in isolation does not
// trip — "忽略这个错误" / "ignore this typo" / "你的提示词" pass through.
// This biases towards false-negatives over false-positives, since the
// cost of an over-eager refusal on a benign platform question is high.
//
// Known false-negative surface (acceptable per ticket):
//   - Multi-turn social-engineering (build trust, then attack) — pattern
//     match only inspects a single message; no cross-turn state.
//   - Encoded payloads (base64, URL-encoded, ROT13, character-by-
//     character spelling) — left for a follow-up that normalizes
//     suspected encodings before pattern check.
//   - Novel jailbreak templates not represented in the corpus below —
//     pattern is intentionally narrow; broad fuzzy match would
//     over-trigger on benign platform language.

// detectionPattern is one (verb token, domain noun token) compound. A
// single message must contain BOTH (in either order, within any
// distance) for the pattern to fire. Encoded as a regex that requires
// both substrings via lookahead-free alternation + a second pass; keeps
// it readable and avoids regex catastrophic backtracking.
type detectionPattern struct {
	// name is the structured ID written to trace.guardrails.matched_pattern
	// if/when we add per-pattern observability. Today the engine-side
	// trace only logs the category, but per-pattern counts are the next
	// useful slice for tuning.
	name string

	// verbRe matches the override action.
	verbRe *regexp.Regexp

	// domainRe matches the instruction-domain noun. BOTH verbRe and
	// domainRe must match the same message (no positional constraint).
	domainRe *regexp.Regexp
}

var jailbreakPatterns = []detectionPattern{
	// English override-instruction pattern. "ignore/disregard/forget +
	// (previous/all/above/prior) + (instructions/rules/prompt/prompts)".
	// Captured by two anchors so the order is flexible.
	{
		name:     "en_ignore_instructions",
		verbRe:   regexp.MustCompile(`(?i)\b(ignore|disregard|forget|override|bypass)\b`),
		domainRe: regexp.MustCompile(`(?i)\b(previous|prior|above|all)\s+(instruction|instructions|rule|rules|prompt|prompts|guideline|guidelines|directive|directives|constraint|constraints)\b`),
	},
	// English system-prompt-extraction pattern. "print/reveal/show/output
	// + (your|the) + (system) prompt|instructions".
	{
		name:     "en_extract_prompt",
		verbRe:   regexp.MustCompile(`(?i)\b(print|reveal|show|output|disclose|tell\s+me|leak)\b`),
		domainRe: regexp.MustCompile(`(?i)\b(your|the)\s+(system\s+)?(prompt|instructions|rules|directives|guidelines)\b`),
	},
	// English role-override pattern. "you are now / pretend / act as +
	// (different|another|jailbreak|DAN|evil|unfiltered)". Targets the
	// well-known "DAN" / role-injection corpus.
	{
		name:     "en_role_override",
		verbRe:   regexp.MustCompile(`(?i)\b(you\s+are\s+now|pretend|act\s+as|roleplay|simulate)\b`),
		domainRe: regexp.MustCompile(`(?i)\b(DAN|developer\s+mode|jailbreak|unfiltered|uncensored|evil|opposite|no\s+restrictions|without\s+restrictions|different\s+(AI|assistant|model))\b`),
	},
	// Chinese override-instruction pattern. "忽略/无视/忘记/绕过 +
	// (之前|前面|上面|你的|所有) + (指令|提示词|规则|系统提示)".
	{
		name:     "zh_ignore_instructions",
		verbRe:   regexp.MustCompile(`(忽略|无视|忘记|忘掉|绕过|跳过)`),
		domainRe: regexp.MustCompile(`(之前|前面|上面|所有|你的|系统|原本|当前)?(的)?(指令|提示词|系统提示|规则|限制|约束|设定|准则)`),
	},
	// Chinese system-prompt-extraction pattern. "打印/显示/告诉我/输出
	// + (你的|系统) + (prompt|提示词|指令|规则)".
	{
		name:     "zh_extract_prompt",
		verbRe:   regexp.MustCompile(`(打印|显示|告诉我|输出|展示|揭示|泄露|说出)`),
		domainRe: regexp.MustCompile(`(你的|这个|完整的|原始的|内部的)?(系统)?(prompt|提示词|指令|规则|设定)`),
	},
	// Chinese role-override pattern. domain noun list intentionally
	// excludes the bare "你是" — the bigram appears in countless benign
	// platform questions ("现在你是 helpful 还是 not", "你是哪个版本")
	// and would flood the false-positive surface. The negation form
	// "你不是" stays because it's specifically the role-override
	// inversion phrasing.
	{
		name:     "zh_role_override",
		verbRe:   regexp.MustCompile(`(扮演|假装|假设你是|现在你是|从现在(开始|起))`),
		domainRe: regexp.MustCompile(`(你不是|不受任何限制|不受任何规则|没有任何限制|没有任何规则|没有限制|不受限|不受规则|无任何限制|越狱|DAN|开发者模式|没有规则)`),
	},
}

// trim limits input scanning to a reasonable head — jailbreak prompts
// concentrate at the start (or beginning of an attack section), and
// extremely long inputs would otherwise pay full regex cost per turn.
// 4 KB is well above typical user messages and above all known
// jailbreak corpus entries.
const detectionScanLimit = 4 * 1024

// DetectJailbreakAttempt returns true if the message structurally
// matches a jailbreak / instruction-override / prompt-extraction
// pattern. Idempotent and cheap; safe to call on every user turn.
//
// Returns false on empty input. Trims to first 4 KB before scanning.
func DetectJailbreakAttempt(s string) bool {
	if s == "" {
		return false
	}
	if len(s) > detectionScanLimit {
		s = s[:detectionScanLimit]
	}
	// Normalise full-width punctuation that some attackers use to evade
	// half-width keyword scans. Cheap whitelist conversion; full Unicode
	// confusables left for a follow-up if seen in the wild.
	s = strings.NewReplacer(
		"：", ":",
		"，", ",",
		"。", ".",
	).Replace(s)
	for _, p := range jailbreakPatterns {
		if p.verbRe.MatchString(s) && p.domainRe.MatchString(s) {
			return true
		}
	}
	return false
}
