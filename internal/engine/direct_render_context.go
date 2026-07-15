package engine

import (
	"strings"
	"time"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/intent"
	grounded "github.com/compshare-agent/internal/renderer"
	openai "github.com/sashabaranov/go-openai"
)

// ConversationPair is a committed, complete exchange. It deliberately excludes
// the current unanswered user message and every raw tool transcript.
type ConversationPair struct {
	User      string
	Assistant string
}

// ToolObservationView is the bounded semantic projection of a prior tool
// result. Summary is present only while the observation is fresh. Raw tool JSON
// is never part of AgentContext.
type ToolObservationView struct {
	Kind            string
	SubjectID       string
	Summary         string
	Source          string
	Completeness    string
	Freshness       string
	RefreshRequired bool
	ProducedAtUnix  int64
}

// AgentContext is the single immutable, read-only projection compiled at turn
// entry. SessionState and committed messages remain the sources of truth; this
// value is not persisted and never grants execution authority.
type AgentContext struct {
	TurnID             string
	CurrentQuestion    string
	RecentConversation []ConversationPair
	ConversationDigest ConversationDigest
	ActiveTask         *TaskSnapshot
	SelectedEntities   []SemanticEntityHint
	RecentObservations []ToolObservationView
	VerifiedKnowledge  []VerifiedKnowledgeTurn
	ContinuityNotices  []string
	BuiltAtUnix        int64
}

// TurnContextView remains as a source-compatible name while callers migrate to
// the owner name. It is not a second representation.
type TurnContextView = AgentContext

const (
	maxAgentContextPairs             = 8
	maxAgentContextObservations      = 8
	maxAgentContextVerifiedKnowledge = 4
)

// ContextCompiler owns all conversion from persisted/in-memory state to the
// model-visible semantic view. It is intentionally stateless; BuildAt is
// injected by the caller so hot, cold and takeover paths can be compared at the
// same instant.
type ContextCompiler struct{}

func (ContextCompiler) Compile(e *Engine, userMsg string, buildAt time.Time) AgentContext {
	return (ContextCompiler{}).CompileForTurn(e, userMsg, "", buildAt)
}

func (ContextCompiler) CompileForTurn(e *Engine, userMsg, turnID string, buildAt time.Time) AgentContext {
	view := AgentContext{
		TurnID:          safeContextText(turnID),
		CurrentQuestion: safeContextText(userMsg),
		BuiltAtUnix:     buildAt.Unix(),
	}
	if e != nil {
		view.RecentConversation = e.recentCompleteConversationPairs(maxAgentContextPairs)
		if id, name := e.singleRegistryInstance(); id != "" {
			view.SelectedEntities = append(view.SelectedEntities, SemanticEntityHint{
				Kind: "instance", ID: id, Name: name, Source: "account_registry_single", Freshness: ContinuityFreshnessFresh,
			})
		}
	}
	if e == nil || !e.sessionStateHydrated {
		return cloneAgentContext(view)
	}
	view.ConversationDigest = cloneConversationDigest(e.sessionState.ConversationDigest)
	view.VerifiedKnowledge = cloneVerifiedKnowledge(e.sessionState.VerifiedKnowledge, maxAgentContextVerifiedKnowledge)
	view.ContinuityNotices = compactSemanticItems(append([]string(nil), e.continuityAdvisories.Notices...))
	if e.continuityAdvisories.ReadOnly {
		view.ContinuityNotices = compactSemanticItems(append(view.ContinuityNotices, "本轮上下文只读，不得执行写操作"))
	}
	view.RecentObservations, view.ContinuityNotices = compileObservationViews(
		e.sessionState.RecentFacts,
		buildAt,
		view.ContinuityNotices,
	)
	if task := e.sessionState.TaskSnapshot; task.Status != TaskSnapshotStatusResolved && !taskSnapshotEmpty(task) {
		copy := cloneTaskSnapshot(task)
		view.ActiveTask = &copy
		view.SelectedEntities = append(view.SelectedEntities, task.Entities...)
	}
	view.SelectedEntities = append(view.SelectedEntities, e.sessionState.ConversationDigest.EntityHints...)
	if id := strings.TrimSpace(e.sessionState.SelectedInstanceID); id != "" {
		view.SelectedEntities = append(view.SelectedEntities, SemanticEntityHint{
			Kind:      "instance",
			ID:        id,
			Name:      e.sessionState.SelectedInstanceName,
			Freshness: normalizedSelectedInstanceFreshness(e.sessionState),
		})
	}
	view.SelectedEntities = cloneEntityHints(view.SelectedEntities)
	return cloneAgentContext(view)
}

func (e *Engine) buildTurnContextView(userMsg string) TurnContextView {
	return (ContextCompiler{}).Compile(e, userMsg, time.Now())
}

func (e *Engine) contextViewForTurn(userMsg string) TurnContextView {
	if e != nil && e.turnContextViewReady && e.turnContextViewThisTurn.CurrentQuestion == safeContextText(userMsg) {
		return e.turnContextViewThisTurn
	}
	return e.buildTurnContextView(userMsg)
}

// directRenderTaskSpec builds understanding-only input for the grounded
// renderer. The result deliberately excludes ContextFrame slots, tool facts,
// permissions, and trust sources. None of these fields authorize an operation
// or become factual evidence; renderer validates the answer against Envelope.
func (e *Engine) directRenderTaskSpec(plan intent.IntentRoute, userMsg string) grounded.TaskSpec {
	view := e.contextViewForTurn(userMsg)
	spec := grounded.TaskSpec{
		CurrentQuestion: view.CurrentQuestion,
		Intent:          compactSemanticText(string(plan.Intent)),
	}
	recentContext := renderConversationPairs(view.RecentConversation)
	if e == nil || !e.sessionStateHydrated {
		if recentContext != "" {
			spec.ContextSummary = compactSemanticNarrative("最近完整问答：" + recentContext)
		}
		return spec
	}

	if view.ActiveTask != nil {
		task := *view.ActiveTask
		spec.Goal = compactSemanticText(task.Goal)
		spec.Stage = compactSemanticText(task.Stage)
		spec.Freshness = compactSemanticText(task.Freshness)
		spec.Constraints = compactSemanticItems(task.Constraints)
		spec.Decisions = compactSemanticItems(task.Decisions)
		spec.MissingSlots = compactSemanticItems(task.MissingSlots)
		spec.EntityHints = appendTaskSpecEntityHints(spec.EntityHints, task.Entities)
	}

	digest := view.ConversationDigest
	contextParts := []string{}
	if narrative := compactSemanticNarrative(digest.Narrative); narrative != "" {
		contextParts = append(contextParts, narrative)
	}
	if recentContext != "" {
		contextParts = append(contextParts, "最近完整问答："+recentContext)
	}
	spec.ContextSummary = compactSemanticNarrative(strings.Join(contextParts, "。"))
	spec.UnresolvedTasks = compactSemanticItems(digest.UnresolvedTasks)
	spec.Constraints = mergeSemanticItems(spec.Constraints, digest.Constraints)
	spec.Decisions = mergeSemanticItems(spec.Decisions, digest.Decisions)
	spec.EntityHints = appendTaskSpecEntityHints(spec.EntityHints, view.SelectedEntities)
	return spec
}

const directTaskSpecRecentPairs = 2

// recentCompleteConversationForTaskSpec projects only complete plain-text
// user/assistant pairs. The current unanswered user message and raw tool
// transcripts are excluded. This is understanding-only context; the renderer's
// factual validator still receives EvidenceEnvelope alone.
func (e *Engine) recentCompleteConversationPairs(limit int) []ConversationPair {
	if e == nil || len(e.messages) == 0 {
		return nil
	}
	var pendingUser string
	pairs := make([]ConversationPair, 0, limit)
	for _, message := range e.messages {
		switch message.Role {
		case openai.ChatMessageRoleUser:
			pendingUser = safeContextText(message.Content)
		case openai.ChatMessageRoleAssistant:
			if pendingUser == "" || strings.TrimSpace(message.Content) == "" || len(message.ToolCalls) > 0 {
				continue
			}
			pairs = append(pairs, ConversationPair{User: pendingUser, Assistant: safeContextText(message.Content)})
			pendingUser = ""
		}
	}
	if limit > 0 && len(pairs) > limit {
		pairs = pairs[len(pairs)-limit:]
	}
	return pairs
}

func renderConversationPairs(pairs []ConversationPair) string {
	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		if pair.User == "" || pair.Assistant == "" {
			continue
		}
		parts = append(parts, "用户："+pair.User+"；助手："+pair.Assistant)
	}
	return strings.Join(parts, "。")
}

func (e *Engine) recentCompleteConversationForTaskSpec() string {
	return renderConversationPairs(e.recentCompleteConversationPairs(directTaskSpecRecentPairs))
}

// hasReusableCompleteConversation reports only whether a completed prior
// exchange is visible to the model. It does not declare that exchange factual;
// the agent still decides whether it is sufficient, and any new SearchKnowledge
// result still passes the evidence verifier before release.
func (e *Engine) hasReusableCompleteConversation() bool {
	return e.recentCompleteConversationForTaskSpec() != ""
}

// shouldResolveDirectClarificationInAgent keeps a deterministic handler from
// asking the user to repeat information that is already present in a complete
// prior turn. This decision depends on available data, never on wording.
func (e *Engine) shouldResolveContextDependentDirectReplyInAgent(result intent.HandlerResult, userMsg string) bool {
	// Read failures already have a dedicated context-aware fallback that carries
	// the failure advisory into ReAct. Do not steal that path here.
	if result.Status == intent.HandlerStatusFailureAfterTool {
		return false
	}
	return result.NeedsClarification && e.hasReusableCompleteConversation()
}

// contextEnvelopeForPlainDirectReply gives every deterministic handled reply an
// evidence boundary. Conversation remains TaskSpec understanding context and
// can never launder a user's false premise into a fact.
func (e *Engine) contextEnvelopeForPlainDirectReply(result intent.HandlerResult, plan intent.IntentRoute, userMsg string) intent.HandlerResult {
	if result.Envelope != nil || result.Status != intent.HandlerStatusHandled || result.NeedsClarification ||
		strings.TrimSpace(result.Reply) == "" {
		return result
	}
	sources := []string{}
	if action := strings.TrimSpace(result.ToolAction); action != "" {
		sources = append(sources, action)
	}
	env := envelope.Envelope{
		Kind:          envelope.KindContextualDirectReply,
		SourceActions: sources,
		Facts: []envelope.Fact{{
			Key:    "deterministic_reply",
			Label:  "本轮确定性查询结果",
			Value:  result.Reply,
			Source: envelope.FactSourceComputed,
		}},
		Constraints: envelope.Constraints{DoNotInventInstances: true, DoNotInventMetrics: true},
	}
	result.Envelope = &env
	if hash, err := envelope.Hash(env); err == nil && hash != "" {
		result.RendererInputEnvelopeHashes = []string{hash}
	}
	return result
}

func appendTaskSpecEntityHints(existing []grounded.TaskSpecEntityHint, incoming []SemanticEntityHint) []grounded.TaskSpecEntityHint {
	positions := make(map[string]int, len(existing)+len(incoming))
	for i, hint := range existing {
		positions[strings.ToLower(hint.Kind+"\x00"+hint.ID+"\x00"+hint.Name)] = i
	}
	for _, hint := range incoming {
		converted := grounded.TaskSpecEntityHint{
			Kind:      compactSemanticText(hint.Kind),
			ID:        compactSemanticText(hint.ID),
			Name:      compactSemanticText(hint.Name),
			Freshness: compactSemanticText(hint.Freshness),
		}
		if converted.ID == "" && converted.Name == "" {
			continue
		}
		key := strings.ToLower(converted.Kind + "\x00" + converted.ID + "\x00" + converted.Name)
		if idx, ok := positions[key]; ok {
			existing[idx] = converted
			continue
		}
		positions[key] = len(existing)
		existing = append(existing, converted)
	}
	return existing
}
