package engine

// toolDelivery says what the ReAct loop must do with a tool's text.
//
// The default is deliverToModel: the text is an observation, the model reads it
// and writes the answer. The other two exist because some sentences are not the
// model's to paraphrase — an authorization refusal, a consent outcome, a
// channel-owned support entry, or the result of a write that may already have
// committed upstream. Those reach the user exactly as the engine wrote them.
type toolDelivery uint8

const (
	// deliverToModel is an ordinary observation. The zero value, so a tool that
	// says nothing about delivery gets the safe answer.
	deliverToModel toolDelivery = iota
	// deliverVerbatim puts the text in front of the user byte-identical and lets
	// the turn continue, so a mixed question can still be finished.
	deliverVerbatim
	// deliverFinal puts the text in front of the user byte-identical and ends the
	// turn. No further model round runs.
	deliverFinal
)

// toolOutcome is what one tool call produced.
//
// Observation is what the model sees. It is empty for the two delivered kinds:
// the loop composes model history from the delivered text there, because a
// deterministic reply is joined with any committed-write sentences first, and a
// verbatim block is deliberately replaced in history by a figure-free note.
type toolOutcome struct {
	Observation string
	Reply       string
	Delivery    toolDelivery
}

// observed carries an ordinary tool result back to the model.
func observed(result string) toolOutcome {
	return toolOutcome{Observation: result}
}

// deterministicReply ends the turn with text the engine wrote. Use it where the
// sentence carries the outcome itself — a refusal, a decline, an upstream error
// already phrased for a human, or a write whose effect must not be narrated by
// a model that did not observe it.
func deterministicReply(reply string) toolOutcome {
	return toolOutcome{Reply: reply, Delivery: deliverFinal}
}

// verbatimReply delivers text exactly as written without ending the turn. Use
// it for figures rendered from structured fields, which the model must not
// restate or recompute.
//
// Observation is what the model gets in place of the text: it must say that the
// user already has the authoritative detail without repeating any of it. That
// note is also what a repeat of the same call replays, so re-asking cannot
// launder the withheld figures into context through the reuse cache.
func verbatimReply(reply, observation string) toolOutcome {
	return toolOutcome{Observation: observation, Reply: reply, Delivery: deliverVerbatim}
}

func (o toolOutcome) terminatesTurn() bool { return o.Delivery == deliverFinal }

func (o toolOutcome) deliversToUser() bool { return o.Delivery != deliverToModel }
