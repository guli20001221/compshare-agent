// Package router holds the Chat()-head pre-LLM decision chain.
//
// PreBlock is a thin, domain-agnostic rule dispatcher: callers register
// predicates + (category, reply) pairs at construction, and Decide(msg)
// returns the first match in registration order. The package has zero
// knowledge of CompShare-specific keywords; that lives with each caller
// alongside its data.
//
// The split exists so that:
//
//  1. Adding a new pre-block category is one line at the engine wiring
//     site (a Rule literal) rather than 30 lines scattered across
//     engine.go Chat().
//  2. Per-category test fixtures + per-category eval slices can pivot
//     on Rule.Category, matching the trace contract on the consumer
//     side.
//  3. Future per-tenant rule overlays inject extra Rules at session
//     construction without engine.Chat changing.
//
// Decide() does NOT mutate engine state; the caller still owns side
// effects (pendingResourceSelection clear, hardBlockObserver fire,
// message append). The dispatcher only returns the decision triplet.
package router

// Rule is one entry in the pre-block chain: a predicate plus the reply
// + category to emit when it fires. Category MUST be a stable string
// (downstream MySQL trace ingest pivots on it); use the constants in
// internal/refusal.
type Rule struct {
	// Match is the predicate evaluated against the raw user message.
	// MUST be pure (no IO, no state). Returns true to short-circuit
	// the chain. Typical implementations call textutil.Normalize once
	// and then strings.Contains against a narrow keyword list.
	Match func(userMsg string) bool

	// Category is the engine_hard_block.category trace value when this
	// rule fires.
	Category string

	// Reply is the user-facing text returned verbatim when this rule
	// fires. Byte-stable; engine integration tests pin assert.Equal.
	Reply string
}

// Decision is the outcome of PreBlock.Decide. Matched == false means
// the chain fell through; callers proceed with the regular planner /
// retrieval / ReAct path.
type Decision struct {
	Matched  bool
	Category string
	Reply    string
}

// PreBlock holds the ordered rule chain.
type PreBlock struct {
	rules []Rule
}

// New constructs a PreBlock from an ordered rule list. The order is the
// evaluation order — earlier rules win in case of overlap. Caller is
// responsible for putting more-specific rules first.
//
// Empty rule list is allowed; Decide will always return zero-value
// Decision (Matched=false), which is the engine pass-through behavior.
func New(rules ...Rule) *PreBlock {
	return &PreBlock{rules: rules}
}

// Decide evaluates the rule chain in registration order against the
// raw user message. Returns the first match; if none, returns
// zero-value Decision (Matched=false). No engine state is mutated.
func (p *PreBlock) Decide(userMsg string) Decision {
	if p == nil {
		return Decision{}
	}
	for _, r := range p.rules {
		if r.Match != nil && r.Match(userMsg) {
			return Decision{Matched: true, Category: r.Category, Reply: r.Reply}
		}
	}
	return Decision{}
}

// Categories returns the registered categories in evaluation order.
// Used by tests to assert the rule chain shape without exposing the
// internal rules slice.
func (p *PreBlock) Categories() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.rules))
	for _, r := range p.rules {
		out = append(out, r.Category)
	}
	return out
}
