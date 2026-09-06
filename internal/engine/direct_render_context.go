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

// ConversationPair is a committed user endpoint with an optional final assistant
// answer. An interrupted user remains an exchange even without an answer. The
// current turn is appended after context compilation, outside this history view.
type ConversationPair struct {
	User      string
	Assistant string
	// Transcript is the tool work that produced Assistant, projected back to
	// well-formed chat messages. It is nil for tool-free turns and rows written
	// before canonical transcripts existed.
	Transcript []openai.ChatCompletionMessage
}

// AgentContext is the single immutable, read-only projection compiled at turn
// entry. SessionState and committed messages remain the sources of truth; this
// value is not persisted and never grants execution authority.
type AgentContext struct {
	TurnID             string
	CurrentQuestion    string
	RecentConversation []ConversationPair
	SelectedEntities   []SelectedEntityHint
	BuiltAtUnix        int64
}

// maxReplayedHistoryRunes is the canonical history-detail budget. All user
// endpoints and their available final answers remain in history while plain text
// fits this budget; only older tool-call/tool-result detail is compacted first.
// This keeps the actual conversation continuous without introducing an LLM
// summary, a second memory store, or a hot/cold-only state.
//
// The newest detailed exchanges win the remaining budget because they are the
// likeliest antecedent of “它/刚才那个”. If the plain conversation alone exceeds
// the budget, the fallback remains a newest-first whole-exchange suffix. The
// assembled-request ceiling below is still the final request safety boundary.
const maxReplayedHistoryRunes = 48000

// maxRawHistoryRunes bounds the raw sources from which the canonical view is
// derived: plain e.messages and recorded tool turns. It deliberately exceeds the
// detail budget, so a source ceiling cannot silently become the memory policy.
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
		view.RecentConversation = e.recentConversationPairs()
		if id, name := e.singleRegistryInstance(); id != "" {
			view.SelectedEntities = append(view.SelectedEntities, SelectedEntityHint{
				Kind: "instance", ID: id, Name: name, Source: "account_registry_single", Freshness: ContinuityFreshnessFresh,
			})
		}
	}
	if e == nil || !e.sessionStateHydrated {
		return cloneAgentContext(view)
	}
	if !isPersistedSelectionExpired(buildAt.Unix(), e.sessionState) {
		for _, item := range e.sessionState.PendingSelectionItems {
			view.SelectedEntities = append(view.SelectedEntities, SelectedEntityHint{
				Kind: "instance", ID: item.ID, Name: item.Name, Ordinal: item.Index,
				Source: "pending_selection", Freshness: ContinuityFreshnessFresh,
			})
		}
	}
	if id := strings.TrimSpace(e.sessionState.SelectedInstanceID); id != "" {
		// Retain where and when this referent was recorded; the Agent decides
		// whether it is the target of the current operation.
		view.SelectedEntities = append(view.SelectedEntities, SelectedEntityHint{
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

// recentConversationPairs projects the existing canonical history at turn entry,
// before the new user is appended. User-only interrupted turns are retained; raw
// tool traffic is not. Recorded ended turns can add observed tool evidence even
// when no assistant answer was delivered.
// The existing rune budget, not a message count, bounds the result.
func (e *Engine) recentConversationPairs() []ConversationPair {
	if e == nil || len(e.messages) == 0 {
		return nil
	}
	pairs := e.attachRecordedTranscripts(conversationPairsFromMessages(e.messages))
	return budgetReplayedPairs(pairs, maxReplayedHistoryRunes)
}

// conversationPairsFromMessages is the shared role projection for the outer
// model and the instance-ops bridge. A new user starts a new exchange rather than
// overwriting an unanswered one. Unpaired assistants and raw tool traffic cannot
// invent answers for those interrupted requests.
func conversationPairsFromMessages(messages []openai.ChatCompletionMessage) []ConversationPair {
	var pairs []ConversationPair
	pendingPair := -1
	for _, message := range messages {
		switch message.Role {
		case openai.ChatMessageRoleUser:
			pendingPair = -1
			if content := historyConversationText(message.Role, message.Content); strings.TrimSpace(content) != "" {
				pairs = append(pairs, ConversationPair{User: content})
				pendingPair = len(pairs) - 1
			}
		case openai.ChatMessageRoleAssistant:
			if pendingPair < 0 || len(message.ToolCalls) > 0 {
				continue
			}
			pairs[pendingPair].Assistant = historyConversationText(message.Role, message.Content)
			pendingPair = -1
		}
	}
	return pairs
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
// Matching is indexed because rune-bounded histories may contain many short
// turns. Records for one text are consumed in chronological order.
func (e *Engine) attachRecordedTranscripts(pairs []ConversationPair) []ConversationPair {
	if e == nil || len(e.recentTurns) == 0 {
		return pairs
	}
	unconsumed := make(map[string][]int, len(e.recentTurns))
	for j, record := range e.recentTurns {
		key := recordedTurnKey(
			historyConversationText(openai.ChatMessageRoleUser, record.User),
			historyConversationText(openai.ChatMessageRoleAssistant, record.Assistant),
		)
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

// budgetReplayedPairs compacts one canonical history view in two tiers:
//
//   - retain plain user endpoints and available final answers whenever they fit;
//   - retain projected tool detail from the newest exchanges with the remaining
//     budget, and downgrade older exchanges to their original plain pair.
//
// Plain dialogue is lossless and is the better semantic history than a newly
// invented summary. Tool detail is factual evidence, but old detail can be
// queried again; dropping it before dropping the dialogue keeps short sessions
// coherent without a parallel memory system. The returned pairs are a copy, so
// compaction never mutates hot state and cold rebuild derives the same view.
//
// If plain dialogue alone is too large, tailReplayedPairs falls back to the
// existing whole-exchange policy. It never truncates one side of an exchange.
func budgetReplayedPairs(pairs []ConversationPair, budgetRunes int) []ConversationPair {
	if budgetRunes <= 0 || len(pairs) == 0 {
		return pairs
	}
	plainRunes := 0
	for _, pair := range pairs {
		plainRunes += conversationPairPlainRunes(pair)
	}
	if plainRunes > budgetRunes {
		return tailReplayedPairs(pairs, budgetRunes)
	}

	compacted := append([]ConversationPair(nil), pairs...)
	for i := range compacted {
		// The transcript augments a retained replayed exchange; it is not a
		// lossy substitute for that exchange. A persisted message may have been
		// bounded to protect storage; when that means it no longer replays this pair's exact
		// user question and final answer, fall back to the complete plain pair.
		if !transcriptReplaysCompletePair(compacted[i]) {
			compacted[i].Transcript = nil
		}
	}
	remainingDetailRunes := budgetRunes - plainRunes
	for i := len(compacted) - 1; i >= 0; i-- {
		detailRunes := conversationTranscriptDetailRunes(compacted[i])
		if detailRunes == 0 {
			continue
		}
		// A transcript is optional evidence, never permission to overrun the
		// history budget. Preserve the newest upgrades that actually fit; when a
		// single recent tool result is too large, keep its complete plain dialogue
		// instead of relying on the later whole-request trim to make an unrelated
		// decision for us.
		if detailRunes <= remainingDetailRunes {
			remainingDetailRunes -= detailRunes
			continue
		}
		compacted[i].Transcript = nil
	}
	return compacted
}

// tailReplayedPairs is the only fallback when plain dialogue itself exceeds the
// budget. It keeps newest whole exchanges, never a cut message or orphaned
// tool result. The newest exchange remains even when it alone exceeds budget.
func tailReplayedPairs(pairs []ConversationPair, budgetRunes int) []ConversationPair {
	spent := 0
	keepFrom := len(pairs)
	for i := len(pairs) - 1; i >= 0; i-- {
		cost := conversationPairRenderedRunes(pairs[i])
		if i < len(pairs)-1 && spent+cost > budgetRunes {
			break
		}
		spent += cost
		keepFrom = i
	}
	return pairs[keepFrom:]
}

func conversationPairPlainRunes(pair ConversationPair) int {
	return len([]rune(pair.User)) + len([]rune(pair.Assistant))
}

// transcriptReplaysCompletePair reports whether Transcript can replace the
// prior turn without losing semantic dialogue. Tool results can be abbreviated
// with an explicit marker, but the user question and final assistant answer are
// the continuous conversation and must survive byte-for-byte. An interrupted
// turn has no final answer and can replay its paired observations alone.
func transcriptReplaysCompletePair(pair ConversationPair) bool {
	if len(pair.Transcript) == 0 {
		return false
	}
	first, last := pair.Transcript[0], pair.Transcript[len(pair.Transcript)-1]
	if first.Role != openai.ChatMessageRoleUser || first.Content != pair.User {
		return false
	}
	if pair.Assistant == "" {
		// An interrupted turn can finish at a tool result. Never import a stored
		// final answer into a row whose transport did not commit an answer.
		return last.Role != openai.ChatMessageRoleAssistant || len(last.ToolCalls) > 0
	}
	return last.Role == openai.ChatMessageRoleAssistant && last.Content == pair.Assistant && len(last.ToolCalls) == 0
}

// conversationTranscriptDetailRunes is the positive additional cost of
// upgrading a plain pair to its canonical transcript. A projected transcript
// already contains the user question and final assistant answer, so charging
// both forms would double-count exactly the conversation we are trying to
// preserve. A transcript that cannot replay the complete pair is not an
// upgrade at all; the caller falls back to the complete plain dialogue.
func conversationTranscriptDetailRunes(pair ConversationPair) int {
	if !transcriptReplaysCompletePair(pair) {
		return 0
	}
	transcriptRunes := conversationTranscriptRunes(pair)
	if transcriptRunes == 0 {
		return 0
	}
	return max(0, transcriptRunes-conversationPairPlainRunes(pair))
}

func conversationPairRenderedRunes(pair ConversationPair) int {
	if transcriptReplaysCompletePair(pair) {
		if transcriptRunes := conversationTranscriptRunes(pair); transcriptRunes > 0 {
			return transcriptRunes
		}
	}
	return conversationPairPlainRunes(pair)
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
func (e *Engine) contextEnvelopeForPlainDirectReply(result capability.ReadResult, fallbackAction string) capability.ReadResult {
	if result.Envelope != nil ||
		(result.Status != platform.ReadStatusHandled && result.Status != platform.ReadStatusEmpty) ||
		result.NeedsClarification ||
		strings.TrimSpace(result.Reply) == "" {
		return result
	}
	sources := []string{}
	if action := strings.TrimSpace(result.ToolAction); action != "" {
		sources = append(sources, action)
	} else if fallbackAction = strings.TrimSpace(fallbackAction); fallbackAction != "" {
		sources = append(sources, fallbackAction)
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
