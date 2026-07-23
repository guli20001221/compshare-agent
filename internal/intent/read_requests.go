package intent

import "github.com/compshare-agent/internal/platform"

// The read-request contract (interface, MissingField, status vocabulary) is
// owned by internal/platform so a typed capability can implement it without
// importing this router package. These aliases keep intent.ReadRequest /
// intent.MissingField references compiling unchanged. Every concrete read
// request type now lives in the capability package beside its handler — the
// last (CFS) ones moved out in P3.4.
type ReadRequest = platform.ReadRequest

type MissingField = platform.MissingField
