package intent

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/boundarypacks"
)

// stockVsResourceRule is the directive that PR5 moved out of the base scaffold
// into the stock_vs_resource boundary pack. The test pins that the move is
// clean: the base no longer carries it, the pack does, and the assembled prompt
// carries it exactly once — no duplication / attention shift.
const stockVsResourceRule = "Classify inventory availability questions about whether a GPU model has stock, is available, is sold out, or has data-center inventory as stock_availability. Do not route them to resource_info; resource_info is only for the user's own CompShare instances."

func TestStockVsResourceBoundary_MovedOutOfBasePrompt(t *testing.T) {
	// 1. The base scaffold must no longer carry the rule (it moved to the pack).
	if base := basePromptScaffold(); strings.Contains(base, stockVsResourceRule) {
		t.Error("base scaffold still contains the stock-vs-resource rule; it must live only in the boundary pack")
	}

	// 2. The boundary-pack projection must carry the rule.
	boundary := strings.Join(boundarypacks.BoundaryPromptFragments(), "\n")
	if !strings.Contains(boundary, stockVsResourceRule) {
		t.Error("boundary-pack projection does not contain the stock-vs-resource rule")
	}

	// 3. The fully assembled prompt must carry the rule EXACTLY once — no
	// duplication between base and pack (duplication would bloat the prompt and
	// shift the model's attention).
	if n := strings.Count(buildSystemPrompt(), stockVsResourceRule); n != 1 {
		t.Errorf("assembled system prompt contains the stock-vs-resource rule %d times, want exactly 1", n)
	}
}
