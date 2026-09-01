package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/sanitizer"
	"github.com/compshare-agent/internal/security"
	"github.com/compshare-agent/internal/zones"
)

var (
	ErrPolicyMissing          = errors.New("tool execution policy missing")
	ErrDestructiveAction      = errors.New("destructive action refused")
	ErrUserDeclined           = errors.New("user declined confirmation")
	ErrNonExternalAction      = errors.New("non-external action cannot be executed by API executor")
	ErrToolCapExceeded        = errors.New("tool cap exceeded")
	ErrHistoryWindowExceeded  = errors.New("history window exceeded")
	ErrMutatingActionDisabled = errors.New("mutating action disabled")
	// ErrCFSZoneUnresolved is returned when GetCompShareCFSUpgradePrice cannot
	// resolve the internal zone_id. The upstream otherwise returns a misleading
	// zero-price success, so this path fails closed.
	ErrCFSZoneUnresolved = errors.New("cannot resolve CFS availability zone")
)

type ExecutionOrigin string

const (
	OriginDirectLLM         ExecutionOrigin = "direct_llm"
	OriginWorkflowInternal  ExecutionOrigin = "workflow_internal"
	OriginDiagnosisInternal ExecutionOrigin = "diagnosis_internal"
)

type ConfirmFunc func(action string, args map[string]any) bool

type SafeToolRequest struct {
	Action string
	Args   map[string]any
	Origin ExecutionOrigin
	Hooks  SafeToolHooks
}

type SafeToolHooks struct {
	OnConfirmNeeded func(action string, args map[string]any)
	OnBeforeCall    func(action string, args map[string]any)
}

type SafeToolResult struct {
	Action      string
	Args        map[string]any
	RawResult   map[string]any
	LLMResult   map[string]any
	TraceResult map[string]any
	Attempts    int
	Policy      ToolExecutionPolicy
}

type SafeToolExecutor struct {
	inner                ToolExecutor
	policies             map[string]ToolExecutionPolicy
	confirm              ConfirmFunc
	mutatingToolsEnabled bool
	// zoneCatalog resolves public Zone strings to numeric ZoneIDs for upstream
	// endpoints whose model-facing contracts deliberately hide that field.
	// Defaults to the shared, process-wide,
	// TTL-cached zones.Default() (same instance engine.Engine uses for the
	// create/deploy paths); tests inject zones.NewCatalog(0) for isolation
	// (same convention as engine.Engine.zoneCatalog).
	zoneCatalog *zones.Catalog
}

type SafeOption func(*SafeToolExecutor)

func NewSafeToolExecutor(inner ToolExecutor, opts ...SafeOption) *SafeToolExecutor {
	s := &SafeToolExecutor{
		inner:                inner,
		policies:             DefaultToolExecutionPolicies(),
		mutatingToolsEnabled: true,
		zoneCatalog:          zones.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func WithPolicies(policies map[string]ToolExecutionPolicy) SafeOption {
	return func(s *SafeToolExecutor) {
		s.policies = policies
	}
}

// WithZoneCatalog overrides the catalog used for numeric backend zone lookup.
// Tests should pass zones.NewCatalog(0) for isolation from the shared cache.
func WithZoneCatalog(cat *zones.Catalog) SafeOption {
	return func(s *SafeToolExecutor) {
		s.zoneCatalog = cat
	}
}

func WithConfirmFunc(confirm ConfirmFunc) SafeOption {
	return func(s *SafeToolExecutor) {
		s.confirm = confirm
	}
}

func WithMutatingToolsEnabled(enabled bool) SafeOption {
	return func(s *SafeToolExecutor) {
		s.mutatingToolsEnabled = enabled
	}
}

func (s *SafeToolExecutor) SetMutatingToolsEnabled(enabled bool) {
	s.mutatingToolsEnabled = enabled
}

func (s *SafeToolExecutor) SetConfirmFunc(fn ConfirmFunc) {
	s.confirm = fn
}

func (s *SafeToolExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	result, err := s.ExecuteSafe(ctx, SafeToolRequest{
		Action: action,
		Args:   args,
		Origin: OriginDirectLLM,
	})
	if err != nil {
		return nil, err
	}
	return result.RawResult, nil
}

func (s *SafeToolExecutor) AsToolExecutor(origin ExecutionOrigin) ToolExecutor {
	return originExecutor{safe: s, origin: origin}
}

func (s *SafeToolExecutor) ExternalExecutor() *ExternalExecutor {
	if ext, ok := s.inner.(*ExternalExecutor); ok {
		return ext
	}
	if provider, ok := s.inner.(interface{ ExternalExecutor() *ExternalExecutor }); ok {
		return provider.ExternalExecutor()
	}
	return nil
}

func (s *SafeToolExecutor) FilterArgs(action string, args map[string]any) map[string]any {
	policy, ok := s.policies[action]
	if !ok {
		return copyMap(args)
	}
	return filterSafeArgs(args, allowedParamsForOrigin(policy, OriginDirectLLM))
}

func (s *SafeToolExecutor) RedactArgs(action string, args map[string]any) map[string]any {
	return sanitizer.SanitizeArgs(action, args)
}

func (s *SafeToolExecutor) PolicyForAction(action string) (ToolExecutionPolicy, bool) {
	policy, ok := s.policies[action]
	return policy, ok
}

type originExecutor struct {
	safe   *SafeToolExecutor
	origin ExecutionOrigin
}

func (e originExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	result, err := e.safe.ExecuteSafe(ctx, SafeToolRequest{
		Action: action,
		Args:   args,
		Origin: e.origin,
	})
	if err != nil {
		return nil, err
	}
	return result.RawResult, nil
}

func (s *SafeToolExecutor) ExecuteSafe(ctx context.Context, req SafeToolRequest) (*SafeToolResult, error) {
	if req.Origin == "" {
		req.Origin = OriginDirectLLM
	}

	policy, ok := s.policies[req.Action]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrPolicyMissing, req.Action)
	}
	if policy.Route != ActionRouteExternalAPI {
		return nil, fmt.Errorf("%w: %s (%s)", ErrNonExternalAction, req.Action, policy.Route)
	}
	if policy.SecurityLevel == security.L2 || policy.Class == ActionClassDestructive {
		return nil, fmt.Errorf("%w: %s", ErrDestructiveAction, req.Action)
	}
	if !s.mutatingToolsEnabled && policy.Class == ActionClassMutating {
		return nil, fmt.Errorf("%w: %s", ErrMutatingActionDisabled, req.Action)
	}

	args := filterSafeArgs(req.Args, allowedParamsForOrigin(policy, req.Origin))
	resolvedArgs, err := s.resolveBackendZoneID(ctx, req.Action, args)
	if err != nil {
		return nil, err
	}
	args = resolvedArgs
	if err := checkPolicyCaps(policy, args); err != nil {
		return nil, err
	}
	if shouldConfirm(policy, req.Origin) {
		if req.Hooks.OnConfirmNeeded != nil {
			req.Hooks.OnConfirmNeeded(req.Action, args)
		}
		if s.confirm == nil || !s.confirm(req.Action, args) {
			return nil, fmt.Errorf("%w: %s", ErrUserDeclined, req.Action)
		}
	}

	if req.Hooks.OnBeforeCall != nil {
		req.Hooks.OnBeforeCall(req.Action, args)
	}
	raw, attempts, err := s.executeWithRetry(ctx, policy, req.Action, args)
	if err != nil {
		return nil, err
	}
	guarded := applyHistoryGuard(policy, args, raw)
	guarded = dedupeModelRepositoryTags(req.Action, guarded)

	return &SafeToolResult{
		Action:      req.Action,
		Args:        args,
		RawResult:   guarded,
		LLMResult:   redactForLLM(req.Action, policy, guarded),
		TraceResult: redactForTrace(req.Action, policy, guarded),
		Attempts:    attempts,
		Policy:      policy,
	}, nil
}

func (s *SafeToolExecutor) executeWithRetry(ctx context.Context, policy ToolExecutionPolicy, action string, args map[string]any) (map[string]any, int, error) {
	var attempts int
	var lastErr error
	maxAttempts := policy.MaxRetries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	for attempts = 1; attempts <= maxAttempts; attempts++ {
		// Apply the per-attempt timeout. The derived context is
		// scoped to this single Execute call so a hung backend cannot
		// outlast the policy budget. If policy.TimeoutMS is zero we
		// pass the parent ctx through unchanged — the inner executor's
		// ambient http.Client.Timeout still applies as the last-resort
		// safety net.
		attemptCtx := ctx
		var cancel context.CancelFunc
		if policy.TimeoutMS > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, time.Duration(policy.TimeoutMS)*time.Millisecond)
		}

		raw, err := s.inner.Execute(attemptCtx, action, args)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return raw, attempts, nil
		}
		lastErr = err
		if attempts >= maxAttempts || !shouldRetry(err, policy.RetryOn) {
			return nil, attempts, err
		}

		// Linear backoff between retries. Sleep BackoffBaseMS *
		// attempt ms, but break early if the parent ctx is cancelled
		// (caller deadline or shutdown). For read_cheap with
		// MaxRetries=1 + BackoffBaseMS=300 this is at most one 300ms
		// wait; mutating/destructive with MaxRetries=0 never reach here.
		if policy.BackoffBaseMS > 0 {
			wait := time.Duration(policy.BackoffBaseMS*attempts) * time.Millisecond
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, attempts, ctx.Err()
			}
		}
	}
	return nil, attempts, lastErr
}

// resolveBackendZoneID supplies internal numeric zones that the upstream reads
// but the model-facing contracts deliberately do not expose. Existing
// caller-supplied values from an internal workflow pass through unchanged.
func (s *SafeToolExecutor) resolveBackendZoneID(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	switch action {
	case "GetCompShareCFSUpgradePrice":
		return s.resolveCFSUpgradeZoneID(ctx, args)
	case "DescribeCFS":
		return s.resolveCFSListZoneID(ctx, args)
	case "StartCompShareInstance", "StopCompShareInstance":
		return s.resolvePodLifecycleZoneID(ctx, action, args)
	case "DescribeCompShareJupyterToken":
		return s.resolveJupyterTokenZoneID(ctx, args)
	default:
		return args, nil
	}
}

// resolvePodLifecycleZoneID supplies the routing fact used by the upstream
// Start/Stop dispatcher. UHost requests need no numeric zone; cpod requests do.
// This is routing for an existing, already-described instance, so an exact
// Zone-to-ID match from the last good catalog remains usable during a transient
// catalog refresh failure. New resource selection continues to use GetStrict.
func (s *SafeToolExecutor) resolvePodLifecycleZoneID(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	if hasNonZeroUint(args["zone_id"]) || !platform.IsPodInstanceID(stringArg(args["UHostId"])) {
		return args, nil
	}
	zone := strings.TrimSpace(stringArg(args["Zone"]))
	if zone == "" {
		return nil, fmt.Errorf("%s: pod instance zone is missing", action)
	}
	topOrg, org := identityFromContext(ctx)
	zoneList, err := s.zoneCatalog.Get(ctx, originExecutor{safe: s, origin: OriginDiagnosisInternal}, topOrg, org)
	if err != nil {
		return nil, fmt.Errorf("%s: support-zone catalog unavailable: %w", action, err)
	}
	canonical, ok := zones.ExactZone(zoneList, zone)
	if !ok {
		return nil, fmt.Errorf("%s: zone %s is not in the live support-zone catalog", action, zone)
	}
	zoneID := zoneIDForZoneString(zoneList, canonical)
	if zoneID == 0 {
		return nil, fmt.Errorf("%s: numeric zone id is unavailable for %s", action, zone)
	}
	out := copyMap(args)
	out["zone_id"] = zoneID
	return out, nil
}

// resolveCFSListZoneID makes the public DescribeCFS(Zone) contract true. The
// upstream list endpoint filters only on BaseRequest.zone_id; forwarding the
// human Zone/Region strings alone would silently return every CFS in the account.
func (s *SafeToolExecutor) resolveCFSListZoneID(ctx context.Context, args map[string]any) (map[string]any, error) {
	if hasNonZeroUint(args["zone_id"]) || strings.TrimSpace(stringArg(args["CfsId"])) != "" {
		return args, nil
	}
	zone := strings.TrimSpace(stringArg(args["Zone"]))
	if zone == "" {
		return args, nil
	}
	topOrg, org := identityFromContext(ctx)
	zoneList, err := s.zoneCatalog.Get(ctx, originExecutor{safe: s, origin: OriginDiagnosisInternal}, topOrg, org)
	if err != nil {
		return nil, fmt.Errorf("%w for %s: support-zone catalog unavailable", ErrCFSZoneUnresolved, zone)
	}
	canonical, ok := zones.ExactZone(zoneList, zone)
	if !ok {
		return nil, fmt.Errorf("%w for %s: zone is not in the live support-zone catalog", ErrCFSZoneUnresolved, zone)
	}
	zoneID := zoneIDForZoneString(zoneList, canonical)
	if zoneID == 0 {
		return nil, fmt.Errorf("%w for %s: numeric zone id is unavailable", ErrCFSZoneUnresolved, zone)
	}
	out := copyMap(args)
	out["zone_id"] = zoneID
	return out, nil
}

// resolveCFSUpgradeZoneID derives the required zone from DescribeCFS. It rejects
// an unresolved zone because upstream otherwise returns a misleading zero price.
func (s *SafeToolExecutor) resolveCFSUpgradeZoneID(ctx context.Context, args map[string]any) (map[string]any, error) {
	if hasNonZeroUint(args["zone_id"]) {
		return args, nil
	}
	cfsID := strings.TrimSpace(stringArg(args["CfsId"]))
	if cfsID == "" {
		// Missing CfsId is a separate validation problem the upstream call
		// will surface on its own; nothing to resolve here.
		return args, nil
	}
	result, err := s.ExecuteSafe(ctx, SafeToolRequest{
		Action: "DescribeCFS",
		Args:   map[string]any{"CfsId": cfsID},
		Origin: OriginDiagnosisInternal,
	})
	var zoneID uint32
	if err == nil && result != nil {
		zoneID = cfsZoneIDFromDescribeResult(result.RawResult)
	}
	if zoneID == 0 {
		return nil, fmt.Errorf("%w for CFS %s: cannot verify availability zone, refusing to price (upstream would otherwise return a misleading ¥0)", ErrCFSZoneUnresolved, cfsID)
	}
	out := copyMap(args)
	out["zone_id"] = zoneID
	return out, nil
}

// resolveJupyterTokenZoneID resolves zone_id for a DescribeCompShareJupyterToken
// call whose (first) UHostId is a Pod instance (cpod-* prefix).
//
// Pod Describe rows expose a string Zone rather than numeric zone_id, so the
// support-zone catalog supplies the numeric value. UHost calls are unchanged;
// unresolved Pod values remain an upstream validation error.
func (s *SafeToolExecutor) resolveJupyterTokenZoneID(ctx context.Context, args map[string]any) (map[string]any, error) {
	if hasNonZeroUint(args["zone_id"]) {
		return args, nil
	}
	podID := firstPodUHostID(args["UHostIds"])
	if podID == "" {
		return args, nil
	}
	result, err := s.ExecuteSafe(ctx, SafeToolRequest{
		Action: "DescribeCompShareInstance",
		Args:   map[string]any{"UHostIds": []string{podID}},
		Origin: OriginDiagnosisInternal,
	})
	if err != nil || result == nil {
		return args, nil
	}
	zone := instanceZoneFromDescribeResult(result.RawResult)
	if zone == "" {
		return args, nil
	}
	topOrg, org := identityFromContext(ctx)
	zoneList, err := s.zoneCatalog.Get(ctx, originExecutor{safe: s, origin: OriginDiagnosisInternal}, topOrg, org)
	if err != nil {
		return args, nil
	}
	zoneID := zoneIDForZoneString(zoneList, zone)
	if zoneID == 0 {
		return args, nil
	}
	out := copyMap(args)
	out["zone_id"] = zoneID
	return out, nil
}

// identityFromContext reads the per-request tenant identity stashed by
// WithUser, returning zero values when absent (callers treat that as
// "cannot resolve" and degrade gracefully rather than failing).
func identityFromContext(ctx context.Context) (topOrg, org uint32) {
	u, ok := UserFrom(ctx)
	if !ok {
		return 0, 0
	}
	return u.TopOrganizationID, u.OrganizationID
}

// instanceZoneFromDescribeResult returns the string Zone (e.g. "cn-bj2-03")
// off the first UHostSet entry — unlike ZoneId/ZoneID, this field reliably
// exists on DescribeCompShareInstance responses.
func instanceZoneFromDescribeResult(raw map[string]any) string {
	if raw == nil {
		return ""
	}
	hostSet, ok := raw["UHostSet"].([]any)
	if !ok || len(hostSet) == 0 {
		return ""
	}
	host, ok := hostSet[0].(map[string]any)
	if !ok {
		return ""
	}
	zone, _ := host["Zone"].(string)
	return strings.TrimSpace(zone)
}

// zoneIDForZoneString matches a Zone string (case-insensitive) against the
// support-zone catalog and returns its numeric ZoneID, or 0 if not found.
func zoneIDForZoneString(list []zones.ZoneInfo, zone string) uint32 {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return 0
	}
	for _, z := range list {
		if strings.EqualFold(z.Zone, zone) {
			return z.ZoneID
		}
	}
	return 0
}

func stringArg(v any) string {
	s, _ := v.(string)
	return s
}

func hasNonZeroUint(v any) bool {
	switch x := v.(type) {
	case uint32:
		return x != 0
	case uint64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	}
	return false
}

func firstPodUHostID(v any) string {
	for _, id := range stringSliceArg(v) {
		if platform.IsPodInstanceID(id) {
			return id
		}
	}
	return ""
}

func stringSliceArg(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// cfsZoneIDFromDescribeResult accepts the upstream ZoneId spellings at the top
// level and inside CFSSet rows.
func cfsZoneIDFromDescribeResult(raw map[string]any) uint32 {
	if raw == nil {
		return 0
	}
	if id := uint32FieldAny(raw, "ZoneId", "ZoneID", "zone_id"); id != 0 {
		return id
	}
	rows, _ := raw["CFSSet"].([]any)
	for _, rowAny := range rows {
		row, ok := rowAny.(map[string]any)
		if !ok {
			continue
		}
		if id := uint32FieldAny(row, "ZoneId", "ZoneID", "zone_id"); id != 0 {
			return id
		}
	}
	return 0
}

func uint32FieldAny(m map[string]any, keys ...string) uint32 {
	for _, key := range keys {
		switch v := m[key].(type) {
		case int:
			if v > 0 {
				return uint32(v)
			}
		case int32:
			if v > 0 {
				return uint32(v)
			}
		case int64:
			if v > 0 {
				return uint32(v)
			}
		case uint32:
			if v > 0 {
				return v
			}
		case uint64:
			if v > 0 {
				return uint32(v)
			}
		case float64:
			if v > 0 {
				return uint32(v)
			}
		}
	}
	return 0
}

// dedupeModelRepositoryTags removes duplicate tag tokens from the two
// model-repository response shapes. DescribeModelRepositoryModels
// puts a comma-separated "Tag" string on each Models[] entry, and its sibling
// DescribeModelRepositoryTags returns a flat "Tags" array — this only touches
// those two fields on those two actions; it is not a generic response hook.
func dedupeModelRepositoryTags(action string, raw map[string]any) map[string]any {
	if raw == nil {
		return raw
	}
	switch action {
	case "DescribeModelRepositoryModels":
		models, ok := raw["Models"].([]any)
		if !ok {
			return raw
		}
		for _, item := range models {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if tag, ok := entry["Tag"].(string); ok && tag != "" {
				entry["Tag"] = dedupeCSVTags(tag)
			}
		}
	case "DescribeModelRepositoryTags":
		if tags, ok := raw["Tags"].([]any); ok {
			raw["Tags"] = dedupeAnyStrings(tags)
		}
	}
	return raw
}

func dedupeCSVTags(value string) string {
	parts := strings.Split(value, ",")
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, trimmed)
	}
	return strings.Join(out, ",")
}

func dedupeAnyStrings(values []any) []any {
	seen := make(map[string]struct{}, len(values))
	out := make([]any, 0, len(values))
	for _, v := range values {
		s, ok := v.(string)
		if !ok {
			out = append(out, v)
			continue
		}
		trimmed := strings.TrimSpace(s)
		key := strings.ToLower(trimmed)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	return out
}

func checkPolicyCaps(policy ToolExecutionPolicy, args map[string]any) error {
	if policy.MaxTargetsPerCall > 0 {
		targets := countRequestedTargets(args)
		if targets > policy.MaxTargetsPerCall {
			return fmt.Errorf("%w: requested %d targets exceeds max %d", ErrToolCapExceeded, targets, policy.MaxTargetsPerCall)
		}
	}
	if policy.MaxHistoryWindowSeconds > 0 {
		window, ok := requestedHistoryWindowSeconds(args)
		if ok && countRequestedTargets(args) < 1 {
			return fmt.Errorf("%w: historical monitor requires at least one target", ErrHistoryWindowExceeded)
		}
		if ok && window > int64(policy.MaxHistoryWindowSeconds) {
			return fmt.Errorf("%w: requested %d seconds exceeds max %d", ErrHistoryWindowExceeded, window, policy.MaxHistoryWindowSeconds)
		}
	}
	return nil
}

func countRequestedTargets(args map[string]any) int {
	if args == nil {
		return 0
	}
	if ids, ok := args["UHostIds"]; ok {
		return valueLen(ids)
	}
	if id, ok := args["UHostId"]; ok && id != nil {
		return 1
	}
	return 0
}

func valueLen(v any) int {
	switch x := v.(type) {
	case []any:
		return len(x)
	case []string:
		return len(x)
	case []int:
		return len(x)
	case []float64:
		return len(x)
	default:
		return 0
	}
}

func requestedHistoryWindowSeconds(args map[string]any) (int64, bool) {
	if args == nil {
		return 0, false
	}
	startValue, hasStart := args["StartTime"]
	endValue, hasEnd := args["EndTime"]
	if !hasStart || !hasEnd {
		if hasStart || hasEnd {
			return maxInt64, true
		}
		return 0, false
	}
	start, okStart := secondsArg(startValue)
	end, okEnd := secondsArg(endValue)
	if !okStart || !okEnd {
		return maxInt64, true
	}
	if start < 0 || end < 0 {
		return maxInt64, true
	}
	if end <= start {
		return maxInt64, true
	}
	if end > maxInt64-start {
		return maxInt64, true
	}
	return end - start, true
}

const maxInt64 = int64(1<<63 - 1)
const (
	minInt64Float          = -9223372036854775808.0
	maxInt64ExclusiveFloat = 9223372036854775808.0
)

func secondsArg(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case int32:
		return int64(x), true
	case float64:
		if !isSafeIntegerFloat64(x) {
			return 0, false
		}
		return int64(x), true
	case float32:
		f := float64(x)
		if !isSafeIntegerFloat64(f) {
			return 0, false
		}
		return int64(f), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n, err == nil
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

func isSafeIntegerFloat64(v float64) bool {
	return !math.IsNaN(v) &&
		!math.IsInf(v, 0) &&
		v >= minInt64Float &&
		v < maxInt64ExclusiveFloat &&
		math.Trunc(v) == v
}

func shouldConfirm(policy ToolExecutionPolicy, origin ExecutionOrigin) bool {
	if !policy.NeedsConfirm {
		return false
	}
	switch origin {
	case OriginWorkflowInternal, OriginDiagnosisInternal:
		return false
	default:
		return true
	}
}

func allowedParamsForOrigin(policy ToolExecutionPolicy, origin ExecutionOrigin) []string {
	allowed := policy.AllowedParams
	switch origin {
	case OriginWorkflowInternal, OriginDiagnosisInternal:
		if len(policy.InternalAllowedParams) == 0 {
			return allowed
		}
		allowed = append([]string{}, allowed...)
		for _, p := range policy.InternalAllowedParams {
			allowed = appendAllowedParam(allowed, p)
		}
		return allowed
	default:
		return allowed
	}
}

func filterSafeArgs(args map[string]any, allowed []string) map[string]any {
	if args == nil {
		return nil
	}
	if allowed == nil {
		return copyMap(args)
	}
	filtered := make(map[string]any, len(allowed))
	for _, key := range allowed {
		if v, ok := args[key]; ok {
			filtered[key] = v
		}
	}
	return filtered
}

// applyHistoryGuard marks an empty historical-monitor response so later
// rendering does not substitute current realtime data or invented values.
func applyHistoryGuard(policy ToolExecutionPolicy, args map[string]any, raw map[string]any) map[string]any {
	if !policy.HistoryMonitorGuard || !hasMonitorTimeRangeArgs(args) || raw == nil {
		return raw
	}
	if monitorResultHasSamples(raw) {
		return raw
	}
	raw["MonitorDataStatus"] = "NO_DATA_IN_REQUESTED_WINDOW"
	raw["MonitorDataGuidance"] = "该请求时间窗没有返回有效监控采样点；不要使用当前实时数据替代，也不要编造 CPU/内存/GPU 数值。"
	return raw
}

func hasMonitorTimeRangeArgs(args map[string]any) bool {
	if args == nil {
		return false
	}
	_, hasStart := args["StartTime"]
	_, hasEnd := args["EndTime"]
	return hasStart || hasEnd
}

func monitorResultHasSamples(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "Value" && val != nil {
				return true
			}
			if monitorResultHasSamples(val) {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if monitorResultHasSamples(item) {
				return true
			}
		}
	}
	return false
}

func redactForLLM(action string, policy ToolExecutionPolicy, raw map[string]any) map[string]any {
	redacted := mapFromAny(security.RedactForLLM(raw))
	for _, field := range policy.RedactInResult {
		redactFieldByName(redacted, field)
	}
	return sanitizer.Sanitize(action, redacted)
}

func redactForTrace(action string, policy ToolExecutionPolicy, raw map[string]any) map[string]any {
	redacted := mapFromAny(security.RedactForTrace(raw))
	for _, field := range policy.RedactInResult {
		redactFieldByName(redacted, field)
	}
	return sanitizer.Sanitize(action, redacted)
}

func mapFromAny(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return m
}

func redactFieldByName(m map[string]any, field string) {
	for k, v := range m {
		if k == field {
			m[k] = "[REDACTED]"
			continue
		}
		switch typed := v.(type) {
		case map[string]any:
			redactFieldByName(typed, field)
		case []any:
			for _, item := range typed {
				if child, ok := item.(map[string]any); ok {
					redactFieldByName(child, field)
				}
			}
		}
	}
}

func copyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func shouldRetry(err error, retryOn []ErrorClass) bool {
	if errors.Is(err, ErrToolCapExceeded) ||
		errors.Is(err, ErrHistoryWindowExceeded) ||
		errors.Is(err, governance.ErrRateLimited) {
		return false
	}
	if len(retryOn) == 0 {
		return false
	}
	classes := classifyError(err)
	for _, have := range classes {
		for _, want := range retryOn {
			if have == want {
				return true
			}
		}
	}
	return false
}

var statusCodeRE = regexp.MustCompile(`(?i)(status|status code)\D*(\d{3})`)

func classifyError(err error) []ErrorClass {
	if err == nil {
		return nil
	}
	out := []ErrorClass{}
	if errors.Is(err, io.EOF) {
		out = append(out, ErrorClassEOF)
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		out = append(out, ErrorClassNetwork)
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		out = append(out, ErrorClassNetwork)
	}

	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "connection reset") || strings.Contains(msg, "connection refused") {
		out = append(out, ErrorClassNetwork)
	}
	if strings.Contains(msg, "eof") {
		out = append(out, ErrorClassEOF)
	}
	if m := statusCodeRE.FindStringSubmatch(msg); len(m) == 3 && strings.HasPrefix(m[2], "5") {
		out = append(out, ErrorClassHTTP5xx)
	}
	return out
}
