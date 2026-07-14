package engine

import (
	"regexp"
	"strconv"
	"strings"
)

// Quantities are checked locally after the semantic verifier. This is a small,
// high-precision backstop for the most damaging verifier mistake: approving a
// claim whose amount, duration, percentage, capacity or CUDA version differs
// from the evidence quote it names.
var groundingQuantityRE = regexp.MustCompile(`(?i)(?:cuda\s*v?\s*(\d+(?:\.\d+)?)|([$¥￥])\s*(\d+(?:\.\d+)?)|(\d+(?:\.\d+)?)\s*(%|％|元|人民币|rmb|cny|美元|usd|秒|secs?|seconds?|分钟|分|min|mins|minutes?|小时|hrs?|hours?|天|日|days?|周|weeks?|月|months?|年|years?|kb|mb|gb|tb|kib|mib|gib|tib|张|卡|台|核|次))`)

func groundingQuantitiesConsistent(answerQuote, evidenceQuote string) bool {
	answerQuantities := extractGroundingQuantities(answerQuote)
	if len(answerQuantities) == 0 {
		return true
	}
	evidenceQuantities := extractGroundingQuantities(evidenceQuote)
	available := make(map[string]int, len(evidenceQuantities))
	for _, quantity := range evidenceQuantities {
		available[quantity]++
	}
	for _, quantity := range answerQuantities {
		if available[quantity] == 0 {
			return false
		}
		available[quantity]--
	}
	return true
}

func extractGroundingQuantities(text string) []string {
	matches := groundingQuantityRE.FindAllStringSubmatch(strings.ToLower(text), -1)
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		switch {
		case match[1] != "":
			out = append(out, normalizeGroundingNumber(match[1])+"|cuda")
		case match[3] != "":
			out = append(out, normalizeGroundingNumber(match[3])+"|"+normalizeGroundingUnit(match[2]))
		case match[4] != "":
			out = append(out, normalizeGroundingNumber(match[4])+"|"+normalizeGroundingUnit(match[5]))
		}
	}
	return out
}

func normalizeGroundingNumber(raw string) string {
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return strings.TrimSpace(raw)
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func normalizeGroundingUnit(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "%", "％":
		return "%"
	case "$", "美元", "usd":
		return "usd"
	case "¥", "￥", "元", "人民币", "rmb", "cny":
		return "cny"
	case "秒", "sec", "secs", "second", "seconds":
		return "second"
	case "分钟", "分", "min", "mins", "minute", "minutes":
		return "minute"
	case "小时", "hr", "hrs", "hour", "hours":
		return "hour"
	case "天", "日", "day", "days":
		return "day"
	case "周", "week", "weeks":
		return "week"
	case "月", "month", "months":
		return "month"
	case "年", "year", "years":
		return "year"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}
