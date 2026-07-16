package capability

import "github.com/compshare-agent/internal/intent"

// Aliases keep the capability catalog as the public discovery surface while
// the concrete request contracts live beside the handlers that consume them.
type ReadRequest = intent.ReadRequest
type MissingField = intent.MissingField
type MonitorHistoryRequest = intent.MonitorHistoryRequest
type GPUSpecsRequest = intent.GPUSpecsRequest
type StockAvailabilityRequest = intent.StockAvailabilityRequest
type ImageListRequest = intent.ImageListRequest
type ImageTagCatalogRequest = intent.ImageTagCatalogRequest
type ModelRepositoryRequest = intent.ModelRepositoryRequest
type CFSListRequest = intent.CFSListRequest
type CFSCreatePriceRequest = intent.CFSCreatePriceRequest
type CFSUpgradePriceRequest = intent.CFSUpgradePriceRequest
type CFSRefundEstimateRequest = intent.CFSRefundEstimateRequest
