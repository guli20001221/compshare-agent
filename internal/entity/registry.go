package entity

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	recentlyReleasedTTL         = 24 * time.Hour
	DefaultRegistryFreshnessTTL = 30 * time.Second
)

type SyncEvent string

const (
	SyncEventUnavailable SyncEvent = "unavailable"
	SyncEventInit        SyncEvent = "init"
	SyncEventSyncRefresh SyncEvent = "sync_refresh"
	SyncEventWarmCache   SyncEvent = "warm_cache"
	SyncEventFailed      SyncEvent = "failed"
)

type RefreshReason string

const (
	RefreshReasonInit      RefreshReason = "init"
	RefreshReasonManual    RefreshReason = "manual"
	RefreshReasonTTL       RefreshReason = "ttl"
	RefreshReasonWarmCache RefreshReason = "warm_cache"
)

// Executor is the narrow dependency needed for T-004a. It is intentionally
// compatible with tools.ToolExecutor and mock executors.
type Executor interface {
	Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error)
}

type RegistryOption func(*EntityRegistry)

// WithClock injects time for deterministic age/release tests.
func WithClock(now func() time.Time) RegistryOption {
	return func(r *EntityRegistry) {
		if now != nil {
			r.now = now
		}
	}
}

// EntityRegistry stores the current account entity snapshot for a conversation.
type EntityRegistry struct {
	mu sync.RWMutex

	// Deprecated: use Snapshot, ResolveByID, ResolveByName, or Filter.
	// Direct map access is not part of the runtime-safe T-004b contract.
	Instances map[string]InstanceSnapshot
	// NameIndex maps normalizeName(instance.Name) to UHostIds. Callers should
	// prefer ResolveByName instead of reading this normalized index directly.
	//
	// Deprecated: use Snapshot or ResolveByName. Direct map access is not
	// protected from concurrent refreshes outside EntityRegistry methods.
	NameIndex        map[string][]string
	LastFullSync     time.Time
	LastSyncEvent    string
	LastSyncError    string
	TotalCount       int
	Truncated        bool
	recentlyReleased map[string]time.Time
	now              func() time.Time
	invalidated      bool
	invalidation     string
}

// RegistrySnapshot is an immutable copy of the registry state at one point in time.
// Mutating the returned maps must not affect the source EntityRegistry.
type RegistrySnapshot struct {
	SnapshotID    string
	Instances     map[string]InstanceSnapshot
	NameIndex     map[string][]string
	LastFullSync  time.Time
	SyncEvent     string
	LastSyncError string
	TotalCount    int
	Truncated     bool
	// Invalidated mirrors the source registry's invalidated flag: a successful
	// state-changing action ran after this snapshot's sync, so the snapshot's
	// inventory/state can no longer be trusted for freshness decisions. Carried on
	// the copy so a consumer can judge freshness from the snapshot alone, without
	// reaching back into the live EntityRegistry.
	Invalidated bool
}

// RegistryTraceState is the compact entity registry block written into trace.
type RegistryTraceState struct {
	SnapshotID string
	AgeSeconds int64
	SyncEvent  string
}

func NewRegistry(opts ...RegistryOption) *EntityRegistry {
	r := &EntityRegistry{
		Instances:        map[string]InstanceSnapshot{},
		NameIndex:        map[string][]string{},
		LastSyncEvent:    string(SyncEventUnavailable),
		recentlyReleased: map[string]time.Time{},
		now:              time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func (r *EntityRegistry) Age() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.LastFullSync.IsZero() {
		return 0
	}
	return r.now().Sub(r.LastFullSync)
}

// CanAssertAbsence reports whether this registry has the standing to say an
// instance is NOT in the user's account.
//
// ResolveByID / ResolveByName answer NOT_FOUND_IN_ACCOUNT for anything they have not
// seen — which is correct only if they have seen EVERYTHING. Three ways they have not:
//
//   - never synced (LastFullSync zero). The HTTP path skips engine.Init(), so a session
//     that never lists instances carries an empty registry for its whole life.
//   - the last sync FAILED.
//   - the listing was TRUNCATED: DescribeCompShareInstance pages, and Truncated is set
//     precisely when TotalCount > len(fetched) (registry.go, refresh). A live account here
//     holds 20 instances and the call returns 10 — so HALF the user's machines are absent
//     from a registry that will nonetheless swear they do not exist.
//
// In all three the honest answer is "I have not seen it", and "I have not seen it" is not
// "it does not exist". Callers that turn NOT_FOUND into a hard refusal must consult this
// first; the resolver is a cache, not the account.
//
// A genuinely empty account (synced cleanly, TotalCount 0) IS authoritative — absence is
// then a fact, not an artefact.
func (r *EntityRegistry) CanAssertAbsence() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return canAssertAbsence(r.LastFullSync, r.LastSyncEvent, r.Truncated, len(r.Instances), r.TotalCount)
}

// CanAssertAbsence: see EntityRegistry.CanAssertAbsence. Same rule, immutable copy.
func (s RegistrySnapshot) CanAssertAbsence() bool {
	return canAssertAbsence(s.LastFullSync, s.SyncEvent, s.Truncated, len(s.Instances), s.TotalCount)
}

func canAssertAbsence(lastFullSync time.Time, syncEvent string, truncated bool, known, total int) bool {
	if lastFullSync.IsZero() || truncated {
		return false
	}
	switch SyncEvent(syncEvent) {
	case SyncEventUnavailable, SyncEventFailed, "":
		return false
	}
	// Synced cleanly and completely: either we hold instances, or the account really
	// has none.
	return known > 0 || total == 0
}

func (r *EntityRegistry) Snapshot() RegistrySnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	instances := copyInstances(r.Instances)
	nameIndex := copyNameIndex(r.NameIndex)
	snapshotID := ""
	if !r.LastFullSync.IsZero() {
		snapshotID = computeSnapshotID(instances, r.TotalCount, r.Truncated)
	}
	return RegistrySnapshot{
		SnapshotID:    snapshotID,
		Instances:     instances,
		NameIndex:     nameIndex,
		LastFullSync:  r.LastFullSync,
		SyncEvent:     r.LastSyncEvent,
		LastSyncError: r.LastSyncError,
		TotalCount:    r.TotalCount,
		Truncated:     r.Truncated,
		Invalidated:   r.invalidated,
	}
}

func (r *EntityRegistry) TraceState(now time.Time) RegistryTraceState {
	snap := r.Snapshot()
	if snap.LastFullSync.IsZero() {
		event := snap.SyncEvent
		if event == "" {
			event = string(SyncEventUnavailable)
		}
		return RegistryTraceState{SyncEvent: event}
	}
	age := now.Sub(snap.LastFullSync)
	if age < 0 {
		age = 0
	}
	return RegistryTraceState{
		SnapshotID: snap.SnapshotID,
		AgeSeconds: int64(age.Seconds()),
		SyncEvent:  snap.SyncEvent,
	}
}

func (r *EntityRegistry) Sync(ctx context.Context, exec Executor) error {
	_, err := r.RefreshResult(ctx, exec, RefreshReasonManual)
	return err
}

// Refresh synchronously reloads the registry from DescribeCompShareInstance.
// It records a low-cardinality failed sync event on transport or parse errors
// while preserving the last successful snapshot for best-effort reads.
func (r *EntityRegistry) Refresh(ctx context.Context, exec Executor, reason RefreshReason) error {
	_, err := r.RefreshResult(ctx, exec, reason)
	return err
}

// RefreshResult is Refresh plus the raw DescribeCompShareInstance result.
// Engine.Init uses the raw result for its existing prompt context while the
// registry records the same call as an observable Phase 0 snapshot.
func (r *EntityRegistry) RefreshResult(ctx context.Context, exec Executor, reason RefreshReason) (map[string]any, error) {
	result, err := exec.Execute(ctx, "DescribeCompShareInstance", map[string]any{"Limit": 100})
	if err != nil {
		r.recordRefreshFailure(err)
		return nil, err
	}
	if err := r.SyncFromDescribe(result, string(syncEventForReason(reason))); err != nil {
		r.recordRefreshFailure(err)
		return nil, err
	}
	return result, nil
}

// WarmRefresh starts one caller-triggered background refresh and returns its
// completion channel. The caller owns ctx timeout/cancellation; this method does
// not schedule periodic refreshes or retry internally.
func (r *EntityRegistry) WarmRefresh(ctx context.Context, exec Executor) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- r.Refresh(ctx, exec, RefreshReasonWarmCache)
		close(done)
	}()
	return done
}

// NeedsRefresh reports whether the current snapshot is missing, stale, failed,
// or explicitly invalidated by a successful state-changing action.
func (r *EntityRegistry) NeedsRefresh(at time.Time) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return needsRefresh(r.LastFullSync, r.LastSyncEvent, r.invalidated, at)
}

// needsRefresh is the single freshness rule shared by the live registry and its
// snapshot copy, so no caller re-assembles the TTL / failed / invalidated / never-
// synced conditions by hand.
func needsRefresh(lastFullSync time.Time, syncEvent string, invalidated bool, at time.Time) bool {
	if invalidated || lastFullSync.IsZero() {
		return true
	}
	if SyncEvent(syncEvent) == SyncEventFailed {
		return true
	}
	return at.Sub(lastFullSync) > DefaultRegistryFreshnessTTL
}

// NeedsRefreshAt is NeedsRefresh for an immutable snapshot: stale, failed, never-
// synced, or invalidated by a successful state-changing action after the copy.
func (s RegistrySnapshot) NeedsRefreshAt(at time.Time) bool {
	return needsRefresh(s.LastFullSync, s.SyncEvent, s.Invalidated, at)
}

// FreshAndCompleteAt reports whether the snapshot may be trusted as an existence
// oracle at `at`: it is fresh (not stale/failed/invalidated/never-synced) AND
// complete (not truncated). A hit in such a snapshot proves the id exists; a miss
// (with CanAssertAbsenceAt) proves it does not — no point-query needed. A stale or
// truncated snapshot is neither: it must be re-verified upstream.
func (s RegistrySnapshot) FreshAndCompleteAt(at time.Time) bool {
	return !s.NeedsRefreshAt(at) && !s.Truncated
}

// CanAssertAbsenceAt is CanAssertAbsence with freshness: only a fresh, complete
// snapshot may say an id is genuinely NOT in the account. A stale-but-complete
// registry (bug: a released instance can linger, or a new one be missing) can no
// longer assert absence — a miss there is "unverified", not "absent".
func (s RegistrySnapshot) CanAssertAbsenceAt(at time.Time) bool {
	if !s.FreshAndCompleteAt(at) {
		return false
	}
	return len(s.Instances) > 0 || s.TotalCount == 0
}

// MarkInvalidated records that a successful action changed instance inventory
// or state and the next registry consumer should refresh before trusting it.
func (r *EntityRegistry) MarkInvalidated(action string) bool {
	if !invalidatesRegistry(action) {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invalidated = true
	r.invalidation = action
	return true
}

func (r *EntityRegistry) SyncFromDescribe(result map[string]any, event string) error {
	if r == nil {
		return fmt.Errorf("entity registry is nil")
	}
	rawHosts, ok := result["UHostSet"].([]any)
	if !ok {
		return fmt.Errorf("DescribeCompShareInstance result missing UHostSet")
	}

	next := make(map[string]InstanceSnapshot, len(rawHosts))
	for _, raw := range rawHosts {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		inst := instanceFromMap(row)
		if inst.UHostId == "" {
			continue
		}
		next[inst.UHostId] = inst
	}

	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.Instances {
		if _, stillPresent := next[id]; !stillPresent {
			r.recentlyReleased[id] = now
		}
	}
	for id := range next {
		delete(r.recentlyReleased, id)
	}
	r.pruneRecentlyReleased(now)

	r.Instances = next
	r.rebuildNameIndexLocked()
	r.LastFullSync = now
	r.LastSyncEvent = event
	r.LastSyncError = ""
	r.TotalCount = intField(result, "TotalCount")
	r.Truncated = r.TotalCount > len(next)
	r.invalidated = false
	r.invalidation = ""
	return nil
}

func (r *EntityRegistry) rebuildNameIndexLocked() {
	r.NameIndex = make(map[string][]string)
	for id, inst := range r.Instances {
		key := normalizeName(inst.Name)
		if key == "" {
			continue
		}
		r.NameIndex[key] = append(r.NameIndex[key], id)
	}
	for key := range r.NameIndex {
		sort.Strings(r.NameIndex[key])
	}
}

func (r *EntityRegistry) pruneRecentlyReleased(now time.Time) {
	for id, releasedAt := range r.recentlyReleased {
		if now.Sub(releasedAt) > recentlyReleasedTTL {
			delete(r.recentlyReleased, id)
		}
	}
}

func copyInstances(in map[string]InstanceSnapshot) map[string]InstanceSnapshot {
	out := make(map[string]InstanceSnapshot, len(in))
	for id, inst := range in {
		// InstanceSnapshot is currently scalar-only. Deep-copy any reference
		// fields here if future registry domains add slices, maps, or pointers.
		out[id] = inst
	}
	return out
}

func copyNameIndex(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for key, ids := range in {
		copied := append([]string(nil), ids...)
		out[key] = copied
	}
	return out
}

func computeSnapshotID(instances map[string]InstanceSnapshot, totalCount int, truncated bool) string {
	items := make([]InstanceSnapshot, 0, len(instances))
	for _, inst := range instances {
		items = append(items, inst)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].UHostId < items[j].UHostId
	})
	payload := struct {
		Instances  []InstanceSnapshot `json:"instances"`
		TotalCount int                `json:"total_count"`
		Truncated  bool               `json:"truncated"`
	}{
		Instances:  items,
		TotalCount: totalCount,
		Truncated:  truncated,
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum[:8])
}

func (r *EntityRegistry) recordRefreshFailure(err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.LastSyncEvent = string(SyncEventFailed)
	if err != nil {
		r.LastSyncError = refreshErrorClass(err)
	}
}

func syncEventForReason(reason RefreshReason) SyncEvent {
	switch reason {
	case RefreshReasonInit:
		return SyncEventInit
	case RefreshReasonWarmCache:
		return SyncEventWarmCache
	default:
		return SyncEventSyncRefresh
	}
}

// invalidatesRegistry reports whether a completed action changed something this
// registry caches, and therefore must force a re-Describe rather than serve the
// snapshot taken before it ran.
//
// The bar is "does the action mutate a field of InstanceSnapshot", not "is the
// action a write" — ResetPassword and CreateCustomImage are writes that change
// nothing this cache holds, so they deliberately stay out. Every registered
// workflow must be classified one way or the other; TestEveryWorkflowActionIsClassifiedForInvalidation
// fails when a new one is added and nobody decides, because the failure mode of
// forgetting is silent (a stale snapshot served for up to
// DefaultRegistryFreshnessTTL), not loud.
func invalidatesRegistry(action string) bool {
	switch action {
	case "CreateCompShareInstance",
		"CreateInstanceWorkflow",
		"StartCompShareInstance",
		"StopCompShareInstance",
		"RebootCompShareInstance",
		"StartInstanceWorkflow",
		"StopInstanceWorkflow",
		"RebootInstanceWorkflow",
		"ModifyCompShareInstanceName",
		"RenameInstanceWorkflow",
		"UpdateCompShareStopScheduler",
		"DeleteCompShareStopScheduler",
		"SetStopSchedulerWorkflow",
		"CancelStopSchedulerWorkflow",
		// Resize rewrites GPU/GpuType/CPU/Memory and Reinstall rewrites
		// OsType/ImageType — all InstanceSnapshot fields. Both were absent here
		// while being registered workflows, so a resize or reinstall left the
		// cache serving the pre-change spec.
		"ResizeInstanceWorkflow",
		"ReinstallInstanceWorkflow":
		return true
	default:
		return false
	}
}

func refreshErrorClass(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline"):
		return "timeout"
	case strings.Contains(msg, "network") || strings.Contains(msg, "connection") || strings.Contains(msg, "eof"):
		return "network"
	case strings.Contains(msg, "uhostset") || strings.Contains(msg, "parse") || strings.Contains(msg, "decode"):
		return "parse_error"
	default:
		return "refresh_error"
	}
}
