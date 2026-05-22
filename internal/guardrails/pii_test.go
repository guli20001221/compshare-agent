package guardrails

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRedactPII_Phone covers Chinese-mainland mobile redaction with
// boundary detection so model numbers / instance IDs / trace IDs don't
// trip the matcher.
func TestRedactPII_Phone(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare", "13800138000", PhoneRedacted},
		{"中文夹带", "我的手机号是13800138000请回电", "我的手机号是" + PhoneRedacted + "请回电"},
		{"两个手机号", "13800138000和13900139000", PhoneRedacted + "和" + PhoneRedacted},
		{"行首+空格", "13800138000 hello", PhoneRedacted + " hello"},
		{"开头9不算手机", "12800138000 12500138000", "12800138000 12500138000"},
		{"长度不对不算", "138001380001234", "138001380001234"},
		{"短不算", "1380013800", "1380013800"},
		{"嵌入更多数字不算", "abc138001380001 def", "abc138001380001 def"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RedactPII(tc.in))
		})
	}
}

// TestRedactPII_PhoneDoesNotMatchGPUModel pins the routing-safety
// invariant: GPU model numbers + zone codes + IPv4 + instance IDs MUST
// pass through unchanged or PII redaction breaks the actual routing
// signal user-input carries.
func TestRedactPII_PhoneDoesNotMatchGPUModel(t *testing.T) {
	routingSignals := []string{
		"4090",             // GPU
		"5090",             // GPU
		"A100",             // GPU
		"H100",             // GPU
		"H200",             // GPU
		"V100",             // GPU
		"A10",              // GPU
		"4090D",            // GPU variant
		"uhost-abc123",     // instance ID
		"uhostid-deadbeef", // instance ID
		"cn-wlcb-01",       // zone
		"cn-shanghai-02",   // zone
		"192.168.1.1",      // private IPv4
		"10.0.0.1",         // private IPv4
		"226604",           // common error code
		"req-uuid-1234",    // request UUID prefix
		"4090 多少钱一小时",      // pricing question
		"上海机房 A100 显存多大",   // GPU spec question
	}
	for _, s := range routingSignals {
		t.Run(s, func(t *testing.T) {
			assert.Equal(t, s, RedactPII(s),
				"routing signal %q must not be touched by PII redaction", s)
		})
	}
}

// TestRedactPII_IDCard covers 18-digit ID with the YYYYMMDD birthdate
// substructure. The structure is what distinguishes a real ID card from
// a random 18-digit number — pattern only matches valid month/day +
// 19xx/20xx birth century, so trace IDs / hex strings rarely collide.
func TestRedactPII_IDCard(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare", "110101199003078888", IDCardRedacted},
		{"末位 X", "11010119900307888X", IDCardRedacted},
		{"末位 x", "11010119900307888x", IDCardRedacted},
		{"中文夹带", "我的身份证是110101199003078888,请核对", "我的身份证是" + IDCardRedacted + ",请核对"},
		{"18 位但月份非法", "110101199013078888", "110101199013078888"},
		// Day-32 is structurally invalid in the regex (day group 01-31);
		// Feb-30 is NOT filtered (regex is a pre-filter, not a calendar
		// validator) — fine for our purpose since real cards never have
		// Feb-30 birthdays.
		{"18 位但日期非法", "110101199002328888", "110101199002328888"},
		{"18 位但出生年非法", "110101180003078888", "110101180003078888"},
		// 17-digit non-Luhn so bank-card pattern (16-19 digits) also
		// passes through. A 17-digit Luhn-valid sequence WOULD be
		// redacted as a bank card — that's intentional.
		{"17 位非 Luhn", "11010119900307889", "11010119900307889"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RedactPII(tc.in))
		})
	}
}

// TestRedactPII_Email covers standard user-typed addresses. Domain TLD
// minimum 2 chars excludes "user@host" (no TLD) — operators / dev
// environments occasionally have such addresses but routing-relevant
// content like "uhost-abc@cn-wlcb-01" lacks a dot-separated TLD.
func TestRedactPII_Email(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare", "user@example.com", EmailRedacted},
		{"带数字 local part", "user123@example.com", EmailRedacted},
		{"带点+加号", "user.name+tag@example.co.uk", EmailRedacted},
		{"中文夹带", "联系我 user@example.com 谢谢", "联系我 " + EmailRedacted + " 谢谢"},
		{"无 TLD 不算", "user@host", "user@host"},
		{"无 @ 不算", "user.example.com", "user.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RedactPII(tc.in))
		})
	}
}

// TestRedactPII_BankCardWithLuhn pins that bank-card redaction requires
// BOTH the 16-19 digit shape AND a valid Luhn checksum. This is what
// keeps trace IDs / long error codes / accidental digit runs out of the
// redaction set.
func TestRedactPII_BankCardWithLuhn(t *testing.T) {
	// 4532015112830366 is a textbook Luhn-valid 16-digit test card.
	// 5500000000000004 is a textbook Luhn-valid 16-digit Mastercard test.
	luhnValid16 := "4532015112830366"
	luhnValid19 := "1094532015112830366" // Luhn-valid 19-digit (prefix-extension of luhnValid16)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"Luhn-valid 16", luhnValid16, BankCardRedacted},
		{"Luhn-valid 19", luhnValid19, BankCardRedacted},
		{"Luhn-valid 中文夹带", "我的卡号" + luhnValid16 + "请记下", "我的卡号" + BankCardRedacted + "请记下"},
		// Random 16-digit hex-as-decimal that fails Luhn — must pass through.
		{"非 Luhn 16 位", "1234567890123456", "1234567890123456"},
		{"非 Luhn 19 位", "1234567890123456789", "1234567890123456789"},
		// Trace ID / error code shape — must pass through.
		{"6 位错误码", "226604", "226604"},
		{"15 位不到 16 不算", "123456789012345", "123456789012345"},
		{"20 位超 19 不算", "12345678901234567890", "12345678901234567890"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RedactPII(tc.in))
		})
	}
}

// TestRedactPII_Composite covers the realistic case where one message
// contains multiple PII types — each must redact independently.
func TestRedactPII_Composite(t *testing.T) {
	in := "我叫张三,手机 13800138000,邮箱 user@example.com,身份证 110101199003078888,卡号 4532015112830366,实例 uhost-abc123 在 cn-wlcb-01 跑 4090"
	got := RedactPII(in)

	assert.Contains(t, got, PhoneRedacted)
	assert.Contains(t, got, EmailRedacted)
	assert.Contains(t, got, IDCardRedacted)
	assert.Contains(t, got, BankCardRedacted)

	// Routing-relevant tokens preserved.
	assert.Contains(t, got, "uhost-abc123", "instance ID must survive PII redaction")
	assert.Contains(t, got, "cn-wlcb-01", "zone code must survive PII redaction")
	assert.Contains(t, got, "4090", "GPU model must survive PII redaction")

	// Original PII tokens removed.
	assert.NotContains(t, got, "13800138000")
	assert.NotContains(t, got, "user@example.com")
	assert.NotContains(t, got, "110101199003078888")
	assert.NotContains(t, got, "4532015112830366")
}

// TestRedactPII_Idempotent — re-running RedactPII on already-redacted
// output must be a no-op. The placeholder text contains "[已脱敏:...]"
// with no digits or '@', so none of the matchers will fire on it.
func TestRedactPII_Idempotent(t *testing.T) {
	in := "手机 13800138000 邮箱 user@example.com"
	once := RedactPII(in)
	twice := RedactPII(once)
	assert.Equal(t, once, twice, "RedactPII must be idempotent")
}

// TestRedactPII_Empty — empty input returns empty (not the placeholder).
func TestRedactPII_Empty(t *testing.T) {
	assert.Equal(t, "", RedactPII(""))
}

// TestRedactPII_NoMatchUnchanged — input with no PII returns unchanged.
// Important for the no-cost happy path: 99% of user messages have no PII.
func TestRedactPII_NoMatchUnchanged(t *testing.T) {
	inputs := []string{
		"4090 多少钱一小时",
		"A100 显存多大",
		"推荐用什么 GPU 跑 LoRA",
		"为什么我的实例 uhost-abc123 连不上 SSH",
		"上海机房有 H100 库存吗",
		"账户余额怎么查",
		"",
	}
	for _, s := range inputs {
		assert.Equal(t, s, RedactPII(s))
	}
}

// TestLuhn covers the checksum itself. Standard test vectors.
func TestLuhn(t *testing.T) {
	assert.True(t, luhnValid("4532015112830366"))
	assert.True(t, luhnValid("5500000000000004"))
	assert.True(t, luhnValid("79927398713")) // Wikipedia example
	assert.False(t, luhnValid("1234567890123456"))
	assert.False(t, luhnValid("79927398714"))
	assert.False(t, luhnValid(""))
	assert.False(t, luhnValid("abcd1234"))
}
