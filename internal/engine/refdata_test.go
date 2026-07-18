package engine

import (
	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/workflow"
)

// zoneRefData wraps a zone snapshot as the workflow.ReferenceData executeWorkflow
// now takes. It keeps the many zone-era test callers terse (and import-free) after
// the signature moved from a bare *ZoneCatalogSnapshot to the whole ReferenceData
// so the image catalog can ride alongside it.
func zoneRefData(zone *deployment.ZoneCatalogSnapshot) workflow.ReferenceData {
	return workflow.ReferenceData{ZoneCatalog: zone}
}
