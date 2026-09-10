package engine

import (
	"encoding/json"

	openai "github.com/sashabaranov/go-openai"
)

// History is bounded by size, not by message or exchange counts. Two budgets
// apply in order: maxReplayedHistoryRunes over prior exchanges, then
// maxAssembledRequestRunes over messages plus the serialized tool window.
// Assembly removes oldest history before oldest current-turn tool groups, and
// keeps system messages, the current question and tool-call/result pairing
// intact.

// trimHistory keeps the message list under maxRawHistoryRunes by dropping the
// oldest non-system messages. The system prompt (index 0) is always kept. The cut
// point is aligned to a safe message boundary to avoid orphaned tool_calls or
// tool responses (which would make the history malformed for the LLM).
func (e *Engine) trimHistory() {
	e.messages = stripHistoricalToolTranscript(e.messages)
	safeStart := rawHistoryCutPoint(e.messages, maxRawHistoryRunes)
	if safeStart < 0 {
		return
	}
	keep := e.messages[safeStart:]
	e.messages = append([]openai.ChatCompletionMessage{e.messages[0]}, keep...)
}

// rawHistoryCutPoint returns the index of the oldest message to KEEP so that the
// suffix from it fits budgetRunes, aligned FORWARD to a safe boundary. It returns
// -1 when nothing needs dropping, or when no safe boundary exists — in which case
// the caller leaves the list alone rather than risk an API-invalid transcript.
//
// Cost is charged with assembledRequestRunes, deliberately the same accounting
// the request budget uses. A raw list measured one way and a request measured
// another is how a "source list" quietly becomes the narrower of the two.
//
// This is the sole history cut-point calculation. Keeping it separate from
// assembly lets both paths account in the same units without another historical
// compaction mode.
func rawHistoryCutPoint(messages []openai.ChatCompletionMessage, budgetRunes int) int {
	if budgetRunes <= 0 || len(messages) <= 1 {
		return -1
	}
	spent, candidate := 0, len(messages)
	for i := len(messages) - 1; i >= 1; i-- {
		spent += assembledRequestRunes(messages[i : i+1])
		if spent > budgetRunes {
			break
		}
		candidate = i
	}
	// candidate <= 1 means everything after the system prompt already fits.
	// safeHistoryStart rejects that same case, but checking it here states the
	// no-op explicitly rather than relying on the boundary walk's edge condition.
	if candidate <= 1 {
		return -1
	}
	return safeHistoryStart(messages, candidate)
}

// buildMessagesForLLM returns the message slice to send to the LLM.
// Freshness and refresh requirements are part of the single compiled
// AgentContext; this function does not add turn-local policy prompts.
// buildMessagesForLLM assembles the message list for one request. toolWindow is
// the FINAL, already-narrowed window that will travel alongside it: the size
// budget covers the whole request, and the tools are a large part of it that the
// message list never mentions.
func (e *Engine) buildMessagesForLLM(toolWindow []openai.Tool) []openai.ChatCompletionMessage {
	assembled := messagesFromAgentContext(e.messages, e.turnContextViewThisTurn, e.turnContextViewReady)
	messageBudget := maxAssembledRequestRunes - toolWindowRunes(toolWindow)
	capped := trimAssembledRequest(assembled, messageBudget)
	e.recordPromptAssembly(len(e.messages), len(assembled), len(capped))
	return capped
}

// toolWindowRunes is what the tool schemas cost on the wire. Serializing them is
// the honest measure: the provider receives this JSON, and the schemas' own
// descriptions and enums are most of it.
func toolWindowRunes(tools []openai.Tool) int {
	if len(tools) == 0 {
		return 0
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		// Unmarshalable tools would fail the request anyway; charge the production
		// window's size rather than 0, so a marshalling bug cannot silently hand
		// the message list the whole budget.
		return maxAssembledRequestRunes / 4
	}
	return len([]rune(string(raw)))
}

// maxAssembledRequestRunes bounds the complete provider request: messages plus tools.
// History is budgeted earlier, before this turn accumulates tool results, so this final
// assembly ceiling is still required. The 100k cap leaves headroom under the measured
// 130k provider floor for completion, reasoning and wrapper overhead.
const maxAssembledRequestRunes = 100000

// assembledRequestRunes is the size a request is charged at: message content
// plus tool-call names and arguments, which are as real to the provider as the
// content and are what a tool-heavy turn is mostly made of.
func assembledRequestRunes(msgs []openai.ChatCompletionMessage) int {
	total := 0
	for _, msg := range msgs {
		total += len([]rune(msg.Content))
		for _, call := range msg.ToolCalls {
			total += len([]rune(call.Function.Name)) + len([]rune(call.Function.Arguments))
		}
	}
	return total
}

// trimAssembledRequest bounds an already-assembled request by SIZE, without ever
// producing an API-invalid transcript. maxRunes may be 0 to disable it.
//
// This is the whole of "fill from the newest history block that fits", expressed
// as its complement — the assembled list already holds everything, so filling
// forward from the newest and shedding backward from the oldest reach the same
// slice, and shedding is the form that can see what the current turn has already
// spent.
//
// Shedding order is an order of preference, not an implementation detail:
// restored history goes first (oldest exchange first — it is context, and the
// turn can still be answered without it), and only if that is not enough do the
// oldest in-turn tool groups go (the model asked for those this turn, so losing
// them may cost a re-read). The current question and the leading system block are
// never shed.
func trimAssembledRequest(msgs []openai.ChatCompletionMessage, maxRunes int) []openai.ChatCompletionMessage {
	if maxRunes <= 0 || assembledRequestRunes(msgs) <= maxRunes {
		return msgs
	}
	headEnd := 0
	for headEnd < len(msgs) && msgs[headEnd].Role == openai.ChatMessageRoleSystem {
		headEnd++
	}
	currentUserIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == openai.ChatMessageRoleUser {
			currentUserIdx = i
			break
		}
	}
	// Structure not recognizable (no leading system block, or no user message):
	// leave the request untouched rather than risk an API-invalid drop.
	if currentUserIdx < headEnd {
		return msgs
	}

	assemble := func(dropFromPairs, cut int) []openai.ChatCompletionMessage {
		out := make([]openai.ChatCompletionMessage, 0, len(msgs)-dropFromPairs)
		out = append(out, msgs[:headEnd]...)
		out = append(out, msgs[headEnd+dropFromPairs:currentUserIdx]...)
		out = append(out, msgs[currentUserIdx])
		out = append(out, msgs[cut:]...)
		return out
	}
	fits := func(candidate []openai.ChatCompletionMessage) bool {
		return assembledRequestRunes(candidate) <= maxRunes
	}

	// Phase 1: drop whole restored exchanges from the oldest end.
	//
	// A restored exchange is not a fixed two messages. With the canonical
	// transcript projected in it is {user, assistant(tool_calls), tool…,
	// assistant}, so shedding a fixed stride of two would cut into the middle of
	// one and leave a tool result behind with no assistant call declaring it.
	// That is not a degraded answer, it is a provider 400 on the whole request.
	// The boundaries are therefore found, not assumed: each restored exchange
	// begins at a user message.
	exchangeStarts := make([]int, 0, 8)
	for i := headEnd; i < currentUserIdx; i++ {
		if msgs[i].Role == openai.ChatMessageRoleUser {
			exchangeStarts = append(exchangeStarts, i)
		}
	}
	dropFromPairs, firstTurnCut := 0, currentUserIdx+1
	for j := 0; j < len(exchangeStarts); j++ {
		if fits(assemble(dropFromPairs, firstTurnCut)) {
			break
		}
		end := currentUserIdx
		if j+1 < len(exchangeStarts) {
			end = exchangeStarts[j+1]
		}
		dropFromPairs = end - headEnd
	}

	// Phase 2: if still over, drop oldest complete in-turn tool groups after the
	// current question. A group = one assistant message plus the tool results
	// that answer it; both go together so nothing is orphaned.
	cut := firstTurnCut
	for cut < len(msgs) && !fits(assemble(dropFromPairs, cut)) {
		cut++ // drop the message that starts this group
		for cut < len(msgs) && msgs[cut].Role == openai.ChatMessageRoleTool {
			cut++
		}
	}
	return assemble(dropFromPairs, cut)
}

// recordPromptAssembly captures per-turn, content-free observability for the
// context assembler: the peak raw history size, the peak assembled request size,
// and whether the conservative message cap ever shed anything this turn. Prompt
// tokens are already recorded by the trace recorders (Outcome.PromptTokens).
func (e *Engine) recordPromptAssembly(raw, assembled, final int) {
	if raw > e.promptMessagesRawPeakThisTurn {
		e.promptMessagesRawPeakThisTurn = raw
	}
	if assembled > e.promptMessagesAssembledPeakThisTurn {
		e.promptMessagesAssembledPeakThisTurn = assembled
	}
	if final < assembled {
		e.promptMessagesCapAppliedThisTurn = true
	}
}

// messagesFromAgentContext is the sole history entrance for the main model.
// It restores bounded complete exchanges and execution-continuity state from
// the compiled view, then appends only this turn's live assistant/tool transcript.
// Previous raw tool payloads can therefore never survive a hot cache merely
// because e.messages happened to retain them.
func messagesFromAgentContext(messages []openai.ChatCompletionMessage, view AgentContext, ready bool) []openai.ChatCompletionMessage {
	if !ready || len(messages) == 0 {
		return messages
	}
	out := make([]openai.ChatCompletionMessage, 0, 2+len(view.RecentConversation)*2+4)
	for _, message := range messages {
		if message.Role != openai.ChatMessageRoleSystem {
			break
		}
		out = append(out, message)
	}
	if card := renderAgentContextCard(view); card != "" {
		out = append(out, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: card})
	}
	for _, pair := range view.RecentConversation {
		if pair.User == "" {
			continue
		}
		// A usable transcript opens with the exact user question and preserves
		// the committed answer, if any. An interrupted turn keeps its observations
		// without inventing a successful completion.
		// A bounded/foreign transcript that cannot make that promise falls back to
		// the complete plain pair rather than replacing history with a prefix.
		if transcriptReplaysCompletePair(pair) {
			out = append(out, pair.Transcript...)
			continue
		}
		out = append(out, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: pair.User})
		if pair.Assistant != "" {
			out = append(out, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: pair.Assistant})
		}
	}
	// Shared with the persisted canonical transcript so the stored record and
	// the model's view can never disagree about the turn boundary.
	if currentStart := currentTurnStart(messages); currentStart >= 0 {
		out = append(out, messages[currentStart:]...)
	}
	return out
}

func withEphemeralSystemBeforeLastUser(messages []openai.ChatCompletionMessage, content string) []openai.ChatCompletionMessage {
	insertAt := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == openai.ChatMessageRoleUser {
			insertAt = i
			break
		}
	}
	out := make([]openai.ChatCompletionMessage, 0, len(messages)+1)
	out = append(out, messages[:insertAt]...)
	out = append(out, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: content})
	out = append(out, messages[insertAt:]...)
	return out
}
