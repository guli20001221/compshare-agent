package server

// Answer-delta framing — C11 Phase A.
//
// Why this exists
// ---------------
// The WS protocol reserves ServerMsgAnswerDelta (protocol.go:69) for
// incremental answer chunks, but pre-PR #88 the server emitted only
// ServerMsgAnswerFinal with the full reply. This Phase A wires the
// frame type end-to-end:
//
//   - server splits the engine's final reply into N chunks
//   - emits one ServerMsgAnswerDelta per chunk
//   - then emits ServerMsgAnswerFinal with the canonical full text
//
// Phase A is SERVER-ONLY — engine.Chat still returns the full reply
// synchronously. True per-token streaming (engine forwards delta from
// the LLM as soon as each chunk arrives) is Phase B, which requires
// engine + llm/client.go changes and a cited-contract gate review.
//
// Client contract
// ---------------
// Clients can consume EITHER stream:
//   - Subscribe to answer_delta: render incrementally; concat all
//     deltas equals the final reply (byte-for-byte).
//   - Ignore answer_delta: read answer_final; equivalent to pre-PR #88.
//
// Both consumers see the same reply. The delta path delivers nothing
// new in Phase A (no latency reduction) but unlocks the protocol slot
// so v1 clients can prepare to handle deltas in Phase B without a
// further breaking change.

import (
	"strings"
	"unicode/utf8"
)

// answerDeltaMaxRunes caps individual answer_delta chunks. Chosen to
// keep each WS frame small enough to render incrementally without
// dropping noticeable latency (~one short sentence worth of UTF-8).
// Phase B should reconsider as part of the per-token streaming work.
const answerDeltaMaxRunes = 80

// chunkReplyForDelta splits text into chunks suitable for ServerMsgAnswerDelta
// framing. Invariants:
//   - Concatenating the returned chunks reproduces the input byte-for-byte.
//   - Empty input returns nil (no delta frames emitted).
//   - Chunks never split a UTF-8 rune (Chinese characters stay intact).
//   - Newline boundaries are preferred; falls back to size-based splits when
//     a single line exceeds answerDeltaMaxRunes.
//
// Phase A doesn't pace deltas — the client receives them as fast as the
// WS write loop drains. Phase B will tie chunk boundaries to actual LLM
// stream chunks for genuine latency reduction.
func chunkReplyForDelta(text string) []string {
	if text == "" {
		return nil
	}
	out := []string{}
	var current strings.Builder
	currentRunes := 0
	flush := func() {
		if current.Len() > 0 {
			out = append(out, current.String())
			current.Reset()
			currentRunes = 0
		}
	}
	for _, line := range splitKeepNewline(text) {
		lineRunes := utf8.RuneCountInString(line)
		if currentRunes+lineRunes > answerDeltaMaxRunes && currentRunes > 0 {
			flush()
		}
		if lineRunes > answerDeltaMaxRunes {
			// Single line exceeds cap — fold the in-progress chunk first,
			// then break the long line into rune-aligned chunks.
			flush()
			for _, sub := range splitByRuneCount(line, answerDeltaMaxRunes) {
				out = append(out, sub)
			}
			continue
		}
		current.WriteString(line)
		currentRunes += lineRunes
	}
	flush()
	return out
}

// splitKeepNewline splits text on '\n' but keeps the newline byte on the
// preceding chunk. Output concatenated equals input.
func splitKeepNewline(text string) []string {
	if text == "" {
		return nil
	}
	out := []string{}
	for {
		idx := strings.IndexByte(text, '\n')
		if idx < 0 {
			out = append(out, text)
			return out
		}
		out = append(out, text[:idx+1])
		text = text[idx+1:]
	}
}

// splitByRuneCount breaks text into at-most-runes-per-chunk windows,
// always on a UTF-8 rune boundary. Used as the fallback when a single
// line is longer than the chunk cap.
func splitByRuneCount(text string, runesPerChunk int) []string {
	if runesPerChunk <= 0 || text == "" {
		return []string{text}
	}
	out := []string{}
	var current strings.Builder
	currentRunes := 0
	for _, r := range text {
		current.WriteRune(r)
		currentRunes++
		if currentRunes >= runesPerChunk {
			out = append(out, current.String())
			current.Reset()
			currentRunes = 0
		}
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}
