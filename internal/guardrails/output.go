package guardrails

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Output leak protection — redacts platform credentials and customer
// identifiers from assistant replies before they land in HTTP
// messages.content rows for role=assistant.
//
// Distinct from RedactPII (input side, this same package): the patterns
// here target what the LLM might render INTO a reply (e.g. an IP it
// learned from a tool result that internal/sanitizer didn't blank, or
// a credential string the model rendered with a marker keyword like
// "AccessKey=..."). Known provider prefixes are also removed when they
// appear without a field name.
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
//   - Marker-prefixed prose: `AccessKey: <opaque-value>`
//     redacts because an assigned credential field is treated as sensitive
//     regardless of value shape.
//   - Bearer prefix + 20-char alpha-ish prose ("token expired_after_X")
//     redacts. Same root cause — bearer/token regex requires the
//     marker prefix + 20-char value but cannot validate cred entropy.

// Output-side placeholders. Distinct from PhoneRedacted etc. so an
// operator scanning persisted messages can attribute redactions to the
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

	// Persistence-grade credential coverage shared by assistant output and the
	// durable execution envelope. Unlike RedactOutputLeak, these patterns do not
	// touch IP addresses, project IDs, email addresses, or other routing inputs.
	credentialAssignmentRegex = regexp.MustCompile(
		`(?i)(["']?\b(?:access[_-]?key(?:[_-]?id|[_-]?secret)?|secret[_-]?key|api[_-]?key|x[_-]?api[_-]?key|ak|sk|access[_-]?token|security[_-]?token|refresh[_-]?token|id[_-]?token|session[_-]?token|token|password|passwd|pwd|private[_-]?key)\b["']?\s*[:=]\s*["']?)([^"'\s,;}&\]]+)`,
	)
	passwordAssignmentRegex    = regexp.MustCompile(`(?i)(?:密码|改密|password)\s*(为|成|是|:|：)\s*([^\s，。；;]+)`)
	authorizationRegex         = regexp.MustCompile(`(?i)(\bauthorization\s*[:=]\s*)(?:(bearer|basic)[\s:=]+)?([^\s,;}\]]+)`)
	privateKeyBlockRegex       = regexp.MustCompile(`(?is)-----BEGIN [^-\r\n]*PRIVATE KEY-----.*?-----END [^-\r\n]*PRIVATE KEY-----`)
	knownCredentialPrefixRegex = regexp.MustCompile(`(?i)\b(?:AKIA|ASIA)[A-Z0-9]{16}\b|\bsk-[A-Za-z0-9_\-]{12,}\b|\bgh[pousr]_[A-Za-z0-9]{20,}\b|\bxox[baprs]-[A-Za-z0-9-]{16,}\b`)
	compShareAccessTokenRegex  = regexp.MustCompile(`\bUCloud-CompShare-[A-Za-z0-9]+\b`)
)

// RedactCredentials removes credential values while preserving semantic and
// routing inputs such as email, IPv4, project IDs, instance IDs, regions, and
// ordinary prose. It is safe to use before a recoverable request is persisted.
func RedactCredentials(s string) string {
	return redactCredentialsWith(s, CredentialRedactedOutput, TokenRedactedOutput, false)
}

// RedactCredentialsWithReplacement applies the same shared credential rules
// at boundaries that require a neutral placeholder instead of user-facing
// output labels.
func RedactCredentialsWithReplacement(s, replacement string) string {
	if replacement == "" {
		replacement = CredentialRedactedOutput
	}
	return redactCredentialsWith(s, replacement, replacement, true)
}

func redactCredentialsWith(s, credentialReplacement, tokenReplacement string, preserveSeparators bool) string {
	if s == "" {
		return s
	}
	out := privateKeyBlockRegex.ReplaceAllString(s, credentialReplacement)
	out = redactPasswordAssignments(out, credentialReplacement)
	out = jwtRegex.ReplaceAllString(out, tokenReplacement)
	out = authorizationRegex.ReplaceAllStringFunc(out, func(match string) string {
		if !preserveSeparators {
			return redactAuthorizationKeepMarkerNormalized(match, tokenReplacement)
		}
		return redactAuthorizationKeepMarkerWith(match, tokenReplacement)
	})
	out = bearerRegex.ReplaceAllStringFunc(out, func(match string) string {
		if !preserveSeparators {
			return redactBearerKeepMarkerNormalized(match, tokenReplacement)
		}
		return redactBearerKeepMarkerWith(match, tokenReplacement)
	})
	out = credentialAssignmentRegex.ReplaceAllStringFunc(out, func(match string) string {
		return redactAssignmentKeepMarkerWith(match, credentialReplacement)
	})
	out = knownCredentialPrefixRegex.ReplaceAllString(out, credentialReplacement)
	out = compShareAccessTokenRegex.ReplaceAllString(out, "UCloud-CompShare-"+credentialReplacement)
	return out
}

// IsCredentialKey is the single field-name policy used by model, trace and
// tool-result boundaries. Pagination keys such as next_token intentionally do
// not match.
func IsCredentialKey(key string) bool {
	normalized := strings.ToLower(key)
	normalized = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, normalized)
	return strings.Contains(normalized, "password") ||
		strings.Contains(normalized, "passwd") ||
		strings.Contains(normalized, "privatekey") ||
		strings.Contains(normalized, "publickey") ||
		strings.Contains(normalized, "secretkey") ||
		strings.Contains(normalized, "accesskey") ||
		strings.Contains(normalized, "apikey") ||
		strings.Contains(normalized, "apitoken") ||
		strings.Contains(normalized, "accesstoken") ||
		strings.Contains(normalized, "authtoken") ||
		strings.Contains(normalized, "sessiontoken") ||
		strings.Contains(normalized, "refreshtoken") ||
		strings.Contains(normalized, "idtoken") ||
		strings.Contains(normalized, "jupytertoken") ||
		strings.Contains(normalized, "jupyterlabtoken") ||
		strings.Contains(normalized, "bearertoken") ||
		strings.Contains(normalized, "clientsecret") ||
		strings.Contains(normalized, "webhooksecret") ||
		strings.Contains(normalized, "credential") ||
		(strings.Contains(normalized, "ssh") && strings.Contains(normalized, "command"))
}

// ContainsCredential reports whether the shared credential rules would redact
// any part of s. Callers that must reject rather than sanitize an input should
// use this instead of defining another pattern set.
func ContainsCredential(s string) bool {
	return s != "" && RedactCredentials(s) != s
}

// ClassifyPasswordAssignment is the single parser used by both durable input
// redaction and the sealed workflow-secret channel. It returns byte offsets so
// callers redact exactly the value matched by the shared credential policy.
// Questions containing an example password are redacted but never executed.
func ClassifyPasswordAssignment(text string) (value string, start, end int, executable bool) {
	for _, match := range passwordAssignmentRegex.FindAllStringSubmatchIndex(text, -1) {
		if len(match) < 6 || match[2] < 0 || match[3] < 0 || match[4] < 0 || match[5] < 0 {
			continue
		}
		value = strings.TrimSpace(text[match[4]:match[5]])
		if passwordQuestion(text) {
			questionValue := strings.TrimSuffix(strings.TrimSuffix(value, "?"), "？")
			if allHanText(questionValue) {
				return "", 0, 0, false
			}
			if prefixBytes := leadingASCIICredentialBytes(questionValue); prefixBytes > 0 {
				if prefixBytes == len(questionValue) && explicitPasswordReset(text, match[0], match[2]) {
					return value, match[4], match[5], true
				}
				return value[:prefixBytes], match[4], match[4] + prefixBytes, false
			}
			return value, match[4], match[5], false
		}
		if prefixBytes := leadingASCIICredentialBytes(value); prefixBytes > 0 && prefixBytes < len(value) {
			return value, match[4], match[5], false
		}
		return value, match[4], match[5], true
	}
	return "", 0, 0, false
}

func explicitPasswordReset(text string, matchStart, connectorStart int) bool {
	prefix := strings.ToLower(strings.TrimSpace(text[:matchStart]))
	head := strings.ToLower(strings.TrimSpace(text[matchStart:connectorStart]))
	if strings.HasPrefix(head, "改密") {
		return true
	}
	return strings.HasPrefix(head, "密码") && strings.HasSuffix(prefix, "重置") ||
		strings.HasPrefix(head, "password") && strings.HasSuffix(prefix, "reset")
}

func redactPasswordAssignments(text, replacement string) string {
	matches := passwordAssignmentRegex.FindAllStringSubmatchIndex(text, -1)
	for index := len(matches) - 1; index >= 0; index-- {
		match := matches[index]
		if len(match) < 6 || match[0] < 0 || match[1] < 0 || match[4] < 0 || match[5] < 0 {
			continue
		}
		candidate := text[match[0]:match[1]]
		value, start, end, _ := ClassifyPasswordAssignment(candidate)
		if value == "" {
			continue
		}
		start += match[0]
		end += match[0]
		text = text[:start] + replacement + text[end:]
	}
	return text
}

func leadingASCIICredentialBytes(value string) int {
	for index, r := range value {
		if r > unicode.MaxASCII {
			if !unicode.Is(unicode.Han, r) {
				return 0
			}
			return index
		}
	}
	return len(value)
}

func passwordQuestion(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasSuffix(value, "?") || strings.HasSuffix(value, "？") || strings.HasSuffix(value, "吗")
}

func allHanText(value string) bool {
	value = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(value), "?"), "？")
	if value == "" {
		return false
	}
	for _, r := range value {
		if !unicode.Is(unicode.Han, r) {
			return false
		}
	}
	return true
}

// RedactOutputLeak returns a copy of s with output-side leak patterns
// replaced. Designed for assistant-reply persistence (HTTP messages
// role=assistant messages.content). Routing-relevant tokens are preserved.
//
// Credential cleanup runs through RedactCredentials first; IP and UUID
// cleanup is independent. Idempotent — placeholders contain no
// digits in IP-octet positions, no dashes in UUID grouping, no eyJ
// prefix, etc., so re-running is a no-op.
func RedactOutputLeak(s string) string {
	if s == "" {
		return s
	}
	out := RedactCredentials(s)
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

// redactBearerKeepMarker preserves the "Bearer"/"token" prefix and
// replaces only the credential body.
func redactBearerKeepMarker(match string) string {
	return redactBearerKeepMarkerNormalized(match, TokenRedactedOutput)
}

func redactBearerKeepMarkerNormalized(match, replacement string) string {
	groups := bearerRegex.FindStringSubmatch(match)
	if len(groups) < 3 {
		return match
	}
	return groups[1] + " " + replacement
}

func redactBearerKeepMarkerWith(match, replacement string) string {
	indices := bearerRegex.FindStringSubmatchIndex(match)
	if len(indices) < 6 || indices[4] < 0 || indices[5] < 0 {
		return match
	}
	return match[:indices[4]] + replacement + match[indices[5]:]
}

func redactAuthorizationKeepMarker(match string) string {
	return redactAuthorizationKeepMarkerNormalized(match, TokenRedactedOutput)
}

func redactAuthorizationKeepMarkerNormalized(match, replacement string) string {
	groups := authorizationRegex.FindStringSubmatch(match)
	if len(groups) < 4 || strings.HasPrefix(groups[3], "[") {
		return match
	}
	prefix := groups[1]
	if groups[2] != "" {
		prefix += groups[2] + " "
	}
	return prefix + replacement
}

func redactAuthorizationKeepMarkerWith(match, replacement string) string {
	indices := authorizationRegex.FindStringSubmatchIndex(match)
	if len(indices) < 8 || indices[6] < 0 || indices[7] < 0 {
		return match
	}
	value := match[indices[6]:indices[7]]
	if strings.HasPrefix(value, "[") {
		return match
	}
	return match[:indices[6]] + replacement + match[indices[7]:]
}

func redactAssignmentKeepMarker(match string) string {
	return redactAssignmentKeepMarkerWith(match, CredentialRedactedOutput)
}

func redactAssignmentKeepMarkerWith(match, replacement string) string {
	groups := credentialAssignmentRegex.FindStringSubmatch(match)
	if len(groups) < 3 || strings.HasPrefix(groups[2], "[") {
		return match
	}
	return groups[1] + replacement
}
