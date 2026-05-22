package guardrails

import (
	"regexp"
	"strconv"
	"strings"
)

// Output leak protection — redacts platform credentials and customer
// identifiers from assistant_message before it lands in
// agent_messages.assistant_message.
//
// Distinct from RedactPII (input side, this same package): the patterns
// here target what the LLM might render INTO a reply (e.g. an IP it
// learned from a tool result that internal/sanitizer didn't blank, or
// a credential string the model rendered with a marker keyword like
// "AccessKey=..."). Note: standalone credentials WITHOUT a marker
// keyword (e.g. the LLM pasting "AKIA..." mid-sentence) pass through
// — see Known FN below.
//
// Scope:
//   - IPv4 — both private (10.*, 192.168.*) and public; ops/dev review
//     should not see customer IPs. Zone codes (cn-wlcb-01, cn-shanghai-02)
//     are non-IP and survive.
//   - Project UUID — 8-4-4-4-12 hex with optional "project_id" /
//     "proj-" prefix; collapses to a placeholder.
//   - Access / Secret keys — credential-shaped opaque strings after a
//     marker keyword (access_key, ak, secret_key, sk, AccessKey...).
//   - Bearer / JWT tokens — `eyJ`-prefixed JWT shape or generic
//     "Bearer <token>" / "token=<long-string>" / "token: <long-string>"
//     form. Separator may be whitespace, '=', ':' or any combination so
//     both prose phrasing and config-file phrasing are caught.
//
// NOT in scope (deliberately preserved):
//   - GPU model numbers (4090 / 5090 / A100 / H200) — pricing/spec
//     answers depend on these. The 4-digit token can't accidentally
//     match an IPv4 octet because IPv4 requires three dots.
//   - Instance IDs (uhost-xxx) — answers about specific instances
//     remain readable.
//   - Zone codes (cn-wlcb-01) — needed for region-specific answers.
//   - Prices ("¥1.69/小时") — must be preserved verbatim.
//
// Known false-positive surface (acceptable per ticket):
//   - Localhost / loopback (127.0.0.1) is also redacted. Acceptable
//     — operator dashboards don't lose information by masking it.
//   - Public IP ranges legitimately quoted in documentation snippets
//     (e.g. example IPs in a how-to) will redact. Acceptable in the
//     persistence boundary even though it looks odd in transcripts.
//   - Marker-prefixed prose: `AccessKey: somelongdescription16chars+`
//     redacts because the credential regex requires marker + sep + a
//     16-char value but cannot distinguish prose from base64 cred.
//     Probability LOW (the LLM is unlikely to write "AccessKey:" prose).
//   - Bearer prefix + 20-char alpha-ish prose ("token expired_after_X")
//     redacts. Same root cause — bearer/token regex requires the
//     marker prefix + 20-char value but cannot validate cred entropy.
//
// Known false-negative surface (acceptable trade-off):
//   - Standalone credential strings without a marker keyword
//     (e.g. the LLM writes "AKIAIOSFODNN7EXAMPLE" mid-sentence) pass
//     through. Lowering the marker requirement would FP heavily on
//     prose; we accept that the agent rarely produces marker-less
//     credentials because tool responses always come with field
//     names that the LLM tends to echo verbatim.

// Output-side placeholders. Distinct from PhoneRedacted etc. so an
// operator scanning agent_messages can attribute redactions to the
// right Guardrails layer ("[output]:" vs "[已脱敏:..]").
const (
	IPRedacted               = "[已脱敏:IP]"
	ProjectIDRedacted        = "[已脱敏:项目ID]"
	CredentialRedactedOutput = "[已脱敏:凭据]"
	TokenRedactedOutput      = "[已脱敏:令牌]"
)

var (
	// IPv4 candidate. Each octet pre-validated 0-255 in callback because
	// `\d{1,3}` allows 999 which is invalid; the post-match filter
	// converts FPs (like "300.0.0.1") back to passthrough.
	ipv4Regex = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

	// 8-4-4-4-12 hex UUID. Used for CompShare project_id values and
	// (incidentally) any standard UUID the LLM might quote.
	uuidRegex = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)

	// Marker-based credential capture. We require the value to follow a
	// keyword + separator (= / : / 空格), so prose like "请问 access" or
	// "key 的作用" doesn't FP. Value continues until whitespace / quote
	// / comma / Chinese punctuation. Marker keywords cover the common
	// CompShare + AWS shapes the LLM might render verbatim.
	//
	// `(?i)` for case-insensitive — LLMs render with varied case.
	credentialMarkerRegex = regexp.MustCompile(
		`(?i)\b(access[_-]?key|secret[_-]?key|access[_-]?token|api[_-]?key|access[_-]?key[_-]?id|ak|sk)\b\s*[:=]\s*["']?([A-Za-z0-9+/=_\-]{16,})["']?`,
	)

	// JWT shape: 3 base64url segments joined by dots, starts with eyJ
	// (the base64url encoding of '{"'). Captures the most common bearer
	// token form (Anthropic / Bedrock / many internal services). Length
	// floor 20 chars per segment to avoid matching short test tokens
	// that look JWT-like.
	jwtRegex = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{16,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\b`)

	// "Bearer xxx" / "token xxx" / "Authorization: Bearer xxx" /
	// "token=xxx" / "token: xxx" form. Separator accepts any combination
	// of whitespace, '=', ':' so that both prose ("Bearer abc...") and
	// config-style ("token=eyJ..." / "token: AKIA...") shapes are caught.
	// Captures the token-value group; the marker word itself is preserved.
	bearerRegex = regexp.MustCompile(`(?i)\b(bearer|token)[\s:=]+([A-Za-z0-9+/=_\-\.]{20,})\b`)
)

// RedactOutputLeak returns a copy of s with output-side leak patterns
// replaced. Designed for assistant-reply persistence (agent_messages
// .assistant_message). Routing-relevant tokens are preserved.
//
// Order matters: JWT before marker-credential before generic-bearer,
// because JWT values would also match the credential / bearer windows;
// IP and UUID are independent. Idempotent — placeholders contain no
// digits in IP-octet positions, no dashes in UUID grouping, no eyJ
// prefix, etc., so re-running is a no-op.
func RedactOutputLeak(s string) string {
	if s == "" {
		return s
	}
	out := s
	out = jwtRegex.ReplaceAllString(out, TokenRedactedOutput)
	out = credentialMarkerRegex.ReplaceAllStringFunc(out, redactCredentialKeepMarker)
	out = bearerRegex.ReplaceAllStringFunc(out, redactBearerKeepMarker)
	out = uuidRegex.ReplaceAllString(out, ProjectIDRedacted)
	out = ipv4Regex.ReplaceAllStringFunc(out, redactIPv4IfValid)
	return out
}

// redactIPv4IfValid validates each dotted octet is 0-255 before
// redacting. A match like "300.0.0.1" passes the regex (digits + dots)
// but is not a real IPv4 — leave it alone so legitimate phrasings
// (chunk IDs, version strings) aren't mangled.
func redactIPv4IfValid(match string) string {
	octets := strings.Split(match, ".")
	if len(octets) != 4 {
		return match
	}
	for _, o := range octets {
		n, err := strconv.Atoi(o)
		if err != nil || n < 0 || n > 255 {
			return match
		}
		// Disallow leading zeros (except "0" itself) to match
		// strict IPv4 textual form. "01.02.03.04" is not a valid
		// dotted-quad presentation.
		if len(o) > 1 && o[0] == '0' {
			return match
		}
	}
	return IPRedacted
}

// redactCredentialKeepMarker preserves the marker keyword and separator
// so operators reviewing the column know WHICH credential was redacted
// without seeing the value. Input shape: "AccessKey=AKIA...".
func redactCredentialKeepMarker(match string) string {
	groups := credentialMarkerRegex.FindStringSubmatch(match)
	if len(groups) < 3 {
		return match
	}
	marker := groups[1]
	// Find the separator (':' or '=') that follows the marker in the match.
	sepIdx := strings.IndexAny(match, ":=")
	if sepIdx < 0 {
		return marker + "=" + CredentialRedactedOutput
	}
	return match[:sepIdx+1] + " " + CredentialRedactedOutput
}

// redactBearerKeepMarker preserves the "Bearer"/"token" prefix and
// replaces only the credential body.
func redactBearerKeepMarker(match string) string {
	groups := bearerRegex.FindStringSubmatch(match)
	if len(groups) < 3 {
		return match
	}
	marker := groups[1]
	return marker + " " + TokenRedactedOutput
}
