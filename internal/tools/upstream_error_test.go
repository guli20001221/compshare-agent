package tools

import (
	"fmt"
	"strings"
	"testing"
)

// TestUpstreamAPIError_ErrorStringByteIdentical is the coupling guard: the
// Error() string MUST stay byte-identical to the historical flat format, because
// engine.isImageUnavailableMessage keys off the "230"+"CompShareImageId"
// substrings and saga step wrappers embed it via %v. If this drifts, zone-image
// auto-recovery silently breaks.
func TestUpstreamAPIError_ErrorStringByteIdentical(t *testing.T) {
	cases := []struct {
		code int
		msg  string
	}{
		{230, "Params [CompShareImageId] not available"},
		{8433, "no available resource"},
		{17000, "some other error"},
	}
	for _, c := range cases {
		got := NewUpstreamAPIError(c.code, c.msg).Error()
		want := fmt.Sprintf("API error (RetCode=%d): %s", c.code, c.msg)
		if got != want {
			t.Fatalf("Error() drifted: got %q want %q", got, want)
		}
	}

	// The exact string the create-image recovery matches on must round-trip.
	const recoveryMatch = "API error (RetCode=230): Params [CompShareImageId] not available"
	if got := NewUpstreamAPIError(230, "Params [CompShareImageId] not available").Error(); got != recoveryMatch {
		t.Fatalf("recovery-coupling string drifted: %q", got)
	}
}

// hintedCodes are the upstream RetCodes that carry an actionable recovery hint,
// pinned to the upstream gateway errors/code.go (audited 2026-06-23): 230 params,
// 226604 real capacity, 226603 image-gpu incompat, 8433 generic service error.
var hintedCodes = []int{230, 226604, 226603, 8433}

func TestRetCodeHint_KnownAndUnknown(t *testing.T) {
	for _, code := range hintedCodes {
		if h := retCodeHint(code); h == "" {
			t.Errorf("expected a hint for RetCode %d", code)
		}
	}
	if h := retCodeHint(0); h != "" {
		t.Errorf("expected no hint for RetCode 0, got %q", h)
	}
	if h := retCodeHint(99999); h != "" {
		t.Errorf("expected no hint for unknown RetCode, got %q", h)
	}
}

// TestRetCodeHint_NoForbiddenTokens guards the reply_not_contains regression gate
// (eval/regression_6cat_cases.json): a hint is surfaced to BOTH the model (ReAct
// tool result) and the user (direct-dispatch reply via UserMessage), so it must
// never carry the raw upstream tokens or they could reach the reply.
func TestRetCodeHint_NoForbiddenTokens(t *testing.T) {
	forbidden := []string{"RetCode=230", "RetCode", "not available", "CompShareImageId"}
	for _, code := range hintedCodes {
		h := retCodeHint(code)
		for _, tok := range forbidden {
			if strings.Contains(h, tok) {
				t.Errorf("hint for %d contains forbidden token %q: %s", code, tok, h)
			}
		}
	}
}

// TestUpstreamAPIError_UserMessage is the direct-dispatch contract: UserMessage()
// returns the hint for a hinted code (so intent.failureAfterToolForError replies
// with it) and "" for an un-hinted code (so the caller falls back to the generic
// friendly reply rather than answering blank).
func TestUpstreamAPIError_UserMessage(t *testing.T) {
	if msg := NewUpstreamAPIError(230, "Params [Zone] not available").UserMessage(); msg == "" {
		t.Error("expected a user-facing message for a hinted code (230)")
	}
	if msg := NewUpstreamAPIError(226604, "out of resources").UserMessage(); msg == "" {
		t.Error("expected a user-facing message for a hinted code (226604)")
	}
	if msg := NewUpstreamAPIError(17000, "some unhinted error").UserMessage(); msg != "" {
		t.Errorf("expected empty user message for an un-hinted code, got %q", msg)
	}
}

func TestUpstreamAPIErrorFrom(t *testing.T) {
	base := NewUpstreamAPIError(230, "Params [CompShareImageId] not available")
	wrapped := fmt.Errorf("步骤「检查库存」执行失败: %w", base)
	got, ok := UpstreamAPIErrorFrom(wrapped)
	if !ok {
		t.Fatal("expected to extract UpstreamAPIError from wrapped error")
	}
	if got.Code != 230 || got.Hint == "" {
		t.Fatalf("unexpected extracted error: %+v", got)
	}
	if _, ok := UpstreamAPIErrorFrom(fmt.Errorf("plain error")); ok {
		t.Error("did not expect to extract UpstreamAPIError from a plain error")
	}
}
