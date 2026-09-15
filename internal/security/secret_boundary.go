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

// RedactForTrace replaces credential-named fields and masks/hash-stabilizes
// sensitive telemetry before a structured value is hashed for a trace or
// written to an audit log. It returns a deep-redacted copy and never mutates
// the input value. The model-visible copy of the same value is not redacted:
// the platform's own fields reach the Agent as the platform returned them.
func RedactForTrace(v any) any {
	return redactValue(v, "")
}

func redactValue(v any, parentKey string) any {
	switch typed := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, child := range typed {
			out[k] = redactField(k, child)
		}
		return out
	case map[any]any:
		out := make(map[any]any, len(typed))
		for k, child := range typed {
			key, _ := k.(string)
			out[k] = redactField(key, child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = redactValue(child, parentKey)
		}
		return out
	default:
		if s, ok := typed.(string); ok && isIPKey(parentKey) {
			return maskIPv4(s)
		}
		return typed
	}
}

func redactField(key string, value any) any {
	if guardrails.IsCredentialKey(key) {
		return redactedValue
	}
	if isBillingOrCostKey(key) {
		return hashValue(value)
	}
	if isIPKey(key) {
		if s, ok := value.(string); ok {
			return maskIPv4(s)
		}
	}
	return redactValue(value, key)
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
