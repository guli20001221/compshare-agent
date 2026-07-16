package engine

import (
	"strings"
	"time"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
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
	// LastIntent carries the session's most recent routed intent label. It is
	// understanding-only context (never dispatch authority) and exists so the
	// single context card remains a strict superset of the retired
	// buildReActHistorySummary block, which surfaced it as "上次意图".
	LastIntent         string
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
	view.LastIntent = safeContextText(e.sessionState.LastIntent)
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
	if !isPersistedSelectionExpired(buildAt.Unix(), e.sessionState) {
		for _, item := range e.sessionState.PendingSelectionItems {
			view.SelectedEntities = append(view.SelectedEntities, SemanticEntityHint{
				Kind: "instance", ID: item.ID, Name: item.Name, Ordinal: item.Index,
				Source: "pending_selection", Freshness: ContinuityFreshnessFresh,
			})
		}
	}
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

// contextEnvelopeForPlainDirectReply gives every deterministic handled reply an
// evidence boundary. Conversation remains TaskSpec understanding context and
// can never launder a user's false premise into a fact.
func (e *Engine) contextEnvelopeForPlainDirectReply(result capability.ReadResult) capability.ReadResult {
	if result.Envelope != nil || result.Status != platform.ReadStatusHandled || result.NeedsClarification ||
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
	return result
}
