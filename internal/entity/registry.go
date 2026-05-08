package entity

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

const recentlyReleasedTTL = 24 * time.Hour

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
	TotalCount       int
	Truncated        bool
	recentlyReleased map[string]time.Time
	now              func() time.Time
}

// RegistrySnapshot is an immutable copy of the registry state at one point in time.
// Mutating the returned maps must not affect the source EntityRegistry.
type RegistrySnapshot struct {
	SnapshotID   string
	Instances    map[string]InstanceSnapshot
	NameIndex    map[string][]string
	LastFullSync time.Time
	SyncEvent    string
	TotalCount   int
	Truncated    bool
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
		SnapshotID:   snapshotID,
		Instances:    instances,
		NameIndex:    nameIndex,
		LastFullSync: r.LastFullSync,
		SyncEvent:    r.LastSyncEvent,
		TotalCount:   r.TotalCount,
		Truncated:    r.Truncated,
	}
}

func (r *EntityRegistry) TraceState(now time.Time) RegistryTraceState {
	snap := r.Snapshot()
	if snap.LastFullSync.IsZero() {
		return RegistryTraceState{SyncEvent: "unavailable"}
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
	result, err := exec.Execute(ctx, "DescribeCompShareInstance", map[string]any{"Limit": 100})
	if err != nil {
		return err
	}
	return r.SyncFromDescribe(result, "describe_success")
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
	r.TotalCount = intField(result, "TotalCount")
	r.Truncated = r.TotalCount > len(next)
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
