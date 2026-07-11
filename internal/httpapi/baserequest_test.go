package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBaseRequestJSONGeneratesRequestUUID(t *testing.T) {
	c := testContext("application/json", `{"Action":"GetCSAgentMeta","top_organization_id":123,"organization_id":456}`)

	raw, base, err := ParseBaseRequest(c)

	require.NoError(t, err)
	assert.Equal(t, "GetCSAgentMeta", base.Action)
	assert.Equal(t, uint32(123), base.Owner.TopOrganizationID)
	assert.Equal(t, uint32(456), base.Owner.OrganizationID)
	assert.Empty(t, base.ProjectID)
	assert.NotEmpty(t, base.RequestUUID)
	got, _ := raw.Get("request_uuid").String()
	assert.Equal(t, base.RequestUUID, got)
}

func TestParseBaseRequestPicksUpProjectID(t *testing.T) {
	c := testContext("application/json", `{"Action":"GetCSAgentMeta","top_organization_id":123,"organization_id":456,"ProjectId":"org-cwy2qk"}`)

	_, base, err := ParseBaseRequest(c)

	require.NoError(t, err)
	assert.Equal(t, "org-cwy2qk", base.ProjectID)
}

func TestParseBaseRequestPicksUpUserEmail(t *testing.T) {
	c := testContext("application/json", `{"Action":"GetCSAgentMeta","top_organization_id":123,"organization_id":456,"user_email":"operator@example.com"}`)

	_, base, err := ParseBaseRequest(c)

	require.NoError(t, err)
	assert.Equal(t, "operator@example.com", base.UserEmail)
}

func TestParseBaseRequestForm(t *testing.T) {
	c := testContext("application/x-www-form-urlencoded", "Action=SendCSAgentChat&SessionId=sess-1&request_uuid=req-1&top_organization_id=123&organization_id=456&user_email=operator%40example.com")

	raw, base, err := ParseBaseRequest(c)

	require.NoError(t, err)
	assert.Equal(t, "SendCSAgentChat", base.Action)
	assert.Equal(t, "req-1", base.RequestUUID)
	assert.Equal(t, "sess-1", raw.Get("SessionId").MustString())
	assert.Equal(t, "operator@example.com", base.UserEmail)
}

func TestParseBaseRequestParsesNumericIdentityFields(t *testing.T) {
	c := testContext("application/json", `{"Action":"GetCSAgentMeta","top_organization_id":123,"organization_id":456,"company_id":789,"account_id":321,"channel":9}`)

	_, base, err := ParseBaseRequest(c)

	require.NoError(t, err)
	assert.Equal(t, uint32(789), base.CompanyID)
	assert.Equal(t, uint32(321), base.AccountID)
	assert.Equal(t, uint32(9), base.Channel)
}

func TestParseBaseRequestCompanyIDDefaultsToTopOrganization(t *testing.T) {
	c := testContext("application/json", `{"Action":"GetCSAgentMeta","top_organization_id":123,"organization_id":456}`)

	_, base, err := ParseBaseRequest(c)

	require.NoError(t, err)
	assert.Equal(t, uint32(123), base.CompanyID)
}

// TestParseBaseRequestToleratesNonNumericIdentityFields guards against a
// fail-closed regression: company_id/account_id/channel are gateway
// billing-attribution passthroughs, not identity/auth fields, so a
// malformed value must degrade to 0 rather than rejecting the whole
// request (which would 400 every Action sharing this parser, not just
// the one that cares about these fields).
func TestParseBaseRequestToleratesNonNumericIdentityFields(t *testing.T) {
	c := testContext("application/json", `{"Action":"GetCSAgentMeta","top_organization_id":123,"organization_id":456,"company_id":"app_ios","account_id":"app_ios","channel":"app_ios"}`)

	_, base, err := ParseBaseRequest(c)

	require.NoError(t, err)
	assert.Equal(t, uint32(123), base.CompanyID) // falls back to top_organization_id
	assert.Equal(t, uint32(0), base.AccountID)
	assert.Equal(t, uint32(0), base.Channel)
}

func TestParseBaseRequestFromHeadersCarriesGatewayIdentity(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/ws?Action=CreateCSAgentWS&ProjectId=org-cwy2qk", nil)
	req.Header.Set("X-Company-Id", "101")
	req.Header.Set("X-Organization-Id", "202")
	req.Header.Set("Api-Metadata", "user-id=303, channel-id=124")
	req.Header.Set("X-Forwarded-For", "10.0.0.9, 10.0.0.10")
	req.RemoteAddr = "[::1]:12345"

	base, err := ParseBaseRequestFromHeaders(req)

	require.NoError(t, err)
	assert.Equal(t, "CreateCSAgentWS", base.Action)
	assert.Equal(t, uint32(101), base.Owner.TopOrganizationID)
	assert.Equal(t, uint32(202), base.Owner.OrganizationID)
	assert.Equal(t, uint32(101), base.CompanyID)
	assert.Equal(t, uint32(303), base.AccountID)
	assert.Equal(t, uint32(124), base.Channel)
	assert.Equal(t, "10.0.0.9", base.ClientIP)
	assert.Equal(t, "org-cwy2qk", base.ProjectID)
}

func TestParseBaseRequestRejectsMissingOrganization(t *testing.T) {
	c := testContext("application/json", `{"Action":"GetCSAgentMeta","top_organization_id":123}`)

	_, _, err := ParseBaseRequest(c)

	require.Error(t, err)
	apiErr := AsAPIError(err)
	assert.Equal(t, "InvalidParam", apiErr.Code)
}

// TestParseBaseRequestJSONWithCharset verifies that "application/json; charset=utf-8"
// is treated as JSON (not rejected or misclassified as form data).
func TestParseBaseRequestJSONWithCharset(t *testing.T) {
	c := testContext("application/json; charset=utf-8", `{"Action":"GetCSAgentMeta","top_organization_id":1,"organization_id":2}`)

	_, base, err := ParseBaseRequest(c)

	require.NoError(t, err)
	assert.Equal(t, "GetCSAgentMeta", base.Action)
}

// TestParseBaseRequestFormWithCharset verifies that
// "application/x-www-form-urlencoded; charset=utf-8" is correctly parsed as form data.
func TestParseBaseRequestFormWithCharset(t *testing.T) {
	c := testContext("application/x-www-form-urlencoded; charset=utf-8", "Action=SendCSAgentChat&SessionId=sess-x&request_uuid=req-2&top_organization_id=1&organization_id=2")

	raw, base, err := ParseBaseRequest(c)

	require.NoError(t, err)
	assert.Equal(t, "SendCSAgentChat", base.Action)
	assert.Equal(t, "sess-x", raw.Get("SessionId").MustString())
	assert.Equal(t, "req-2", base.RequestUUID)
}

func testContext(contentType, body string) *gin.Context {
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/api/gateway", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	return c
}
