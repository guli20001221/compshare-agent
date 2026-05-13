package envelope

import (
	"net/url"
	"regexp"
	"strings"
)

var internalSurfacePathRE = regexp.MustCompile(`(?i)/(?:admin|workorder|internal)(?:/|$)`)

var signedSurfaceQueryKeys = map[string]struct{}{
	"token":           {},
	"signature":       {},
	"sign":            {},
	"x-oss-signature": {},
	"x-amz-signature": {},
}

var temporarySurfaceQueryKeys = map[string]struct{}{
	"expires":    {},
	"expire":     {},
	"expiration": {},
}

type SurfaceURLDecision struct {
	Allowed bool
	Reason  string
}

// Keep this policy in sync with scripts/rag_w0/common.py. Python remains the
// offline validator for W0 chunks; this Go copy protects runtime projections.
func IsAllowedSurfaceURL(rawURL string) SurfaceURLDecision {
	if strings.TrimSpace(rawURL) == "" {
		return rejectSurfaceURL("surface_url_empty")
	}

	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return rejectSurfaceURL("invalid_url")
	}

	host := strings.ToLower(parsed.Hostname())
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}

	if parsed.Scheme != "https" {
		return rejectSurfaceURL("scheme_not_https")
	}
	if deniedInternalSurfaceHost(host) {
		return rejectSurfaceURL("denied_internal_host")
	}
	if internalSurfacePathRE.MatchString(path) {
		return rejectSurfaceURL("internal_path")
	}
	if hasSurfaceQueryKey(parsed, signedSurfaceQueryKeys) {
		return rejectSurfaceURL("signed_url_query")
	}
	if isTemporarySurfaceDownload(parsed, host) {
		return rejectSurfaceURL("temporary_download")
	}
	if host == "console.compshare.cn" {
		return SurfaceURLDecision{Allowed: true}
	}
	if host == "www.compshare.cn" && strings.HasPrefix(path, "/docs/") {
		return SurfaceURLDecision{Allowed: true}
	}
	return rejectSurfaceURL("host_not_in_allowlist")
}

func rejectSurfaceURL(reason string) SurfaceURLDecision {
	return SurfaceURLDecision{Reason: reason}
}

func deniedInternalSurfaceHost(host string) bool {
	return strings.Contains(host, "gitlab") ||
		host == "feishu.cn" ||
		strings.HasSuffix(host, ".feishu.cn") ||
		host == "lark.com" ||
		strings.HasSuffix(host, ".lark.com") ||
		strings.Contains(host, ".feishu.")
}

func hasSurfaceQueryKey(parsed *url.URL, keys map[string]struct{}) bool {
	for key := range parsed.Query() {
		if _, ok := keys[strings.ToLower(key)]; ok {
			return true
		}
	}
	return false
}

func isTemporarySurfaceDownload(parsed *url.URL, host string) bool {
	if hasSurfaceQueryKey(parsed, temporarySurfaceQueryKeys) {
		return true
	}
	hostSegments := strings.FieldsFunc(host, func(r rune) bool {
		return r == '.'
	})
	for _, segment := range hostSegments {
		if segment == "tmp" || segment == "temporary" {
			return true
		}
		if segment == "download" && parsed.RawQuery != "" {
			return true
		}
	}
	return false
}
