package guardrails

import "strings"

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
