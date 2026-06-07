package tracegate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// ProbeObservation is one live-CLI turn captured by eval/agentic_rag_probe.ps1.
// It is the deterministic, REAL-DATA anchor for the mis-routing gap the unified
// agentic-RAG plan (P2) measures and that later phases (P4b / GATE A) diff
// against. Fields mirror the runner's summary.jsonl rows verbatim, so an
// after-state capture loads through the same path with no schema drift.
type ProbeObservation struct {
	ProbeID              string   `json:"probe_id"`
	Run                  int      `json:"run"`
	Intent               string   `json:"intent"`
	ActualRuntimeForm    string   `json:"actual_runtime_form"`
	RetrievalFired       bool     `json:"retrieval_fired"`
	RetrievalHits        int      `json:"retrieval_hits"`
	CitedChunkIDs        []string `json:"cited_chunk_ids"`
	SearchKnowledgeFired bool     `json:"search_knowledge_fired"`
	ToolActions          []string `json:"tool_actions"`
	StepTools            []string `json:"step_tools"`
	ReplyHead            string   `json:"reply_head"`
}

// ProbeSet returns the probe-set prefix ("sym" / "howto") encoded in ProbeID.
func (o ProbeObservation) ProbeSet() string {
	if i := strings.IndexByte(o.ProbeID, '-'); i > 0 {
		return o.ProbeID[:i]
	}
	return o.ProbeID
}

// ContractKey is the (intent, runtime_form, retrieval_fired) triple -- the three
// signals the before/after gate compares. Used to assert per-probe determinism.
func (o ProbeObservation) ContractKey() string {
	return fmt.Sprintf("intent=%s form=%q retrieval_fired=%t", o.Intent, o.ActualRuntimeForm, o.RetrievalFired)
}

// trimBOM drops a single leading UTF-8 BOM (U+FEFF) if present. PowerShell
// Add-Content -Encoding UTF8 writes one on the first line of the summary JSONL.
func trimBOM(s string) string {
	if r, sz := utf8.DecodeRuneInString(s); r == 0xFEFF {
		return s[sz:]
	}
	return s
}

// LoadProbeObservations parses the runner's JSONL summary, tolerating a leading
// UTF-8 BOM on the first line.
func LoadProbeObservations(r io.Reader) ([]ProbeObservation, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var out []ProbeObservation
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := trimBOM(strings.TrimSpace(scanner.Text()))
		if line == "" {
			continue
		}
		var o ProbeObservation
		if err := json.Unmarshal([]byte(line), &o); err != nil {
			return nil, fmt.Errorf("decode observation line %d: %w", lineNo, err)
		}
		out = append(out, o)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan observations: %w", err)
	}
	return out, nil
}
