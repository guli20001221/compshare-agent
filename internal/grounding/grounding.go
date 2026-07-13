// Package grounding answers one question, for any intent, with no per-kind code:
//
//	does every checkable claim in this reply trace back to something a tool
//	actually returned during this turn?
//
// It exists because the two mechanisms that keep the fast path honest today do
// not generalise. Deterministic templates (RenderResourceSummary and the
// isFastTierEnvelope kinds) cannot fabricate only because no model writes them —
// they are also stiff. The grounded renderer's validator is fluent but its
// checks are hand-written per envelope kind (validateResourceInfoClaims,
// validateMonitorClaims, ...), so a thirteenth intent means a thirteenth
// validator, and an envelope built from arbitrary tool output would be checked
// by nothing at all.
//
// The bet here is that the agent loop's fabrications are overwhelmingly of two
// shapes — a number wearing a domain unit ("最大支持 10 卡", "FP16 82.6 TFLOPS",
// "128 核") and an entity ID ("uhost-xxxx") — and that both are checkable
// against the turn's raw tool results without knowing which tool ran or what it
// meant. Grounding is therefore literal, not semantic: a claim is grounded when
// its value appears in what some tool returned, not when it is "consistent
// with" it. Semantics is what the model already failed at; literal containment
// is what it cannot argue with.
//
// A claim this package cannot check is a claim it must not flag. Unit adjacency
// is the whole filter, and it is deliberately narrow (see checkableUnits):
// dates, ordinals, list indices and bare integers in prose are invisible to it
// by construction, because a validator that cries wolf on "第 2 台" gets turned
// off and then protects nothing.
//
// # What this does NOT catch, and why you must not read more into a clean result
//
// It catches INVENTED values — a figure or ID that appears nowhere in anything the
// tools returned. It does NOT catch MISATTRIBUTED ones. Facts is a flat set of
// values with no binding to the entity or the field they came from, so:
//
//	tools returned:  uhost-A (4090, 24GB) and uhost-B (3090, 12GB)
//	model writes:    "uhost-B 显存 24GB"          <- WRONG, and reported CLEAN
//
// The 24 exists in the session's evidence, so the check passes, even though it
// belongs to the other machine. Numbers of this shape (24/16/8/64) repeat across
// the whole GPU catalog, so the collision is routine rather than exotic. Closing
// it needs per-entity fact scoping — harvesting {UHostId -> its own values} and
// checking a claim against the instance the sentence is actually about — which is
// a real change, not a tweak, and is deliberately NOT attempted here.
//
// Stated honestly, then: a clean result means "nothing here was conjured out of
// nothing." It does not mean "this is true." Do not build a UI affordance, a
// metric, or an argument on the stronger reading.
package grounding

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

// Facts is the set of literal values the tools returned during one CONVERSATION.
// It is built by walking raw tool payloads without interpreting them, so a tool
// added tomorrow is covered today.
//
// Session-scoped, not turn-scoped, and that is load-bearing. A user who uploads a
// screenshot of their instance on turn 1 and asks "还是一样的问题" on turn 3 is owed
// an answer that still knows the instance; a turn-scoped fact set calls that
// carried-forward ID a fabrication and punishes the model for the one behaviour
// this design exists to buy. "The model did not invent this" is a claim about the
// conversation. (A fact that has gone STALE since it was fetched is a different
// bug, and not this guard's to catch.)
//
// Both members are sets, so a long session cannot grow them without bound: what
// accumulates is the distinct values seen, not one entry per tool call.
type Facts struct {
	// numbers holds every numeric leaf, canonicalised (see canonNum), plus every
	// number found lexically inside a string leaf — retrieved knowledge chunks
	// arrive as prose, and "最大支持 8 卡" in a chunk is a fact just as much as
	// {"GpuCount": 8} is.
	numbers map[string]struct{}
	// entities holds every platform ID seen in any string leaf, lowercased.
	entities map[string]struct{}
}

func NewFacts() *Facts {
	return &Facts{numbers: map[string]struct{}{}, entities: map[string]struct{}{}}
}

// Empty reports whether no tool returned anything this turn. Callers must treat
// an empty fact set as "cannot judge" and skip validation entirely: with nothing
// to check against, every claim looks unsupported, and a validator that fails
// closed on a turn where no tool ran would reject the model's own general
// knowledge — which is not what this guards.
func (f *Facts) Empty() bool { return len(f.numbers) == 0 && len(f.entities) == 0 }

// Dump exposes the harvested facts for offline measurement. Not used by the
// reply path.
func Dump(f *Facts) (numbers []string, entities []string) {
	if f == nil {
		return nil, nil
	}
	for n := range f.numbers {
		numbers = append(numbers, n)
	}
	for e := range f.entities {
		entities = append(entities, e)
	}
	return numbers, entities
}

// numberInText finds numbers embedded in prose, e.g. a retrieved chunk that
// reads "单机最多 8 卡" or a raw string field "24GB".
var numberInText = regexp.MustCompile(`\d+(?:\.\d+)?`)

// AddRaw walks a raw tool payload and harvests every scalar leaf. It is
// deliberately schema-blind: no action names, no field names, no per-kind cases.
//
// The fast cases below cover JSON-shaped payloads (external API results arrive as
// map[string]any from json.Unmarshal). They are NOT sufficient on their own: local
// tools hand back typed Go structs, and the original type switch walked straight
// past them. GetGPUSpecs returns map[string]any{"spec": knowledge.GPUSpec{FP16:82.6}}
// — the struct is a leaf to a type switch, so the 4090's real FP16 figure was never
// harvested, and a reply that correctly read it off the tool got reported as a
// fabrication. "Schema-blind" has to mean blind to the SHAPE too, not just to the
// field names, so anything the fast cases miss falls through to a reflective walk.
func (f *Facts) AddRaw(v any) {
	switch x := v.(type) {
	case map[string]any:
		for _, sub := range x {
			f.AddRaw(sub)
		}
	case []any:
		for _, sub := range x {
			f.AddRaw(sub)
		}
	case string:
		f.addText(x)
	case float64:
		f.addNum(strconv.FormatFloat(x, 'f', -1, 64))
	case int:
		f.addNum(strconv.Itoa(x))
	case int64:
		f.addNum(strconv.FormatInt(x, 10))
	case bool, nil:
		// nothing checkable
	default:
		f.addReflected(reflect.ValueOf(v), 0)
	}
}

// maxReflectDepth bounds the reflective walk. Tool payloads are shallow trees; this
// is a backstop against a pathological or cyclic value, not a real limit.
const maxReflectDepth = 12

// addReflected harvests scalar leaves from values the type switch cannot name —
// typed structs, named scalar types, pointers, and typed slices/maps.
func (f *Facts) addReflected(rv reflect.Value, depth int) {
	if depth > maxReflectDepth || !rv.IsValid() {
		return
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !rv.IsNil() {
			f.addReflected(rv.Elem(), depth+1)
		}
	case reflect.Struct:
		t := rv.Type()
		for i := 0; i < rv.NumField(); i++ {
			ft := t.Field(i)
			if ft.PkgPath != "" {
				continue // unexported: never serialized, so never shown to the model
			}
			// json:"-" is exported Go but absent from the payload the model saw. A
			// fact the model was NOT shown must never enter the bag: harvesting it
			// would let the validator certify an invented number as "grounded" —
			// the one failure a fabrication check cannot be allowed to have. No
			// struct reaching AddRaw carries such a field today; this is here so the
			// next one cannot quietly launder a claim.
			if name, _, _ := strings.Cut(ft.Tag.Get("json"), ","); name == "-" {
				continue
			}
			f.addReflected(rv.Field(i), depth+1)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			f.addReflected(rv.Index(i), depth+1)
		}
	case reflect.Map:
		for _, k := range rv.MapKeys() {
			f.addReflected(rv.MapIndex(k), depth+1)
		}
	case reflect.String:
		f.addText(rv.String())
	case reflect.Float32, reflect.Float64:
		f.addNum(strconv.FormatFloat(rv.Float(), 'f', -1, 64))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		f.addNum(strconv.FormatInt(rv.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		f.addNum(strconv.FormatUint(rv.Uint(), 10))
	}
}

// addText harvests the numbers and platform entity IDs embedded in a string leaf.
func (f *Facts) addText(s string) {
	for _, n := range numberInText.FindAllString(s, -1) {
		f.addNum(n)
	}
	for _, id := range entityRE.FindAllString(s, -1) {
		f.entities[strings.ToLower(id)] = struct{}{}
	}
}

func (f *Facts) addNum(s string) {
	if c, ok := canonNum(s); ok {
		f.numbers[c] = struct{}{}
	}
}

// canonNum collapses the spellings of one quantity to a single key so that 8,
// 8.0 and 08 are the same fact. Without this, {"GpuCount": 8.0} (JSON numbers
// decode to float64) would fail to ground the string "8 卡".
func canonNum(s string) (string, bool) {
	fv, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return "", false
	}
	return strconv.FormatFloat(fv, 'f', -1, 64), true
}

// checkableUnits is the closed list of units that make a number worth checking.
//
// Every entry here is a quantity a tool can return and a model can invent.
// Everything NOT here — 年/月/日/号/点/分 (dates and clocks), 台/个/次 (counts of
// things in a list the model can legitimately derive), 第 N (ordinals) — is
// outside the contract on purpose. Those are the false positives that would
// discredit the validator, and none of them is a shape the loop was observed to
// fabricate.
// Every unit here has been paid for. A validator's most expensive failure is being
// obviously, stupidly wrong about a sentence the reader can see is fine — that is how a
// guard gets switched off, and a guard that is off protects nothing. So a unit earns its
// place only if a wrong number wearing it would actually hurt, and if the unit does not
// also occur in ordinary Chinese.
//
// Removed after adversarial review found each one firing on a perfectly good reply:
//
//	张   "根据您上传的 2 张截图"        — measure word for screenshots. This product HAS a
//	                                   screenshot feature; 卡 already covers GPU counts.
//	g    "使用 5G 热点连接控制台失败"     — mobile network. Cost: we no longer check "940G",
//	                                   but gb/tb/mb cover the units the API actually emits.
//	w    "一年下来大概 10w 左右"         — 十万, not Watts. Watts is not a claim anyone acts on.
//	线程  "num_workers 设为 8 线程"      — generic tuning advice, not a claim about the account.
//	t    "2080Ti"                     — the card model, chopped into "2080 T".
var checkableUnits = []string{
	"卡", "核",
	"tflops", "tops", "gbps",
	"gb", "tb", "mb",
	"ghz", "mhz",
	"元", "块钱",
	"%",
}

// claimRE captures a number immediately followed by one of the checkable units.
// Adjacency is required: "8 卡" is a claim, "8" alone is not, and "2026 年" is
// not (年 is not a checkable unit).
var claimRE = func() *regexp.Regexp {
	esc := make([]string, 0, len(checkableUnits))
	for _, u := range checkableUnits {
		esc = append(esc, regexp.QuoteMeta(u))
	}
	// (?i) so GB/gb/Gb all match; \s* so "8卡" and "8 卡" are the same claim.
	return regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(` + strings.Join(esc, "|") + `)`)
}()

// unitEndsToken reports whether the matched unit really ends there. RE2 has no
// lookahead, so the check is done on the byte after the match: "24GB。" is a
// claim, "24GBps" is not the unit we think it is, and "2080Ti" is a card model,
// not 2080 of anything.
func unitEndsToken(s string, end int) bool {
	if end >= len(s) {
		return true
	}
	c := s[end]
	return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')
}

const (
	screenshotOpen  = "系统自动识别到以下内容"
	screenshotClose = "（以上为截图自动识别内容，到此结束）"
)

// AddScreenshotEvidence harvests the OCR block that WrapScreenshotContext fences
// into the user message, and nothing else from that message.
//
// The distinction is the point. A screenshot is a rendering of the platform's own
// UI: when the model reports "11GB 显存" after the user photographed a page saying
// 11GB, it has invented nothing, and flagging it would make the validator useless
// on every screenshot turn. The user's TYPED words are not evidence — they are a
// claim. Grounding in those would let the model launder a false premise ("4090 是
// 10 卡吧?" -> "是的，最大支持 10 卡") straight through the check that exists to
// catch exactly that sentence. So the fence is honoured strictly: what the OCR saw
// counts, what the user asserted does not.
func (f *Facts) AddScreenshotEvidence(userMsg string) {
	i := strings.Index(userMsg, screenshotOpen)
	if i < 0 {
		return
	}
	rest := userMsg[i+len(screenshotOpen):]
	if j := strings.Index(rest, screenshotClose); j >= 0 {
		rest = rest[:j]
	}
	f.AddRaw(rest)
}

// AddUserReferents harvests the platform IDs the user typed, and ONLY those — no
// numbers.
//
// The asymmetry is the whole idea. An entity ID is a REFERENT: when the user types
// "uhost-1exampleaa02" and the assistant answers "uhost-1exampleaa02 不存在", it has
// invented nothing — and note that this is exactly the case where no tool can ever
// ground the ID, because the machine does not exist. Refusing to let the model name
// what the user just named would flag the correct answer and nothing else.
//
// A number is a CLAIM. "4090 是 10 卡吧?" -> "是的，最大支持 10 卡" adds a false fact to
// the world on the user's say-so, and that is the exact sentence this package exists
// to catch. So the user's IDs count as established; the user's figures do not.
func (f *Facts) AddUserReferents(userMsg string) {
	for _, id := range entityRE.FindAllString(userMsg, -1) {
		f.entities[strings.ToLower(id)] = struct{}{}
	}
}

// entityRE matches the platform's ID shapes. A fabricated instance ID is the
// one hallucination that can send a user to operate on a machine that does not
// exist, so IDs are checked verbatim, not numerically.
var entityRE = regexp.MustCompile(`(?i)\b(uhost|cpod|cfs|img|disk)-[a-z0-9]{4,}\b`)

// A Violation is one claim the reply makes that no tool supports.
type Violation struct {
	Claim string // the exact substring of the reply, e.g. "10 卡"
	Kind  string // "number" | "entity"
}

func (v Violation) String() string { return fmt.Sprintf("%s(%s)", v.Claim, v.Kind) }

// Check returns the claims in reply that facts does not support.
//
// It never returns violations when facts is empty — see Facts.Empty. A caller
// that gets no violations has learned "nothing provably invented", which is not
// the same as "true"; this is a floor, not a proof.
func Check(reply string, facts *Facts) []Violation {
	if facts == nil || facts.Empty() || strings.TrimSpace(reply) == "" {
		return nil
	}
	var out []Violation
	seen := map[string]struct{}{}

	for _, loc := range claimRE.FindAllStringSubmatchIndex(reply, -1) {
		whole := reply[loc[0]:loc[1]]
		num := reply[loc[2]:loc[3]]
		if !unitEndsToken(reply, loc[1]) {
			continue
		}
		c, ok := canonNum(num)
		if !ok {
			continue
		}
		if _, grounded := facts.numbers[c]; grounded {
			continue
		}
		if _, dup := seen[whole]; dup {
			continue
		}
		seen[whole] = struct{}{}
		out = append(out, Violation{Claim: strings.TrimSpace(whole), Kind: "number"})
	}

	for _, id := range entityRE.FindAllString(reply, -1) {
		low := strings.ToLower(id)
		if _, dup := seen[low]; dup {
			continue
		}
		if _, grounded := facts.entities[low]; grounded {
			continue
		}
		seen[low] = struct{}{}
		out = append(out, Violation{Claim: id, Kind: "entity"})
	}
	return out
}
