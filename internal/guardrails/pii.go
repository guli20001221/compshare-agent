// Package guardrails redacts personally-identifiable information from user
// inputs before they cross persistence boundaries (currently:
// agent_messages.user_message). Distinct from internal/sanitizer (which
// redacts tool responses) and internal/policy (which redacts internal
// case/staff leak markers in retrieval traces) — separate boundaries,
// separate vocabularies, separate failure modes.
//
// Routing-safe by construction: phone (11-digit 1[3-9]xxxxxxxxx) and
// bank-card (16-19 contiguous digits with Luhn) patterns cannot match
// GPU model numbers (4090/5090/A100, ≤5 chars), instance IDs
// (uhost-/uhostid-), zone codes (cn-wlcb-01), or IPv4 segments. ID-card
// pattern requires the YYYYMMDD birth-date substructure, ruling out
// 18-digit hex strings.
package guardrails

import (
	"regexp"
)

// User-facing placeholder values. The "[已脱敏:...]" prefix is consistent
// with internal/sanitizer's "[已设置]" / "[已获取...]" style — operators
// scanning agent_messages can grep for "[已脱敏:" to count redactions.
const (
	PhoneRedacted    = "[已脱敏:手机号]"
	IDCardRedacted   = "[已脱敏:身份证]"
	EmailRedacted    = "[已脱敏:邮箱]"
	BankCardRedacted = "[已脱敏:银行卡]"
)

var (
	// Chinese mainland mobile: 1[3-9] + 9 digits, word-boundary delimited
	// so adjacent ASCII chars (letters/digits) prevent the match but
	// adjacent CJK / punctuation / whitespace permit it. Using \b avoids
	// the boundary-consumption issue of capture-group boundaries (which
	// breaks back-to-back-with-separator matches like "1380...000和1390...000").
	phoneRegex = regexp.MustCompile(`\b1[3-9]\d{9}\b`)

	// 18-digit Chinese ID card: 6-digit region + YYYYMMDD birth + 3-digit
	// sequence + checksum (digit or X). YYYY restricted to 19xx/20xx and
	// MM/DD validated to real calendar ranges — these constraints exclude
	// arbitrary 18-digit numeric strings (e.g. UNIX-timestamp expansions).
	// Day range 01-31 is over-broad (Feb 30 not filtered); the regex is a
	// pre-filter, not a calendar validator.
	idCardRegex = regexp.MustCompile(
		`\b[1-9]\d{5}(?:19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx]\b`,
	)

	// RFC 5321-shaped email. Conservative enough to avoid catastrophic
	// backtracking but permissive enough to cover user-typed addresses.
	emailRegex = regexp.MustCompile(
		`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`,
	)

	// Bank-card candidate: 16-19 contiguous digits, word-boundary
	// delimited. Luhn validation in the redaction step prunes false
	// positives (trace IDs, accidental long digit runs). 16-19 is the
	// PAN-length window per ISO/IEC 7812; 6-digit error codes like
	// "226604" are well below the floor.
	bankCardRegex = regexp.MustCompile(`\b\d{16,19}\b`)
)

// RedactPII returns a copy of s with phone / ID-card / email / bank-card
// patterns replaced by friendly placeholders. Empty input returns empty.
//
// Order matters: email first (may contain digits that would otherwise
// trigger phone / bank-card scanning of the local-part), then ID card
// (more specific than phone), then phone, then bank-card with Luhn.
//
// Idempotent: re-running RedactPII on already-redacted output is a no-op
// because the replacement strings contain no digits + no '@'.
func RedactPII(s string) string {
	if s == "" {
		return s
	}
	out := s
	out = emailRegex.ReplaceAllString(out, EmailRedacted)
	out = idCardRegex.ReplaceAllString(out, IDCardRedacted)
	out = phoneRegex.ReplaceAllString(out, PhoneRedacted)
	out = bankCardRegex.ReplaceAllStringFunc(out, redactBankCardIfLuhn)
	return out
}

// redactBankCardIfLuhn is the bank-card replacement callback. The regex
// uses word boundaries so the full match IS the digit run (no boundary
// chars consumed). Luhn-valid runs are redacted; non-Luhn (random
// numeric trace IDs, accidental long digit runs) pass through.
func redactBankCardIfLuhn(match string) string {
	if !luhnValid(match) {
		return match
	}
	return BankCardRedacted
}

// luhnValid is the standard Luhn checksum. Iterating right-to-left:
// double every second digit, sum digit-of-doubled, total mod 10 == 0.
// Non-digit input (shouldn't happen given regex pre-filter) returns false.
func luhnValid(digits string) bool {
	if len(digits) == 0 {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		c := digits[i]
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}
