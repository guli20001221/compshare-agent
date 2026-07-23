package engine

import (
	"github.com/compshare-agent/internal/actionresolver"
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

// mustConfirmable builds the confirmableAction that executeResolvedWorkflow now
// takes, going through the SAME newConfirmableAction gate production does — tests
// exercise the real door rather than reaching around it. It marks the action
// ReadyForConfirmation (the direct-execution case these callers assert), so a
// non-gate-eligible action would panic here rather than silently execute.
func mustConfirmable(operation string, args map[string]any, ref workflow.ReferenceData) confirmableAction {
	ca, ok := newConfirmableAction(resolvedProposal{
		action:        actionresolver.ResolvedAction{Operation: operation, Arguments: args, ReadyForConfirmation: true},
		referenceData: ref,
	})
	if !ok {
		panic("mustConfirmable: constructed a non-gate-eligible confirmableAction")
	}
	return ca
}
