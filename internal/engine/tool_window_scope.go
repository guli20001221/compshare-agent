package engine

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"

	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/tools"
)

// modelToolWindowScope is the server-owned plan for the remainder of one
// central-Agent turn. It is derived only from a typed Request<Workflow> tool
// call that the Agent has already made in that same turn; it never interprets
// user text or treats a directory browse as a user choice.
//
// A named scope is both advertised to the model and checked again at execution
// time. The latter matters because a model can still emit a remembered tool
// name that was not in the request's Tools array.
type modelToolWindowScope struct {
	Mode   tools.ToolScopeMode
	Names  []string
	Reason string
}

const (
	imageListToolName = "ReadCapability_image_list"

	scopeReasonFeatureOff          = "feature_off"
	scopeReasonRolloutNotSelected  = "rollout_not_selected"
	scopeReasonAwaitingTypedIntent = "awaiting_typed_intent"
	scopeReasonKnowledgeOnly       = "knowledge_only"
	scopeReasonCreateCustomImage   = "create_custom_image"
	scopeReasonCloneCustomImage    = "clone_custom_image"
	scopeReasonReinstallInstance   = "reinstall_instance"
)

func fullModelToolWindowScope(mutatingEnabled bool, reason string) modelToolWindowScope {
	mode := tools.ToolScopeReadOnlyFull
	if mutatingEnabled {
		mode = tools.ToolScopeMutableFull
	}
	return modelToolWindowScope{Mode: mode, Reason: reason}
}

func namedModelToolWindowScope(reason string, names ...string) modelToolWindowScope {
	seen := make(map[string]struct{}, len(names))
	kept := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		kept = append(kept, name)
	}
	sort.Strings(kept)
	return modelToolWindowScope{Mode: tools.ToolScopeNamed, Names: kept, Reason: reason}
}

func (s modelToolWindowScope) allows(action string) bool {
	if s.Mode != tools.ToolScopeNamed {
		return true
	}
	for _, name := range s.Names {
		if name == action {
			return true
		}
	}
	return false
}

func (s modelToolWindowScope) activeNamedScope() bool {
	return s.Mode == tools.ToolScopeNamed && len(s.Names) > 0
}

// centralAgentToolWindowForScope builds the normal window first, then applies a
// named allowlist. This keeps flag-off and unknown-state behavior byte-for-byte
// aligned with the existing central Agent surface.
func centralAgentToolWindowForScope(mutatingEnabled, instanceOpsEnabled bool, scope modelToolWindowScope) []openai.Tool {
	window := centralAgentToolWindow(mutatingEnabled, instanceOpsEnabled)
	if !scope.activeNamedScope() {
		return window
	}

	out := make([]openai.Tool, 0, len(scope.Names))
	for _, tool := range window {
		if tool.Function != nil && scope.allows(tool.Function.Name) {
			out = append(out, tool)
		}
	}
	return out
}

// workflowToolScope narrows only after the Agent has selected a concrete write
// workflow. It retains every read-only capability and exposes only the selected
// write proposal, so an incomplete proposal can still inspect instances, prices
// or all image sources before asking the user. It deliberately does not bind
// ImageSource: image-list reads can be multi-source recommendations, and the
// resolver remains the authority for validating an image ID/source pair.
func workflowToolScope(operation string) (modelToolWindowScope, bool) {
	var reason string
	switch operation {
	case "CreateCustomImageWorkflow":
		reason = scopeReasonCreateCustomImage
	case "CloneCustomImageWorkflow":
		reason = scopeReasonCloneCustomImage
	case "ReinstallInstanceWorkflow":
		reason = scopeReasonReinstallInstance
	default:
		// In particular, preserve the proven guided CreateInstance flow. Its
		// form/card is already the authoritative collector and should not be
		// narrowed by this rollout.
		return modelToolWindowScope{}, false
	}
	// Use the normal read-only window as the allowlist source. Passing true for
	// instanceOps includes the optional diagnose name too; the actual resolver
	// below still omits it when that runner is not wired for this session.
	names := append(centralAgentToolNames(false, true), proposalToolName(operation))
	return namedModelToolWindowScope(reason, names...), true
}

// initializeModelToolWindowScope starts every new user turn with the existing
// full window. A persisted ContextFrame is not proof that this new message is a
// continuation (and may describe an incomplete proposal rather than a UI card),
// so it never narrows the new turn. Confirm cards resolve inside their original
// turn and are therefore unaffected.
func (e *Engine) initializeModelToolWindowScope() {
	if e == nil {
		return
	}
	reason := scopeReasonFeatureOff
	if e.intentScopedToolsEnabled {
		reason = scopeReasonRolloutNotSelected
		if e.scopedToolsRolloutSelected() {
			reason = scopeReasonAwaitingTypedIntent
		}
	}
	e.modelToolWindowScopeThisTurn = fullModelToolWindowScope(e.mutatingToolsEnabled, reason)
}

// scopedToolsRolloutSelected is deterministic per opaque rate-limit subject.
// It exposes neither the subject nor its bucket in traces. With no tenant
// subject we fail closed for partial rollouts; 100% remains useful for local
// controlled validation.
func (e *Engine) scopedToolsRolloutSelected() bool {
	if e == nil || !e.intentScopedToolsEnabled {
		return false
	}
	percent := e.intentScopedToolsRolloutPercent
	if percent <= 0 {
		return false
	}
	if percent >= 100 {
		return true
	}
	if e.rateLimitSubject == "" || e.rateLimitSubject == governance.AnonymousSubjectKey {
		return false
	}
	return scopedToolsRolloutBucket(e.rateLimitSubject) < percent
}

// scopedToolsRolloutBucket uses 64 bits of SHA-256 rather than one byte, so a
// configured percentage does not inherit the large modulo bias of 0..255.
func scopedToolsRolloutBucket(subject string) int {
	digest := sha256.Sum256([]byte(subject))
	return int(binary.BigEndian.Uint64(digest[:8]) % 100)
}

func (e *Engine) modelToolWindow(knowledgeOnly bool) []openai.Tool {
	if knowledgeOnly {
		return centralAgentKnowledgeToolWindow()
	}
	return centralAgentToolWindowForScope(
		e.mutatingToolsEnabled,
		e.instanceOps != nil,
		e.modelToolWindowScopeThisTurn,
	)
}

func (e *Engine) modelToolCallAllowed(action string) bool {
	if e == nil {
		return false
	}
	if e.knowledgeOnlyThisTurn {
		// Knowledge-only is a separate, stricter server policy. It replaces a
		// carried named scope rather than intersecting with it: otherwise a
		// previous workflow scope could reject SearchKnowledge even though that
		// is the only tool shown in this turn's actual model window.
		return knowledgeOnlyToolAllowed(action)
	}
	scope := e.modelToolWindowScopeThisTurn
	if !scope.activeNamedScope() {
		// Flag-off and full-window behavior remain unchanged. Existing policy
		// gates still own those paths.
		return true
	}
	if !scope.allows(action) {
		return false
	}
	// A named scope can contain a proposal tool, while the current session may
	// still be read-only. Match the exact advertised window so a remembered
	// model tool name cannot bypass either dimension of the P3 boundary.
	for _, tool := range centralAgentToolWindowForScope(e.mutatingToolsEnabled, e.instanceOps != nil, scope) {
		if tool.Function != nil && tool.Function.Name == action {
			return true
		}
	}
	return false
}

// advanceModelToolWindowScope consumes a typed Agent operation selection only.
// It runs after JSON validation and before execution, so later tool calls in
// the same model response cannot jump to an unrelated workflow. A new user
// turn always resets to the full window above.
func (e *Engine) advanceModelToolWindowScope(action string, _ map[string]any) {
	if e == nil || !e.scopedToolsRolloutSelected() {
		return
	}
	// Full-window compatibility intentionally allows the legacy executor to
	// reject a remembered model tool name. Do not let such a hidden write name
	// alter the P3 lane: it must have been in the actual model window first.
	if !e.modelToolWindowContains(action) {
		return
	}
	operation, ok := proposalOperationForTool(action)
	if !ok {
		return
	}
	if scope, ok := workflowToolScope(operation); ok {
		e.modelToolWindowScopeThisTurn = scope
	}
}

func (e *Engine) modelToolWindowContains(action string) bool {
	if e == nil {
		return false
	}
	for _, tool := range e.modelToolWindow(e.knowledgeOnlyThisTurn) {
		if tool.Function != nil && tool.Function.Name == action {
			return true
		}
	}
	return false
}

func (e *Engine) recordLastOutboundToolWindow(scope modelToolWindowScope, window []openai.Tool) {
	if e == nil {
		return
	}
	scope.Names = append([]string(nil), scope.Names...)
	e.lastOutboundToolWindowScopeThisTurn = scope
	e.lastOutboundToolNamesThisTurn = modelToolWindowNames(window)
	e.lastOutboundToolWindowObservedThisTurn = true
}

func modelToolWindowNames(window []openai.Tool) []string {
	names := make([]string, 0, len(window))
	for _, tool := range window {
		if tool.Function != nil && tool.Function.Name != "" {
			names = append(names, tool.Function.Name)
		}
	}
	return names
}

func (e *Engine) completionToolWindowScope() modelToolWindowScope {
	if e == nil {
		return fullModelToolWindowScope(false, scopeReasonFeatureOff)
	}
	if e.knowledgeOnlyThisTurn {
		return modelToolWindowScope{Mode: tools.ToolScopeReadOnlyFull, Reason: scopeReasonKnowledgeOnly}
	}
	if e.modelToolWindowScopeThisTurn.Mode == "" {
		return fullModelToolWindowScope(e.mutatingToolsEnabled, scopeReasonFeatureOff)
	}
	return e.modelToolWindowScopeThisTurn
}

// completionToolNames mirrors the actual resolved model window. This is
// intentionally not scope.Names: a named scope may contain a Request* tool
// that the current read-only session did not expose.
func (e *Engine) completionToolNames(scope modelToolWindowScope) []string {
	var window []openai.Tool
	if e != nil && e.knowledgeOnlyThisTurn {
		window = centralAgentKnowledgeToolWindow()
	} else {
		mutatingEnabled := false
		instanceOpsEnabled := false
		if e != nil {
			mutatingEnabled = e.mutatingToolsEnabled
			instanceOpsEnabled = e.instanceOps != nil
		}
		window = centralAgentToolWindowForScope(mutatingEnabled, instanceOpsEnabled, scope)
	}
	return modelToolWindowNames(window)
}
