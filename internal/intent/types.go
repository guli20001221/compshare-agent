package intent

// Intent is the stable identifier used by the typed read-capability catalog.
// It does not select a separate runtime or prompt.
type Intent string

const (
	IntentMonitorQuery          Intent = "monitor_query"
	IntentMonitorHistory        Intent = "monitor_history"
	IntentResourceInfo          Intent = "resource_info"
	IntentInstanceAccess        Intent = "instance_access"
	IntentGPUSpecsQuery         Intent = "gpu_specs_query"
	IntentStockAvailability     Intent = "stock_availability"
	IntentImageList             Intent = "image_list"
	IntentImageTagCatalog       Intent = "image_tag_catalog"
	IntentZoneCatalog           Intent = "zone_catalog"
	IntentModelRepositoryBrowse Intent = "model_repository_browse"
	IntentNetAcceleratorStatus  Intent = "network_accelerator_status"
	IntentRefundEstimate        Intent = "refund_estimate"
	IntentCFSInfo               Intent = "cfs_info"
	IntentPricingQuery          Intent = "pricing_query"
)
