package engine

import (
	"strings"

	"github.com/compshare-agent/internal/knowledge"
)

type diagnosisSkillSeed struct {
	SymptomType           string                    `json:"SymptomType,omitempty"`
	UHostId               string                    `json:"UHostId,omitempty"`
	Service               string                    `json:"Service,omitempty"`
	TargetInstanceSummary map[string]any            `json:"TargetInstanceSummary,omitempty"`
	EvidenceLedger        *knowledge.EvidenceLedger `json:"EvidenceLedger,omitempty"`
	NextStepExpectation   string                    `json:"NextStepExpectation,omitempty"`
}

func buildDiagnosisSkillSeed(skillName string, args map[string]any, evidenceLedger knowledge.EvidenceLedger) map[string]any {
	seed := diagnosisSkillSeed{
		SymptomType:         diagnosisSymptomType(skillName),
		NextStepExpectation: diagnosisNextStepExpectation(skillName),
	}

	target := map[string]any{}
	if uid := stringArg(args, "UHostId"); uid != "" {
		seed.UHostId = uid
		target["UHostId"] = uid
	}
	if svc := stringArg(args, "Service"); svc != "" {
		seed.Service = svc
		target["Service"] = svc
	}
	if len(target) > 0 {
		seed.TargetInstanceSummary = target
	}
	if !evidenceLedger.Empty() {
		ledger := evidenceLedger
		seed.EvidenceLedger = &ledger
	}

	return diagnosisSkillSeedMap(seed)
}

func diagnosisSkillSeedMap(seed diagnosisSkillSeed) map[string]any {
	out := map[string]any{}
	if seed.SymptomType != "" {
		out["SymptomType"] = seed.SymptomType
	}
	if seed.UHostId != "" {
		out["UHostId"] = seed.UHostId
	}
	if seed.Service != "" {
		out["Service"] = seed.Service
	}
	if len(seed.TargetInstanceSummary) > 0 {
		out["TargetInstanceSummary"] = seed.TargetInstanceSummary
	}
	if seed.EvidenceLedger != nil && !seed.EvidenceLedger.Empty() {
		out["EvidenceLedger"] = *seed.EvidenceLedger
	}
	if seed.NextStepExpectation != "" {
		out["NextStepExpectation"] = seed.NextStepExpectation
	}
	return out
}

func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}

func diagnosisSymptomType(skillName string) string {
	switch skillName {
	case "diagnose-ssh":
		return "ssh"
	case "diagnose-init-failure":
		return "init_failure"
	case "diagnose-gpu-not-detected":
		return "gpu_not_detected"
	case "diagnose-image-issue":
		return "image_issue"
	case "diagnose-port-firewall":
		return "port_firewall"
	default:
		return strings.TrimSpace(skillName)
	}
}

func diagnosisNextStepExpectation(skillName string) string {
	switch skillName {
	case "diagnose-ssh":
		return "Use read-only instance state and monitor evidence before explaining SSH authentication or reachability checks."
	case "diagnose-init-failure":
		return "Use read-only instance state before explaining initialization status and next safe action."
	case "diagnose-gpu-not-detected":
		return "Use read-only instance and monitor evidence before distinguishing cloud GPU state from in-instance CUDA or driver issues."
	case "diagnose-image-issue":
		return "Use read-only instance image facts before explaining dependency or environment checks."
	case "diagnose-port-firewall":
		return "Use read-only instance and service-port evidence before explaining public reachability checks."
	default:
		return "Use only read-only evidence before giving the next diagnostic step."
	}
}
