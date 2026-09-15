package security

import (
	"crypto/sha256"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/compshare-agent/internal/agentprotocol"
	"github.com/compshare-agent/internal/guardrails"
)

const redactedValue = "[REDACTED]"

var separatorRE = regexp.MustCompile(`[^a-z0-9]+`)

// RedactForLLM replaces credential-named fields before a structured value is
// passed into model context. It returns a deep-redacted copy and never mutates
// the input value.
func RedactForLLM(v any) any {
	return redactValue(v, redactModeLLM, "")
}

// SSHLoginCommandForLLM returns the upstream login command only when it matches
// the same narrow, credential-free shape accepted by RedactForLLM.
func SSHLoginCommandForLLM(raw string) (string, bool) {
	projected, _ := redactField("SshLoginCommand", raw, redactModeLLM).(string)
	if projected == redactedValue || strings.TrimSpace(projected) == "" {
		return "", false
	}
	return strings.TrimSpace(projected), true
}

// RedactForTrace replaces credential-named fields and masks/hash-stabilizes
// sensitive telemetry before writing traces or audit logs. It returns a
// deep-redacted copy and never mutates the input value.
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
		if s, ok := typed.(string); ok && mode == redactModeTrace && isIPKey(parentKey) {
			return maskIPv4(s)
		}
		return typed
	}
}

func redactField(key string, value any, mode redactMode) any {
	if isSecretKey(key) {
		if mode == redactModeLLM && isPlainSSHLoginCommand(key, value) {
			// An allowlisted login command is an endpoint, not a credential;
			// trace persistence remains unchanged.
			return value
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

// sshConnectionKeys are exact fields whose value is an SSH endpoint command.
// Credential-like fields that merely mention SSH remain redacted.
var sshConnectionKeys = map[string]bool{
	"sshlogincommand": true,
}

// isPlainSSHLoginCommand reports whether this is the authoritative SSH field
// carrying a value that is safe to show. Both halves must hold.
//
// The value check fails closed and accepts only the shapes the upstream returns:
//
//	ssh root@1.2.3.4 -p 22
//	ssh -p 23120 root@cpod-abc.podtcp.compshare.cn
//	ssh ubuntu@1.2.3.5
//
// One `ssh`, at most one `-p <port>`, exactly one `user@host`, and no other
// tokens are allowed. Everything else falls back to [REDACTED].
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

// UserAuthorizationReference is a request-local secret paired with the opaque
// marker the inner probe tool accepts in its place. Value stays on the private
// transport path; Reference is an identifier, not an authorization grant, and
// is not valid in a later turn.
type UserAuthorizationReference struct {
	Reference string
	Value     string
}

// UserAuthorizationHeaders extracts the current request's usable Authorization
// header values. The text the model reads is not rewritten; a reference is
// exposed only through the short-lived probe tool schema and is never written
// into conversation text, where a cold replay could mistake an expired
// reference for a live one.
func UserAuthorizationHeaders(s string) []UserAuthorizationReference {
	extracted := guardrails.AuthorizationHeaderValues(s)
	refs := make([]UserAuthorizationReference, 0, len(extracted))
	for _, item := range extracted {
		refs = append(refs, UserAuthorizationReference{Reference: item.Reference, Value: item.Value})
	}
	return refs
}

// RedactKnownAuthorizationText removes the exact values already captured by the
// request-local capability channel. A planner Task that copied one of them, or
// only its credential part, cannot carry it into a confirmation, inner prompt or
// AuditWriter; the inner run reaches that value through its reference alone.
func RedactKnownAuthorizationText(s string, authorizations []string) string {
	for _, authorization := range authorizations {
		authorization = strings.TrimSpace(authorization)
		if len(authorization) < 4 {
			continue
		}
		s = strings.ReplaceAll(s, authorization, redactedValue)
		if parts := strings.Fields(authorization); len(parts) > 1 {
			credential := parts[len(parts)-1]
			if len(credential) >= 4 {
				s = strings.ReplaceAll(s, credential, redactedValue)
			}
		}
	}
	return s
}

// PersistedAssistantText is the persisted form of an assistant conversation
// endpoint. Canonical history uses the same form, so hot and cold replays share
// one exact boundary. The Feishu support marker is an adapter-private display
// instruction, not conversation content: the semantic completion is persisted
// instead, so a cold session cannot replay the marker to the model or trigger
// the adapter without the original handoff tool call.
func PersistedAssistantText(s string) string {
	return strings.ReplaceAll(s, agentprotocol.FeishuCustomerSupportMarker,
		agentprotocol.CustomerSupportHistoryCompletion)
}

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

// RedactKnownSecretsInText removes explicit secret values already known to the
// caller (for example a password submitted through a workflow form). Empty and
// very short values are ignored to avoid over-redacting common words.
func RedactKnownSecretsInText(s string, secrets []string) string {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if len(secret) < 4 {
			continue
		}
		s = strings.ReplaceAll(s, secret, redactedValue)
	}
	return s
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
