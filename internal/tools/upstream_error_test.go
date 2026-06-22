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

func TestRetCodeHint_KnownAndUnknown(t *testing.T) {
	if h := retCodeHint(230); h == "" {
		t.Error("expected a hint for RetCode 230")
	}
	if h := retCodeHint(8433); h == "" {
		t.Error("expected a hint for RetCode 8433")
	}
	if h := retCodeHint(0); h != "" {
		t.Errorf("expected no hint for RetCode 0, got %q", h)
	}
	if h := retCodeHint(99999); h != "" {
		t.Errorf("expected no hint for unknown RetCode, got %q", h)
	}
}

// TestRetCodeHint_NoForbiddenTokens guards the reply_not_contains regression gate
// (eval/regression_6cat_cases.json): a hint surfaced to the model must never
// carry the raw upstream tokens, or the model could echo them into the reply.
func TestRetCodeHint_NoForbiddenTokens(t *testing.T) {
	forbidden := []string{"RetCode=230", "not available", "CompShareImageId"}
	for _, code := range []int{230, 8433} {
		h := retCodeHint(code)
		for _, tok := range forbidden {
			if strings.Contains(h, tok) {
				t.Errorf("hint for %d contains forbidden token %q: %s", code, tok, h)
			}
		}
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
