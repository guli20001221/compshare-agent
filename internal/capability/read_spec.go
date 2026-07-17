package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
	openai "github.com/sashabaranov/go-openai"
)

// ReadExecutor is the upstream-call surface a typed read handler needs. The
// engine's executor satisfies it structurally. A read handler receives this plus
// its own typed request and read metadata — never the user's raw sentence.
type ReadExecutor interface {
	Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error)
	ExecuteInternal(ctx context.Context, action string, args map[string]any) (map[string]any, error)
}

// EntityResolver resolves instance references to registry snapshots. It is
// structurally satisfied by entity.RegistrySnapshot; a handler resolves the
// structured TargetRefs it was given, it never re-extracts ids from text.
type EntityResolver interface {
	ResolveByID(id string) (*entity.InstanceSnapshot, entity.ResolveResult)
	ResolveByName(name string) ([]*entity.InstanceSnapshot, entity.ResolveResult)
	InstanceIDTokensInText(text string) []string
}

// ReadRuntime carries the server-owned dependencies a read handler needs. None
// of these is a model parameter: the handler never receives UserText, QueryText
// or an intent route, and never re-parses natural language.
type ReadRuntime struct {
	Executor           ReadExecutor
	Resolver           EntityResolver
	FallbackInstanceID string
	FallbackGPUModel   string
}

// ReadResult is the neutral outcome a typed read capability produces. It carries
// the same read-relevant fields the legacy handler result exposed so the engine
// serialises a migrated capability's observation byte-identically to the
// pre-migration one. Empty Status is a sentinel used only inside a Handle return
// to mean "not terminal — render the Response".
type ReadResult struct {
	Status             platform.ReadStatus
	Reply              string
	NeedsClarification bool
	FailureClass       platform.ReadFailureClass
	FallbackReason     platform.ReadFallbackReason
	ToolAction         string
	Envelope           *envelope.Envelope
	MissingFields      []platform.MissingField
	// Alternatives is the payload of the unavailable status only: the supported
	// capabilities the model should redirect the user to. Read solely by the
	// engine's Unavailable observation branch, never by the general read path.
	Alternatives []string
	// Effects are typed context side-effects the capability declares. The engine
	// applies only the effect types it recognizes — capability-specific data
	// reaches the engine as a declared typed effect, never as an untyped field
	// bag growing on this result.
	Effects []ReadEffect
}

// ReadEffect is a typed context side-effect a read capability declares in its
// result. A new side-effect is a new named type the engine can choose to apply,
// not a new field on ReadResult.
type ReadEffect interface{ readEffect() }

// RememberStockReferent records the single GPU model a stock turn resolved to,
// so a later subject-eliding follow-up ("现在还有吗") resolves to it (RC017)
// instead of re-expanding to every model.
type RememberStockReferent struct{ GPUModel string }

func (RememberStockReferent) readEffect() {}

// ReadUnavailable marks a deliberately-unsupported capability: a deterministic
// "not available in real time" answer plus the supported alternatives, produced
// without any upstream call.
func ReadUnavailable(reply string, alternatives []string) ReadResult {
	return ReadResult{Status: platform.ReadStatusUnavailable, Reply: reply, Alternatives: alternatives}
}

// ReadHandled marks a completed factual answer.
func ReadHandled(reply string) ReadResult {
	return ReadResult{Status: platform.ReadStatusHandled, Reply: reply}
}

// ReadClarification marks a deterministic clarification rather than an answer.
func ReadClarification(reply string) ReadResult {
	r := ReadHandled(reply)
	r.NeedsClarification = true
	return r
}

// ReadEmpty marks a successful query that returned no data. The reply explains
// the emptiness; the Agent asserts the status, not the wording.
func ReadEmpty(reply string) ReadResult {
	return ReadResult{Status: platform.ReadStatusEmpty, Reply: reply}
}

// ReadConflict marks an ambiguous request that resolved to multiple candidates.
// The reply is the disambiguation prompt; NeedsClarification drives the Agent to
// ask which one, rather than guessing.
func ReadConflict(reply string) ReadResult {
	return ReadResult{Status: platform.ReadStatusConflict, Reply: reply, NeedsClarification: true}
}

// ReadFallbackBeforeTool marks a pre-tool fallback with a structured reason.
func ReadFallbackBeforeTool(reason platform.ReadFallbackReason) ReadResult {
	return ReadResult{Status: platform.ReadStatusFallbackBeforeTool, FallbackReason: reason}
}

// FriendlyReadFailureReply is the generic post-tool failure reply. It is
// byte-identical to the legacy route-dispatch failure string it replaced, so a
// migrated capability's failure observation matches the pre-migration one.
const FriendlyReadFailureReply = "查询暂时失败，请稍后再试。"

// userFacingError is the structural half of a typed upstream error carrying an
// actionable recovery message (e.g. *tools.UpstreamAPIError on a 230). Declared
// structurally so this package does not import the error's package.
type userFacingError interface{ UserMessage() string }

// ReadFailureAfterTool classifies an upstream error into a terminal read result,
// mirroring the legacy failureAfterToolForError: a typed upstream error with a
// non-empty user message surfaces it as an actionable failure; otherwise the
// generic friendly reply prefixed by the capability label.
func ReadFailureAfterTool(action, label string, err error) ReadResult {
	var friendly userFacingError
	if errors.As(err, &friendly) {
		if msg := strings.TrimSpace(friendly.UserMessage()); msg != "" {
			return ReadResult{
				Status:       platform.ReadStatusFailureAfterTool,
				Reply:        msg,
				FailureClass: platform.ReadFailureActionableUpstream,
				ToolAction:   action,
			}
		}
	}
	reply := FriendlyReadFailureReply
	if label = strings.TrimSpace(label); label != "" {
		reply = label + ": " + reply
	}
	return ReadResult{
		Status:       platform.ReadStatusFailureAfterTool,
		Reply:        reply,
		FailureClass: platform.ReadFailureGenericRead,
		ToolAction:   action,
	}
}

// ReadCapabilitySpec is the single definition of one read capability: tool
// schema, JSON decode, validation, handler and renderer all reference the same
// Request/Response types. A schema that omits a request field, or a handler
// wired to the wrong request type, is caught at compile time (the generic type
// parameter) or by the catalog consistency test (schema property set vs. the
// Request struct's JSON tags).
type ReadCapabilitySpec[Request platform.ReadRequest, Response any] struct {
	// Label is the capability identity, e.g. "pricing_query". It is the tool-name
	// suffix and the observation's capability tag.
	Label string
	// Description is the model-facing tool description.
	Description string
	// Params is the capability's parameter field contract — the single source
	// for the model-facing JSON schema (Params.jsonSchema()), the runtime
	// argument validator (Params.validate, which enforces the enum/minimum the
	// decoder does not) and the consistency-test expectation. Its property set
	// must equal Request's JSON field set (enforced by the catalog consistency
	// test).
	Params schemaNode
	// Handle runs the upstream calls and produces a typed Response. When it needs
	// to short-circuit (missing data, clarification, fallback, failure) it returns
	// a terminal ReadResult (non-empty Status); otherwise it returns the Response
	// with a zero ReadResult and Render is called.
	Handle func(ctx context.Context, req Request, rt ReadRuntime) (Response, ReadResult)
	// Render is a pure function of the typed Response — never of the request or
	// user text — producing the reply and evidence envelope.
	Render func(Response) ReadResult
	// Observe optionally derives typed context side-effects from the response
	// (nil = none). It runs only on the success path — a rendered response, never
	// a terminal short-circuit — and the engine applies the effects it returns.
	Observe func(Response) []ReadEffect
}

// RegisteredRead is the type-erased catalog entry. Type erasure happens exactly
// once, here at the registry edge; past decodeInto the flow stays fully typed
// and never falls back to map[string]any or intent.Slots.
type RegisteredRead struct {
	Label       string
	Description string
	Tool        openai.Tool
	schema      map[string]any
	params      schemaNode
	requestType reflect.Type
	decode      func(map[string]any) (platform.ReadRequest, error)
	run         func(ctx context.Context, req platform.ReadRequest, rt ReadRuntime) ReadResult
}

// Decode parses tool arguments into this capability's concrete request type.
func (r RegisteredRead) Decode(args map[string]any) (platform.ReadRequest, error) {
	return r.decode(args)
}

// Run executes the typed handler + renderer for an already-decoded request.
func (r RegisteredRead) Run(ctx context.Context, req platform.ReadRequest, rt ReadRuntime) ReadResult {
	return r.run(ctx, req, rt)
}

// Schema returns the model-facing parameter schema (for the consistency test).
func (r RegisteredRead) Schema() map[string]any { return r.schema }

// Params returns the capability's parameter field contract (for the recursive
// schema/struct consistency test).
func (r RegisteredRead) Params() schemaNode { return r.params }

// RequestType returns the reflect.Type of the concrete request (for the
// consistency test).
func (r RegisteredRead) RequestType() reflect.Type { return r.requestType }

// NewReadCapability erases a typed spec into a catalog entry. The Request type
// ties the decoder, the MissingFields validator, the handler and the renderer
// together: a mismatch is a compile error, not a runtime surprise.
func NewReadCapability[Request platform.ReadRequest, Response any](spec ReadCapabilitySpec[Request, Response]) RegisteredRead {
	toolName := ReadToolPrefix + spec.Label
	schema := spec.Params.jsonSchema()
	return RegisteredRead{
		Label:       spec.Label,
		Description: spec.Description,
		Tool: openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
			Name: toolName, Description: strings.TrimSpace(spec.Description), Parameters: schema,
		}},
		schema:      schema,
		params:      spec.Params,
		requestType: reflect.TypeOf(*new(Request)),
		decode: func(args map[string]any) (platform.ReadRequest, error) {
			// Enforce the enum/minimum the strict JSON decoder cannot: an
			// out-of-contract value is a validation fallback, not a silent
			// default. Runs before decode so a bogus enum never reaches a handler.
			if err := spec.Params.validate(args, ""); err != nil {
				return nil, err
			}
			return decodeStrictRead[Request](args)
		},
		run: func(ctx context.Context, req platform.ReadRequest, rt ReadRuntime) ReadResult {
			typed, ok := req.(Request)
			if !ok {
				// Unreachable: Decode produced exactly Request. Defensive only.
				return ReadFallbackBeforeTool(platform.ReadFallbackValidation)
			}
			resp, terminal := spec.Handle(ctx, typed, rt)
			if terminal.Status != "" {
				return terminal
			}
			result := spec.Render(resp)
			if spec.Observe != nil {
				result.Effects = spec.Observe(resp)
			}
			return result
		},
	}
}

// decodeStrictRead decodes tool arguments into a concrete request type, refusing
// unknown fields and trailing JSON so a schema/struct drift surfaces as a decode
// error rather than a silently dropped field.
func decodeStrictRead[T platform.ReadRequest](args map[string]any) (platform.ReadRequest, error) {
	payload, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var request T
	if err := decoder.Decode(&request); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON value")
	}
	return request, nil
}
