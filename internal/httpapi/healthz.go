package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Healthz is a simple liveness probe handler.
func Healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
