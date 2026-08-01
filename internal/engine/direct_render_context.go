package engine

import (
	"strings"
	"time"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
	openai "github.com/sashabaranov/go-openai"
)

// ConversationPair is a committed, complete exchange. It deliberately excludes
// the current unanswered user message and every raw tool transcript.
type ConversationPair struct {
	User      string
	Assistant string
	// Transcript is the tool work that produced Assistant, projected back to
	// well-formed chat messages. Populated only when the canonical transcript is
	// enabled; nil for tool-free turns and for rows written before it existed.
	Transcript []openai.ChatCompletionMessage
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
	// maxAgentContextPairs is the count ceiling on replayed exchanges. It is not a
	// second number: config owns it, because validateHTTPConfig has to reject a
	// max_session_turns larger than it (a session outliving the window forgets its
	// own opening with no error). The real bound on size is maxReplayedHistoryRunes
	// below; this only caps how far back we look.
	maxAgentContextPairs             = config.MaxReplayedExchanges
	maxAgentContextObservations      = 8
	maxAgentContextVerifiedKnowledge = 4
)

// maxReplayedHistoryRunes budgets the restored exchanges by SIZE, which the pair
// count alone does not. Replaying exchanges verbatim (the fix for the 320-rune
// truncation) removed the only size bound they had: 8 pairs x 2 sides x 320 runes
// was a hard 5,120-rune ceiling no matter what the user pasted, and 20 unbounded
// pairs of the largest messages in the production export would be ~155,000 runes.
//
// Derivation, from the constraint that actually binds. agent.rate_limit
// .max_tokens_per_turn = 400000 is prompt+completion summed across the WHOLE
// turn, and history is re-sent on every one of up to maxReActRounds = 16 model
// calls, so history's real cost is 16x its per-request size — comparing a single
// request against 400000 is the wrong comparison. Holding history to at most half
// the turn budget gives 200000/16 = 12500 tokens per request; at a deliberately
// conservative 1 rune = 1 token for CJK that is ~12000 runes.
//
// Cross-check against production: the largest full-history replay in the three
// 2026-07 exports is 11,271 runes (p90 1,801; p99 5,764), so every session
// observed to date still replays complete. The budget bites only past that.
const maxReplayedHistoryRunes = 12000

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
	if !isPersistedSelectionExpired(buildAt.Unix(), e.sessionState) {
		for _, item := range e.sessionState.PendingSelectionItems {
			view.SelectedEntities = append(view.SelectedEntities, SemanticEntityHint{
				Kind: "instance", ID: item.ID, Name: item.Name, Ordinal: item.Index,
				Source: "pending_selection", Freshness: ContinuityFreshnessFresh,
			})
		}
	}
	if id := strings.TrimSpace(e.sessionState.SelectedInstanceID); id != "" {
		// Carry the binding's provenance (observed vs user_selected) so the write
		// verifier can tell an OBSERVED referent — read-only, never a selection —
		// from an instance the user genuinely chose.
		view.SelectedEntities = append(view.SelectedEntities, SemanticEntityHint{
			Kind:      "instance",
			ID:        id,
			Name:      e.sessionState.SelectedInstanceName,
			Source:    e.sessionState.SelectedInstanceSource,
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
			pendingUser = safeConversationText(message.Content)
		case openai.ChatMessageRoleAssistant:
			if pendingUser == "" || strings.TrimSpace(message.Content) == "" || len(message.ToolCalls) > 0 {
				continue
			}
			pairs = append(pairs, ConversationPair{User: pendingUser, Assistant: safeConversationText(message.Content)})
			pendingUser = ""
		}
	}
	if limit > 0 && len(pairs) > limit {
		pairs = pairs[len(pairs)-limit:]
	}
	pairs = e.attachRecordedTranscripts(pairs)
	return budgetReplayedPairs(pairs, maxReplayedHistoryRunes)
}

// attachRecordedTranscripts pairs each replayed exchange with the tool work
// that produced it.
//
// Matching is by content, not by position: the pair list is derived from
// e.messages and the record list from turn exits, and while they normally track
// each other, a mismatch must degrade to "no transcript" rather than to
// attaching one turn's tool results to a different turn's answer. That failure
// would be invisible and would tell the model a diagnosis belonged to an
// instance it was never run against.
//
// Content equality alone is not enough to identify a turn, because turns repeat:
// "再试一次" answered by "已确认。" is an ordinary thing to say twice about two
// different instances. So a record is consumed once and never reused. Both lists
// are chronological, so first-unused is also the right one; the used[] guard is
// what stops the second occurrence from silently inheriting the first one's tool
// evidence. Hot/cold parity cannot catch this on its own — both sides would make
// the identical substitution and agree.
func (e *Engine) attachRecordedTranscripts(pairs []ConversationPair) []ConversationPair {
	if !canonicalTranscriptEnabled || e == nil || len(e.recentTurns) == 0 {
		return pairs
	}
	consumed := make([]bool, len(e.recentTurns))
	for i := range pairs {
		for j, record := range e.recentTurns {
			if consumed[j] || record.Transcript == nil {
				continue
			}
			if safeConversationText(record.User) != pairs[i].User ||
				safeConversationText(record.Assistant) != pairs[i].Assistant {
				continue
			}
			consumed[j] = true
			pairs[i].Transcript = ProjectTranscript(record.Transcript)
			break
		}
	}
	return pairs
}

// budgetReplayedPairs keeps the newest exchanges that fit in budget and drops
// whole older ones. It never truncates a message: a half-exchange is the defect
// this whole change removed, and a reply cut mid-table is worse than an absent
// one because the model cannot tell it is reading a fragment.
//
// The newest exchange is always kept, even alone over budget. It is the turn the
// user is most likely referring to ("它"/"第一台"/"刚才那个"), so dropping it to
// respect a budget would reintroduce the amnesia at the one place it hurts most.
// That single exchange is itself bounded: agent.http.max_input_length caps the
// user side at 4000 runes.
func budgetReplayedPairs(pairs []ConversationPair, budgetRunes int) []ConversationPair {
	if budgetRunes <= 0 || len(pairs) == 0 {
		return pairs
	}
	spent := 0
	keepFrom := len(pairs)
	for i := len(pairs) - 1; i >= 0; i-- {
		// The transcript is costed with the exchange it belongs to. Leaving it
		// out would let the one part of history that actually grew escape the
		// budget meant to bound history.
		cost := len([]rune(pairs[i].User)) + len([]rune(pairs[i].Assistant)) + conversationTranscriptRunes(pairs[i])
		if i < len(pairs)-1 && spent+cost > budgetRunes {
			break
		}
		spent += cost
		keepFrom = i
	}
	return pairs[keepFrom:]
}

// conversationTranscriptRunes is the replay cost of a pair's tool work.
func conversationTranscriptRunes(pair ConversationPair) int {
	total := 0
	for _, msg := range pair.Transcript {
		total += len([]rune(msg.Content))
		for _, call := range msg.ToolCalls {
			total += len([]rune(call.Function.Arguments)) + len([]rune(call.Function.Name))
		}
	}
	return total
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
