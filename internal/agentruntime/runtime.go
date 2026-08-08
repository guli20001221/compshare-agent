// Package agentruntime owns the lifecycle of the model/tool reasoning loop.
// Domain-specific prompt, grounding and tool behavior stays behind the Driver
// callback while the migration is in progress; callers do not own round
// counting or terminal-state semantics anymore.
package agentruntime

import (
	"context"
	"errors"
	"fmt"
)

var ErrRoundLimit = errors.New("agent runtime round limit reached")

type FinishReason string

const (
	FinishFinalAnswer        FinishReason = "final_answer"
	FinishDeterministicReply FinishReason = "deterministic_reply"
	FinishBudgetRecovery     FinishReason = "budget_recovery"
	FinishBudgetRefusal      FinishReason = "budget_refusal"
	FinishRateLimit          FinishReason = "rate_limit"
	FinishOutputTruncated    FinishReason = "output_truncated"
	FinishRoundLimit         FinishReason = "round_limit"
)

type EventType string

const (
	EventRoundStarted EventType = "round_started"
	EventModelStep    EventType = "model_step"
	EventObservation  EventType = "observation"
	EventFinished     EventType = "finished"
)

type Event struct {
	Type          EventType
	Round         int
	ToolCallCount int
	HasFinalText  bool
	Action        string
	FinishReason  FinishReason
}

type Observer func(Event)

type Result struct {
	Reply  string
	Done   bool
	Reason FinishReason
}

func Continue() Result { return Result{} }

func Final(reply string, reason FinishReason) Result {
	return Result{Reply: reply, Done: true, Reason: reason}
}

type Driver func(context.Context, *Round) (Result, error)

type Runtime struct {
	maxRounds int
	observer  Observer
}

func New(maxRounds int, observer Observer) (*Runtime, error) {
	if maxRounds <= 0 {
		return nil, fmt.Errorf("agent runtime: max rounds must be positive")
	}
	return &Runtime{maxRounds: maxRounds, observer: observer}, nil
}

func MustNew(maxRounds int, observer Observer) *Runtime {
	runtime, err := New(maxRounds, observer)
	if err != nil {
		panic(err)
	}
	return runtime
}

func (r *Runtime) Run(ctx context.Context, driver Driver) (Result, error) {
	if r == nil || r.maxRounds <= 0 {
		return Result{}, fmt.Errorf("agent runtime: invalid runtime")
	}
	if driver == nil {
		return Result{}, fmt.Errorf("agent runtime: nil driver")
	}
	for index := 0; index < r.maxRounds; index++ {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		round := &Round{index: index, observer: r.observer}
		round.emit(Event{Type: EventRoundStarted, Round: index})
		result, err := driver(ctx, round)
		if err != nil {
			return Result{}, err
		}
		if !result.Done {
			continue
		}
		if result.Reason == "" {
			result.Reason = FinishFinalAnswer
		}
		round.emit(Event{Type: EventFinished, Round: index, FinishReason: result.Reason})
		return result, nil
	}
	if r.observer != nil {
		r.observer(Event{Type: EventFinished, Round: r.maxRounds - 1, FinishReason: FinishRoundLimit})
	}
	return Result{Reason: FinishRoundLimit}, ErrRoundLimit
}

type Round struct {
	index    int
	observer Observer
}

func (r *Round) Index() int {
	if r == nil {
		return 0
	}
	return r.index
}

func (r *Round) ModelStep(toolCallCount int, hasFinalText bool) {
	if r == nil {
		return
	}
	r.emit(Event{Type: EventModelStep, Round: r.index, ToolCallCount: toolCallCount, HasFinalText: hasFinalText})
}

func (r *Round) Observation(action string) {
	if r == nil {
		return
	}
	r.emit(Event{Type: EventObservation, Round: r.index, Action: action})
}

func (r *Round) emit(event Event) {
	if r != nil && r.observer != nil {
		r.observer(event)
	}
}
