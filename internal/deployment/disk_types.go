package deployment

// Data-disk type names. CreateAndAttachCompshareDisk's DiskType and
// GetCompShareInstancePrice's Disks[].Type use different naming schemes for
// Catalog/price APIs and the create API use different names for the same disk
// type. Keep that mapping in one place.
const (
	DiskTypeCloudSSD = "CLOUD_SSD"   // catalog / price-query disk type name
	DataDiskTypeSSD  = "SSDDataDisk" // CreateAndAttachCompshareDisk's DiskType for CLOUD_SSD
)
