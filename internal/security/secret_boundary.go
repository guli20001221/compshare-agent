package security

import (
	"crypto/sha256"
	"fmt"
	"net"
	"regexp"
	"strings"
)

const redactedValue = "[REDACTED]"

var separatorRE = regexp.MustCompile(`[^a-z0-9]+`)
var bearerTokenRE = regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9_\-.]{20,})`)

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
		if s, ok := typed.(string); ok && containsBearerToken(s) {
			return redactBearerTokens(s)
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
	normalized := normalizeKey(key)
	return strings.Contains(normalized, "password") ||
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
		// Match any SSH access command key, not just "SSHCommand": the upstream
		// DescribeCompShareInstance field is "SshLoginCommand" (→ "sshlogincommand"),
		// which the literal "sshcommand" substring misses because the "login" infix
		// breaks it. ssh+command covers SshCommand / SshLoginCommand / future
		// variants. B8.3 deploy surfaces this field into saga StepTrace.Result.
		(strings.Contains(normalized, "ssh") && strings.Contains(normalized, "command"))
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

func containsBearerToken(s string) bool {
	return bearerTokenRE.MatchString(s)
}

func redactBearerTokens(s string) string {
	return bearerTokenRE.ReplaceAllString(s, "Bearer "+redactedValue)
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
