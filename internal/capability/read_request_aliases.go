package capability

import "github.com/compshare-agent/internal/intent"

// The read-request interface and MissingField vocabulary are re-exported so the
// catalog and kernel keep compiling; every concrete request contract now lives
// in the capability package beside the handler that consumes it.
type ReadRequest = intent.ReadRequest
type MissingField = intent.MissingField
