package tracegate

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/observability"
)

// TestMisroutingBeforeState pins the live-captured BEFORE-state of the
// agentic-RAG mis-routing gap (unified plan P2). It is the deterministic
// baseline every later behavioral gate (P4b / GATE A) diffs against.
//
// Source: eval/trace_gate/before_state_observations.jsonl, captured 2026-06-06
// by eval/agentic_rag_probe.ps1 at N=5/probe, COMPSHARE_EXTERNAL_KNOWLEDGE=1,
// against the exec branch (routing byte-identical to origin/main 293f944). Zero
// jitter across all 25 turns. This is REAL data
// from the runtime entry path, not a hand-authored fixture.
//
// Contract (the gap):
//   - symptom set  -> intent "diagnosis", retrieval NEVER fires, and NO
//     observable runtime form (ActualExecutionPath==""): the pre-ReAct
//     which-instance clarification dead-end fires before any tool/retrieval, so
//     the turn never reaches the agent loop. RAG never runs even though the
//     answering external chunks exist and are retrievable (proven by how-to).
//   - how-to set   -> intent "knowledge_qa", retrieval fires, runtime form
//     observability.ExecutionPathTerminalRAG.
//
// Cross-package note (plan section 3): runtime form is compared against the
// observability.ExecutionPath* STRING consts returned by DeriveActualExecutionPath,
// NOT intent.ExecutionPath* (a distinct typed string).
func TestMisroutingBeforeState(t *testing.T) {
	f, err := os.Open("before_state_observations.jsonl")
	if err != nil {
		t.Fatalf("open before-state observations: %v", err)
	}
	defer f.Close()
	obs, err := LoadProbeObservations(f)
	if err != nil {
		t.Fatalf("load observations: %v", err)
	}
	if len(obs) == 0 {
		t.Fatal("no before-state observations loaded")
	}

	byProbe := map[string][]ProbeObservation{}
	for _, o := range obs {
		byProbe[o.ProbeID] = append(byProbe[o.ProbeID], o)
	}

	// Determinism: a non-deterministic baseline cannot anchor a behavioral gate.
	// Require N>=5 per probe and identical contract signals across every run.
	for id, runs := range byProbe {
		if len(runs) < 5 {
			t.Errorf("probe %s captured %d runs, want >=5 (jitter-robust baseline)", id, len(runs))
		}
		first := runs[0].ContractKey()
		for _, r := range runs[1:] {
			if r.ContractKey() != first {
				t.Errorf("probe %s jitters: %q vs %q", id, first, r.ContractKey())
			}
		}
	}

	sawSymptom, sawHowto := false, false
	for _, o := range obs {
		switch o.ProbeSet() {
		case "sym":
			sawSymptom = true
			if o.Intent != "diagnosis" {
				t.Errorf("%s run%d: intent=%q, want diagnosis", o.ProbeID, o.Run, o.Intent)
			}
			if o.RetrievalFired {
				t.Errorf("%s run%d: retrieval fired, want NOT-fired (which-instance dead-end before RAG)", o.ProbeID, o.Run)
			}
			if o.ActualExecutionPath != "" {
				t.Errorf("%s run%d: actual_execution_path=%q, want \"\" (pre-ReAct dead-end, never reaches agent loop)", o.ProbeID, o.Run, o.ActualExecutionPath)
			}
		case "howto":
			sawHowto = true
			if o.Intent != "knowledge_qa" {
				t.Errorf("%s run%d: intent=%q, want knowledge_qa", o.ProbeID, o.Run, o.Intent)
			}
			if !o.RetrievalFired {
				t.Errorf("%s run%d: retrieval did NOT fire, want fired", o.ProbeID, o.Run)
			}
			if o.ActualExecutionPath != observability.ExecutionPathTerminalRAG {
				t.Errorf("%s run%d: actual_execution_path=%q, want %q", o.ProbeID, o.Run, o.ActualExecutionPath, observability.ExecutionPathTerminalRAG)
			}
		default:
			t.Errorf("%s: unknown probe set %q", o.ProbeID, o.ProbeSet())
		}
	}
	if !sawSymptom || !sawHowto {
		t.Fatalf("baseline missing a probe set: symptom=%t howto=%t", sawSymptom, sawHowto)
	}
}

// TestBeforeStateSymptomProbesCorpusCovered is the corpus-coverage precheck the
// plan requires (P2 / hazards section): every symptom probe in the before-state
// set must have >=1 answer-bearing external chunk, so a later P3 substance-gate
// NO-GO is attributable to architecture, not a corpus gap (the uncovered
// "sglang port" probe was deliberately swapped for the covered sglang-OOM one).
func TestBeforeStateSymptomProbesCorpusCovered(t *testing.T) {
	coverage := map[string][]string{
		"sym-vllm-killed":   {"ext-gpu-kill-process-001", "ext-vllm-startup-hang-001", "ext-gpu-oom-vllm-001"},
		"sym-sglang-oom":    {"ext-sglang-oom-001"},
		"sym-vllm-valueerr": {"ext-vllm-cuda-error-001", "ext-vllm-startup-hang-001"},
	}
	ids := loadExternalChunkIDs(t)
	for probe, candidates := range coverage {
		found := false
		for _, id := range candidates {
			if ids[id] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("symptom probe %s: none of %v present in external corpus (corpus gap, not architecture)", probe, candidates)
		}
	}
}

func loadExternalChunkIDs(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open("../../deploy/kb/external_w0.jsonl")
	if err != nil {
		t.Fatalf("open external corpus: %v", err)
	}
	defer f.Close()
	ids := map[string]bool{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row struct {
			ChunkID string `json:"chunk_id"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("decode external chunk: %v", err)
		}
		if row.ChunkID != "" {
			ids[row.ChunkID] = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan external corpus: %v", err)
	}
	return ids
}
