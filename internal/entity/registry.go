package entity

import (
	"context"
	"fmt"
	"sort"
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
	Instances        map[string]InstanceSnapshot
	NameIndex        map[string][]string
	LastFullSync     time.Time
	LastSyncEvent    string
	TotalCount       int
	Truncated        bool
	recentlyReleased map[string]time.Time
	now              func() time.Time
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
	if r.LastFullSync.IsZero() {
		return 0
	}
	return r.now().Sub(r.LastFullSync)
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
	r.rebuildNameIndex()
	r.LastFullSync = now
	r.LastSyncEvent = event
	r.TotalCount = intField(result, "TotalCount")
	r.Truncated = r.TotalCount > len(next)
	return nil
}

func (r *EntityRegistry) rebuildNameIndex() {
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
