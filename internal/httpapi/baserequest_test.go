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

func TestParseBaseRequestForm(t *testing.T) {
	c := testContext("application/x-www-form-urlencoded", "Action=SendCSAgentChat&SessionId=sess-1&request_uuid=req-1&top_organization_id=123&organization_id=456")

	raw, base, err := ParseBaseRequest(c)

	require.NoError(t, err)
	assert.Equal(t, "SendCSAgentChat", base.Action)
	assert.Equal(t, "req-1", base.RequestUUID)
	assert.Equal(t, "sess-1", raw.Get("SessionId").MustString())
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
