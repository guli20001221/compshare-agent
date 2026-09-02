package engine

import (
	"encoding/json"
	"fmt"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// Canonical turn transcript (schema agent_transcript_v1).
//
// It records one user turn's ordered tool traffic: each assistant tool_call and
// the tool result that answered it. It is the model's persisted semantic history;
// session state does not duplicate these facts as prose summaries.
//
// It is deliberately NOT verbatim, and must not be "fixed" to become so. Content
// clears the same redaction boundary as replayed history, an over-long body is
// truncated behind a marker, and whole rounds are shed to fit the budget. What
// crosses a persistence boundary and is later fed back to a model is a
// sanitized, bounded replay by design: restoring byte-parity with the live turn
// would put a credential the assistant line already dropped back into a stored
// row.
//
// Ordering and pairing are part of the contract: a stored transcript must be
// replayable as well-formed chat messages, so an assistant message carrying
// tool_calls is never separated from the tool messages that answer it. The
// bounding pass below therefore sheds whole rounds, never half of one.
//
// This type is deliberately NOT the in-memory representation. It exists to
// cross a persistence boundary, so it is versioned and additive-only: readers
// must ignore unknown fields and refuse unknown major versions.
const transcriptSchemaVersion = 1

// Bounding limits. These are persistence limits, not prompt limits — the
// projector that later feeds this back to the model applies its own budget.
// They exist so one pathological turn cannot write an unbounded row.
const (
	// maxTranscriptMessageRunes caps a single message's content. Exceeding it
	// truncates that message and sets Truncated + OrigRunes, so a reader can
	// always tell a short result from a shortened one.
	maxTranscriptMessageRunes = 20000
	// maxTranscriptTotalRunes caps the sum across kept messages. Exceeding it
	// drops whole oldest rounds and sets DroppedRounds.
	maxTranscriptTotalRunes = 40000
	// maxTranscriptBytes is the final guard on the serialized envelope, applied
	// after the rune budgets. A transcript that cannot fit is not persisted at
	// all rather than persisted corrupt.
	maxTranscriptBytes = 262144
)

// TranscriptToolCall is one tool invocation requested by the model.
type TranscriptToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// TranscriptMessage is one canonical chat message from a turn.
type TranscriptMessage struct {
	Role      string               `json:"role"`
	Content   string               `json:"content,omitempty"`
	ToolCalls []TranscriptToolCall `json:"tool_calls,omitempty"`
	// ToolCallID pairs a tool result back to its assistant tool_call.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Name is the tool name, correlated from the assistant tool_call. The wire
	// format of a tool message does not carry it, but a reader needs it to
	// project or inspect the transcript without re-walking the pairing.
	Name string `json:"name,omitempty"`
	// Truncated marks Content as shortened; OrigRunes is the pre-truncation
	// length. Both absent means the content was not shortened — it may still
	// differ from the live message, which is redacted on the way in.
	Truncated bool `json:"truncated,omitempty"`
	OrigRunes int  `json:"orig_runes,omitempty"`
}

// TranscriptV1 is the persisted envelope stored under
// messages.metadata -> agent_transcript_v1 on the assistant row of a turn.
type TranscriptV1 struct {
	V        int                 `json:"v"`
	Messages []TranscriptMessage `json:"messages"`
	// DroppedRounds counts whole tool rounds shed to fit the budget. Non-zero
	// means this transcript is incomplete at the FRONT (oldest rounds go first).
	DroppedRounds int `json:"dropped_rounds,omitempty"`
}

// transcriptEnvelope is the metadata document written to messages.metadata.
// It is a wrapper object rather than a bare transcript so other consumers can
// later add sibling keys without a schema migration.
type transcriptEnvelope struct {
	Transcript *TranscriptV1 `json:"agent_transcript_v1,omitempty"`
}

// currentTurnStart returns the index of the message that begins the current
// user turn, or -1 if there is none.
//
// This is the single definition of "where does this turn start". Both the model
// history assembler and the persisted transcript call it, so the two can never
// disagree about which messages belong to the turn — a drift that would make the
// persisted transcript silently unlike what the model saw.
func currentTurnStart(messages []openai.ChatCompletionMessage) int {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == openai.ChatMessageRoleUser {
			return index
		}
	}
	return -1
}

// buildTranscriptV1 converts the live turn slice into the persisted form.
// It returns nil when the turn holds no tool round-trips: a plain
// question/answer turn is already fully represented by the user and assistant
// rows, so storing it again would be pure duplication.
func buildTranscriptV1(messages []openai.ChatCompletionMessage) *TranscriptV1 {
	start := currentTurnStart(messages)
	if start < 0 {
		return nil
	}
	turn := messages[start:]

	hasToolTraffic := false
	for _, msg := range turn {
		if msg.Role == openai.ChatMessageRoleTool || len(msg.ToolCalls) > 0 {
			hasToolTraffic = true
			break
		}
	}
	if !hasToolTraffic {
		return nil
	}

	// Tool messages carry only a ToolCallID on the wire. Correlate the tool name
	// from the assistant message that requested it so the stored result is
	// self-describing.
	nameByCallID := make(map[string]string, 4)
	for _, msg := range turn {
		for _, call := range msg.ToolCalls {
			if call.ID != "" && call.Function.Name != "" {
				nameByCallID[call.ID] = call.Function.Name
			}
		}
	}

	out := make([]TranscriptMessage, 0, len(turn))
	for _, msg := range turn {
		converted := TranscriptMessage{
			Role:       string(msg.Role),
			ToolCallID: msg.ToolCallID,
		}
		// Redact before bounding, and before anything is persisted.
		//
		// Ordinary replayed history reaches the model through the same
		// role-specific canonicalConversationText boundary. The transcript is a
		// second road to the same place, so it must use that exact form — otherwise
		// a Jupyter token or password stripped from the assistant line would
		// survive verbatim in metadata and be handed back on the next turn, with
		// different forms of the same turn sitting in one row.
		converted.Content, converted.Truncated, converted.OrigRunes = boundContent(canonicalConversationText(msg.Role, msg.Content))
		if msg.Role == openai.ChatMessageRoleTool {
			converted.Name = nameByCallID[msg.ToolCallID]
		}
		for _, call := range msg.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, TranscriptToolCall{
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: boundToolArguments(safeToolConversationText(call.Function.Arguments)),
			})
		}
		out = append(out, converted)
	}

	transcript := &TranscriptV1{V: transcriptSchemaVersion, Messages: out}
	shedRoundsToBudget(transcript)
	return transcript
}

// maxRawTurnBytes caps the UNPROCESSED size of a turn the transcript will look
// at. It is a CPU/latency guard, not a storage one — maxTranscriptBytes bounds
// what is written, this bounds what is read. Generous next to the 40000-rune
// storage budget so only a genuinely pathological turn trips it.
const maxRawTurnBytes = 1 << 20 // 1 MiB

// oversizedRawTurn reports whether this turn's raw bytes exceed what is worth
// scanning. It measures len() only: no redaction, no rune conversion, no
// allocation — the whole point is to decide before paying for any of those.
func oversizedRawTurn(messages []openai.ChatCompletionMessage) bool {
	start := currentTurnStart(messages)
	if start < 0 {
		return false
	}
	total := 0
	for _, msg := range messages[start:] {
		total += len(msg.Content)
		for _, call := range msg.ToolCalls {
			total += len(call.Function.Arguments) + len(call.Function.Name)
		}
		if total > maxRawTurnBytes {
			return true
		}
	}
	return false
}

// boundContent truncates one message body, reporting whether it did and what
// the original length was.
func boundContent(content string) (string, bool, int) {
	runes := []rune(content)
	if len(runes) <= maxTranscriptMessageRunes {
		return content, false, 0
	}
	return string(runes[:maxTranscriptMessageRunes]), true, len(runes)
}

// maxToolArgumentPrefixRunes is how much of an over-long argument string is kept
// inside the marker. It is generous enough to retain the identifying fields —
// UHostId, Region, Zone — which is the part a later turn actually reads.
const maxToolArgumentPrefixRunes = 512

// boundToolArguments bounds an argument string without ever emitting invalid
// JSON.
//
// A tool call's arguments are parsed, not read as prose, so a truncated prefix
// is not a shortened version of the call — it is a broken one, and the model is
// shown a call it believes it made in a form it could not have made. Over-length
// arguments are therefore replaced wholesale by a well-formed object that says
// so and carries the identifying head.
func boundToolArguments(args string) string {
	runes := []rune(args)
	if len(runes) <= maxTranscriptMessageRunes {
		return args
	}
	marker, err := json.Marshal(struct {
		Truncated bool   `json:"__truncated__"`
		OrigRunes int    `json:"__orig_runes__"`
		Prefix    string `json:"__prefix__"`
	}{true, len(runes), string(runes[:maxToolArgumentPrefixRunes])})
	if err != nil {
		return `{"__truncated__":true}`
	}
	return string(marker)
}

// truncationNotice is appended to a shortened body on projection. The stored
// Truncated flag is useless to the model if only Content is replayed: it would
// read a prefix of an instance list as the whole list and answer confidently
// about machines that were simply cut off.
func truncationNotice(origRunes int) string {
	return fmt.Sprintf("\n…[内容已截断，原文共 %d 字符；如需完整结果请重新查询]", origRunes)
}

// shedRoundsToBudget drops whole oldest tool rounds until the transcript fits
// maxTranscriptTotalRunes.
//
// A "round" is one assistant message bearing tool_calls plus every tool message
// answering it. Rounds are the shedding unit because a stored transcript must
// stay replayable: an assistant tool_call without its tool result — or a tool
// result whose call is gone — is not a well-formed message list. The leading
// user message and the trailing final assistant message are never shed; they are
// the turn's question and answer, and they are also what the messages table
// already holds, so dropping them would make the transcript disagree with its
// own row.
func shedRoundsToBudget(transcript *TranscriptV1) {
	if transcript == nil {
		return
	}
	for transcriptRunes(transcript.Messages) > maxTranscriptTotalRunes {
		roundStart, roundEnd := firstToolRound(transcript.Messages)
		if roundStart < 0 {
			return
		}
		transcript.Messages = append(
			append([]TranscriptMessage{}, transcript.Messages[:roundStart]...),
			transcript.Messages[roundEnd:]...,
		)
		transcript.DroppedRounds++
	}
}

// firstToolRound returns the [start, end) span of the earliest assistant
// tool_call message and the tool results that answer it, or (-1, -1) when the
// transcript holds no complete round left to shed.
func firstToolRound(messages []TranscriptMessage) (int, int) {
	for i, msg := range messages {
		if len(msg.ToolCalls) == 0 {
			continue
		}
		end := i + 1
		for end < len(messages) && messages[end].Role == openai.ChatMessageRoleTool {
			end++
		}
		return i, end
	}
	return -1, -1
}

func transcriptRunes(messages []TranscriptMessage) int {
	total := 0
	for _, msg := range messages {
		total += len([]rune(msg.Content))
		for _, call := range msg.ToolCalls {
			total += len([]rune(call.Arguments)) + len([]rune(call.Name))
		}
	}
	return total
}

// marshalTranscriptMetadata serializes the envelope for the metadata column.
// It returns nil (not an error) when there is nothing worth storing, and an
// error only when the result would exceed maxTranscriptBytes even after rune
// budgeting — persisting a truncated JSON document is never acceptable, so the
// caller drops the write and records it instead.
func marshalTranscriptMetadata(transcript *TranscriptV1) (json.RawMessage, error) {
	if transcript == nil || len(transcript.Messages) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(transcriptEnvelope{Transcript: transcript})
	if err != nil {
		return nil, err
	}
	if len(raw) > maxTranscriptBytes {
		return nil, errTranscriptOversized
	}
	return raw, nil
}

type transcriptError string

func (e transcriptError) Error() string { return string(e) }

// errTranscriptOversized reports a transcript that could not be bounded to the
// byte guard. It is a metric, not a turn failure.
const errTranscriptOversized = transcriptError("canonical transcript exceeds byte budget")

// transcriptReplayableRoles is the closed set of roles a stored turn can hold.
//
// system is absent on purpose. Persisted rows must never introduce instructions
// into replayed model context; the producer itself starts each slice at the user
// message.
var transcriptReplayableRoles = map[string]bool{
	openai.ChatMessageRoleUser:      true,
	openai.ChatMessageRoleAssistant: true,
	openai.ChatMessageRoleTool:      true,
}

// validTranscriptStructure reports whether a stored transcript may be replayed.
//
// An illegal role or incomplete tool-call identity invalidates the row. A
// malformed argument string is handled later at round scope, so the remaining
// transcript can still be replayed.
func validTranscriptStructure(transcript *TranscriptV1) bool {
	if transcript == nil {
		return false
	}
	for _, msg := range transcript.Messages {
		if !transcriptReplayableRoles[msg.Role] {
			return false
		}
		if len(msg.ToolCalls) > 0 && msg.Role != openai.ChatMessageRoleAssistant {
			return false
		}
		for _, call := range msg.ToolCalls {
			// A provider pairs results to calls by id and dispatches by name.
			// Either missing makes the round unreplayable, and the producer sets
			// both from a real tool call, so neither can be absent by accident.
			if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
				return false
			}
		}
	}
	return true
}

// ParseTranscriptMetadata reads a persisted messages.metadata document back
// into a transcript. It returns nil for absent, malformed, unknown-version or
// structurally illegal documents rather than an error: this is a migration
// carrier, and a row whose metadata cannot be understood must degrade to "no
// transcript", never to a failed rebuild.
func ParseTranscriptMetadata(raw json.RawMessage) *TranscriptV1 {
	if len(raw) == 0 {
		return nil
	}
	var envelope transcriptEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil
	}
	transcript := envelope.Transcript
	if transcript == nil || transcript.V != transcriptSchemaVersion ||
		len(transcript.Messages) == 0 || !validTranscriptStructure(transcript) {
		return nil
	}
	return transcript
}

// ProjectTranscript rebuilds chat messages from a persisted transcript.
//
// This is the read side of the persisted transcript and the reason the stored form
// keeps ordering and tool_call pairing: the output must be a well-formed message
// list, indistinguishable from what the hot engine held for that turn.
//
// It deliberately drops any tool_call whose result did not survive, and any tool
// result whose call did not. Bounding sheds whole rounds so this should never
// fire, but a half round reaching a provider is a 400 on the whole request —
// this is the last line, not the first.
//
// validToolArguments reports whether an argument string may be replayed to a
// provider. Empty is allowed — a no-argument call legitimately carries "" — but
// anything else must parse, because `arguments` is consumed as JSON and a
// malformed value is not a shortened call, it is a call the model could not have
// meant. Replaying one also teaches the model that the malformed form was
// accepted.
func validToolArguments(arguments string) bool {
	trimmed := strings.TrimSpace(arguments)
	return trimmed == "" || json.Valid([]byte(trimmed))
}

// projectedContent restores one stored body, re-attaching the fact that it was
// shortened. Storing Truncated and then replaying only Content would leave the
// model unable to tell a prefix from a complete result.
func projectedContent(msg TranscriptMessage) string {
	if !msg.Truncated {
		return msg.Content
	}
	return msg.Content + truncationNotice(msg.OrigRunes)
}

func ProjectTranscript(transcript *TranscriptV1) []openai.ChatCompletionMessage {
	if transcript == nil {
		return nil
	}
	// ProjectTranscript is exported, so validate values supplied without the
	// metadata parser as well.
	if !validTranscriptStructure(transcript) {
		return nil
	}
	// A result answers only an earlier call. Duplicate ids use their first
	// declaration; later copies cannot be paired unambiguously.
	declIndex := make(map[string]int, 4)
	for i, msg := range transcript.Messages {
		for _, call := range msg.ToolCalls {
			if call.ID == "" {
				continue
			}
			if _, dup := declIndex[call.ID]; dup {
				continue
			}
			declIndex[call.ID] = i
		}
	}
	// Only the CONTIGUOUS run of tool messages immediately after a call can
	// answer it. Ordering alone is not enough: a provider pairs by adjacency, so
	// `assistant(c1)` / `assistant("稍等")` / `tool(c1)` is rejected even though
	// the result does follow the call. Scanning the run also subsumes the
	// ordering check — a result placed BEFORE its call is in no run at all.
	answered := make(map[string]bool, 4)
	for i, msg := range transcript.Messages {
		if len(msg.ToolCalls) == 0 {
			continue
		}
		for j := i + 1; j < len(transcript.Messages) &&
			transcript.Messages[j].Role == openai.ChatMessageRoleTool; j++ {
			id := transcript.Messages[j].ToolCallID
			if id != "" && declIndex[id] == i {
				answered[id] = true
			}
		}
	}

	// Drop rounds whose arguments are not parseable JSON.
	//
	// boundToolArguments guarantees this only for arguments IT shortened. The
	// commoner case is arguments that were never valid: executeToolOnce records
	// that roughly 4% of SearchKnowledge calls arrive with a leaked tag or a bare
	// query string instead of a JSON object (engine.go, "parameter parse error").
	// Those assistant messages, and the error string that answered them, are in
	// e.messages like any other and would be replayed verbatim.
	//
	// Clearing the ID from `answered` is all that is needed: the loop below drops
	// a tool_call that nothing answers, and the tool result is then dropped in
	// turn because its call was never declared. So the whole round leaves
	// together, which is the invariant the rest of this file is built on.
	for _, msg := range transcript.Messages {
		for _, call := range msg.ToolCalls {
			if !validToolArguments(call.Arguments) {
				delete(answered, call.ID)
			}
		}
	}

	out := make([]openai.ChatCompletionMessage, 0, len(transcript.Messages))
	declared := make(map[string]bool, 4)
	// A second result for the same call is dropped rather than replayed. One
	// tool_call answered twice is not a richer record — it is a pairing a
	// provider cannot resolve, and the later copy would silently override the
	// result the model actually observed.
	resultEmitted := make(map[string]bool, 4)
	for _, msg := range transcript.Messages {
		if msg.Role == openai.ChatMessageRoleTool {
			if msg.ToolCallID == "" || !declared[msg.ToolCallID] || resultEmitted[msg.ToolCallID] {
				continue
			}
			resultEmitted[msg.ToolCallID] = true
			out = append(out, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				Content:    projectedContent(msg),
				ToolCallID: msg.ToolCallID,
			})
			continue
		}

		converted := openai.ChatCompletionMessage{Role: msg.Role, Content: projectedContent(msg)}
		for _, one := range msg.ToolCalls {
			// declared[] here is the duplicate guard, not bookkeeping: reaching a
			// call whose id was already emitted means the row declared it twice.
			if !answered[one.ID] || declared[one.ID] {
				continue
			}
			declared[one.ID] = true
			// Type is restored rather than stored: every tool this agent exposes
			// is a function tool (VisibleRegistry builds nothing else), so the
			// field is a constant on the wire and storing it would only create a
			// way for the record to disagree with the registry. If a non-function
			// tool type is ever added, this becomes lossy and the schema must
			// carry it — the parity test is what will say so.
			converted.ToolCalls = append(converted.ToolCalls, openai.ToolCall{
				ID:       one.ID,
				Type:     openai.ToolTypeFunction,
				Function: openai.FunctionCall{Name: one.Name, Arguments: one.Arguments},
			})
		}
		// An assistant turn that was nothing but unanswered tool_calls carries no
		// information once they are dropped, and an empty assistant message is
		// not valid input.
		if converted.Role == openai.ChatMessageRoleAssistant &&
			converted.Content == "" && len(converted.ToolCalls) == 0 {
			continue
		}
		out = append(out, converted)
	}
	return out
}

// recordedTurn is one ended turn plus its tool work. An empty Assistant means
// the request was left unanswered, not that its observed tool work was undone.
//
// This is the single place a turn's cross-turn memory lives, and it exists
// because the two rebuild paths must agree by construction rather than by
// coincidence: the hot engine appends one at every turn exit, a cold rebuild
// appends one per persisted row, and both feed the same projector. Deriving the
// hot side from e.messages instead would have meant reading a list the strip
// pass has already emptied, and aligning it to the cold side positionally.
type recordedTurn struct {
	User       string
	Assistant  string
	Transcript *TranscriptV1
}

// recordTurn appends a completed exchange, keeping the newest that fit
// maxRawHistoryRunes — a bound set above what the replay budget can admit, so the
// transcript can never disappear before the exchange it belongs to.
// transcriptFromRow reads the canonical key from the general-purpose metadata
// document. Unknown or old rows safely degrade to a plain user/assistant pair.
func transcriptFromRow(raw json.RawMessage) *TranscriptV1 {
	return ParseTranscriptMetadata(raw)
}

func (e *Engine) recordTurn(turn recordedTurn) {
	if strings.TrimSpace(turn.User) == "" {
		return
	}
	e.recentTurns = append(e.recentTurns, turn)
	e.recentTurns = budgetRecordedTurns(e.recentTurns, maxRawHistoryRunes)
}

// budgetRecordedTurns keeps the newest records that fit budgetRunes, dropping
// whole records from the oldest end. The newest is kept unconditionally, matching
// budgetReplayedPairs: this list is a SOURCE for that one, so a record must not
// disappear before the exchange it belongs to could even be considered.
//
// The raw-history budget stays above the replay budget so this source list does
// not become a hidden memory ceiling.
func budgetRecordedTurns(turns []recordedTurn, budgetRunes int) []recordedTurn {
	if budgetRunes <= 0 || len(turns) == 0 {
		return turns
	}
	spent := 0
	// Seed after the slice so the loop itself keeps the newest record even when it
	// alone exceeds the budget.
	keepFrom := len(turns)
	for i := len(turns) - 1; i >= 0; i-- {
		cost := len([]rune(turns[i].User)) + len([]rune(turns[i].Assistant))
		if turns[i].Transcript != nil {
			cost += transcriptRunes(turns[i].Transcript.Messages)
		}
		if i < len(turns)-1 && spent+cost > budgetRunes {
			break
		}
		spent += cost
		keepFrom = i
	}
	return turns[keepFrom:]
}

// turnEndpoints returns the user question and final assistant answer. The
// answer is empty when the turn ended before a final answer was committed.
func turnEndpoints(turn []openai.ChatCompletionMessage) (string, string) {
	if len(turn) == 0 || turn[0].Role != openai.ChatMessageRoleUser {
		return "", ""
	}
	last := turn[len(turn)-1]
	if last.Role != openai.ChatMessageRoleAssistant || len(last.ToolCalls) > 0 {
		return turn[0].Content, ""
	}
	return turn[0].Content, last.Content
}

// TranscriptStats distinguishes "nothing to store" from a rejected payload.
type TranscriptStats struct {
	Attempted bool
	Bytes     int
	Messages  int
	Dropped   int
	Oversized bool
	Invalid   bool
}

// LastTurnTranscript returns the canonical transcript captured at the end of the
// most recent turn, along with its stats. A nil payload means the turn had no
// tool traffic worth persisting, or that bounding rejected it — Stats says which.
//
// This accessor is the PERSISTENCE side: it hands the serialized document to the
// storage path and nothing else. Model replay runs through recentTurns and
// attachRecordedTranscripts.
// The nil receiver is answered rather than panicking: callers reach this
// through an interface, where a nil *Engine is not a nil interface.
func (e *Engine) LastTurnTranscript() (json.RawMessage, TranscriptStats) {
	if e == nil {
		return nil, TranscriptStats{}
	}
	return e.lastTurnTranscript, e.lastTurnTranscriptStats
}

// captureTurnTranscript records the just-finished turn's canonical transcript.
// It runs on every exit path from a turn, including errors, because a turn that
// failed after three tool calls is exactly the turn whose transcript is worth
// keeping.
func (e *Engine) captureTurnTranscript() {
	e.lastTurnTranscript = nil
	e.lastTurnTranscriptStats = TranscriptStats{}
	// Close an unanswered turn in the raw history without inventing an answer.
	// This empty boundary is never sent to the provider; replay retains its user
	// and any paired tool results instead. It distinguishes a prior interrupted
	// request from the current user message while compiling the next turn.
	if start := currentTurnStart(e.messages); start >= 0 {
		last := e.messages[len(e.messages)-1]
		if last.Role != openai.ChatMessageRoleAssistant || len(last.ToolCalls) > 0 {
			e.messages = append(e.messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant})
		}
	}

	// Cheap raw-size guard BEFORE any transcript work.
	//
	// This runs on the response path — the deferred call returns before the HTTP
	// layer writes `done` — and the storage limits below do not bound it. They
	// bound the OUTPUT: content is redacted (regexes over the whole string) and
	// converted to []rune (a full copy) and only then bounded. A single
	// pathological tool result therefore costs its full size in scan and copy no
	// matter how little of it is kept, and the user waits for that.
	//
	// Over the limit the turn is recorded as Oversized and nothing is persisted.
	// Deliberately not "truncate the raw text instead": cutting a body at a byte
	// offset before redaction can slice a credential in half and store the
	// halves, which is the one outcome worse than storing nothing.
	if oversizedRawTurn(e.messages) {
		if start := currentTurnStart(e.messages); start >= 0 {
			if user, assistant := turnEndpoints(e.messages[start:]); user != "" {
				e.recordTurn(recordedTurn{User: user, Assistant: assistant})
			}
		}
		e.lastTurnTranscriptStats = TranscriptStats{Attempted: true, Oversized: true}
		return
	}

	transcript := buildTranscriptV1(e.messages)

	// Record the exchange whether or not it had tool traffic. A turn that only
	// answered a question is still a turn the next one may refer to, and the
	// recorded window has to hold the same exchanges the replayed conversation
	// does or the two would disagree about what "the last five turns" means.
	if start := currentTurnStart(e.messages); start >= 0 {
		if user, assistant := turnEndpoints(e.messages[start:]); user != "" {
			e.recordTurn(recordedTurn{User: user, Assistant: assistant, Transcript: transcript})
		}
	}

	if transcript == nil {
		return
	}
	stats := TranscriptStats{
		Attempted: true,
		Messages:  len(transcript.Messages),
		Dropped:   transcript.DroppedRounds,
	}
	raw, err := marshalTranscriptMetadata(transcript)
	switch {
	case err == errTranscriptOversized:
		stats.Oversized = true
	case err != nil:
		stats.Invalid = true
	default:
		stats.Bytes = len(raw)
		e.lastTurnTranscript = raw
	}
	e.lastTurnTranscriptStats = stats
}

// MarkLastTurnInterrupted reconciles hot history with an aborted/error assistant
// row after ChatWithOptions returns. A transport can be cancelled even when the
// engine returned nil error; that candidate answer must not become a completed
// answer only on the hot path. Existing tool observations stay in the canonical
// transcript, subject to the same redaction and size limits as successful turns.
// This method records history only; it never dispatches or retries a tool.
func (e *Engine) MarkLastTurnInterrupted() {
	if e == nil {
		return
	}
	start := currentTurnStart(e.messages)
	if start < 0 {
		return
	}
	user, assistant := turnEndpoints(e.messages[start:])
	if n := len(e.recentTurns); n > 0 && e.recentTurns[n-1].User == user && e.recentTurns[n-1].Assistant == assistant {
		e.recentTurns = e.recentTurns[:n-1]
	}
	last := e.messages[len(e.messages)-1]
	if last.Role == openai.ChatMessageRoleAssistant && len(last.ToolCalls) == 0 {
		e.messages = e.messages[:len(e.messages)-1]
	}
	e.messages = append(e.messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant})
	e.captureTurnTranscript()
}
