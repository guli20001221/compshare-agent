package guardrails

import (
	"regexp"
	"strconv"
	"unicode"
	"unicode/utf8"
)

// Authorization is parsed in two stages: this regex locates only the field
// prefix, then authorizationValueSpans scans the complete quote- or line-
// bounded value. Keeping schemes out of the regex avoids a Bearer/Basic
// allowlist and preserves multi-parameter RFC credentials such as Digest,
// Signature and AWS4-HMAC-SHA256 without truncating them at the first comma.
var authorizationRegex = regexp.MustCompile(`(?i)(?:(-H|--header=)(["'\x60]?)authorization\b|(["'\x60]?)\bauthorization\b)(?:\*{1,2}|_{1,2}|~{2})?(["'\x60]?)[ \t]*([:=：])(?:\*{1,2}|_{1,2}|~{2})?[ \t]*(["'\x60]?)`)

const (
	maxAuthorizationHeaderValues = 4
	maxAuthorizationHeaderBytes  = 2048
	authorizationReferencePrefix = "current-user-authorization-"
)

// AuthorizationHeaderReference is one current-text Authorization value paired
// with an opaque request-local marker. Value is the credential itself and stays
// on the private transport path; Reference is safe for a tool schema but is not
// reusable outside that request.
type AuthorizationHeaderReference struct {
	Reference string
	Value     string
}

// AuthorizationHeaderValues extracts the HTTP Authorization header values in
// text that can back a probe capability. The text is not rewritten: the model
// reads what the user typed, and the reference exists so the inner probe can
// send the exact header without the model retyping it. Duplicate values share
// one reference and at most four distinct values become references. Assignment
// syntax, unterminated quotes, inline prose and over-limit or non-ASCII values
// mint nothing, and a URL query parameter is not a header at all.
func AuthorizationHeaderValues(text string) []AuthorizationHeaderReference {
	references := make([]AuthorizationHeaderReference, 0, 1)
	byValue := make(map[string]string)
	for _, span := range authorizationValueSpans(text) {
		value := text[span.valueStart:span.valueEnd]
		if !span.capabilityValid || authorizationSpanIsPlaceholder(text, span) ||
			len(value) > maxAuthorizationHeaderBytes || !printableASCII(value) {
			continue
		}
		if _, exists := byValue[value]; exists || len(references) == maxAuthorizationHeaderValues {
			continue
		}
		ref := authorizationReferencePrefix + strconv.Itoa(len(references)+1)
		byValue[value] = ref
		references = append(references, AuthorizationHeaderReference{Reference: ref, Value: value})
	}
	return references
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
	secretStart     int
	capabilityValid bool
}

// authorizationValueSpans locates every Authorization value in text. The prefix
// regexp never attempts to understand an auth scheme. This scanner instead takes
// the complete quoted value, or the complete bare header line, so
// extensible/multi-parameter HTTP authentication cannot be truncated into a
// different credential.
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

		// A colon is the HTTP-header spelling; '=' is an assignment or query
		// spelling. Only the header spelling can become a capability, but both
		// are bounded completely so a later match cannot start inside this value.
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
			_, secretStart = authorizationSecretBounds(text, valueStart, valueEnd)
		}
		credentialStart := valueStart
		if secretStart > valueStart {
			credentialStart = secretStart
		}
		spans = append(spans, authorizationValueSpan{
			valueStart: valueStart, valueEnd: valueEnd, secretStart: secretStart,
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

// A bracketed value such as `[REDACTED]` is a placeholder, not a credential.
func authorizationSpanIsPlaceholder(text string, span authorizationValueSpan) bool {
	return span.secretStart < span.valueEnd && text[span.secretStart] == '['
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
