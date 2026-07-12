package engine

// Eval-only capture for the PR B step-1 gate. The grounding validator has to be
// measured on real answers over real tool results before any of it is wired into
// the reply path, and the JSONL traces cannot supply that: observability hashes
// tool payloads (tool_calls[].args_hash / result_hash), by design, so a trace
// tells you a tool ran but not what it said.
//
// This writes the two things the offline measurement needs — the final reply and
// the turn's harvested facts — and nothing else. It is inert unless
// COMPSHARE_GROUNDING_DUMP names a file. Delete this file once the gate is past.

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/compshare-agent/internal/grounding"
)

var (
	groundingDumpMu   sync.Mutex
	groundingDumpPath = os.Getenv("COMPSHARE_GROUNDING_DUMP")
)

// groundingObserveOnly reports ungrounded claims WITHOUT touching the reply.
//
// Deliberately observe-only for step 2. The tempting thing is to strip or refuse on a
// violation, but the honest position is that we do not yet know what the surviving
// violations look like once deterministic rendering removes the instance-table class:
// the 2026-07-12 capture's remaining flags were mostly TRUE GPU specs the model knows
// and our corpus does not (82.6 TFLOPS is the real 4090 FP16 figure), and suppressing
// those would degrade correct answers to protect against nothing. Measure first, enforce
// on evidence.
func (e *Engine) observeGrounding(reply string) []grounding.Violation {
	if e.turnFacts == nil || reply == "" {
		return nil
	}
	return grounding.Check(reply, e.turnFacts)
}

type groundingDumpRecord struct {
	Turn       int      `json:"turn"`
	UserText   string   `json:"user_text"`
	Reply      string   `json:"reply"`
	FactCount  int      `json:"fact_count"`
	Numbers    []string `json:"numbers"`
	Text       []string `json:"text"`
	Violations []string `json:"violations,omitempty"`
	// TableOffered: an instance table was rendered and handed to the model this turn.
	// PlaceholderUsed: the model actually referenced it instead of hand-writing a list.
	// Offered-but-not-used is the interesting cell — that is the turn where the whole
	// mechanism was available and the model declined it, and therefore the turn where it
	// can still invent a machine.
	TableOffered    bool `json:"table_offered"`
	PlaceholderUsed bool `json:"placeholder_used"`
}

// dumpGroundingTurn appends one turn's (reply, facts) pair. Best-effort: an eval
// hook must never be able to fail a user's turn, so every error is swallowed.
func (e *Engine) dumpGroundingTurn(userMsg, reply string, tableOffered, placeholderUsed bool) {
	if groundingDumpPath == "" || e.turnFacts == nil {
		return
	}
	nums, text := grounding.Dump(e.turnFacts)
	var viol []string
	for _, v := range e.observeGrounding(reply) {
		viol = append(viol, v.String())
	}
	rec := groundingDumpRecord{
		Turn:            e.userTurn,
		UserText:        userMsg,
		Reply:           reply,
		FactCount:       len(nums) + len(text),
		Numbers:         nums,
		Text:            text,
		Violations:      viol,
		TableOffered:    tableOffered,
		PlaceholderUsed: placeholderUsed,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	groundingDumpMu.Lock()
	defer groundingDumpMu.Unlock()
	f, err := os.OpenFile(groundingDumpPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}
