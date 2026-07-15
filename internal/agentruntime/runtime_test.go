package agentruntime

import (
	"context"
	"errors"
	"testing"
)

func TestRuntimeOwnsRoundsAndTerminalResult(t *testing.T) {
	var events []Event
	runtime := MustNew(3, func(event Event) { events = append(events, event) })
	result, err := runtime.Run(context.Background(), func(_ context.Context, round *Round) (Result, error) {
		round.ModelStep(1, false)
		round.Observation("SearchKnowledge")
		if round.Index() == 0 {
			return Continue(), nil
		}
		round.ModelStep(0, true)
		return Final("answer", FinishFinalAnswer), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reply != "answer" || result.Reason != FinishFinalAnswer {
		t.Fatalf("result = %#v", result)
	}
	if len(events) != 8 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].Type != EventRoundStarted || events[len(events)-1].Type != EventFinished {
		t.Fatalf("event boundary = %#v", events)
	}
}

func TestRuntimeReportsRoundLimit(t *testing.T) {
	runtime := MustNew(2, nil)
	result, err := runtime.Run(context.Background(), func(context.Context, *Round) (Result, error) {
		return Continue(), nil
	})
	if !errors.Is(err, ErrRoundLimit) || result.Reason != FinishRoundLimit {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestRuntimeStopsBeforeDriverOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, err := MustNew(2, nil).Run(ctx, func(context.Context, *Round) (Result, error) {
		called = true
		return Continue(), nil
	})
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("called=%v err=%v", called, err)
	}
}

func TestRuntimeRejectsInvalidConfiguration(t *testing.T) {
	if _, err := New(0, nil); err == nil {
		t.Fatal("expected max-round validation")
	}
	if _, err := MustNew(1, nil).Run(context.Background(), nil); err == nil {
		t.Fatal("expected nil-driver validation")
	}
}
