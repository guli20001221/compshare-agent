package platform

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// matchUserTokensToAPINames returns the subset of API Names (preserving case)
// that the user mentioned anywhere in their question. The API name set is the
// matching vocabulary — no hand-maintained GPU dictionary required.
//
// Word boundaries are required on both sides so a shorter model name does not
// substring-match a longer one — e.g. "H20" must not match "H200 96G". Word
// chars are [0-9A-Za-z_]; CJK and space are non-word, so a name surrounded by
// space/Chinese matches as expected.
func MatchUserTokensToAPINames(userText string, apiNames []string) []string {
	if userText == "" || len(apiNames) == 0 {
		return nil
	}
	upper := strings.ToUpper(userText)
	matched := []string{}
	seen := map[string]struct{}{}
	for _, name := range apiNames {
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		if ContainsAsWord(upper, strings.ToUpper(name)) {
			matched = append(matched, name)
			seen[name] = struct{}{}
		}
	}
	if len(matched) == 0 {
		matched = MatchUserGPUVariantAliases(userText, apiNames)
	}
	return matched
}

func MatchUserGPUVariantAliases(userText string, apiNames []string) []string {
	if userText == "" || len(apiNames) == 0 {
		return nil
	}
	tokens := GpuLikeTokenRegex.FindAllString(userText, -1)
	if len(tokens) == 0 {
		return nil
	}
	seenTokens := map[string]struct{}{}
	matched := []string{}
	seenNames := map[string]struct{}{}
	for _, token := range tokens {
		token = strings.ToUpper(strings.TrimSpace(token))
		if token == "" {
			continue
		}
		if _, ok := seenTokens[token]; ok {
			continue
		}
		seenTokens[token] = struct{}{}
		for _, name := range apiNames {
			if name == "" {
				continue
			}
			if _, ok := seenNames[name]; ok {
				continue
			}
			upperName := strings.ToUpper(name)
			if !strings.HasPrefix(upperName, token) || len(upperName) == len(token) {
				continue
			}
			next := rune(upperName[len(token)])
			if !IsGPUVariantSuffixRune(next) {
				continue
			}
			matched = append(matched, name)
			seenNames[name] = struct{}{}
		}
	}
	return matched
}

func IsGPUVariantSuffixRune(r rune) bool {
	return r == '_' || (r >= 'A' && r <= 'Z')
}

// containsAsWord reports whether needle appears in haystack with word
// boundaries on both sides. A word char is [0-9A-Za-z_]; any other rune
// (including CJK, space, punctuation, start/end of string) counts as a
// boundary. Substring matches like "H20" inside "H200" return false.
func ContainsAsWord(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	from := 0
	for from <= len(haystack)-len(needle) {
		idx := strings.Index(haystack[from:], needle)
		if idx < 0 {
			return false
		}
		abs := from + idx
		if !IsWordCharBefore(haystack, abs) && !IsWordCharAfter(haystack, abs+len(needle)) {
			return true
		}
		from = abs + 1
	}
	return false
}

func IsWordCharBefore(s string, pos int) bool {
	if pos <= 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:pos])
	return IsWordRune(r)
}

func IsWordCharAfter(s string, pos int) bool {
	if pos >= len(s) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[pos:])
	return IsWordRune(r)
}

func IsWordRune(r rune) bool {
	return r == '_' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
}

var GpuMemoryHintRegex = regexp.MustCompile(`(?i)\b(\d{2,3})\s*(?:gb|g)\b`)
var GpuMemorySuffixRegex = regexp.MustCompile(`(?i)_(\d{2,3})g$`)

func MatchUserTextToInstanceTypeNames(userText string, items []any, includeFamilyMemoryVariants bool) []string {
	apiNames := CollectAPINamesFromInstanceTypes(items)
	matched := MatchUserTokensToAPINames(userText, apiNames)
	hints := ExtractGPUMemoryHints(userText)
	if len(hints) > 0 {
		if memoryMatched := MatchMemoryHintedInstanceTypeNames(hints, items, matched); len(memoryMatched) > 0 {
			return memoryMatched
		}
		if len(matched) > 0 {
			return nil
		}
	}
	if includeFamilyMemoryVariants {
		return ExpandMemoryVariantMatches(matched, apiNames)
	}
	return matched
}

func MatchMemoryHintedInstanceTypeNames(hints map[string]struct{}, items []any, matchedNames []string) []string {
	wantedBases := map[string]struct{}{}
	for _, name := range matchedNames {
		if name == "" {
			continue
		}
		wantedBases[name] = struct{}{}
		wantedBases[MemoryVariantBaseName(name)] = struct{}{}
	}

	out := []string{}
	seen := map[string]struct{}{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := SafeString(entry, "Name")
		if name == "" {
			continue
		}
		if len(wantedBases) > 0 {
			base := MemoryVariantBaseName(name)
			if _, ok := wantedBases[name]; !ok {
				if _, ok := wantedBases[base]; !ok {
					continue
				}
			}
		}
		if !MemoryHintMatchesInstanceType(hints, entry) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func ExtractGPUMemoryHints(userText string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, match := range GpuMemoryHintRegex.FindAllStringSubmatch(userText, -1) {
		if len(match) < 2 {
			continue
		}
		if normalized := NormalizeMemoryGB(match[1]); normalized != "" {
			out[normalized] = struct{}{}
		}
	}
	return out
}

func MemoryHintMatchesInstanceType(hints map[string]struct{}, entry map[string]any) bool {
	memory := NormalizeMemoryGB(NestedValue(entry, "GraphicsMemory"))
	if memory == "" {
		memory = ApiNameMemoryGB(SafeString(entry, "Name"))
	}
	if memory == "" {
		return false
	}
	_, ok := hints[memory]
	return ok
}

func NormalizeMemoryGB(value string) string {
	normalized := strings.ToUpper(strings.TrimSpace(value))
	normalized = strings.TrimSuffix(normalized, "GB")
	normalized = strings.TrimSuffix(normalized, "G")
	return strings.TrimSpace(normalized)
}

func ApiNameMemoryGB(name string) string {
	match := GpuMemorySuffixRegex.FindStringSubmatch(name)
	if len(match) < 2 {
		return ""
	}
	return NormalizeMemoryGB(match[1])
}

func MemoryVariantBaseName(name string) string {
	return GpuMemorySuffixRegex.ReplaceAllString(name, "")
}

func ExpandMemoryVariantMatches(matchedNames []string, apiNames []string) []string {
	if len(matchedNames) == 0 {
		return nil
	}
	wantedNames := map[string]struct{}{}
	wantedBases := map[string]struct{}{}
	for _, name := range matchedNames {
		if name == "" {
			continue
		}
		wantedNames[name] = struct{}{}
		wantedBases[MemoryVariantBaseName(name)] = struct{}{}
	}

	out := []string{}
	seen := map[string]struct{}{}
	for _, name := range apiNames {
		_, exact := wantedNames[name]
		_, variant := wantedBases[MemoryVariantBaseName(name)]
		if !exact && !(variant && ApiNameMemoryGB(name) != "") {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// collectAPINamesFromInstanceTypes returns the deduped set of "Name" fields
// from a DescribeAvailableCompShareInstanceTypes response.
func CollectAPINamesFromInstanceTypes(items []any) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := SafeString(entry, "Name")
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

var GpuLikeTokenRegex = regexp.MustCompile(`(?i)\b([a-z]{1,3}\d{2,4}[a-z0-9_]*|\d{4}(?:_\d+g)?)\b`)
