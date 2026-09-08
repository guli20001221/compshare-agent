package engine

import (
	"strings"

	"github.com/compshare-agent/internal/tools"
)

// RetCode 230 is shared by parameter errors, so the typed field name also has
// to identify the unavailable image. Other errors retain their own meaning.
func isImageUnavailableError(err error) bool {
	apiErr, ok := tools.UpstreamAPIErrorFrom(err)
	return ok && apiErr.Code == 230 && strings.Contains(apiErr.Message, "CompShareImageId")
}
