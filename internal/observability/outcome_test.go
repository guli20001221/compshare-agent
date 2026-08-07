package observability

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

// genuineBlock is a record with a real engine hard-block (a category that is
// neither the synthetic chat_error nor token-budget special cases).
func genuineBlock() TraceRecord {
	return TraceRecord{EngineHardBlock: EngineHardBlockTrace{Hit: true, Category: "account_billing"}}
}

// TestDeriveTerminatedBy_Precedence pins the first-match-wins ordering
// blocked > user_cancel > timeout > budget > error > empty_reply > done. Each case
// makes a HIGHER-precedence and at least one LOWER-precedence condition true at
// once, so a mis-ordered chain would pick the wrong value.
func TestDeriveTerminatedBy_Precedence(t *testing.T) {
	someErr := errors.New("boom")
	cases := []struct {
		name string
		rec  TraceRecord
		sig  FinishSignals
		want string
	}{
		{
			// genuine block + cancel + ceiling + empty all true → blocked wins.
			"blocked beats all",
			genuineBlock(),
			FinishSignals{ChatErr: context.Canceled, ReplyEmpty: true, RoundCeilingHit: true},
			TerminatedByBlocked,
		},
		{
			"rate-limit denial is blocked",
			TraceRecord{RateLimit: RateLimitTrace{Checked: true, Allowed: false}},
			FinishSignals{},
			TerminatedByBlocked,
		},
		{
			// cancel + timeout-ish + ceiling all signalled → user_cancel wins.
			"user_cancel beats timeout/budget",
			TraceRecord{},
			FinishSignals{ChatErr: context.Canceled, RoundCeilingHit: true, ReplyEmpty: true},
			TerminatedByUserCancel,
		},
		{
			"timeout beats budget/error",
			TraceRecord{},
			FinishSignals{ChatErr: context.DeadlineExceeded, RoundCeilingHit: true},
			TerminatedByTimeout,
		},
		{
			"token-budget hard-block is budget, not blocked",
			TraceRecord{EngineHardBlock: EngineHardBlockTrace{Hit: true, Category: HardBlockCategoryTokenBudget}},
			FinishSignals{},
			TerminatedByBudget,
		},
		{
			"round ceiling is budget",
			TraceRecord{},
			FinishSignals{RoundCeilingHit: true},
			TerminatedByBudget,
		},
		{
			"budget beats error",
			TraceRecord{},
			FinishSignals{ChatErr: someErr, RoundCeilingHit: true},
			TerminatedByBudget,
		},
		{
			"chat_error hard-block does not block; error terminus",
			TraceRecord{EngineHardBlock: EngineHardBlockTrace{Hit: true, Category: HardBlockCategoryChatError}},
			FinishSignals{ChatErr: someErr, ReplyEmpty: true},
			TerminatedByError,
		},
		{
			"error beats empty_reply",
			TraceRecord{},
			FinishSignals{ChatErr: someErr, ReplyEmpty: true},
			TerminatedByError,
		},
		{
			"empty_reply beats done",
			TraceRecord{},
			FinishSignals{ReplyEmpty: true},
			TerminatedByEmptyReply,
		},
		{
			"clean turn is done",
			TraceRecord{},
			FinishSignals{},
			TerminatedByDone,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.rec.DeriveTerminatedBy(c.sig); got != c.want {
				t.Fatalf("DeriveTerminatedBy = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDeriveAbortCause(t *testing.T) {
	rec := TraceRecord{}
	cases := []struct {
		terminatedBy string
		want         string
	}{
		{TerminatedByUserCancel, AbortCauseClientDisconnect},
		{TerminatedByEmptyReply, AbortCauseLLMEmptyStream},
		{TerminatedByDone, ""},
		{TerminatedByError, ""},
		{TerminatedByBlocked, ""},
		{TerminatedByBudget, ""},
	}
	for _, c := range cases {
		t.Run(c.terminatedBy, func(t *testing.T) {
			if got := rec.DeriveAbortCause(FinishSignals{}, c.terminatedBy); got != c.want {
				t.Fatalf("DeriveAbortCause(%s) = %q, want %q", c.terminatedBy, got, c.want)
			}
		})
	}
}

func TestDeriveErrorClass(t *testing.T) {
	rec := TraceRecord{}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil → empty", nil, ""},
		{"canceled", context.Canceled, ErrorClassContextCanceled},
		{"deadline", context.DeadlineExceeded, ErrorClassTimeout},
		{"no rows", sql.ErrNoRows, ErrorClassNotFound},
		{"wrapped no rows", fmt.Errorf("query: %w", sql.ErrNoRows), ErrorClassNotFound},
		{"generic", errors.New("upstream 500"), ErrorClassModelError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rec.DeriveErrorClass(FinishSignals{ChatErr: c.err}); got != c.want {
				t.Fatalf("DeriveErrorClass = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDeriveResolution(t *testing.T) {
	cases := []struct {
		name         string
		rec          TraceRecord
		terminatedBy string
		want         string
	}{
		{"blocked → blocked", TraceRecord{}, TerminatedByBlocked, ResolutionBlocked},
		{"refusal → refused", TraceRecord{Retrieval: RetrievalTrace{RefusedReason: "no_evidence"}}, TerminatedByDone, ResolutionRefused},
		{"done, no refusal → empty (judge decides)", TraceRecord{}, TerminatedByDone, ""},
		{"error → empty (judge decides)", TraceRecord{}, TerminatedByError, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.rec.DeriveResolution(FinishSignals{}, c.terminatedBy); got != c.want {
				t.Fatalf("DeriveResolution = %q, want %q", got, c.want)
			}
		})
	}
}

// TestFinalizeOutcome_StampsAllAxes is the integration check: one call stamps the
// four axes plus react_rounds / budget_hit consistently.
func TestFinalizeOutcome_StampsAllAxes(t *testing.T) {
	rec := TraceRecord{}
	rec.FinalizeOutcome(FinishSignals{ReplyEmpty: true, ReactRounds: 4})
	if rec.Outcome.TerminatedBy != TerminatedByEmptyReply {
		t.Fatalf("terminated_by = %q, want empty_reply", rec.Outcome.TerminatedBy)
	}
	if rec.Outcome.AbortCause != AbortCauseLLMEmptyStream {
		t.Fatalf("abort_cause = %q, want llm_empty_stream", rec.Outcome.AbortCause)
	}
	if rec.Outcome.ErrorClass != "" {
		t.Fatalf("error_class = %q, want empty (no chatErr)", rec.Outcome.ErrorClass)
	}
	if rec.Outcome.ReactRounds != 4 {
		t.Fatalf("react_rounds = %d, want 4", rec.Outcome.ReactRounds)
	}
	if rec.Outcome.BudgetHit {
		t.Fatalf("budget_hit = true, want false (no budget/ceiling)")
	}
}

// TestFinalizeOutcome_StampsDisposition pins the resolver disposition
// (action_proposal_disposition) onto the persisted trace, so a restart can
// reconstruct why a create did or did not card without re-running the model. It
// stays omitted when unset (SHA-stable for turns without it).
func TestFinalizeOutcome_StampsDisposition(t *testing.T) {
	rec := TraceRecord{}
	rec.FinalizeOutcome(FinishSignals{
		ActionProposalDisposition: "rejected:Zone=invalid_value",
	})
	if rec.Outcome.ActionProposalDisposition != "rejected:Zone=invalid_value" {
		t.Errorf("action_proposal_disposition = %q, want rejected:Zone=invalid_value", rec.Outcome.ActionProposalDisposition)
	}

	none := TraceRecord{}
	none.FinalizeOutcome(FinishSignals{ReactRounds: 1})
	if none.Outcome.ActionProposalDisposition != "" {
		t.Errorf("disposition must stay empty when unset; got %q", none.Outcome.ActionProposalDisposition)
	}
}

func TestFinalizeOutcome_RoundCeilingBudget(t *testing.T) {
	rec := TraceRecord{}
	rec.FinalizeOutcome(FinishSignals{RoundCeilingHit: true, ReactRounds: 10})
	if rec.Outcome.TerminatedBy != TerminatedByBudget {
		t.Fatalf("terminated_by = %q, want budget", rec.Outcome.TerminatedBy)
	}
	if !rec.Outcome.BudgetHit {
		t.Fatalf("budget_hit = false, want true")
	}
	if rec.Outcome.ReactRounds != 10 {
		t.Fatalf("react_rounds = %d, want 10", rec.Outcome.ReactRounds)
	}
}

func TestFinalizeOutcome_IntermediateRateLimitDoesNotOverrideSuccessfulCompletion(t *testing.T) {
	rec := TraceRecord{
		RateLimit: RateLimitTrace{Checked: true, Allowed: false, Action: "grounded_renderer"},
		Completion: TurnCompletionTrace{
			Class:  CompletionClassDeterministicAnswer,
			Reason: CompletionReasonDirectDispatch,
		},
	}
	rec.FinalizeOutcome(FinishSignals{})
	if rec.Outcome.TerminatedBy != TerminatedByDone {
		t.Fatalf("terminated_by = %q, want done after successful fallback", rec.Outcome.TerminatedBy)
	}
}

func TestFinalizeOutcome_TerminalRateLimitUsesCompletion(t *testing.T) {
	rec := TraceRecord{
		RateLimit: RateLimitTrace{Checked: true, Allowed: false, Action: "main_react_chat"},
		Completion: TurnCompletionTrace{
			Class:  CompletionClassSafetyBlock,
			Reason: CompletionReasonRateLimit,
		},
	}
	rec.FinalizeOutcome(FinishSignals{})
	if rec.Outcome.TerminatedBy != TerminatedByBlocked {
		t.Fatalf("terminated_by = %q, want blocked for terminal denial", rec.Outcome.TerminatedBy)
	}
}

// TestFinalizeOutcome_EmptyOnCleanFixtureStaysSHAStable verifies a clean,
// non-finalized record marshals without any outcome block (omitempty), so adding
// these fields does not change byte output for records that carry none of them.
func TestFinalizeOutcome_ZeroValueIsOmitted(t *testing.T) {
	if traceOutcomeObserved(OutcomeTrace{}) {
		t.Fatal("a zero OutcomeTrace must not be observed (would break SHA stability)")
	}
	// done is the minimum a finalized turn carries → it MUST be observed/emitted.
	if !traceOutcomeObserved(OutcomeTrace{TerminatedBy: TerminatedByDone}) {
		t.Fatal("a finalized done turn must be observed so terminated_by is emitted")
	}
}

// TestStatusFromTrace_TerminatedBy covers the post-FinalizeOutcome mapping into
// the 3-value status ENUM, including the two bugs this fixes: a chat error
// (previously "blocked" via the synthetic hard-block) and an empty reply
// (previously "success") both now report "error".
func TestStatusFromTrace_TerminatedBy(t *testing.T) {
	withTB := func(tb string) TraceRecord {
		r := TraceRecord{}
		r.Outcome.TerminatedBy = tb
		return r
	}
	cases := []struct {
		tb   string
		want string
	}{
		{TerminatedByDone, "success"},
		{TerminatedByBlocked, "blocked"},
		{TerminatedByBudget, "blocked"}, // engine-imposed cap; token-budget kept its prior "blocked"
		{TerminatedByError, "error"},
		{TerminatedByEmptyReply, "error"}, // was "success" — the dark hole
		{TerminatedByTimeout, "error"},
		{TerminatedByUserCancel, "error"},
	}
	for _, c := range cases {
		t.Run(c.tb, func(t *testing.T) {
			if got := statusFromTrace(withTB(c.tb)); got != c.want {
				t.Fatalf("statusFromTrace(terminated_by=%s) = %q, want %q", c.tb, got, c.want)
			}
		})
	}

	// A chat-error turn carries BOTH the synthetic chat_error hard-block AND
	// terminated_by=error; it must report "error", not "blocked".
	chatErrRec := TraceRecord{
		EngineHardBlock: EngineHardBlockTrace{Hit: true, Category: HardBlockCategoryChatError},
	}
	chatErrRec.Outcome.TerminatedBy = TerminatedByError
	if got := statusFromTrace(chatErrRec); got != "error" {
		t.Fatalf("chat-error turn status = %q, want error (not blocked)", got)
	}
}

func TestRetentionCutoff(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	if got := retentionCutoff(now, 30); !got.Equal(now.AddDate(0, 0, -30)) {
		t.Fatalf("retentionCutoff(30) = %s, want %s", got, now.AddDate(0, 0, -30))
	}
	// Non-positive retention falls back to the default, never "delete everything".
	if got := retentionCutoff(now, 0); !got.Equal(now.AddDate(0, 0, -DefaultTraceRetentionDays)) {
		t.Fatalf("retentionCutoff(0) = %s, want default window", got)
	}
}
