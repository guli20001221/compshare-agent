package renderer

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/compshare-agent/internal/envelope"
)

var (
	uhostTokenPattern         = regexp.MustCompile(`uhost-[A-Za-z0-9_-]+`)
	instanceLikeTokenPattern  = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9]*(?:[-_][A-Za-z0-9]+)+\b`)
	monitorNumberClaimPattern = regexp.MustCompile(`百分之\s*\d+(?:\.\d+)?|\d+(?:\.\d+)?\s*[%％]?`)
	resourceCountPatterns     = []*regexp.Regexp{
		regexp.MustCompile(`共\s*(\d+)\s*(?:个|台)?(?:实例|机器)`),
		regexp.MustCompile(`(\d+)\s*个[^，。\n]{0,24}(?:实例|机器)`),
	}
	accountBillClaimPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(账号|账户).{0,10}(余额|总账单|账单|流水|交易|消费|费用|扣费|花费)`),
		regexp.MustCompile(`(?i)(账号|账户).{0,12}(还剩|还有|用了|用掉|花了|花费|消费|扣费|多少钱|多少元|多少)`),
		regexp.MustCompile(`(?i)(余额|balance|account balance|monthly bill|transaction flow|transactions)`),
		regexp.MustCompile(`(?i)(本月|当月|月度).{0,12}(总账单|账单|消费|费用|扣费|扣了|花费|花了|多少钱|多少)`),
		regexp.MustCompile(`(?i)(总账单|消费流水|交易流水|账户流水|账号流水)`),
	}
)

var monitorMetricAliases = map[string][]string{
	"cpu":    {"cpu", "处理器"},
	"gpu":    {"gpu", "显卡"},
	"vram":   {"vram", "显存"},
	"memory": {"memory", "内存"},
}

func ValidateRenderedText(env envelope.Envelope, text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("rendered text is empty")
	}
	if containsInternalEnvelopeWording(text) {
		return fmt.Errorf("rendered text contains internal envelope wording")
	}
	for _, token := range uhostTokenPattern.FindAllString(text, -1) {
		if _, ok := envelope.AllowedIDs(env)[token]; !ok {
			return fmt.Errorf("rendered text contains unknown instance id %q", token)
		}
	}
	if err := validateKnownNames(env, text); err != nil {
		return err
	}
	if err := validateResourceInfoClaims(env, text); err != nil {
		return err
	}
	if env.Constraints.DoNotAnswerAccountBill {
		for _, pattern := range accountBillClaimPatterns {
			if pattern.MatchString(text) {
				return fmt.Errorf("rendered text contains account billing claim")
			}
		}
	}
	if err := validateMonitorClaims(env, text); err != nil {
		return err
	}
	if err := validateMissingMonitorMetricClaims(env, text); err != nil {
		return err
	}
	return nil
}

func containsInternalEnvelopeWording(text string) bool {
	lower := strings.ToLower(text)
	if strings.Contains(text, "信封") {
		return true
	}
	for _, term := range []string{"envelope", "computed.answer_mode"} {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

func validateResourceInfoClaims(env envelope.Envelope, text string) error {
	if env.Kind != envelope.KindResourceInfo {
		return nil
	}
	if len(env.Subjects) > 1 {
		for _, subject := range env.Subjects {
			if subject.ID == "" {
				continue
			}
			if !strings.Contains(text, subject.ID) {
				return fmt.Errorf("resource_info rendered text omitted instance id %q", subject.ID)
			}
		}
	}
	expected, ok := resourceExpectedListCount(env)
	if !ok {
		return nil
	}
	for _, pattern := range resourceCountPatterns {
		for _, match := range pattern.FindAllStringSubmatch(text, -1) {
			if len(match) < 2 {
				continue
			}
			actual, err := strconv.Atoi(match[1])
			if err != nil {
				continue
			}
			if actual != expected {
				return fmt.Errorf("resource_info rendered count %d does not match envelope count %d", actual, expected)
			}
		}
	}
	return nil
}

func resourceExpectedListCount(env envelope.Envelope) (int, bool) {
	if matched, ok := computedInt(env, "matched_count"); ok {
		return matched, true
	}
	return computedInt(env, "total_count")
}

func computedInt(env envelope.Envelope, key string) (int, bool) {
	for _, fact := range env.Computed {
		if fact.Key != key {
			continue
		}
		switch typed := fact.Value.(type) {
		case int:
			return typed, true
		case int64:
			return int(typed), true
		case float64:
			return int(typed), true
		case string:
			value, err := strconv.Atoi(strings.TrimSpace(typed))
			return value, err == nil
		}
	}
	return 0, false
}

func validateKnownNames(env envelope.Envelope, text string) error {
	allowed := allowedInstanceLikeTokens(env)
	for _, token := range instanceLikeTokenPattern.FindAllString(text, -1) {
		if strings.HasPrefix(strings.ToLower(token), "uhost-") {
			continue
		}
		if _, ok := allowed[token]; !ok {
			return fmt.Errorf("rendered text contains unknown instance-like token %q", token)
		}
	}
	return nil
}

func allowedInstanceLikeTokens(env envelope.Envelope) map[string]struct{} {
	allowed := envelope.AllowedNames(env)
	for _, subject := range env.Subjects {
		if subject.ID != "" {
			allowed[subject.ID] = struct{}{}
		}
	}
	for _, fact := range append(append([]envelope.Fact{}, env.Facts...), env.Computed...) {
		if fact.Key != "" {
			allowed[fact.Key] = struct{}{}
		}
		if fact.Label != "" {
			allowed[fact.Label] = struct{}{}
		}
		if s, ok := fact.Value.(string); ok && s != "" {
			allowed[s] = struct{}{}
		}
	}
	return allowed
}

func validateMonitorClaims(env envelope.Envelope, text string) error {
	allowed := allowedMonitorPercents(env)
	previousClaimEnd := 0
	for _, span := range monitorNumberClaimPattern.FindAllStringIndex(text, -1) {
		claim := text[span[0]:span[1]]
		contextStart := monitorMetricContextStart(text, previousClaimEnd, span[0])
		if !isMonitorNumericClaim(env, text, contextStart, span[0], claim) {
			continue
		}
		normalized := normalizePercent(claim)
		if _, ok := allowed.Values[normalized]; !ok {
			return fmt.Errorf("rendered text contains ungrounded monitor percent %q", claim)
		}
		if !monitorClaimHasGroundedMetric(text, contextStart, span[0], claim, allowed) {
			return fmt.Errorf("rendered text contains monitor percent bound to wrong metric %q", claim)
		}
		previousClaimEnd = span[1]
	}
	return nil
}

func validateMissingMonitorMetricClaims(env envelope.Envelope, text string) error {
	if env.Kind != envelope.KindMonitorQuery {
		return nil
	}
	for _, fact := range append(append([]envelope.Fact{}, env.Facts...), env.Computed...) {
		if !isMissingMonitorMetricFact(fact) {
			continue
		}
		if !strings.Contains(text, fact.Label) || !containsMissingDataWording(text) {
			return fmt.Errorf("rendered text omitted missing monitor metric %q", fact.Label)
		}
	}
	return nil
}

func isMissingMonitorMetricFact(fact envelope.Fact) bool {
	value, ok := fact.Value.(string)
	return ok && value == "未返回数据" && strings.HasPrefix(fact.Key, "missing_")
}

func containsMissingDataWording(text string) bool {
	for _, token := range []string{"未返回", "没有返回", "暂无", "无数据", "未获取", "没返回"} {
		if strings.Contains(text, token) {
			return true
		}
	}
	return false
}

func isMonitorNumericClaim(env envelope.Envelope, text string, contextStart, claimStart int, claim string) bool {
	if contextStart < 0 || claimStart < contextStart || claimStart > len(text) {
		return false
	}
	window := strings.ToLower(text[contextStart:claimStart])
	if env.Kind == envelope.KindMonitorQuery && hasPercentSyntax(claim) {
		return true
	}
	if !windowMentionsMonitorMetric(window) {
		return false
	}
	if hasPercentSyntax(claim) {
		return true
	}
	return windowMentionsMonitorUsage(window)
}

func hasPercentSyntax(claim string) bool {
	return strings.Contains(claim, "%") ||
		strings.Contains(claim, "％") ||
		strings.Contains(claim, "百分之")
}

func windowMentionsMonitorMetric(window string) bool {
	for _, aliases := range monitorMetricAliases {
		for _, alias := range aliases {
			if strings.Contains(window, alias) {
				return true
			}
		}
	}
	return false
}

func windowMentionsMonitorUsage(window string) bool {
	for _, token := range []string{
		"使用率",
		"利用率",
		"占用",
		"占比",
		"负载",
		"usage",
		"utilization",
		"utilisation",
		"load",
	} {
		if strings.Contains(window, token) {
			return true
		}
	}
	return false
}

type monitorPercentAllowance struct {
	Values       map[string]struct{}
	ValueMetrics map[string]map[string]struct{}
}

func allowedMonitorPercents(env envelope.Envelope) monitorPercentAllowance {
	allowed := monitorPercentAllowance{
		Values:       map[string]struct{}{},
		ValueMetrics: map[string]map[string]struct{}{},
	}
	for _, fact := range append(append([]envelope.Fact{}, env.Facts...), env.Computed...) {
		if !isMonitorPercentFact(fact) {
			continue
		}
		for _, value := range percentCandidates(fact.Value) {
			if value != "" {
				allowed.Values[value] = struct{}{}
				if _, ok := allowed.ValueMetrics[value]; !ok {
					allowed.ValueMetrics[value] = map[string]struct{}{}
				}
				for _, token := range monitorMetricTokens(fact) {
					allowed.ValueMetrics[value][token] = struct{}{}
				}
			}
		}
	}
	return allowed
}

func percentCandidates(v any) []string {
	switch typed := v.(type) {
	case string:
		value := strings.TrimSpace(typed)
		if value == "" {
			return nil
		}
		if strings.Contains(value, "%") {
			return []string{normalizePercent(value)}
		}
		if _, err := strconv.ParseFloat(value, 64); err == nil {
			return []string{normalizePercent(value + "%")}
		}
	case float64:
		return []string{normalizePercent(strconv.FormatFloat(typed, 'f', -1, 64) + "%")}
	case float32:
		return []string{normalizePercent(strconv.FormatFloat(float64(typed), 'f', -1, 32) + "%")}
	case int:
		return []string{normalizePercent(strconv.Itoa(typed) + "%")}
	case int64:
		return []string{normalizePercent(strconv.FormatInt(typed, 10) + "%")}
	case jsonNumber:
		return percentCandidates(typed.String())
	}
	return nil
}

func isMonitorPercentFact(fact envelope.Fact) bool {
	if fact.Unit == "%" {
		return true
	}
	key := strings.ToLower(fact.Key)
	for _, token := range []string{"cpu", "gpu", "vram", "memory", "util", "usage", "used"} {
		if strings.Contains(key, token) {
			return true
		}
	}
	return false
}

func monitorMetricTokens(fact envelope.Fact) []string {
	var tokens []string
	text := strings.ToLower(fact.Key + " " + fact.Label)
	if strings.Contains(text, "cpu") || strings.Contains(text, "处理器") {
		addMonitorMetricAliases(&tokens, "cpu")
	}
	if strings.Contains(text, "gpu") || strings.Contains(text, "显卡") {
		addMonitorMetricAliases(&tokens, "gpu")
	}
	if strings.Contains(text, "vram") || strings.Contains(text, "显存") {
		addMonitorMetricAliases(&tokens, "vram")
	}
	if strings.Contains(text, "memory") || strings.Contains(text, "内存") {
		addMonitorMetricAliases(&tokens, "memory")
	}
	return tokens
}

func addMonitorMetricAliases(tokens *[]string, canonical string) {
	for _, alias := range monitorMetricAliases[canonical] {
		exists := false
		for _, existing := range *tokens {
			if existing == alias {
				exists = true
				break
			}
		}
		if !exists {
			*tokens = append(*tokens, alias)
		}
	}
}

func monitorClaimHasGroundedMetric(text string, contextStart, claimStart int, claim string, allowed monitorPercentAllowance) bool {
	metrics := allowed.ValueMetrics[normalizePercent(claim)]
	if len(metrics) == 0 {
		return true
	}
	if contextStart < 0 || claimStart < contextStart || claimStart > len(text) {
		return false
	}
	window := strings.ToLower(text[contextStart:claimStart])
	for _, aliases := range monitorMetricAliases {
		mentioned := false
		allowedForValue := false
		for _, alias := range aliases {
			if strings.Contains(window, alias) {
				mentioned = true
			}
			if _, ok := metrics[alias]; ok {
				allowedForValue = true
			}
		}
		if mentioned && !allowedForValue {
			return false
		}
	}
	for metric := range metrics {
		if strings.Contains(window, metric) {
			return true
		}
	}
	return false
}

func monitorMetricContextStart(text string, lowerBound, claimStart int) int {
	if lowerBound < 0 {
		lowerBound = 0
	}
	if lowerBound > claimStart {
		lowerBound = claimStart
	}
	start := lowerBound
	for offset, r := range text[lowerBound:claimStart] {
		if isMonitorMetricContextBoundary(r) {
			start = lowerBound + offset + utf8.RuneLen(r)
		}
	}
	return start
}

func isMonitorMetricContextBoundary(r rune) bool {
	switch r {
	case ',', '，', '、', ';', '；', '.', '。', '!', '！', '?', '？', '\n', '\r':
		return true
	default:
		return false
	}
}

type jsonNumber interface {
	String() string
}

func normalizePercent(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "％", "%")
	s = strings.TrimPrefix(s, "百分之")
	if !strings.HasSuffix(s, "%") {
		s += "%"
	}
	return s
}
