package workflow

import "github.com/compshare-agent/internal/security"

// ToolBindingKind classifies how a workflow Step binds to an API tool, which is
// what determines whether the step's security risk is knowable from the static
// definition alone.
type ToolBindingKind int

const (
	// ToolBindingNone is a step that calls no tool (e.g. a StepConfirm gate).
	ToolBindingNone ToolBindingKind = iota
	// ToolBindingStatic is a StepToolCall whose tool name is fixed in the
	// definition (Step.Tool); its risk is security.Check(Tool).
	ToolBindingStatic
	// ToolBindingDynamic is a StepToolCall whose tool name is chosen at runtime
	// by Step.ToolFunc; the tool — and therefore its risk — is not knowable from
	// the static definition.
	ToolBindingDynamic
)

func (k ToolBindingKind) String() string {
	switch k {
	case ToolBindingStatic:
		return "static"
	case ToolBindingDynamic:
		return "dynamic"
	default:
		return "none"
	}
}

// ExecutionStepContract is the pure, projected shape of one workflow Step: what
// it is, how it binds to a tool, and — only when statically knowable — its
// security risk. It deliberately fabricates nothing: a dynamic or confirm step
// reports RiskKnown=false rather than inventing a Risk.
type ExecutionStepContract struct {
	Name        string
	Type        StepType
	Tool        string // set only when ToolBinding == ToolBindingStatic
	ToolBinding ToolBindingKind
	RiskKnown   bool
	Risk        security.Level // valid only when RiskKnown
}

// ExecutionContract projects a workflow Definition into a flat, structured
// per-step contract for confirm-card / audit / a future evaluator. It is a pure
// function of the definition — no runtime Context, no tool execution — so a
// parity test can pin it against the live step shape.
//
// Binding precedence mirrors the runtime: a StepConfirm carries no tool;
// otherwise a ToolFunc (which Step documents as "overrides Tool if set") makes
// the step dynamic; otherwise a non-empty Tool makes it static and its risk is
// read from security.Check. This explicit binding is forced by real workflows —
// CreateInstanceDef alone mixes a dynamic image query, a StepConfirm gate, and a
// static CreateCompShareInstance — so the contract cannot assume a static Tool.
func ExecutionContract(def *Definition) []ExecutionStepContract {
	if def == nil {
		return nil
	}
	contracts := make([]ExecutionStepContract, 0, len(def.Steps))
	for _, step := range def.Steps {
		contracts = append(contracts, projectStep(step))
	}
	return contracts
}

func projectStep(step Step) ExecutionStepContract {
	c := ExecutionStepContract{Name: step.Name, Type: step.Type}
	switch {
	case step.Type == StepConfirm:
		c.ToolBinding = ToolBindingNone
	case step.ToolFunc != nil:
		// Tool resolved at runtime → risk not knowable from the definition.
		c.ToolBinding = ToolBindingDynamic
	case step.Tool != "":
		c.ToolBinding = ToolBindingStatic
		c.Tool = step.Tool
		if lvl, err := security.Check(step.Tool); err == nil {
			c.Risk = lvl
			c.RiskKnown = true
		}
		// A static tool absent from the security whitelist stays RiskKnown=false
		// rather than fabricating a level.
	default:
		// StepToolCall with neither Tool nor ToolFunc — not expected today, but
		// projected honestly as an unbound step rather than guessed.
		c.ToolBinding = ToolBindingNone
	}
	return c
}
