package deployment

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

const (
	GPUInventoryPoolExclusive = "Exclusive"
	GPUInventoryPoolSpot      = "Spot"

	GPUInventorySourceOfficial = "official"
	GPUInventorySourcePod      = "pod"
)

// GPUInventorySnapshot is the merged, per-turn view of the two upstream GPU
// inventory backends. DescribeCompShareGpuInventory does not filter by zone:
// zone_id only selects the official or Pod implementation. Keeping that split
// here prevents an official-pool zero for a Pod zone from being presented as a
// real Pod inventory observation.
type GPUInventorySnapshot struct {
	catalog                 *ZoneCatalogSnapshot
	counts                  map[string]map[uint32]map[string]uint32 // pool -> zone id -> gpu -> cards
	sources                 map[string]GPUInventorySourceState
	spotUnsupportedGpuTypes map[string]struct{}
}

// GPUInventorySourceState distinguishes a successful empty response from a
// backend that was not queried or failed. A missing source must never collapse
// to a zero-card observation.
type GPUInventorySourceState struct {
	Attempted  bool
	Available  bool
	UpdateTime int64
}

// NewGPUInventorySnapshot merges one official response and one Pod response.
// officialAvailable/podAvailable mean the corresponding call completed and
// returned a structurally usable GpuInventory object. Rows from the wrong
// backend are discarded using the live ZoneCatalogSnapshot.
func NewGPUInventorySnapshot(
	catalog *ZoneCatalogSnapshot,
	officialRaw map[string]any,
	officialAttempted, officialAvailable bool,
	podRaw map[string]any,
	podAttempted, podAvailable bool,
) *GPUInventorySnapshot {
	s := &GPUInventorySnapshot{
		catalog:                 catalog,
		spotUnsupportedGpuTypes: inventoryStringSet(officialRaw["SpotUnsupportedGpuTypes"]),
		counts: map[string]map[uint32]map[string]uint32{
			GPUInventoryPoolExclusive: {},
			GPUInventoryPoolSpot:      {},
		},
		sources: map[string]GPUInventorySourceState{
			GPUInventorySourceOfficial: {
				Attempted: officialAttempted, Available: officialAvailable,
				UpdateTime: inventoryUpdateTime(officialRaw),
			},
			GPUInventorySourcePod: {
				Attempted: podAttempted, Available: podAvailable,
				UpdateTime: inventoryUpdateTime(podRaw),
			},
		},
	}
	if !catalog.Available() {
		return s
	}
	if officialAvailable {
		s.mergeSource(officialRaw, false)
	}
	if podAvailable {
		s.mergeSource(podRaw, true)
	}
	return s
}

// PodSelectorZoneID returns any live Pod zone id. The upstream action uses the
// value only to choose its Pod implementation; that implementation returns all
// Pod zones, not only the selected one.
func PodSelectorZoneID(catalog *ZoneCatalogSnapshot) (uint32, bool) {
	if !catalog.Available() {
		return 0, false
	}
	for _, zone := range catalog.Zones() {
		placement, ok := catalog.Placement(zone)
		if ok && placement.IsPod && placement.ZoneID != 0 {
			return placement.ZoneID, true
		}
	}
	return 0, false
}

// GPUInventoryPayloadAvailable reports whether a successful call returned the
// inventory object at all. An empty object is a valid successful-empty result;
// a missing or non-object field is unavailable, not an all-zero snapshot.
func GPUInventoryPayloadAvailable(raw map[string]any) bool {
	if raw == nil {
		return false
	}
	v := indirectValue(reflect.ValueOf(raw["GpuInventory"]))
	return v.IsValid() && v.Kind() == reflect.Map
}

// SourceState reports whether the authoritative backend for a source was
// actually observed this turn.
func (s *GPUInventorySnapshot) SourceState(source string) GPUInventorySourceState {
	if s == nil {
		return GPUInventorySourceState{}
	}
	return s.sources[strings.ToLower(strings.TrimSpace(source))]
}

// Counts returns every explicitly returned pool count for one zone/model.
// sourceAvailable=false means the authoritative backend was unavailable;
// present=false means it answered but omitted this exact zone/model.
func (s *GPUInventorySnapshot) Counts(zone string, gpuType string) (exclusive, spot uint32, present, sourceAvailable bool) {
	exclusive, exclusiveOK, exclusiveAvailable := s.PoolCount(zone, gpuType, GPUInventoryPoolExclusive)
	spot, spotOK, spotAvailable := s.PoolCount(zone, gpuType, GPUInventoryPoolSpot)
	return exclusive, spot, exclusiveOK || spotOK, exclusiveAvailable && spotAvailable
}

// PoolCount returns one explicit purchase pool independently. A missing pool
// row is different from an unavailable authoritative backend: callers can say
// "this zone currently exposes only Spot" without turning a failed API call
// into a zero.
func (s *GPUInventorySnapshot) PoolCount(zone, gpuType, pool string) (count uint32, present, sourceAvailable bool) {
	if s == nil || !s.catalog.Available() {
		return 0, false, false
	}
	placement, ok := s.catalog.Placement(zone)
	if !ok || placement.ZoneID == 0 {
		return 0, false, false
	}
	source := GPUInventorySourceOfficial
	if placement.IsPod {
		source = GPUInventorySourcePod
	}
	if !s.SourceState(source).Available {
		return 0, false, false
	}
	pool = canonicalInventoryPool(pool)
	if pool == "" {
		return 0, false, true
	}
	count, present = inventoryGPUCount(s.counts[pool], placement.ZoneID, gpuType)
	return count, present, true
}

// PoolSupported reports whether the requested purchase pool is a supported
// product mode for this exact zone/model. This is deliberately independent of
// PoolCount: a returned zero is an observed quantity, not proof that the mode
// is unsupported or that creation is impossible.
func (s *GPUInventorySnapshot) PoolSupported(zone, gpuType, pool string) (supported, known bool) {
	if s == nil || !s.catalog.Available() {
		return false, false
	}
	placement, ok := s.catalog.Placement(zone)
	if !ok {
		return false, false
	}
	return inventoryPoolSupport(s.ToResultMap(), placement, gpuType, pool)
}

// InventoryPoolSupportFromResult exposes the same product-mode contract to
// workflows after the typed snapshot has crossed the generic step-result
// boundary. Missing source data stays unknown; it never becomes unsupported.
func InventoryPoolSupportFromResult(result map[string]any, placement ZonePlacement, gpuType, pool string) (supported, known bool) {
	return inventoryPoolSupport(result, placement, gpuType, pool)
}

// ToResultMap produces the generic workflow step-result shape consumed by the
// guided form. It always returns fresh maps, so no caller can mutate the typed
// snapshot. Source status is included so future consumers do not have to infer
// "unavailable" from missing counts.
func (s *GPUInventorySnapshot) ToResultMap() map[string]any {
	result := map[string]any{
		"GpuInventory": map[string]any{
			GPUInventoryPoolExclusive: inventoryPoolToMap(nil),
			GPUInventoryPoolSpot:      inventoryPoolToMap(nil),
		},
		"InventorySources":        map[string]any{},
		"SpotUnsupportedGpuTypes": []string{},
	}
	if s == nil {
		return result
	}
	result["GpuInventory"] = map[string]any{
		GPUInventoryPoolExclusive: inventoryPoolToMap(s.counts[GPUInventoryPoolExclusive]),
		GPUInventoryPoolSpot:      inventoryPoolToMap(s.counts[GPUInventoryPoolSpot]),
	}
	sourceMap := result["InventorySources"].(map[string]any)
	for _, source := range []string{GPUInventorySourceOfficial, GPUInventorySourcePod} {
		state := s.SourceState(source)
		sourceMap[source] = map[string]any{
			"Attempted":  state.Attempted,
			"Available":  state.Available,
			"UpdateTime": state.UpdateTime,
		}
	}
	unsupported := make([]string, 0, len(s.spotUnsupportedGpuTypes))
	for gpuType := range s.spotUnsupportedGpuTypes {
		unsupported = append(unsupported, gpuType)
	}
	sort.Strings(unsupported)
	result["SpotUnsupportedGpuTypes"] = unsupported
	return result
}

func inventoryPoolSupport(result map[string]any, placement ZonePlacement, gpuType, pool string) (bool, bool) {
	pool = canonicalInventoryPool(pool)
	if result == nil || pool == "" || placement.ZoneID == 0 || strings.TrimSpace(gpuType) == "" {
		return false, false
	}
	source := GPUInventorySourceOfficial
	if placement.IsPod {
		source = GPUInventorySourcePod
	}
	if !inventorySourceAvailable(result, source) {
		return false, false
	}
	if !placement.IsPod {
		if pool == GPUInventoryPoolExclusive {
			return true, true
		}
		_, unsupported := inventoryStringSet(result["SpotUnsupportedGpuTypes"])[strings.ToLower(strings.TrimSpace(gpuType))]
		return !unsupported, true
	}

	_, requestedPresent := inventoryGPUCount(inventoryPool(result, pool), placement.ZoneID, gpuType)
	other := GPUInventoryPoolExclusive
	if pool == GPUInventoryPoolExclusive {
		other = GPUInventoryPoolSpot
	}
	_, otherPresent := inventoryGPUCount(inventoryPool(result, other), placement.ZoneID, gpuType)
	if requestedPresent {
		return true, true
	}
	if otherPresent {
		return false, true
	}
	return false, false
}

func inventorySourceAvailable(result map[string]any, source string) bool {
	root := indirectValue(reflect.ValueOf(result["InventorySources"]))
	entry := mapValueFold(root, source)
	available := mapValueFold(entry, "Available")
	return available.IsValid() && available.Kind() == reflect.Bool && available.Bool()
}

func inventoryStringSet(value any) map[string]struct{} {
	out := map[string]struct{}{}
	v := indirectValue(reflect.ValueOf(value))
	if !v.IsValid() || (v.Kind() != reflect.Slice && v.Kind() != reflect.Array) {
		return out
	}
	for i := 0; i < v.Len(); i++ {
		item := indirectValue(v.Index(i))
		if !item.IsValid() {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(fmt.Sprint(item.Interface())))
		if name != "" {
			out[name] = struct{}{}
		}
	}
	return out
}

func (s *GPUInventorySnapshot) mergeSource(raw map[string]any, pod bool) {
	for _, pool := range []string{GPUInventoryPoolExclusive, GPUInventoryPoolSpot} {
		for zoneID, gpuCounts := range inventoryPool(raw, pool) {
			placement, ok := inventoryPlacementByID(s.catalog, zoneID)
			if !ok || placement.IsPod != pod {
				continue
			}
			if s.counts[pool][zoneID] == nil {
				s.counts[pool][zoneID] = map[string]uint32{}
			}
			for gpuType, count := range gpuCounts {
				gpuType = strings.TrimSpace(gpuType)
				if gpuType != "" {
					s.counts[pool][zoneID][gpuType] = count
				}
			}
		}
	}
}

func inventoryPlacementByID(catalog *ZoneCatalogSnapshot, zoneID uint32) (ZonePlacement, bool) {
	if !catalog.Available() || zoneID == 0 {
		return ZonePlacement{}, false
	}
	for _, zone := range catalog.Zones() {
		placement, ok := catalog.Placement(zone)
		if ok && placement.ZoneID == zoneID {
			return placement, true
		}
	}
	return ZonePlacement{}, false
}

func inventoryGPUCount(pool map[uint32]map[string]uint32, zoneID uint32, gpuType string) (uint32, bool) {
	row := pool[zoneID]
	for name, count := range row {
		if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(gpuType)) {
			return count, true
		}
	}
	return 0, false
}

func canonicalInventoryPool(pool string) string {
	switch {
	case strings.EqualFold(strings.TrimSpace(pool), GPUInventoryPoolExclusive):
		return GPUInventoryPoolExclusive
	case strings.EqualFold(strings.TrimSpace(pool), GPUInventoryPoolSpot):
		return GPUInventoryPoolSpot
	default:
		return ""
	}
}

func inventoryPoolToMap(pool map[uint32]map[string]uint32) map[string]any {
	out := map[string]any{}
	for zoneID, gpuCounts := range pool {
		row := map[string]any{}
		for gpuType, count := range gpuCounts {
			row[gpuType] = count
		}
		out[strconv.FormatUint(uint64(zoneID), 10)] = row
	}
	return out
}

func inventoryUpdateTime(raw map[string]any) int64 {
	if raw == nil {
		return 0
	}
	n, _ := inventoryUint64(raw["UpdateTime"])
	return int64(n)
}

func inventoryPool(raw map[string]any, poolName string) map[uint32]map[string]uint32 {
	out := map[uint32]map[string]uint32{}
	if raw == nil {
		return out
	}
	root := reflect.ValueOf(raw["GpuInventory"])
	if !root.IsValid() || root.Kind() != reflect.Map {
		return out
	}
	pool := mapValueFold(root, poolName)
	if !pool.IsValid() || pool.Kind() != reflect.Map {
		return out
	}
	iter := pool.MapRange()
	for iter.Next() {
		zoneID, ok := inventoryUint64(iter.Key().Interface())
		if !ok || zoneID == 0 || zoneID > uint64(^uint32(0)) {
			continue
		}
		value := indirectValue(iter.Value())
		if !value.IsValid() || value.Kind() != reflect.Map {
			continue
		}
		row := map[string]uint32{}
		gpuIter := value.MapRange()
		for gpuIter.Next() {
			name := strings.TrimSpace(fmt.Sprint(gpuIter.Key().Interface()))
			count, ok := inventoryUint64(gpuIter.Value().Interface())
			if name == "" || !ok || count > uint64(^uint32(0)) {
				continue
			}
			row[name] = uint32(count)
		}
		out[uint32(zoneID)] = row
	}
	return out
}

func mapValueFold(m reflect.Value, key string) reflect.Value {
	m = indirectValue(m)
	if !m.IsValid() || m.Kind() != reflect.Map {
		return reflect.Value{}
	}
	iter := m.MapRange()
	for iter.Next() {
		if strings.EqualFold(strings.TrimSpace(fmt.Sprint(iter.Key().Interface())), key) {
			return indirectValue(iter.Value())
		}
	}
	return reflect.Value{}
}

func indirectValue(v reflect.Value) reflect.Value {
	for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer) {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

func inventoryUint64(value any) (uint64, bool) {
	v := indirectValue(reflect.ValueOf(value))
	if !v.IsValid() {
		return 0, false
	}
	switch v.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.Int() < 0 {
			return 0, false
		}
		return uint64(v.Int()), true
	case reflect.Float32, reflect.Float64:
		if v.Float() < 0 {
			return 0, false
		}
		return uint64(v.Float()), true
	case reflect.String:
		n, err := strconv.ParseUint(strings.TrimSpace(v.String()), 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}
