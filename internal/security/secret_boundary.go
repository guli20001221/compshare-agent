package security

import (
	"crypto/sha256"
	"fmt"
	"net"
	"regexp"
	"strings"

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
