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
}

// RegistryTraceState is the compact entity registry block written into trace.v0.1.
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
	if r.invalidated || r.LastFullSync.IsZero() {
		return true
	}
	if r.LastSyncEvent == string(SyncEventFailed) {
		return true
	}
	return at.Sub(r.LastFullSync) > DefaultRegistryFreshnessTTL
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
		"CancelStopSchedulerWorkflow":
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
