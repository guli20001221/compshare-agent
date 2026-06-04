package engine

import (
	"encoding/json"
	"testing"

	"github.com/compshare-agent/internal/knowledge"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildDiagnosisSkillSeedStructuresGroundedContext(t *testing.T) {
	rawKB := "If nvidia-smi cannot see the GPU, first confirm the cloud instance has GPU assigned."
	ledger := knowledge.BuildEvidenceLedger("nvidia-smi 检测不到 GPU", []knowledge.RetrievalHit{{
		Chunk: knowledge.KBChunk{
			ChunkID: "runbook-gpu-001",
			Title:   "GPU runtime troubleshooting",
			Content: rawKB,
		},
		Score: 0.95,
		Kept:  true,
	}}, knowledge.DefaultEvidenceLedgerMaxItems)

	seed := buildDiagnosisSkillSeed("diagnose-gpu-not-detected", map[string]any{
		"UHostId": " uhost-diag-001 ",
		"Service": " JupyterLab ",
	}, ledger)

	assert.Equal(t, "gpu_not_detected", seed["SymptomType"])
	assert.Equal(t, "uhost-diag-001", seed["UHostId"])
	assert.Equal(t, "JupyterLab", seed["Service"])
	assert.Contains(t, seed["NextStepExpectation"], "read-only")

	target, ok := seed["TargetInstanceSummary"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "uhost-diag-001", target["UHostId"])
	assert.Equal(t, "JupyterLab", target["Service"])
	assert.Contains(t, seed, "EvidenceLedger")

	raw, err := json.Marshal(seed)
	require.NoError(t, err)
	encoded := string(raw)
	assert.NotContains(t, encoded, rawKB)
	assert.NotContains(t, encoded, "CreateCompShareCustomImage")
	assert.NotContains(t, encoded, "DeleteCompShareDisk")
	assert.NotContains(t, encoded, "Workflow")
}

func TestBuildDiagnosisSkillSeedOmitsUnknownTargetAndEmptyEvidence(t *testing.T) {
	seed := buildDiagnosisSkillSeed("diagnose-port-firewall", nil, knowledge.EvidenceLedger{})

	assert.Equal(t, "port_firewall", seed["SymptomType"])
	assert.Contains(t, seed["NextStepExpectation"], "read-only")
	assert.NotContains(t, seed, "UHostId")
	assert.NotContains(t, seed, "Service")
	assert.NotContains(t, seed, "TargetInstanceSummary")
	assert.NotContains(t, seed, "EvidenceLedger")
}
