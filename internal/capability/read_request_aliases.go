package capability

import "github.com/compshare-agent/internal/intent"

// Aliases keep the capability catalog as the public discovery surface while
// the concrete request contracts live beside the handlers that consume them.
type ReadRequest = intent.ReadRequest
type MissingField = intent.MissingField
type StockAvailabilityRequest = intent.StockAvailabilityRequest
type CFSListRequest = intent.CFSListRequest
type CFSCreatePriceRequest = intent.CFSCreatePriceRequest
type CFSUpgradePriceRequest = intent.CFSUpgradePriceRequest
type CFSRefundEstimateRequest = intent.CFSRefundEstimateRequest
