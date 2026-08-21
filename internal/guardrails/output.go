package guardrails

import (
	"regexp"
	"strings"
	"unicode"
)

// Output leak protection removes credentials, tokens and project UUIDs from
// assistant replies before persistence. It deliberately preserves operational
// facts the owner needs to use or troubleshoot their resources: instance IDs,
// IP addresses, zones, prices and GPU model names. Input-side PII redaction is a
// separate boundary.
//
// Known false-positive surface (acceptable per ticket):
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
	ProjectIDRedacted        = "[已脱敏:项目ID]"
	CredentialRedactedOutput = "[已脱敏:凭据]"
	TokenRedactedOutput      = "[已脱敏:令牌]"
)

var (
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
	// persisted execution record. Unlike RedactOutputLeak, these patterns do not
	// touch IP addresses, project IDs, email addresses, or other routing inputs.
	credentialAssignmentRegex = regexp.MustCompile(
		`(?i)(["']?\b(?:access[_-]?key(?:[_-]?id|[_-]?secret)?|secret[_-]?key|api[_-]?key|x[_-]?api[_-]?key|ak|sk|access[_-]?token|security[_-]?token|refresh[_-]?token|id[_-]?token|session[_-]?token|token|password|passwd|pwd|private[_-]?key)\b["']?\s*[:=]\s*["']?)([^"'\s,;}&\]]+)`,
	)
	passwordAssignmentRegex    = regexp.MustCompile(`(?i)(?:(?:重置|修改|设置)?\s*(?:登录)?密码(?:重置|修改|设置)?|改密|password)\s*(为|成|是|:|：)\s*([^\s，。；;]+)`)
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

// ClassifyPasswordAssignment is the single parser used by both persisted input
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
	compactHead := strings.ReplaceAll(head, " ", "")
	return strings.Contains(compactHead, "密码") &&
		(strings.HasPrefix(compactHead, "重置") || strings.HasSuffix(compactHead, "重置") ||
			strings.HasSuffix(prefix, "重置")) ||
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
// Credential cleanup runs through RedactCredentials first; UUID cleanup is
// independent. IPv4 is deliberately NOT redacted — see the package doc.
// Idempotent — placeholders contain no dashes in UUID grouping, no eyJ
// prefix, etc., so re-running is a no-op.
func RedactOutputLeak(s string) string {
	if s == "" {
		return s
	}
	out := RedactCredentials(s)
	out = uuidRegex.ReplaceAllString(out, ProjectIDRedacted)
	return out
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

func redactAssignmentKeepMarkerWith(match, replacement string) string {
	groups := credentialAssignmentRegex.FindStringSubmatch(match)
	if len(groups) < 3 || strings.HasPrefix(groups[2], "[") {
		return match
	}
	return groups[1] + replacement
}
