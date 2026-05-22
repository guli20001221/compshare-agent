package sse

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	writer := New(c.Writer)
	err := writer.WriteEvent("token", map[string]string{"Text": "你好"})

	require.NoError(t, err)
	assert.Equal(t, "event: token\ndata: {\"Text\":\"你好\"}\n\n", rec.Body.String())
}

func TestWriteKeepalive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	writer := New(c.Writer)
	err := writer.WriteKeepalive()

	require.NoError(t, err)
	assert.Equal(t, ":keepalive\n\n", rec.Body.String())
}
