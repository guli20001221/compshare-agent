package httpapi

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/bitly/go-simplejson"
	"github.com/compshare-agent/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// BaseRequest holds the common identity fields parsed from every gateway request.
type BaseRequest struct {
	Action      string
	RequestUUID string
	Owner       store.Owner
	ProjectID   string
	UserEmail   string
}

// ParseBaseRequest reads the request body (POST only), resolves identity fields,
// and returns the raw simplejson map alongside the typed BaseRequest.
// Supports application/json and application/x-www-form-urlencoded.
func ParseBaseRequest(c *gin.Context) (*simplejson.Json, BaseRequest, error) {
	raw, err := parseBody(c.Request)
	if err != nil {
		return nil, BaseRequest{}, err
	}

	action := raw.Get("Action").MustString()
	if action == "" {
		return nil, BaseRequest{}, ErrInvalidParam.WithMessage("missing Action")
	}

	requestUUID := raw.Get("request_uuid").MustString()
	if requestUUID == "" {
		requestUUID = raw.Get("RequestId").MustString()
	}
	if requestUUID == "" {
		requestUUID = uuid.NewString()
		raw.Set("request_uuid", requestUUID)
	}

	topOrg, err := readUint32(raw, "top_organization_id")
	if err != nil || topOrg == 0 {
		return nil, BaseRequest{}, ErrInvalidParam.WithMessage("missing top_organization_id")
	}
	org, err := readUint32(raw, "organization_id")
	if err != nil || org == 0 {
		return nil, BaseRequest{}, ErrInvalidParam.WithMessage("missing organization_id")
	}

	projectID := raw.Get("ProjectId").MustString()
	userEmail := raw.Get("user_email").MustString()

	return raw, BaseRequest{
		Action:      action,
		RequestUUID: requestUUID,
		Owner:       store.Owner{TopOrganizationID: topOrg, OrganizationID: org},
		ProjectID:   projectID,
		UserEmail:   userEmail,
	}, nil
}

// ParseBaseRequestFromHeaders resolves the connection-scoped identity for a
// WebSocket upgrade. Unlike ParseBaseRequest (which reads the POST body), the
// gateway injects identity into the upgrade request's HTTP headers because a
// WS upgrade is a bodyless GET. Action comes from the query string.
//
// Header → field mapping (per gateway contract):
//
//	X-Company-Id      → top_organization_id
//	X-Organization-Id → organization_id
//	X-Request-Id      → request_uuid (generated when absent)
//	X-User-Email      → user_email (optional; may be absent over WS)
//	?Action=          → Action
//
// Api-Metadata ("user-id=…, organization-id=…, company-id=…, request-id=…")
// is parsed as a fallback for any identity field missing from a dedicated header.
func ParseBaseRequestFromHeaders(r *http.Request) (BaseRequest, error) {
	action := r.URL.Query().Get("Action")
	if action == "" {
		return BaseRequest{}, ErrInvalidParam.WithMessage("missing Action")
	}

	meta := parseAPIMetadata(r.Header.Get("Api-Metadata"))

	topOrg := headerUint32(r, "X-Company-Id", meta["company-id"])
	if topOrg == 0 {
		return BaseRequest{}, ErrInvalidParam.WithMessage("missing top_organization_id")
	}
	org := headerUint32(r, "X-Organization-Id", meta["organization-id"])
	if org == 0 {
		return BaseRequest{}, ErrInvalidParam.WithMessage("missing organization_id")
	}

	requestUUID := firstNonEmpty(r.Header.Get("X-Request-Id"), meta["request-id"])
	if requestUUID == "" {
		requestUUID = uuid.NewString()
	}

	return BaseRequest{
		Action:      action,
		RequestUUID: requestUUID,
		Owner:       store.Owner{TopOrganizationID: topOrg, OrganizationID: org},
		ProjectID:   r.URL.Query().Get("ProjectId"),
		UserEmail:   r.Header.Get("X-User-Email"),
	}, nil
}

// headerUint32 reads a uint32 from the named header, falling back to fallback
// (e.g. a value parsed from Api-Metadata) when the header is empty.
func headerUint32(r *http.Request, header, fallback string) uint32 {
	s := r.Header.Get(header)
	if s == "" {
		s = fallback
	}
	if s == "" {
		return 0
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(v)
}

// parseAPIMetadata splits a comma-separated "key=value, key=value" header into
// a map. Keys and values are trimmed; malformed segments are skipped.
func parseAPIMetadata(s string) map[string]string {
	out := map[string]string{}
	if s == "" {
		return out
	}
	for _, seg := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(seg, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseBody reads and parses the POST body into a simplejson.Json.
// Form-encoded bodies are converted to a JSON map for uniform access.
// Content-Type parameters (e.g. "; charset=utf-8") are stripped via
// mime.ParseMediaType before comparison, so both
// "application/x-www-form-urlencoded" and
// "application/x-www-form-urlencoded; charset=utf-8" are handled correctly.
func parseBody(r *http.Request) (*simplejson.Json, error) {
	if r.Method != http.MethodPost {
		return nil, ErrInvalidParam.WithMessage("only support post request")
	}
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "application/x-www-form-urlencoded" {
		if err := r.ParseForm(); err != nil {
			return nil, ErrInvalidParam.WithMessage("invalid form body")
		}
		m := map[string]any{}
		for k, values := range r.PostForm {
			if len(values) > 0 {
				m[k] = values[0]
			}
		}
		return simplejson.NewJson(mustJSON(m))
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, ErrInvalidParam.WithMessage("read body: %v", err)
	}
	if len(body) == 0 {
		return nil, ErrInvalidParam.WithMessage("empty body")
	}
	data, err := simplejson.NewJson(body)
	if err != nil {
		return nil, ErrInvalidParam.WithMessage("invalid json body")
	}
	return data, nil
}

// mustJSON marshals v to JSON, panicking on error (only possible for unmarshalable types).
func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// readUint32 reads a field from the simplejson map as uint32.
// Accepts both numeric and numeric-string values.
func readUint32(raw *simplejson.Json, key string) (uint32, error) {
	if v, err := raw.Get(key).Uint64(); err == nil {
		return uint32(v), nil
	}
	s := raw.Get(key).MustString()
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseUint(s, 10, 32)
	return uint32(v), err
}
