package workflow

import (
	"fmt"

	"github.com/compshare-agent/internal/deployment"
)

// StepType identifies the kind of workflow step.
type StepType int

// ReferenceData is the server-trusted, read-only reference data a workflow run
// consults but never seals. Each field is an explicit typed snapshot, not a
// catch-all map: reference data has a known shape, and a map would invite the
// same untyped scatter this convergence removes. It carries platform facts the
// user never confirmed (the live zone catalog), so it is deliberately absent
// from the seal — steps consult it, nothing mutates it.
type ReferenceData struct {
	// ZoneCatalog is the live availability-zone catalog for this turn, or nil on
	// paths with no catalog (CLI, or a failed fetch). Its accessors are nil-safe,
	// so a consumer reads it the same way whether or not it is present.
	ZoneCatalog *deployment.ZoneCatalogSnapshot
	// ImageCatalog is the live image catalog for this turn, resolved once by the
	// engine and shared with BOTH the action resolver (CodecImage) and the workflow
	// (selectCreateImage) — the single authority that ends the three-interpreter
	// image resolution. nil on paths that carry no image field; nil-safe accessors,
	// so a consumer reads it the same way whether or not it is present.
	ImageCatalog *deployment.ImageCatalogSnapshot
}

const (
	// StepToolCall executes an API tool via executor.
	StepToolCall StepType = iota
	// StepConfirm waits for user confirmation.
	StepConfirm
	// StepResolve computes derived values from facts earlier steps already
	// established. It calls no tool and no model, and it may not write Params:
	// its product is a CANDIDATE, and a candidate is not a decision the user has
	// agreed to. See Step.Resolve and Step.PromoteOnConfirm for the two halves of
	// that rule.
	StepResolve
)

// Step defines one step in a workflow.
type Step struct {
	Name      string
	Type      StepType
	Tool      string                      // API action name (for StepToolCall only)
	ToolFunc  func(wfCtx *Context) string // dynamic tool name (overrides Tool if set)
	BuildArgs func(wfCtx *Context) (map[string]any, error)
	// CheckResult inspects a SUCCESSFUL upstream response and decides whether the
	// workflow may continue. Rejecting here is how a workflow says "the call
	// worked and the answer is no" — the capacity gate's sold-out is the archetype.
	CheckResult func(wfCtx *Context, result map[string]any) CheckOutcome
	// Resolve computes this step's result from the Context (StepResolve only); the
	// result lands in StepResults exactly like a tool step's would.
	//
	// What is actually guaranteed, and what is not:
	//   - It is handed no runtime-provided tool, model or network dependency: no
	//     context.Context, no executor. That is the only thing the signature
	//     settles — a plain Go function can still reach a global, an HTTP client
	//     or another package, so "calls no tool" describes today's create
	//     Resolver (materializeCreateDraft computes locally and nothing else),
	//     not an invariant of the type.
	//   - runResolveStep rejects any Resolve that mutates Params.
	//   - It is NOT otherwise read-only. It holds the live *Context and could
	//     still write StepResults, InitialParams or Runtime. Nothing stops it.
	//
	// So "pure" is a property of each Resolve today, not of the type. Before
	// StepResolve is used outside the create draft, this should take a read-only
	// snapshot (deep-copied Params + StepResults, Runtime by value) and return its
	// result, rather than being handed the Context itself — then replayability
	// from a trace would follow from the signature instead of from review.
	Resolve func(wfCtx *Context) (map[string]any, error)
	// SkipIf lets adaptive workflows omit a step once earlier context has made
	// that choice unambiguous. nil preserves the legacy "always run" behavior.
	SkipIf func(wfCtx *Context) (bool, error)
	// Optional lets a post-success enrichment step fail without failing the
	// whole workflow. Default false preserves existing fail-stop behavior.
	Optional bool

	// --- Editable confirm form (StepConfirm only; all three nil/empty =
	// legacy boolean confirm, byte-identical). Consumed only by
	// workflow.Engine.Run when a ConfirmEditsFunc is wired (HTTP path with
	// COMPSHARE_CONFIRM_FORM on + client opt-in); the saga runner ignores
	// them, so deploy_model keeps the plain confirm. ---

	// BuildForm builds the editable selection form shown alongside the
	// confirm card. All option values are server-generated; user overrides
	// are validated against them (whitelist) before ApplyOverrides runs.
	BuildForm func(wfCtx *Context) (*ConfirmForm, error)
	// ApplyOverrides merges validated form overrides into wfCtx.Params.
	ApplyOverrides func(wfCtx *Context, overrides map[string]string) error
	// RevalidateFrom names the step this confirm re-runs FROM after
	// ApplyOverrides, before re-entering the gate. Every step from there up to
	// (not including) this confirm is discarded and re-run in DEFINITION order.
	//
	// It replaced a []string list of step names, which had two defects a boundary
	// does not have: the list was resolved through a StepToolCall-only lookup that
	// SILENTLY SKIPPED any name it could not find (a typo, a renamed step, or a
	// step of any other type simply did not re-run, and the user re-confirmed a
	// card built on stale results), and its order was the list's rather than the
	// workflow's. A boundary cannot drift out of order, and an unresolvable one
	// fail-stops the workflow instead of quietly doing less — see revalidateFrom.
	RevalidateFrom string
	// PromoteOnConfirm runs after this gate PASSES and before Run seals, to copy
	// what the user just approved into Params — the map seal() hashes.
	//
	// It exists because a StepResolve may not write Params (an earlier gate's seal
	// may still be live while it runs, and its output is a candidate nobody has
	// agreed to yet). This is the one sanctioned crossing from "computed" to
	// "confirmed", and it is placed exactly where that transition actually
	// happens. An error here fail-stops: failing to record what the user approved
	// must never degrade into executing something else.
	PromoteOnConfirm func(wfCtx *Context) error
	// ConfirmSubmitMode controls what happens after a form-bearing confirm is
	// submitted with Overrides. Empty preserves the legacy edit→revalidate→
	// re-confirm loop. ConfirmSubmitContinue applies the whitelisted selection
	// and advances to the next workflow step, used by guided multi-step forms.
	ConfirmSubmitMode ConfirmSubmitMode
}

// ConfirmSubmitMode controls StepConfirm form-submission behavior.
type ConfirmSubmitMode string

const (
	ConfirmSubmitRevalidate ConfirmSubmitMode = ""
	ConfirmSubmitContinue   ConfirmSubmitMode = "continue"
)

// Definition holds a complete workflow.
type Definition struct {
	Name        string
	Description string
	Steps       []Step
	ResultData  func(wfCtx *Context) map[string]any
	// FailureDraft returns the candidate this workflow was working from, encoded,
	// for the failure record. Like ResultData it is the definition's own way of
	// naming what only it understands — the engine never interprets what comes
	// back, it only carries it to a caller that does.
	//
	// nil for a workflow that resolves no candidate; its failures then carry the
	// failed step and its arguments and nothing more, which is honest.
	FailureDraft func(wfCtx *Context) map[string]any
	// GuidedIntake declares that this workflow offers a guided, multi-step
	// selection form for an incomplete-but-well-formed proposal (see
	// CreateInstanceGuidedDef) instead of a prose back-and-forth. The action
	// catalog reads this to surface the operation as IntakeGuided, so whether an
	// operation supports guided intake is a property the workflow declares here —
	// not a workflow-name switch in the catalog. Default false = prose-only intake.
	GuidedIntake bool
}

// FailureReason classifies a failure for callers that must DO something different
// about different failures, rather than merely retell them.
//
// It exists because the alternative is reading the prose. isCreateStockShortage
// tested strings.Contains(message, "库存不足") — so the sentence a user reads was
// also a control signal, and the two cannot both be free. Rewording the message
// changed behaviour; translating it would have removed it. The reason is now
// produced where the branch is actually taken, and the message is free to be a
// message again.
//
// The zero value classifies nothing, which is the right default: a failure with no
// declared reason must not accidentally match one.
type FailureReason string

const (
	// ReasonCapacitySoldOut: the requested spec exists, and upstream says there is
	// none of it right now. Alternatives are worth offering. This is NOT the same
	// as a spec that does not exist (a typo, a wrong combination) — those need a
	// different answer and must not share a reason.
	ReasonCapacitySoldOut FailureReason = "capacity_sold_out"
)

// CheckOutcome is a CheckResult's verdict on a successful upstream response.
//
// It is a struct rather than (bool, string) so a rejection can carry WHY without
// the reason having to be recovered from the prose afterwards. The message and the
// reason are produced together, at the branch that knows both — the alternative
// was a caller re-deriving the reason by matching text the workflow had already
// decided.
type CheckOutcome struct {
	OK      bool
	Message string
	// Reason is optional. Most rejections need only be explained, not classified;
	// only a caller that must ACT differently needs one, and inventing reasons for
	// rejections nobody branches on would be inventing a vocabulary.
	Reason FailureReason
}

// CheckPassed lets the workflow continue.
func CheckPassed() CheckOutcome { return CheckOutcome{OK: true} }

// CheckFailed stops the workflow with an explanation and no classification.
func CheckFailed(message string) CheckOutcome {
	return CheckOutcome{Message: message}
}

// CheckFailedBecause stops the workflow with an explanation AND a machine-readable
// reason, for the failures a caller has to act on.
func CheckFailedBecause(reason FailureReason, message string) CheckOutcome {
	return CheckOutcome{Message: message, Reason: reason}
}

// StepFailure is what the workflow knows about its own failure, recorded where
// the failure happened.
//
// It exists because the alternative is reconstruction, and reconstruction from
// "whichever params survived" gets it wrong in both directions. A caller had to
// read the spec out of top-level params: on the plain create those params are the
// user's original request, so a zone the resolver derived was simply not in them
// and the caller searched every zone; on the guided create they were whatever
// contract happened to be sealed, which between gates is a SELECTION card's seal
// and authorises nothing. Both readings are guesses about state the workflow
// itself knew exactly.
//
// The five fields are deliberately separate, because conflating them is the bug:
// what failed, what kind of failure it was, what it actually sent, what it was
// working from, and whether any of it was ever approved are five different
// questions.
type StepFailure struct {
	// Step is the step that stopped the workflow. It anchors the others: Args and
	// Draft are that step's, not the workflow's.
	Step string
	// Reason classifies the failure when the step that raised it said what kind it
	// was. Empty means unclassified, which is most failures — a caller that only
	// retells a failure needs no vocabulary for it.
	//
	// Do not infer one from Message. That is what this replaces.
	Reason FailureReason
	// Args are the arguments THAT step actually sent, copied rather than
	// referenced so the record cannot move afterwards. nil when the step built
	// none — it failed before BuildArgs, or it calls no tool at all.
	//
	// It is a record of the request, NOT a source for the decision behind it: the
	// two differ on purpose. ApplyCapacityPlacementArgs drops Zone/Region/az_group
	// for a pod zone, so a capacity request can carry no zone while the draft
	// behind it names one. Read Draft for what was decided; read Args for what was
	// asked.
	Args map[string]any
	// Draft is the candidate the failed step was working from, encoded, as the
	// definition's FailureDraft reported it. nil when the workflow resolves no
	// candidate or none existed yet.
	Draft map[string]any
	// ExecutionAuthorized reports whether a contract authorising this workflow's
	// MUTATING step existed when it failed — which is not the same question as
	// whether Contract is non-nil, and that difference is the whole reason this
	// field exists. It is named for the question it answers rather than for the
	// mechanism behind it: "Sealed" invited exactly the reading that a seal, any
	// seal, means the user agreed to the thing about to happen.
	//
	// The guided create seals after every one of its seven gates, so a failure at
	// 检查库存 leaves a perfectly real contract that authorised an image choice and
	// nothing else. Its Operation is "CreateInstanceWorkflow", exactly like a real
	// create authorisation's, so no reader can tell them apart by inspection.
	//
	// True only when every confirmation gate up to and including the failed step
	// has passed — see confirmGateUnpassed. While any gate has not, whatever is
	// sealed is a selection the user made along the way, not permission to execute.
	ExecutionAuthorized bool
}

// Context accumulates state during workflow execution.
type Context struct {
	// Params holds the mutable business parameters (the pre-seal draft). It is an
	// independent deep copy of the caller's map — a step mutating Params can never
	// reach the ResolvedAction.Arguments the engine still owns.
	Params        map[string]any
	InitialParams map[string]any
	StepResults   map[string]map[string]any
	// Runtime carries server-injected identity/trace lifted out of Params, so
	// business params (and the confirm form / sealed digest) never mix them in.
	Runtime RuntimeMetadata
	// referenceData holds server-trusted read-only snapshots (the turn's zone
	// catalog) attached via a RunOption. It is set once on the fresh context and
	// persists across confirm-form re-runs, yet — unlike Params — it never passes
	// through deepCopyParams and is never captured by seal(), so it can neither
	// alias into a selected placement nor enter the sealed contract.
	//
	// It is private on purpose: read-only is a guarantee, not a convention. Step
	// code reaches it only through ZoneCatalog(), so a step cannot swap the whole
	// reference out from under the run.
	referenceData ReferenceData
	// sealed is set once the user confirms: the immutable snapshot the mutating
	// step consumes. nil before confirmation.
	sealed *SealedActionContract
}

// NewContext creates a workflow context with the given initial parameters. The
// caller's map is never shared or mutated: params are deep-copied (nested maps
// and slices included) and the identity/trace keys are split into Runtime so the
// business Params are exactly what the user will confirm.
func NewContext(params map[string]any) *Context {
	business, runtime := splitRuntimeMetadata(deepCopyParams(params))
	if business == nil {
		business = make(map[string]any)
	}
	return &Context{
		Params:        business,
		InitialParams: deepCopyParams(business),
		StepResults:   make(map[string]map[string]any),
		Runtime:       runtime,
	}
}

// Sealed returns the sealed contract if the workflow has passed its confirmation
// gate, or nil. The engine reads this after Run to narrate results from the
// exact confirmed params rather than re-deriving them from stale input.
func (c *Context) Sealed() *SealedActionContract { return c.sealed }

// ZoneCatalog returns the turn's zone catalog snapshot, or nil when the run
// carries none. ZoneCatalogSnapshot's methods are nil-safe, so callers can chain
// c.ZoneCatalog().Placement(...) without a guard — an absent catalog resolves
// nothing, exactly as a failed fetch does.
func (c *Context) ZoneCatalog() *deployment.ZoneCatalogSnapshot { return c.referenceData.ZoneCatalog }

// ImageCatalog returns the turn's image catalog snapshot, or nil when the run
// carries none. ImageCatalogSnapshot's methods are nil-safe, so callers can chain
// c.ImageCatalog().ByID(...) without a guard — an absent catalog resolves nothing,
// exactly as a failed fetch does.
func (c *Context) ImageCatalog() *deployment.ImageCatalogSnapshot {
	return c.referenceData.ImageCatalog
}

// Result returns the API result from a previous step, or nil.
func (c *Context) Result(stepName string) map[string]any {
	return c.StepResults[stepName]
}

// Result of executing a workflow.
type Result struct {
	Success      bool           `json:"success"`
	StoppedAt    string         `json:"stopped_at,omitempty"`
	Message      string         `json:"message"`
	MissingSlots []string       `json:"missing_slots,omitempty"`
	Data         map[string]any `json:"data,omitempty"`
	Steps        []StepSummary  `json:"steps"`
	// Contract is the seal in force when the workflow ended, or nil if it never
	// passed a confirmation gate. It is server-internal (json:"-": never
	// serialised to the model — it may carry secret-bearing confirmed params);
	// the engine reads it to narrate results and recover from the exact confirmed
	// params instead of stale input.
	//
	// It is NOT, on its own, "the contract that gated the mutating step" — this
	// comment used to say that, and on the guided create it is false. That flow
	// seals after each of its seven gates, so a run that stopped at 检查库存 ends
	// with a contract that authorised the image selection and no more. Ask
	// Failure.Sealed whether a contract authorising execution exists; a non-nil
	// Contract answers a different question.
	Contract *SealedActionContract `json:"-"`
	// Failure describes the step that stopped this workflow, or nil when it
	// succeeded. Server-internal, like Contract and Err.
	Failure *StepFailure `json:"-"`
	// Err is the error that stopped this workflow, unflattened, or nil. Message
	// keeps the human sentence; Err keeps the typed cause, so a caller deciding
	// what to DO about a failure can read the upstream error's fields instead of
	// grepping the sentence. (createImageUnavailable used to match "230" and
	// "CompShareImageId" as substrings of Message — an unanchored match that any
	// "230" anywhere in the text could trip.)
	//
	// It is deliberately NOT set for failures we raise ourselves from a SUCCESSFUL
	// upstream response, e.g. the capacity gate's "库存不足" (CheckResult): those
	// have no upstream error and must not be mistaken for one.
	//
	// Server-internal (json:"-"): the model must never see the raw upstream tokens
	// ("RetCode=230" / "not available"), which the reply_not_contains eval gate
	// forbids — see internal/tools/upstream_error.go. This field adds no bytes to
	// the model-facing payload.
	Err error `json:"-"`
}

// StepSummary records one step's outcome.
type StepSummary struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // success / failed / cancelled
	Message string `json:"message,omitempty"`
}

// ConfirmFunc asks the user to confirm. Receives workflow name + summary args.
type ConfirmFunc func(action string, args map[string]any) bool

// ConfirmForm is the editable selection form attached to a confirmation.
// It is wire-shaped (PascalCase JSON) — the HTTP layer embeds it verbatim in
// the `confirmation` frame's optional Form field. v1 is select-only: every
// editable field carries server-generated Options and user edits may only
// pick one of them (no free text).
type ConfirmForm struct {
	Version int                `json:"Version"`
	Step    *ConfirmFormStep   `json:"Step,omitempty"`
	Fields  []ConfirmFormField `json:"Fields"`
}

// ConfirmFormStep carries guided-form presentation metadata. It is optional so
// v1 forms and legacy clients keep the same JSON shape.
type ConfirmFormStep struct {
	Index          int    `json:"Index"`
	Total          int    `json:"Total"`
	Title          string `json:"Title,omitempty"`
	Description    string `json:"Description,omitempty"`
	PrimaryLabel   string `json:"PrimaryLabel,omitempty"`
	SecondaryLabel string `json:"SecondaryLabel,omitempty"`
	Skippable      bool   `json:"Skippable,omitempty"`
	Final          bool   `json:"Final,omitempty"`
}

// ConfirmFormField is one editable (or display-only) field of a ConfirmForm.
type ConfirmFormField struct {
	Key      string              `json:"Key"`   // override key, e.g. GpuType / Zone / ImageId / ChargeType
	Label    string              `json:"Label"` // display label, backend-formatted
	Type     string              `json:"Type"`  // v1: "select" only
	Value    string              `json:"Value"` // current/default option value
	Render   string              `json:"Render,omitempty"`
	Editable bool                `json:"Editable"`
	Options  []ConfirmFormOption `json:"Options,omitempty"`
}

// ConfirmFormOption is one selectable value of a select field.
type ConfirmFormOption struct {
	Value    string            `json:"Value"`
	Label    string            `json:"Label"`
	Note     string            `json:"Note,omitempty"`
	Reason   string            `json:"Reason,omitempty"`
	Disabled bool              `json:"Disabled,omitempty"`
	Meta     map[string]string `json:"Meta,omitempty"`
}

// Field returns the form field with the given key, or nil.
func (f *ConfirmForm) Field(key string) *ConfirmFormField {
	if f == nil {
		return nil
	}
	for i := range f.Fields {
		if f.Fields[i].Key == key {
			return &f.Fields[i]
		}
	}
	return nil
}

// ValidateOverrides checks user-supplied overrides against the form: every
// key must name an Editable field and every value must be one of that field's
// Options. Returns a user-displayable error on the first violation.
func (f *ConfirmForm) ValidateOverrides(overrides map[string]string) error {
	for k, v := range overrides {
		field := f.Field(k)
		if field == nil || !field.Editable {
			return fmt.Errorf("字段 %s 不可修改", k)
		}
		valid := false
		for _, opt := range field.Options {
			if opt.Value == v && !opt.Disabled {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("字段 %s 的取值不在可选范围内", k)
		}
	}
	return nil
}

// FormDefaultOverrides returns the currently selected value of every editable
// field. Guided multi-step forms use this when the user confirms a card without
// changing anything, so the displayed default becomes the workflow parameter.
func FormDefaultOverrides(form *ConfirmForm) map[string]string {
	if form == nil {
		return nil
	}
	out := map[string]string{}
	for _, field := range form.Fields {
		if field.Editable && field.Value != "" {
			out[field.Key] = field.Value
		}
	}
	return out
}

// ConfirmResolution is the user's decision on a form-bearing confirmation:
// confirmed/denied plus any validated field overrides.
type ConfirmResolution struct {
	Confirmed bool
	Overrides map[string]string
}

// ConfirmEditsFunc is the richer HITL gate used when a StepConfirm carries a
// BuildForm. It presents args + form and returns the user's resolution. The
// transport (HTTP ConfirmBroker) has already validated Overrides against the
// form; Engine.Run re-validates defensively before applying.
type ConfirmEditsFunc func(action string, args map[string]any, form *ConfirmForm) ConfirmResolution

// StepEvent is emitted during workflow execution for UI/CLI display.
type StepEvent struct {
	StepName  string
	StepIndex int
	Total     int
	Type      StepType
	Status    string // running / success / failed / waiting / cancelled
	Tool      string
	Args      map[string]any
	Message   string
}
