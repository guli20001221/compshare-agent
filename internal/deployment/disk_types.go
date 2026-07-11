package deployment

// Data-disk type names. CreateAndAttachCompshareDisk's DiskType and
// GetCompShareInstancePrice's Disks[].Type use different naming schemes for
// the same underlying disk type (catalog/price-query uses the CLOUD_X form,
// the create call uses upstream's UDisk-backend XDataDisk form) — named here
// once instead of as scattered raw string literals, so a future change (or a
// second supported type) has one place to look. Only the CLOUD_SSD pairing
// is used today; CreateDiskWorkflow always requests SSD and doesn't yet read
// the live per-GPU/zone catalog to pick a different type (see the ledger note
// below before wiring that up — no confirmed case has needed it yet).
const (
	DiskTypeCloudSSD = "CLOUD_SSD"   // catalog / price-query disk type name
	DataDiskTypeSSD  = "SSDDataDisk" // CreateAndAttachCompshareDisk's DiskType for CLOUD_SSD
)
