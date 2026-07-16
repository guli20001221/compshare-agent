package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
)

// RuntimeMetadata holds the server-injected identity and trace fields that a
// workflow needs to reach upstream but that are NOT user business parameters:
// the caller never edits them, they never appear in a confirm form, and they are
// deliberately kept out of the sealed contract's business-param digest so a
// contract is identified by what the user confirmed, not by whose tenant ran it.
type RuntimeMetadata struct {
	TopOrganizationID uint32
	OrganizationID    uint32
	TraceID           string
}

// runtimeMetadataKeys are the param keys the engine injects for identity/trace.
// splitRuntimeMetadata lifts them out of the business param set so business
// params stay exactly what the user confirmed.
var runtimeMetadataKeys = []string{"top_organization_id", "organization_id"}

// ExecutionDraft is the mutable, pre-seal working set for one write action:
// business params the resolver/canonicalizer produced (and the confirm form may
// still edit) plus the runtime metadata. It is what precheck and confirmation
// operate on; once the user confirms, it is sealed into a SealedActionContract.
type ExecutionDraft struct {
	Operation      string
	BusinessParams map[string]any
	Runtime        RuntimeMetadata
}

// SealedActionContract is the immutable snapshot produced when the user confirms
// a write action. The mutating step consumes only this: BusinessParams are the
// exact confirmed values, Runtime carries identity/trace, and Digest pins the
// business params so any post-seal mutation is detectable (verifyDigest). It is
// the structural guarantee that "what executes is what the user confirmed".
type SealedActionContract struct {
	Operation      string
	BusinessParams map[string]any
	Runtime        RuntimeMetadata
	Version        int
	Digest         string
}

// currentContractVersion is bumped only if the sealed shape changes in a way a
// stored/replayed contract must distinguish.
const currentContractVersion = 1

// sealDraft freezes a draft into a contract: it deep-copies the business params
// (so later mutation of the draft cannot reach the sealed copy) and records a
// digest of that copy.
func sealDraft(d ExecutionDraft) SealedActionContract {
	frozen := deepCopyParams(d.BusinessParams)
	return SealedActionContract{
		Operation:      d.Operation,
		BusinessParams: frozen,
		Runtime:        d.Runtime,
		Version:        currentContractVersion,
		Digest:         paramsDigest(frozen),
	}
}

// verifyDigest reports whether params still hash to the contract's digest — i.e.
// nothing has silently rewritten the confirmed business params since the seal.
func (c SealedActionContract) verifyDigest(params map[string]any) bool {
	return paramsDigest(params) == c.Digest
}

// seal freezes the confirmed draft (the current business Params + Runtime) into
// an immutable contract stored on the context. sealDraft deep-copies the params,
// so the frozen record is independent of the live Params: a later write to
// Params diverges from the digest and is caught by verifyDigest — the structural
// guarantee that the mutating step executes exactly what the user confirmed.
func (c *Context) seal(operation string) {
	sealed := sealDraft(ExecutionDraft{
		Operation:      operation,
		BusinessParams: c.Params,
		Runtime:        c.Runtime,
	})
	c.sealed = &sealed
}

// paramsDigest is a deterministic content hash of a business-param map. json
// marshalling sorts map keys, so the digest is stable across runs for equal
// content; it is used only for tamper detection, never for security.
func paramsDigest(params map[string]any) string {
	payload, err := json.Marshal(params)
	if err != nil {
		// Business params are JSON-safe (strings/numbers/bools/maps/slices); a
		// marshal error is not expected. Fall back to a non-empty sentinel so a
		// mismatch is reported rather than two errors comparing equal.
		return "digest-error"
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// splitRuntimeMetadata removes the identity/trace keys from a business-param map
// and returns them as RuntimeMetadata. The input map is mutated (keys deleted),
// so callers pass a copy they own. Values may arrive as uint32 (engine/tests) or
// float64 (JSON-decoded); both are coerced.
func splitRuntimeMetadata(m map[string]any) (map[string]any, RuntimeMetadata) {
	rt := RuntimeMetadata{
		TopOrganizationID: uint32Value(m["top_organization_id"]),
		OrganizationID:    uint32Value(m["organization_id"]),
	}
	for _, k := range runtimeMetadataKeys {
		delete(m, k)
	}
	return m, rt
}

// uint32Value coerces a param value (uint32 / float64 / int family) to uint32,
// returning 0 when absent or unparseable.
func uint32Value(v any) uint32 {
	switch n := v.(type) {
	case uint32:
		return n
	case float64:
		return uint32(n)
	case int:
		return uint32(n)
	case int64:
		return uint32(n)
	case uint64:
		return uint32(n)
	default:
		return 0
	}
}

// deepCopyParams returns an independent copy of a business-param map: nested
// maps and slices are cloned recursively so a mutation of the copy (or of the
// original) cannot reach the other. ResolvedAction.Arguments carries structured
// values (nested map[string]any / []any and the engine's zone maps), so a
// top-level copy alone would still share the inner objects.
func deepCopyParams(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

// deepCopyValue clones maps and slices of any element type recursively and
// returns scalars (and any other kind) by value. It preserves concrete types
// (uint32 org ids, map[string]uint32 zone maps) — unlike a JSON round-trip,
// which would collapse them to float64 / map[string]any.
func deepCopyValue(v any) any {
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		out := reflect.MakeMapWithSize(rv.Type(), rv.Len())
		for _, key := range rv.MapKeys() {
			cp := deepCopyValue(rv.MapIndex(key).Interface())
			if cp == nil {
				out.SetMapIndex(key, reflect.Zero(rv.Type().Elem()))
				continue
			}
			out.SetMapIndex(key, reflect.ValueOf(cp))
		}
		return out.Interface()
	case reflect.Slice:
		out := reflect.MakeSlice(rv.Type(), rv.Len(), rv.Len())
		for i := 0; i < rv.Len(); i++ {
			cp := deepCopyValue(rv.Index(i).Interface())
			if cp == nil {
				continue // leave the zero value
			}
			out.Index(i).Set(reflect.ValueOf(cp))
		}
		return out.Interface()
	default:
		return v
	}
}
