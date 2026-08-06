package engine

import (
	"strconv"
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
	SelectedEntities   []SemanticEntityHint
	ContinuityNotices  []string
	BuiltAtUnix        int64
}

// TurnContextView remains as a source-compatible name while callers migrate to
// the owner name. It is not a second representation.
type TurnContextView = AgentContext

const maxAgentContextObservations = 8

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

// maxReplayedHistoryRunes budgets the restored exchanges by SIZE. Since
// 2026-08-05 it is the ONLY thing that decides how far back the model remembers:
// the count ceiling that used to sit in front of it (maxAgentContextPairs, =
// config.MaxReplayedExchanges, = 20) is deleted. A count and a size both
// deciding meant the count won on short turns and the size won on long ones,
// with neither number able to state what the model would actually see.
//
// Replaying exchanges verbatim (the fix for the 320-rune truncation) removed the
// only size bound they had: 8 pairs x 2 sides x 320 runes was a hard 5,120-rune
// ceiling no matter what the user pasted, and 20 unbounded pairs of the largest
// messages in the production export would be ~155,000 runes.
//
// IT IS NOT THE BOUND ON A REQUEST, and an earlier version of this comment said
// it was. It is applied at turn ENTRY by recentCompleteConversationPairs, when
// the turn has issued no tools yet, and is never re-checked as the turn
// accumulates tool results. The derivation it carried —
//
//	130,000 window floor − 40,000 "this turn's own transcript" − 15,000 system = 75,000
//
// — was wrong in its middle term: maxTranscriptTotalRunes = 40000 bounds what
// captureTurnTranscript PERSISTS at the end of a turn, not what is sent during
// one. Nothing bounded the live side at all, so with history at this budget and a
// full replay window, 20 expensive reads at the p90 result size assembled 142,856
// runes. maxAssembledRequestRunes now owns that, at assembly, where the whole
// request is visible.
//
// WHAT THIS NUMBER STILL DOES: it caps how much history is worth ASSEMBLING. On
// an ordinary turn — p90 is 2 tool calls — nothing is shed later, so this is what
// the model actually gets, and it wants to be generous. On a tool-heavy turn the
// assembly bound takes it back down. 48,000 sits well inside what a request can
// hold once system prompt and completion are reserved (130,000 − 15,000 − 16,000
// ≈ 99,000), so history alone can never be the thing that overflows a request.
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

// maxRawHistoryRunes bounds the two lists the replay DRAWS FROM: e.messages (the
// raw transcript, via trimHistory) and e.recentTurns (the recorded transcripts,
// via recordTurn). Both used to be bounded by a message or exchange COUNT —
// maxHistoryMessages = 120 and maxAgentContextPairs = 20 — and both counts are
// deleted here for the same reason: a source list that runs out first decides the
// model's memory instead of the budget, and does it invisibly.
//
// The invariant, which TestNoSourceListShadowsTheReplayBudget pins: a source list
// must never be the narrower of the two. It holds by arithmetic rather than by
// tuning. An exchange costs LESS in either source list than the replay budget
// charges it (e.messages carries no transcript at all — trimHistory strips tool
// messages before budgeting; e.recentTurns carries the transcript but not the
// context card or system block around it), so any set of exchanges that fits
// maxReplayedHistoryRunes fits the same number of runes in either source. Sizing
// the sources at twice the budget is margin on top of that, not the reason it
// holds.
//
// Bounding by size rather than by count also tightens the memory ceiling it
// replaces: 120 messages had no size limit at all, so one session's raw history
// was bounded only by agent.http.max_input_length x however many turns it ran.
const maxRawHistoryRunes = 2 * maxReplayedHistoryRunes

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
		view.RecentConversation = e.recentCompleteConversationPairs()
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
// It takes no count limit. The one it used to take (maxAgentContextPairs) was a
// second decider in front of the rune budget below, and the budget is strictly
// better informed: it knows what an exchange costs, and a count does not.
func (e *Engine) recentCompleteConversationPairs() []ConversationPair {
	if e == nil || len(e.messages) == 0 {
		return nil
	}
	var pendingUser string
	pairs := make([]ConversationPair, 0, len(e.messages)/2)
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
// Matching is indexed rather than nested. The nested scan was O(pairs x records),
// which was harmless while both lists were capped at 20 exchanges; with the count
// caps deleted, both are bounded by maxRawHistoryRunes instead, and a long session
// of short turns can hold hundreds. Indexing preserves the semantics exactly —
// records for one text are consumed front to back, and both lists are
// chronological, so first-unused is still the right one.
func (e *Engine) attachRecordedTranscripts(pairs []ConversationPair) []ConversationPair {
	if !canonicalTranscriptEnabled || e == nil || len(e.recentTurns) == 0 {
		return pairs
	}
	unconsumed := make(map[string][]int, len(e.recentTurns))
	for j, record := range e.recentTurns {
		key := recordedTurnKey(safeConversationText(record.User), safeConversationText(record.Assistant))
		unconsumed[key] = append(unconsumed[key], j)
	}
	for i := range pairs {
		key := recordedTurnKey(pairs[i].User, pairs[i].Assistant)
		queue := unconsumed[key]
		if len(queue) == 0 {
			continue
		}
		unconsumed[key] = queue[1:]
		if record := e.recentTurns[queue[0]]; record.Transcript != nil {
			pairs[i].Transcript = ProjectTranscript(record.Transcript)
		}
	}
	return pairs
}

// recordedTurnKey identifies an exchange by both its sides. The length prefix is
// what makes concatenation safe: without it, ("再试", "一次") and ("再", "试一次")
// would be the same key, and a record would attach to the wrong exchange — the
// misattribution the consume-once rule above exists to prevent, reintroduced by
// the index meant to preserve it.
func recordedTurnKey(user, assistant string) string {
	return strconv.Itoa(len(user)) + "\x00" + user + assistant
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
