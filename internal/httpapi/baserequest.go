package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

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

	return raw, BaseRequest{
		Action:      action,
		RequestUUID: requestUUID,
		Owner:       store.Owner{TopOrganizationID: topOrg, OrganizationID: org},
	}, nil
}

// parseBody reads and parses the POST body into a simplejson.Json.
// Form-encoded bodies are converted to a JSON map for uniform access.
func parseBody(r *http.Request) (*simplejson.Json, error) {
	if r.Method != http.MethodPost {
		return nil, ErrInvalidParam.WithMessage("only support post request")
	}
	contentType := r.Header.Get("Content-Type")
	if contentType == "application/x-www-form-urlencoded" {
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
