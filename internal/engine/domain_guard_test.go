package engine

import (
	"testing"

	"github.com/compshare-agent/internal/knowledge"
)

// TestAllCitedOffDomain pins the #5 verdict, especially the fail-safe arms: an
// unknown question area or no judgeable chunk area must NEVER flag (off=false),
// so the refuse arm can never suppress an answer it cannot actually judge.
func TestAllCitedOffDomain(t *testing.T) {
	cases := []struct {
		name         string
		questionArea string
		areas        []string
		wantOff      bool
		wantEmpty    bool
	}{
		{"unknown question area never flags", "", []string{"billing_rule"}, false, true},
		{"no judgeable chunk area never flags", "resource_purchase", []string{"", ""}, false, false},
		{"all cited off-domain flags", "resource_purchase", []string{"billing_rule", "billing_rule"}, true, false},
		{"one on-domain chunk clears", "resource_purchase", []string{"billing_rule", "resource_purchase"}, false, false},
		{"case-insensitive match clears", "resource_purchase", []string{"Resource_Purchase"}, false, false},
		{"blank areas skipped, remaining off-domain flags", "resource_purchase", []string{"", "billing_rule"}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			off, empty := allCitedOffDomain(c.questionArea, c.areas)
			if off != c.wantOff || empty != c.wantEmpty {
				t.Fatalf("allCitedOffDomain(%q, %v) = (off=%v, empty=%v), want (off=%v, empty=%v)",
					c.questionArea, c.areas, off, empty, c.wantOff, c.wantEmpty)
			}
		})
	}
}

// TestDomainMatchGuardDefaultOff guards the load-bearing contract: the Go-package
// default is OFF, so unit tests and any caller that never calls Set see the
// refuse arm disabled (trace-only). It must be flippable and restorable.
func TestDomainMatchGuardDefaultOff(t *testing.T) {
	if DomainMatchGuardEnabled() {
		t.Fatal("domain match guard must default OFF in the Go package")
	}
	t.Cleanup(func() { SetDomainMatchGuardEnabled(false) })
	SetDomainMatchGuardEnabled(true)
	if !DomainMatchGuardEnabled() {
		t.Fatal("SetDomainMatchGuardEnabled(true) did not enable")
	}
	SetDomainMatchGuardEnabled(false)
	if DomainMatchGuardEnabled() {
		t.Fatal("SetDomainMatchGuardEnabled(false) did not disable")
	}
}

// TestLedgerProductAreas_CarriesProductArea verifies the EvidenceItem.ProductArea
// copy reaches the verdict input — without it the agent-loop guard would always
// see empty areas and never judge.
func TestLedgerProductAreas_CarriesProductArea(t *testing.T) {
	ledger := knowledge.BuildEvidenceLedger("q", []knowledge.RetrievalHit{
		{Kept: true, Chunk: knowledge.KBChunk{ChunkID: "c1", ProductArea: "billing_rule"}},
		{Kept: true, Chunk: knowledge.KBChunk{ChunkID: "c2", ProductArea: "resource_purchase"}},
	}, 5)
	areas := ledgerProductAreas(ledger)
	if len(areas) != 2 || areas[0] != "billing_rule" || areas[1] != "resource_purchase" {
		t.Fatalf("ledgerProductAreas = %v, want [billing_rule resource_purchase]", areas)
	}
}
