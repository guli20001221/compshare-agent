package guardrails

import (
	"regexp"
	"strings"
)

var passwordSecretRegex = regexp.MustCompile(`(?i)(?:密码|password)\s*(?:重置为|设置为|改为|改成|设为|设成|换为|换成|为|=|:)?\s*([^\s，。；,;]+)`)

// ExtractPasswordTurnSecrets finds explicit new password values in one user
// turn. It is a persistence/transport redaction helper; callers should still
// pass the original message to the agent so the requested operation can run.
func ExtractPasswordTurnSecrets(message string) []string {
	if strings.TrimSpace(message) == "" {
		return nil
	}
	matches := passwordSecretRegex.FindAllStringSubmatch(message, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]bool{}
	secrets := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		candidate := strings.Trim(match[1], "`'\"“”‘’")
		candidate = strings.TrimSpace(candidate)
		if !looksLikePasswordTurnSecret(candidate) || seen[candidate] {
			continue
		}
		seen[candidate] = true
		secrets = append(secrets, candidate)
	}
	if len(secrets) == 0 {
		return nil
	}
	return secrets
}

func looksLikePasswordTurnSecret(candidate string) bool {
	if len([]rune(candidate)) < 6 {
		return false
	}
	for _, r := range candidate {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return true
		}
	}
	return false
}

// RedactTurnSecrets replaces exact secrets captured from the current turn.
func RedactTurnSecrets(text string, secrets []string) string {
	out := text
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		out = strings.ReplaceAll(out, secret, CredentialRedactedOutput)
	}
	return out
}
