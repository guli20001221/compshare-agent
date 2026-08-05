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

// AgentContext is the single immutable, read-only projection compiled at turn
// entry. SessionState and committed messages remain the sources of truth; this
// value is not persisted and never grants execution authority.
type AgentContext struct {
	TurnID             string
	CurrentQuestion    string
	RecentConversation []ConversationPair
	ActiveTask         *TaskSnapshot
	SelectedEntities   []SemanticEntityHint
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
	maxAgentContextPairs        = config.MaxReplayedExchanges
	maxAgentContextObservations = 8
)

// measuredContextWindowFloorTokens is a LOWER BOUND on gpt-5.6-terra's context
// window, established by probe rather than by a published figure: on 2026-08-05 a
// 130,000-rune CJK prompt was accepted and billed 130,006 prompt tokens. That
// second number is the more useful half — 1 CJK rune is 1 token exactly, so runes
// and tokens are interchangeable in the derivation below, and the older note
// calling 1:1 "deliberately conservative" was describing a measurement it had not
// taken. Nothing in the codebase reads a model's real window (there is no such
// field anywhere), so this is the only bound available; treat it as a floor that
// a larger probe may raise, never as the window itself.
const measuredContextWindowFloorTokens = 130000

// maxReplayedHistoryRunes budgets the restored exchanges by SIZE, which the pair
// count alone does not. Replaying exchanges verbatim (the fix for the 320-rune
// truncation) removed the only size bound they had: 8 pairs x 2 sides x 320 runes
// was a hard 5,120-rune ceiling no matter what the user pasted, and 20 unbounded
// pairs of the largest messages in the production export would be ~155,000 runes.
//
// DERIVATION, against the limit that actually binds — the context window. One
// request carries the system prompt and tool schemas, the replayed history, and
// this turn's own tool work:
//
//	 130,000  measuredContextWindowFloorTokens
//	- 40,000  this turn's own transcript, bounded by maxTranscriptTotalRunes
//	- 15,000  system prompt + the 40 tool schemas
//	= 75,000  available to history
//
// 48,000 takes roughly two thirds of that. The remaining third is margin for the
// window being a floor from a single probe, not a published number.
//
// WHY NOT THE PREVIOUS DERIVATION, which produced 12,000. It divided half of
// config.ShippedMaxTokensPerTurn by maxReActRounds, on the reasoning that history
// is re-sent on every round so its real cost is 16x its per-request size. Two
// things are wrong with that. It double-counts a guard that already exists: the
// per-turn cap is enforced at runtime by tokenBudgetExceeded, which has its own
// recovery exit, so the history bound does not have to pre-guarantee it. And 16
// is the ceiling, not the shape of traffic — across 648 replayed production turns
// the p90 is 2 tool calls and 0.3% of turns reach 16 rounds, so every ordinary
// turn was sized for the deepest one.
//
// WHAT IT COST, once the canonical transcript began sharing this budget on
// 2026-08-04. 24 real replayed turns carry a median 5,486 runes of transcript
// (p90 7,659; max 17,686 — one turn alone exceeding the entire old budget). At
// the median, budgetReplayedPairs left the model 2 of 20 exchanges; at the p90,
// 1. The 20-exchange window was nominal and actual cross-turn memory was two
// turns, which is the state TestTranscriptSizedHistoryStillReplaysASession pins
// against. The plain-text cross-check that used to justify 12,000 (largest
// full-history replay in the three 2026-07 exports = 11,271 runes; p90 1,801)
// was taken before transcripts existed and measured the wrong payload.
//
// TestReplayBudgetStillMatchesItsDerivation re-runs the arithmetic above against
// the constants, so a raised producer bound or a re-probed window cannot leave
// this number silently behind.
const maxReplayedHistoryRunes = 48000

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
	view.ContinuityNotices = compactSemanticItems(append([]string(nil), e.continuityAdvisories.Notices...))
	if e.continuityAdvisories.ReadOnly {
		view.ContinuityNotices = compactSemanticItems(append(view.ContinuityNotices, "本轮上下文只读，不得执行写操作"))
	}
	view.ContinuityNotices = staleObservationNotices(
		e.sessionState.RecentFacts,
		buildAt,
		view.ContinuityNotices,
	)
	if task := e.sessionState.TaskSnapshot; task.Status != TaskSnapshotStatusResolved && !taskSnapshotEmpty(task) {
		copy := cloneTaskSnapshot(task)
		view.ActiveTask = &copy
		view.SelectedEntities = append(view.SelectedEntities, task.Entities...)
	}
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
// A tool-free turn is recorded too, with a nil Transcript — captureTurnTranscript
// records every exchange so the recorded window and the replayed conversation
// agree about what "the last five turns" means. Such a record must therefore
// still CONSUME its slot: skipping it before the text comparison would let a
// later same-text record slide forward into its place, which is the same
// misattribution by another route. Match on text, then attach only if there is
// anything to attach.
func (e *Engine) attachRecordedTranscripts(pairs []ConversationPair) []ConversationPair {
	if !canonicalTranscriptEnabled || e == nil || len(e.recentTurns) == 0 {
		return pairs
	}
	consumed := make([]bool, len(e.recentTurns))
	for i := range pairs {
		for j, record := range e.recentTurns {
			if consumed[j] {
				continue
			}
			if safeConversationText(record.User) != pairs[i].User ||
				safeConversationText(record.Assistant) != pairs[i].Assistant {
				continue
			}
			consumed[j] = true
			if record.Transcript != nil {
				pairs[i].Transcript = ProjectTranscript(record.Transcript)
			}
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
