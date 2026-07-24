package platform

import "strings"

// IsPodInstanceID follows the upstream resource-id contract used by API
// dispatchers: Pod instances use the cpod- prefix; UHost instances use uhost-.
func IsPodInstanceID(id string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(id)), "cpod-")
}
