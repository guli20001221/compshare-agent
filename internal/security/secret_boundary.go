package security

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/compshare-agent/internal/guardrails"
)

const redactedValue = "[REDACTED]"

var separatorRE = regexp.MustCompile(`[^a-z0-9]+`)

// RedactForLLM removes credentials and operational tokens before values are
// passed into model context. It returns a deep-redacted copy and never mutates
// the input value.
func RedactForLLM(v any) any {
	return redactValue(v, redactModeLLM, "")
}

// RedactForTrace removes credentials and masks/hash-stabilizes sensitive
// telemetry before writing traces or audit logs. It returns a deep-redacted copy
// and never mutates the input value.
func RedactForTrace(v any) any {
	return redactValue(v, redactModeTrace, "")
}

type redactMode int

const (
	redactModeLLM redactMode = iota
	redactModeTrace
)

func redactValue(v any, mode redactMode, parentKey string) any {
	switch typed := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, child := range typed {
			out[k] = redactField(k, child, mode)
		}
		return out
	case map[any]any:
		out := make(map[any]any, len(typed))
		for k, child := range typed {
			key, _ := k.(string)
			out[k] = redactField(key, child, mode)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = redactValue(child, mode, parentKey)
		}
		return out
	default:
		if s, ok := typed.(string); ok {
			s = redactOperationalTokens(s)
			if mode == redactModeTrace && isIPKey(parentKey) {
				return maskIPv4(s)
			}
			return s
		}
		if mode == redactModeTrace && isIPKey(parentKey) {
			if s, ok := typed.(string); ok {
				return maskIPv4(s)
			}
		}
		return typed
	}
}

func redactField(key string, value any, mode redactMode) any {
	if isSecretKey(key) {
		if mode == redactModeLLM && isPlainSSHLoginCommand(key, value) {
			// Not a secret, and treating it as one cost a capability.
			//
			// guardrails.IsCredentialKey matches any key containing both "ssh"
			// and "command", which was folded in with a list of token names in
			// #454 rather than decided about SSH. The value it blanks is a
			// connection line the user owns — "ssh root@203.0.113.9 -p 23" — with
			// no credential in it. The same package already decided that string
			// is displayable: guardrails/output.go documents why IPv4 masking was
			// removed, and TestRedactOutputLeak_IPv4IsPreserved pins that exact
			// line under the label "SSH 登录行必须可直接照抄". So the text rule and
			// the key-name rule disagreed about one string, and the key-name rule
			// won on the path that matters — the model's.
			//
			// The cost was not cosmetic: CLAUDE.md names
			// DescribeCompShareInstance.SshLoginCommand as the source of truth for
			// SSH facts (explicitly NOT DescribeCompShareSoftwarePort), so the
			// agent was denied the one field it is told to use, while IP,
			// PrivateIP, and the identical string sitting in Remark all passed
			// through untouched.
			//
			// The value still clears the credential boundary via redactValue —
			// an embedded password or token is stripped, the endpoint is not.
			// Scoped to redactModeLLM on purpose: persisted traces keep their
			// current behaviour, so nothing at rest changes.
			return redactValue(value, mode, key)
		}
		return redactedValue
	}
	if mode == redactModeTrace {
		if isBillingOrCostKey(key) {
			return hashValue(value)
		}
		if isIPKey(key) {
			if s, ok := value.(string); ok {
				return maskIPv4(s)
			}
		}
	}
	return redactValue(value, mode, key)
}

func isSecretKey(key string) bool {
	return guardrails.IsCredentialKey(key)
}

// sshConnectionKeys are the exact fields whose whole value is an SSH connection
// command. They are matched exactly, not by substring: a substring rule would be
// the same mistake being corrected here, and would also swallow a hypothetical
// SshCommandToken, which IS a secret and must keep being blanked. Adding a name
// to this list is a deliberate act — the value must be a connection line, not a
// credential that merely mentions ssh.
var sshConnectionKeys = map[string]bool{
	"sshlogincommand": true,
}

// isPlainSSHLoginCommand reports whether this is the authoritative SSH field
// carrying a value that is safe to show. Both halves must hold.
//
// The value check fails CLOSED, and it is what makes the whole exception safe.
// Credential redaction alone would not be: those patterns match `key=value` and
// `key: value`, so a command-flag form like `sshpass -p hunter2 ssh root@host`
// passes through them untouched. Rather than chase flag syntaxes, this accepts
// only the shapes the upstream actually returns —
//
//	ssh root@1.2.3.4 -p 22
//	ssh -p 23120 root@cpod-abc.podtcp.compshare.cn
//	ssh ubuntu@1.2.3.5
//
// — one `ssh`, at most one `-p <port>`, exactly one `user@host`, nothing else.
// An unrecognized token, a second command after `&&`, an `-o ProxyCommand`, or
// anything that merely starts with `sshpass` all fall back to [REDACTED]. If
// upstream changes the format the capability is lost again, loudly and safely,
// rather than leaking whatever the new format carries. The instance Password is
// a SEPARATE upstream field (see sshops.FetchCredential), so a well-formed value
// has no credential to strip.
//
// Written as a tokenizer rather than a pattern on purpose: architectureguard
// forbids new regex decision sites, and the token walk states the rule more
// plainly than the equivalent expression would.
func isPlainSSHLoginCommand(key string, value any) bool {
	if !sshConnectionKeys[normalizeKey(key)] {
		return false
	}
	raw, ok := value.(string)
	if !ok {
		return false
	}
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) == 0 || fields[0] != "ssh" {
		return false
	}
	seenTarget, seenPort := false, false
	for i := 1; i < len(fields); i++ {
		switch {
		case fields[i] == "-p":
			// At most one. A repeated flag is not a shape this upstream emits,
			// and an allowlist that accepts inputs its own contract excludes is
			// not an allowlist.
			if seenPort {
				return false
			}
			seenPort = true
			i++
			if i >= len(fields) || !isPortToken(fields[i]) {
				return false
			}
		case !seenTarget && isSSHTargetToken(fields[i]):
			seenTarget = true
		default:
			return false
		}
	}
	return seenTarget
}

// isPortToken accepts a decimal port in 1..65535. Range-checked rather than
// digit-counted: "99999" and "0" are five digits and one digit of nonsense, and
// letting them through would mean the allowlist admits values the upstream
// cannot produce.
func isPortToken(s string) bool {
	if s == "" || len(s) > 5 {
		return false
	}
	port := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
		port = port*10 + int(r-'0')
	}
	return port >= 1 && port <= 65535
}

// isSSHTargetToken accepts exactly one user@host with no shell metacharacters:
// the allowed rune set is what makes "&&", quotes and redirections impossible.
//
// The user part may not begin with "-": ssh would read such a token as a flag,
// so accepting it would mean this function and ssh disagree about what the
// string says.
func isSSHTargetToken(s string) bool {
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	user, host := s[:at], s[at+1:]
	if !isHostRuneSet(user) || !isHostRuneSet(host) {
		return false
	}
	if user[0] == '-' {
		return false
	}
	first := host[0]
	return first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z' || first >= '0' && first <= '9'
}

func isHostRuneSet(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func isBillingOrCostKey(key string) bool {
	normalized := normalizeKey(key)
	return strings.Contains(normalized, "billing") ||
		strings.Contains(normalized, "balance") ||
		strings.Contains(normalized, "charge") ||
		strings.Contains(normalized, "cost") ||
		strings.Contains(normalized, "price") ||
		strings.Contains(normalized, "amount")
}

func isIPKey(key string) bool {
	normalized := normalizeKey(key)
	switch normalized {
	case "ip", "publicip", "privateip", "ipaddress", "internetip", "externalip", "eip", "ipset", "ips":
		return true
	default:
		return false
	}
}

func normalizeKey(key string) string {
	key = strings.ToLower(key)
	return separatorRE.ReplaceAllString(key, "")
}

// RedactOperationalTokensInText removes access tokens embedded inside otherwise
// ordinary strings, such as JupyterLab URLs. It is safe for user-visible text.
func RedactOperationalTokensInText(s string) string {
	return redactOperationalTokens(s)
}

// RestoreUserProvidedCredentialURLs restores an exact signed URL only when it
// was supplied by the current user and the model quoted that exact URL in its
// draft. It lets the user copy a command built from their own one-time link
// without creating a general credential-echo exception: any token invented by
// a tool or the model remains redacted. Callers must still persist the result
// through RedactAssistantConversationText.
func RestoreUserProvidedCredentialURLs(redactedText, userText, draft string) string {
	for _, rawURL := range credentialURLsInText(userText) {
		// Do not turn an unrelated model sentence into an echo just because it
		// happens to contain the same redacted placeholder.
		if strings.ReplaceAll(draft, rawURL, "") == draft {
			continue
		}
		redactedURL := RedactOperationalTokensInText(rawURL)
		if redactedURL == rawURL {
			continue
		}
		// The generic credential sanitizer intentionally accepts an opaque value
		// up to whitespace. In a shell-quoted URL that can consume the closing
		// quote too, so restore the one immediately-adjacent syntactic delimiter
		// from the model's exact draft together with the user-owned URL.
		rawFragment := rawURL
		if _, suffix, found := strings.Cut(draft, rawURL); found {
			rawFragment += credentialURLClosingDelimiter(suffix)
		}
		redactedFragment := RedactOperationalTokensInText(rawFragment)
		redactedText = strings.ReplaceAll(redactedText, redactedFragment, rawFragment)
		redactedText = strings.ReplaceAll(redactedText, redactedURL, rawURL)
	}
	return redactedText
}

func credentialURLsInText(text string) []string {
	var urls []string
	for len(text) > 0 {
		beforeHTTPS, afterHTTPS, hasHTTPS := strings.Cut(text, "https://")
		beforeHTTP, afterHTTP, hasHTTP := strings.Cut(text, "http://")
		if !hasHTTPS && !hasHTTP {
			break
		}
		scheme, after := "https://", afterHTTPS
		if hasHTTP && (!hasHTTPS || len(beforeHTTP) < len(beforeHTTPS)) {
			scheme, after = "http://", afterHTTP
		}
		candidate := scheme + after
		if end := strings.IndexFunc(candidate, credentialURLTerminator); end >= 0 {
			candidate = candidate[:end]
		}
		consumed := len(candidate) - len(scheme)
		if consumed <= 0 {
			break
		}
		parsed, err := url.ParseRequestURI(candidate)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			text = after[consumed:]
			continue
		}
		if guardrails.ContainsCredential(candidate) {
			urls = append(urls, candidate)
		}
		text = after[consumed:]
	}
	return urls
}

func credentialURLTerminator(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("'\"`<>[]{}()，。；、", r)
}

func credentialURLClosingDelimiter(text string) string {
	r, _ := utf8.DecodeRuneInString(text)
	switch r {
	case '\'', '"', '`', ')', ']', '}', '>', '，', '。', '；', '、':
		return string(r)
	}
	return ""
}

// RedactUserConversationText returns the durable form of a user conversation
// endpoint. Persisted user rows and canonical history must use the same form:
// otherwise a restart can no longer associate a valid tool transcript with the
// conversation pair that produced it.
func RedactUserConversationText(s string) string {
	return RedactOperationalTokensInText(guardrails.RedactPII(s))
}

// RedactAssistantConversationText returns the durable form of an assistant
// conversation endpoint. Keep this paired with RedactUserConversationText so
// persistence and canonical history share one exact boundary rather than
// attempting to fuzzy-match redacted text during cold reconstruction.
func RedactAssistantConversationText(s string) string {
	redacted := RedactOperationalTokensInText(guardrails.RedactOutputLeak(s))
	// A redacted command is not a reusable command. The live SSE response may
	// still contain the original value, but the persisted/replayed copy cannot.
	// Make that durable boundary explicit instead of leaving a later reader (or
	// the model) to mistake Authorization=[...] for something executable.
	if guardrails.ContainsCredential(s) {
		return redacted + redactedConversationCredentialNotice
	}
	return redacted
}

const redactedConversationCredentialNotice = "\n\n注：此历史记录中的敏感参数已脱敏，不能直接复制执行；需要重试时请重新提供原始链接或参数。"

// ContainsToolProtocolMarkup detects provider/tool transport syntax that must
// never be rendered as assistant prose. It does not infer user intent or parse
// a tool call; malformed transport is failed closed at the response boundary.
func ContainsToolProtocolMarkup(s string) bool {
	for _, marker := range []string{
		"<｜DSML｜invoke", "<|DSML|invoke", "<tool_call>", "</tool_call>",
		"<function=", "<｜tool▁call｜>",
	} {
		if _, _, found := strings.Cut(s, marker); found {
			return true
		}
	}
	return false
}

// RedactKnownSecretsInText removes operational tokens plus explicit secret
// values already known to the caller (for example a password submitted through a
// workflow form). It is safe for user-visible text and ignores empty/very short
// values to avoid accidental over-redaction of common words.
func RedactKnownSecretsInText(s string, secrets []string) string {
	s = RedactOperationalTokensInText(s)
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if len(secret) < 4 {
			continue
		}
		s = strings.ReplaceAll(s, secret, redactedValue)
	}
	return s
}

func redactOperationalTokens(s string) string {
	return guardrails.RedactCredentialsWithReplacement(s, redactedValue)
}

func hashValue(v any) string {
	sum := sha256.Sum256([]byte(fmt.Sprint(v)))
	return fmt.Sprintf("[HASH:%x]", sum[:8])
}

func maskIPv4(s string) string {
	ip := net.ParseIP(s)
	if ip == nil {
		return s
	}
	v4 := ip.To4()
	if v4 == nil {
		return s
	}
	return fmt.Sprintf("%d.%d.x.x", v4[0], v4[1])
}
