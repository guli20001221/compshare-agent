package guardrails

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Credential protection removes access credentials and tokens before
// persistence. Ordinary conversation information, including phone numbers,
// email addresses, project IDs and operational facts, remains intact.
//
// Known false-positive surface (acceptable per ticket):
//   - Marker-prefixed prose: `AccessKey: <opaque-value>`
//     redacts because an assigned credential field is treated as sensitive
//     regardless of value shape.
//   - Bearer prefix + 20-char alpha-ish prose ("token expired_after_X")
//     redacts. Same root cause — bearer/token regex requires the
//     marker prefix + 20-char value but cannot validate cred entropy.

// User-facing credential placeholders.
const (
	CredentialRedactedOutput = "[已脱敏:凭据]"
	TokenRedactedOutput      = "[已脱敏:令牌]"
)

var (
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
	// persisted execution record. These patterns do not
	// touch IP addresses, project IDs, email addresses, or other routing inputs.
	credentialAssignmentRegex = regexp.MustCompile(
		`(?i)(["']?\b(?:access[_-]?key(?:[_-]?id|[_-]?secret)?|secret[_-]?key|api[_-]?key|x[_-]?api[_-]?key|ak|sk|access[_-]?token|security[_-]?token|refresh[_-]?token|id[_-]?token|session[_-]?token|token|password|passwd|pwd|private[_-]?key)\b["']?\s*[:=]\s*["']?)([^"'\s,;}&\]]+)`,
	)
	passwordAssignmentRegex = regexp.MustCompile(`(?i)(?:(?:重置|修改|设置)?\s*(?:登录)?密码(?:重置|修改|设置)?|改密|password)\s*(为|成|是|:|：)\s*([^\s，。；;]+)`)
	// Authorization is parsed in two stages: this regex locates only the field
	// prefix, then authorizationValueSpans scans the complete quote- or line-
	// bounded value. Keeping schemes out of the regex avoids a Bearer/Basic
	// allowlist and preserves multi-parameter RFC credentials such as Digest,
	// Signature and AWS4-HMAC-SHA256 without truncating them at the first comma.
	authorizationRegex         = regexp.MustCompile(`(?i)(?:(-H|--header=)(["'\x60]?)authorization\b|(["'\x60]?)\bauthorization\b)(?:\*{1,2}|_{1,2}|~{2})?(["'\x60]?)[ \t]*([:=：])(?:\*{1,2}|_{1,2}|~{2})?[ \t]*(["'\x60]?)`)
	privateKeyBlockRegex       = regexp.MustCompile(`(?is)-----BEGIN [^-\r\n]*PRIVATE KEY-----.*?-----END [^-\r\n]*PRIVATE KEY-----`)
	knownCredentialPrefixRegex = regexp.MustCompile(`(?i)\b(?:AKIA|ASIA)[A-Z0-9]{16}\b|\bsk-[A-Za-z0-9_\-]{12,}\b|\bgh[pousr]_[A-Za-z0-9]{20,}\b|\bxox[baprs]-[A-Za-z0-9-]{16,}\b`)
	compShareAccessTokenRegex  = regexp.MustCompile(`\bUCloud-CompShare-[A-Za-z0-9]+\b`)
)

const (
	maxAuthorizationHeaderValues = 4
	maxAuthorizationHeaderBytes  = 2048
	authorizationReferencePrefix = "current-user-authorization-"
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
	out = redactAuthorizationValues(out, tokenReplacement, preserveSeparators, false)
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
		normalized == "authorization" ||
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

// AuthorizationHeaderReference is one current-text Authorization value paired
// with the opaque marker that replaced it.  Value is secret-bearing and must
// stay in request-local memory; Reference is safe for a model prompt and tool
// schema but is not reusable outside that request.
type AuthorizationHeaderReference struct {
	Reference string
	Value     string
}

// ReferenceAuthorizationHeaderValues replaces each valid Authorization value
// at its exact matched byte span with an opaque current-request marker.  The
// positional rewrite matters: replacing by value alone could redact an earlier
// prose occurrence while accidentally leaving the actual header untouched.
// Duplicate values reuse one reference; at most four distinct values become
// capabilities. Invalid, over-limit and fifth-and-later header values are
// replaced with a non-executable redaction marker in the same positional pass.
// URL query parameters are deliberately left alone: they cannot mint an HTTP
// header capability and belong to the existing signed-URL product flow.
func ReferenceAuthorizationHeaderValues(text string) (string, []AuthorizationHeaderReference) {
	type replacement struct {
		start int
		end   int
		ref   string
	}
	var replacements []replacement
	references := make([]AuthorizationHeaderReference, 0, 1)
	byValue := make(map[string]string)
	for _, span := range authorizationValueSpans(text) {
		if !span.liveSensitive {
			continue
		}
		value := text[span.valueStart:span.valueEnd]
		if !span.capabilityValid || authorizationSpanAlreadyRedacted(text, span) ||
			len(value) > maxAuthorizationHeaderBytes || !printableASCII(value) {
			replacements = append(replacements, replacement{start: span.valueStart, end: span.valueEnd})
			continue
		}
		ref, exists := byValue[value]
		if !exists {
			if len(references) == maxAuthorizationHeaderValues {
				replacements = append(replacements, replacement{start: span.valueStart, end: span.valueEnd})
				continue
			}
			ref = authorizationReferencePrefix + strconv.Itoa(len(references)+1)
			byValue[value] = ref
			references = append(references, AuthorizationHeaderReference{Reference: ref, Value: value})
		}
		replacements = append(replacements, replacement{start: span.valueStart, end: span.valueEnd, ref: ref})
	}
	for i := len(replacements) - 1; i >= 0; i-- {
		r := replacements[i]
		marker := "[AUTHORIZATION_REDACTED]"
		if r.ref != "" {
			marker = "[AUTHORIZATION_REF:" + r.ref + "]"
		}
		text = text[:r.start] + marker + text[r.end:]
	}
	return text, references
}

func authorizationHeaderBoundary(text string, start, prefixEnd int) bool {
	if start <= 0 {
		return true
	}
	// A quoted field is self-delimiting. This covers JSON header maps and the
	// common curl spellings -H'Authorization: ...' / --header='Authorization: ...'
	// without teaching the parser about particular command-line flags.
	if text[start] == '\'' || text[start] == '"' || text[start] == '`' {
		return true
	}
	// Markdown emphasis may wrap the field name itself (`**Authorization**:`).
	// Ignore at most the matching marker width when deciding whether the field
	// began at a real prose boundary; an identifier such as fooAuthorization is
	// still not promoted into an executable capability.
	boundaryStart := start
	marker := text[start-1]
	markerWidth := 0
	if marker == '*' || marker == '_' || marker == '~' {
		boundaryStart--
		markerWidth++
		if boundaryStart > 0 && text[boundaryStart-1] == marker {
			boundaryStart--
			markerWidth++
		}
		closingFound := false
		fieldNameEnd := start + len("authorization")
		for i := fieldNameEnd; i+markerWidth <= prefixEnd; i++ {
			matched := true
			for j := 0; j < markerWidth; j++ {
				if text[i+j] != marker {
					matched = false
					break
				}
			}
			if matched {
				closingFound = true
				break
			}
		}
		if !closingFound {
			return false
		}
	}
	if boundaryStart <= 0 {
		return true
	}
	previous, _ := utf8.DecodeLastRuneInString(text[:boundaryStart])
	return unicode.IsSpace(previous) || previous == '{' || previous == ',' || previous == '(' ||
		previous == '[' || previous == ';' || previous == '，' || previous == '；' || previous == '。' ||
		previous == ')' || previous == '）'
}

type authorizationValueSpan struct {
	valueStart      int
	valueEnd        int
	schemeEnd       int
	secretStart     int
	liveSensitive   bool
	capabilityValid bool
}

// authorizationValueSpans is the one parser shared by durable redaction and
// current-request capability extraction. The prefix regexp never attempts to
// understand an auth scheme. This scanner instead takes the complete quoted
// value, or the complete bare header line, so extensible/multi-parameter HTTP
// authentication cannot be truncated into a different credential.
func authorizationValueSpans(text string) []authorizationValueSpan {
	matches := authorizationRegex.FindAllStringSubmatchIndex(text, -1)
	spans := make([]authorizationValueSpan, 0, len(matches))
	lastValueEnd := -1
	for _, indices := range matches {
		if len(indices) < 14 || indices[0] < lastValueEnd || indices[1] < 0 ||
			indices[10] < 0 || indices[11] < 0 {
			continue
		}
		fieldStart, valueStart := indices[0], indices[1]
		attachedOption := indices[2] >= 0 && indices[3] > indices[2]
		assignmentOnly := text[indices[10]:indices[11]] == "="
		capabilityBoundary := authorizationHeaderBoundary(text, fieldStart, valueStart) || attachedOption
		urlQueryAssignment := assignmentOnly && authorizationAssignmentInURLQuery(text, fieldStart)
		// A colon spells an HTTP Authorization field even when adjacent prose or
		// Markdown makes it unsuitable as an executable capability. Keep that
		// credential out of the main model unconditionally. Assignment syntax is
		// handled the same way unless it is actually a URL query parameter; signed
		// URLs retain the established live flow and can never mint a header
		// capability.
		liveSensitive := !urlQueryAssignment
		quote := byte(0)
		if indices[12] >= 0 && indices[13] > indices[12] {
			quote = text[indices[12]] // separately quoted JSON/YAML value
		} else {
			preQuoteStart, preQuoteEnd := indices[6], indices[7]
			if attachedOption {
				preQuoteStart, preQuoteEnd = indices[4], indices[5]
			}
			if preQuoteStart >= 0 && preQuoteEnd > preQuoteStart && indices[9] == indices[8] {
				quote = text[preQuoteStart] // quote wraps the entire `Authorization: value`
			}
		}

		// Colon is the HTTP-header spelling even when surrounding prose prevents
		// it from becoming an executable capability. Parse that complete value for
		// durable redaction; '=' remains an assignment/query spelling.
		headerShaped := !attachedOption && !urlQueryAssignment
		valueEnd, closed := authorizationValueEnd(text, valueStart, quote, headerShaped)
		for valueStart < valueEnd && (text[valueStart] == ' ' || text[valueStart] == '\t') {
			valueStart++
		}
		for valueEnd > valueStart && (text[valueEnd-1] == ' ' || text[valueEnd-1] == '\t') {
			valueEnd--
		}
		if valueStart >= valueEnd {
			continue
		}
		schemeEnd, secretStart := authorizationSecretBounds(text, valueStart, valueEnd)
		if quote == 0 && !urlQueryAssignment {
			valueEnd = authorizationBareCredentialEnd(text, valueStart, valueEnd, schemeEnd, secretStart)
			schemeEnd, secretStart = authorizationSecretBounds(text, valueStart, valueEnd)
		}
		credentialStart := valueStart
		if secretStart > valueStart {
			credentialStart = secretStart
		}
		spans = append(spans, authorizationValueSpan{
			valueStart: valueStart, valueEnd: valueEnd,
			schemeEnd: schemeEnd, secretStart: secretStart,
			liveSensitive: liveSensitive,
			capabilityValid: capabilityBoundary && !assignmentOnly && (quote == 0 || closed) &&
				valueEnd-credentialStart >= 3,
		})
		lastValueEnd = valueEnd
	}
	return spans
}

func authorizationAssignmentInURLQuery(text string, fieldStart int) bool {
	parameterStart := fieldStart
	for parameterStart > 0 && authorizationURLParameterNameByte(text[parameterStart-1]) {
		parameterStart--
	}
	if parameterStart == 0 || (text[parameterStart-1] != '?' && text[parameterStart-1] != '&') {
		return false
	}

	separator := parameterStart - 1
	question := separator
	if text[separator] == '&' {
		question = -1
		for i := separator - 1; i >= 0; i-- {
			if text[i] == '?' {
				question = i
				break
			}
			if text[i] == '\'' || text[i] == '"' || text[i] == '`' ||
				text[i] == ' ' || text[i] == '\t' || text[i] == '\r' || text[i] == '\n' {
				return false
			}
		}
		if question < 0 {
			return false
		}
	}

	uriStart := question
	for uriStart > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:uriStart])
		if unicode.IsSpace(r) || r == '\'' || r == '"' || r == '`' || r == '<' || r == '(' || r == '[' {
			break
		}
		uriStart -= size
	}
	hasScheme := false
	for i := uriStart; i+2 < question; i++ {
		if text[i] == ':' && text[i+1] == '/' && text[i+2] == '/' {
			hasScheme = true
			break
		}
	}
	relativePath := uriStart < question && text[uriStart] == '/'
	if uriStart+1 < question && text[uriStart] == '.' && text[uriStart+1] == '/' {
		relativePath = true
	}
	if uriStart+2 < question && text[uriStart] == '.' && text[uriStart+1] == '.' && text[uriStart+2] == '/' {
		relativePath = true
	}
	return hasScheme || relativePath
}

func authorizationURLParameterNameByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '-' || value == '.' || value == '_' || value == '~'
}

func authorizationValueEnd(text string, start int, quote byte, headerShaped bool) (int, bool) {
	if quote != 0 {
		for i := start; i < len(text); i++ {
			if text[i] == '\r' || text[i] == '\n' {
				return i, false
			}
			if text[i] == quote && !byteIsEscaped(text, i) {
				return i, true
			}
		}
		return len(text), false
	}
	for i := start; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == '\r' || r == '\n' ||
			(headerShaped && (r == '，' || r == '；' || r == '。' || r == '）' || r == '}' || r == ']')) ||
			(!headerShaped && (unicode.IsSpace(r) || r == '&' || r == '#' || r == ',' || r == ';' ||
				r == '，' || r == '；' || r == '。' || r == '}' || r == ']' || r == ')' || r == '）' ||
				r == '\'' || r == '"')) {
			return i, true
		}
		i += size
	}
	return len(text), true
}

func byteIsEscaped(text string, index int) bool {
	backslashes := 0
	for i := index - 1; i >= 0 && text[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func authorizationSecretBounds(text string, start, end int) (int, int) {
	for i := start; i < end; i++ {
		if text[i] != ' ' && text[i] != '\t' && text[i] != ':' && text[i] != '=' {
			continue
		}
		schemeEnd := i
		for i < end && (text[i] == ' ' || text[i] == '\t' || text[i] == ':' || text[i] == '=') {
			i++
		}
		if schemeEnd > start && i < end {
			return schemeEnd, i
		}
		break
	}
	return start, start
}

// A bare RFC Authorization value is `scheme credentials`. Token-style
// credentials end at the first byte outside RFC token68; auth-param forms are
// parsed as a comma-separated name=value list. This keeps both `Bearer x prose`
// and `Digest ..., response=x prose` from swallowing the user's following text.
// Quoted wrapper values are already bounded before this helper.
func authorizationBareCredentialEnd(text string, start, end, schemeEnd, secretStart int) int {
	if schemeEnd <= start || secretStart >= end {
		if tokenEnd := authorizationToken68End(text, start, end); tokenEnd > start {
			return tokenEnd
		}
		return end
	}
	if !authorizationAuthParamAt(text, secretStart, end) {
		if tokenEnd := authorizationToken68End(text, secretStart, end); tokenEnd > secretStart {
			return tokenEnd
		}
		return end
	}

	position := secretStart
	lastValueEnd := secretStart
	for authorizationAuthParamAt(text, position, end) {
		for position < end && authorizationParamNameByte(text[position]) {
			position++
		}
		for position < end && (text[position] == ' ' || text[position] == '\t') {
			position++
		}
		position++ // '=' was established by authorizationAuthParamAt
		for position < end && (text[position] == ' ' || text[position] == '\t') {
			position++
		}
		if position >= end {
			return lastValueEnd
		}
		if text[position] == '\'' || text[position] == '"' {
			quote := text[position]
			position++
			for position < end && (text[position] != quote || byteIsEscaped(text, position)) {
				position++
			}
			if position >= end {
				return end
			}
			position++
		} else {
			for position < end && text[position] != ',' && text[position] != ' ' && text[position] != '\t' {
				position++
			}
		}
		lastValueEnd = position
		for position < end && (text[position] == ' ' || text[position] == '\t') {
			position++
		}
		if position >= end || text[position] != ',' {
			return lastValueEnd
		}
		position++
		for position < end && (text[position] == ' ' || text[position] == '\t') {
			position++
		}
	}
	return lastValueEnd
}

func authorizationToken68End(text string, start, end int) int {
	position := start
	for position < end && authorizationToken68Byte(text[position]) {
		position++
	}
	return position
}

func authorizationToken68Byte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '-' || value == '.' || value == '_' ||
		value == '~' || value == '+' || value == '/' || value == '='
}

func authorizationAuthParamAt(text string, start, end int) bool {
	position := start
	for position < end && authorizationParamNameByte(text[position]) {
		position++
	}
	if position == start {
		return false
	}
	for position < end && (text[position] == ' ' || text[position] == '\t') {
		position++
	}
	if position >= end || text[position] != '=' {
		return false
	}
	position++
	for position < end && (text[position] == ' ' || text[position] == '\t') {
		position++
	}
	return position < end && text[position] != '='
}

func authorizationParamNameByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '_' || value == '-' || value == '.' || value == '~'
}

func authorizationSpanAlreadyRedacted(text string, span authorizationValueSpan) bool {
	return span.secretStart < span.valueEnd && text[span.secretStart] == '['
}

func redactAuthorizationValues(text, replacement string, preserveSeparators, liveOnly bool) string {
	spans := authorizationValueSpans(text)
	for i := len(spans) - 1; i >= 0; i-- {
		span := spans[i]
		if (liveOnly && !span.liveSensitive) || authorizationSpanAlreadyRedacted(text, span) {
			continue
		}
		start := span.valueStart
		rewritten := replacement
		if span.schemeEnd > span.valueStart {
			if preserveSeparators {
				start = span.secretStart
			} else {
				rewritten = text[span.valueStart:span.schemeEnd] + " " + replacement
			}
		}
		text = text[:start] + rewritten + text[span.valueEnd:]
	}
	return text
}

// RedactAuthorizationHeaderValues removes only explicit Authorization header
// values. Unlike the broader credential redactor it deliberately leaves signed
// URLs and unrelated current-turn secrets untouched; this is used for the live
// main-Agent view where those other values have established product flows.
func RedactAuthorizationHeaderValues(text, replacement string) string {
	if replacement == "" {
		replacement = CredentialRedactedOutput
	}
	return redactAuthorizationValues(text, replacement, false, true)
}

func printableASCII(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r >= 0x7f {
			return false
		}
	}
	return true
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

func redactAssignmentKeepMarkerWith(match, replacement string) string {
	groups := credentialAssignmentRegex.FindStringSubmatch(match)
	if len(groups) < 3 || strings.HasPrefix(groups[2], "[") {
		return match
	}
	return groups[1] + replacement
}
