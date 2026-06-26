package tools

import (
	"fmt"
	"os"
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
// pinned to the upstream gateway errors/code.go (audited 2026-06-26).
var hintedCodes = []int{
	120, 150,
	210, 220, 230, 240, 280,
	520,
	8010, 8017, 8027, 8039, 8052, 8067, 8090, 8095, 8097, 8102, 8107, 8108, 8116, 8117,
	8226, 8314, 8315, 8333, 8350, 8351, 8357, 8360, 8366, 8367, 8372, 8374, 8401, 8421,
	8433, 8434, 8436, 8438, 8441, 8442, 8443, 8445, 8498, 8510, 8520, 8580,
	8903, 8905, 8917, 8918, 8919, 8957, 8964, 8968,
	226601, 226602, 226603, 226604, 226605, 226606, 226607, 226608, 226609, 226611, 226612, 226618, 226619, 226620,
}

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
	forbidden := []string{"RetCode=230", "RetCode", "not available", "CompShareImageId", "zone_id", "az_group"}
	for _, code := range hintedCodes {
		h := retCodeHint(code)
		for _, tok := range forbidden {
			if strings.Contains(h, tok) {
				t.Errorf("hint for %d contains forbidden token %q: %s", code, tok, h)
			}
		}
	}
}

func TestRetCodeHint_KeyCodeMeanings(t *testing.T) {
	cases := []struct {
		code       int
		substrings []string
	}{
		{230, []string{"可用区", "规格", "镜像"}},
		{520, []string{"余额不足"}},
		{8010, []string{"不是关机状态"}},
		{8090, []string{"价格查询失败"}},
		{8314, []string{"密码"}},
		{8315, []string{"系统盘容量不足"}},
		{8333, []string{"CPU", "内存"}},
		{8357, []string{"资源不足"}},
		{8442, []string{"不支持无卡启动"}},
		{8903, []string{"正在执行任务"}},
		{226603, []string{"镜像", "不支持该卡型"}},
		{226604, []string{"资源不足"}},
		{226619, []string{"操作过于频繁"}},
	}
	for _, c := range cases {
		h := retCodeHint(c.code)
		for _, want := range c.substrings {
			if !strings.Contains(h, want) {
				t.Errorf("hint for %d = %q, want substring %q", c.code, h, want)
			}
		}
	}
}

func TestRetCodeHint_AuditDateCommentMatches(t *testing.T) {
	src, err := os.ReadFile("upstream_error.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "audited 2026-06-26") {
		t.Fatal("upstream RetCode audit date comment must match the test pin: audited 2026-06-26")
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
