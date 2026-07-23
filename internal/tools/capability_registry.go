package tools

import (
	"fmt"
	"sort"
	"sync"

	"github.com/compshare-agent/internal/security"
	openai "github.com/sashabaranov/go-openai"
)

type ResultContract string

const (
	ResultContractModelObservation ResultContract = "model_observation"
	ResultContractGroundedAnswer   ResultContract = "grounded_answer"
	ResultContractWorkflowResult   ResultContract = "workflow_result"
)

type CapabilityStage string

const (
	CapabilityStageActive CapabilityStage = "active"
	CapabilityStageShadow CapabilityStage = "shadow"
)

type CapabilityDefinition struct {
	Tool  openai.Tool
	Stage CapabilityStage
}

// Capability is the runtime-owned contract for one tool or internal action.
// Tool schema, execution policy, risk, confirmation and result ownership are
// read together so callers cannot accidentally authorize from one table and
// execute using another.
type Capability struct {
	Name             string
	Tool             openai.Tool
	ExposedToAgent   bool
	Stage            CapabilityStage
	Policy           ToolExecutionPolicy
	ResultContract   ResultContract
	ResultOwner      string
	AgentInstruction string
}

type CapabilityRegistry struct {
	ordered []Capability
	byName  map[string]Capability
}

func BuildCapabilityRegistry(toolDefinitions []openai.Tool, policies map[string]ToolExecutionPolicy) (*CapabilityRegistry, error) {
	definitions := make([]CapabilityDefinition, 0, len(toolDefinitions))
	for _, tool := range toolDefinitions {
		definitions = append(definitions, CapabilityDefinition{Tool: tool, Stage: CapabilityStageActive})
	}
	return BuildCapabilityRegistryFromDefinitions(definitions, policies)
}

func BuildCapabilityRegistryFromDefinitions(definitions []CapabilityDefinition, policies map[string]ToolExecutionPolicy) (*CapabilityRegistry, error) {
	registry := &CapabilityRegistry{byName: make(map[string]Capability, len(definitions)+len(policies))}
	for _, definition := range definitions {
		tool := definition.Tool
		if tool.Function == nil || tool.Function.Name == "" {
			return nil, fmt.Errorf("capability registry: tool without function name")
		}
		name := tool.Function.Name
		if _, exists := registry.byName[name]; exists {
			return nil, fmt.Errorf("capability registry: duplicate capability %q", name)
		}
		policy, ok := policies[name]
		if !ok {
			return nil, fmt.Errorf("capability registry: tool %q has no execution policy", name)
		}
		capability := capabilityFromPolicy(name, policy)
		capability.Tool = tool
		capability.ExposedToAgent = true
		capability.Stage = definition.Stage
		if capability.Stage == "" {
			capability.Stage = CapabilityStageActive
		}
		capability.AgentInstruction = tool.Function.Description
		registry.ordered = append(registry.ordered, capability)
		registry.byName[name] = capability
	}

	internalNames := make([]string, 0, len(policies))
	for name := range policies {
		if _, exists := registry.byName[name]; !exists {
			internalNames = append(internalNames, name)
		}
	}
	sort.Strings(internalNames)
	for _, name := range internalNames {
		capability := capabilityFromPolicy(name, policies[name])
		registry.ordered = append(registry.ordered, capability)
		registry.byName[name] = capability
	}
	return registry, nil
}

func capabilityFromPolicy(name string, policy ToolExecutionPolicy) Capability {
	contract := ResultContractModelObservation
	owner := "agent"
	switch policy.Route {
	case ActionRouteKnowledge:
		contract = ResultContractGroundedAnswer
		owner = "grounding"
	case ActionRouteWorkflow:
		contract = ResultContractWorkflowResult
		owner = "workflow"
	}
	return Capability{Name: name, Policy: policy, ResultContract: contract, ResultOwner: owner}
}

func (r *CapabilityRegistry) Lookup(name string) (Capability, bool) {
	if r == nil {
		return Capability{}, false
	}
	capability, ok := r.byName[name]
	return capability, ok
}

func (r *CapabilityRegistry) All() []Capability {
	if r == nil {
		return nil
	}
	return append([]Capability(nil), r.ordered...)
}

func (r *CapabilityRegistry) Policies() map[string]ToolExecutionPolicy {
	out := make(map[string]ToolExecutionPolicy, len(r.ordered))
	for _, capability := range r.ordered {
		policy := capability.Policy
		policy.AllowedParams = cloneStringsPreservingEmpty(policy.AllowedParams)
		policy.InternalAllowedParams = cloneStringsPreservingEmpty(policy.InternalAllowedParams)
		policy.RedactInResult = cloneStringsPreservingEmpty(policy.RedactInResult)
		if policy.RetryOn != nil {
			policy.RetryOn = append([]ErrorClass{}, policy.RetryOn...)
		}
		out[capability.Name] = policy
	}
	return out
}

func cloneStringsPreservingEmpty(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string{}, in...)
}

func (r *CapabilityRegistry) VisibleTools(scope ToolScope, mutatingEnabled bool) []openai.Tool {
	capabilities := r.CapabilitiesForScope(scope, mutatingEnabled)
	visible := make([]openai.Tool, 0, len(capabilities))
	for _, capability := range capabilities {
		visible = append(visible, capability.Tool)
	}
	return visible
}

func (r *CapabilityRegistry) CapabilitiesForScope(scope ToolScope, mutatingEnabled bool) []Capability {
	if r == nil {
		return nil
	}
	allowedNames := map[string]struct{}(nil)
	switch scope.Mode {
	case ToolScopeNamed:
		if len(scope.Names) == 0 {
			return nil
		}
		allowedNames = make(map[string]struct{}, len(scope.Names))
		for _, name := range scope.Names {
			allowedNames[name] = struct{}{}
		}
	case ToolScopeMutableFull:
		// The process flag below still has the final say.
	case ToolScopeReadOnlyFull:
		mutatingEnabled = false
	default:
		mutatingEnabled = false
	}

	visible := make([]Capability, 0, len(r.ordered))
	for _, capability := range r.ordered {
		if !capability.ExposedToAgent || capability.Stage != CapabilityStageActive {
			continue
		}
		if allowedNames != nil {
			if _, ok := allowedNames[capability.Name]; !ok {
				continue
			}
		}
		if !mutatingEnabled && (capability.Policy.Route == ActionRouteWorkflow || capability.Policy.Class == ActionClassMutating) {
			continue
		}
		visible = append(visible, capability)
	}
	return visible
}

func (r *CapabilityRegistry) ValidateSafety() error {
	for _, capability := range r.ordered {
		policy := capability.Policy
		if policy.SecurityLevel == security.L1 && !policy.NeedsConfirm {
			return fmt.Errorf("capability registry: L1 capability %q does not require confirmation", capability.Name)
		}
		if policy.SecurityLevel == security.L2 && policy.Class != ActionClassDestructive {
			return fmt.Errorf("capability registry: L2 capability %q is not destructive", capability.Name)
		}
	}
	return nil
}

var (
	defaultCapabilitiesOnce sync.Once
	defaultCapabilities     *CapabilityRegistry
	defaultCapabilitiesErr  error
)

func DefaultCapabilityRegistry() *CapabilityRegistry {
	defaultCapabilitiesOnce.Do(func() {
		definitions := make([]CapabilityDefinition, 0, len(Registry)+len(ShadowCapabilityDefinitions))
		for _, tool := range Registry {
			definitions = append(definitions, CapabilityDefinition{Tool: tool, Stage: CapabilityStageActive})
		}
		definitions = append(definitions, ShadowCapabilityDefinitions...)
		defaultCapabilities, defaultCapabilitiesErr = BuildCapabilityRegistryFromDefinitions(definitions, buildToolExecutionPolicies())
		if defaultCapabilitiesErr == nil {
			defaultCapabilitiesErr = defaultCapabilities.ValidateSafety()
		}
	})
	if defaultCapabilitiesErr != nil {
		panic(defaultCapabilitiesErr)
	}
	return defaultCapabilities
}
