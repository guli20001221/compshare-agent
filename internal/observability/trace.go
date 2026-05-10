package observability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/compshare-agent/internal/security"
)

const SchemaVersion = "trace.v0.2"

const (
	ToolSourceMainReAct         = "main_react"
	ToolSourceWorkflowInternal  = "workflow_internal"
	ToolSourceDiagnosisInternal = "diagnosis_internal"
	ToolSourceKnowledgeLocal    = "knowledge_local"
	ToolSourceInitContext       = "init_context"
	ToolSourceShadowOnly        = "shadow_only"
	ToolSourcePlannerHandler    = "planner_handler"
)

const (
	ToolStatusSuccess = "success"
	ToolStatusError   = "error"
)

const (
	ToolCappedTargets   = "targets"
	ToolCappedWindow    = "window"
	ToolCappedRateLimit = "rate_limit"
)

const DefaultTraceDir = "logs"

const DefaultTraceFilePerm os.FileMode = 0o600

const DefaultTraceRetentionDays = 30

type WriterOptions struct {
	Dir string
	Now func() time.Time
}

type Writer struct {
	dir string
	now func() time.Time
}

type TraceRecord struct {
	SchemaVersion   string               `json:"schema_version"`
	TraceID         string               `json:"trace_id"`
	TurnID          string               `json:"turn_id"`
	TurnIndex       int                  `json:"turn_index"`
	Timestamp       string               `json:"timestamp"`
	UserMsgHash     string               `json:"user_msg_hash"`
	Runtime         RuntimeTrace         `json:"runtime"`
	Planner         PlannerTrace         `json:"planner"`
	EngineHardBlock EngineHardBlockTrace `json:"engine_hard_block"`
	EntityRegistry  EntityRegistryTrace  `json:"entity_registry"`
	ToolCalls       []ToolCallTrace      `json:"tool_calls"`
	Renderer        RendererTrace        `json:"renderer"`
	Freshness       FreshnessTrace       `json:"freshness"`
	RateLimit       RateLimitTrace       `json:"rate_limit"`
	Retrieval       RetrievalTrace       `json:"retrieval"`
	Outcome         OutcomeTrace         `json:"outcome"`
}

type RuntimeTrace struct {
	PlannerMode    string   `json:"planner_mode"`
	CutoverIntents []string `json:"cutover_intents"`
}

type PlannerTrace struct {
	Enabled       bool         `json:"enabled"`
	Model         string       `json:"model"`
	LatencyMS     int64        `json:"latency_ms"`
	InputTokens   int          `json:"input_tokens"`
	OutputTokens  int          `json:"output_tokens"`
	SchemaValid   bool         `json:"schema_valid"`
	Intent        string       `json:"intent"`
	Slots         PlannerSlots `json:"slots"`
	Confidence    float64      `json:"confidence"`
	HardBlockHint bool         `json:"hard_block_hint"`
	CutoverStatus string       `json:"cutover_status"`
}

type PlannerSlots struct {
	TargetRefs []any    `json:"target_refs"`
	Metrics    []string `json:"metrics"`
	TimeWindow any      `json:"time_window"`
}

type EngineHardBlockTrace struct {
	Hit      bool   `json:"hit"`
	Category string `json:"category"`
}

type EntityRegistryTrace struct {
	SnapshotID string `json:"snapshot_id"`
	AgeSeconds int64  `json:"age_seconds"`
	SyncEvent  string `json:"sync_event"`
}

type ToolCallTrace struct {
	ID               string `json:"id"`
	TurnIndex        int    `json:"turn_index"`
	Action           string `json:"action"`
	Source           string `json:"source"`
	ArgsHash         string `json:"args_hash"`
	LatencyMS        int64  `json:"latency_ms"`
	Attempts         int    `json:"attempts"`
	Status           string `json:"status"`
	ErrorClass       string `json:"error_class"`
	ResultHash       string `json:"result_hash"`
	Capped           string `json:"capped"`
	CapReason        string `json:"cap_reason"`
	RequestedTargets int    `json:"requested_targets"`
	ExecutedTargets  int    `json:"executed_targets"`
	WindowSeconds    int    `json:"window_seconds"`
}

type RendererTrace struct {
	Model              string   `json:"model"`
	LatencyMS          int64    `json:"latency_ms"`
	AttributionMode    string   `json:"attribution_mode"`
	InputToolCallIDs   []string `json:"input_tool_call_ids"`
	InputToolArgHashes []string `json:"input_tool_args_hashes"`
}

type FreshnessTrace struct {
	MonitorCallInCurrentTurn bool `json:"monitor_call_in_current_turn"`
}

type RateLimitTrace struct {
	Checked      bool   `json:"checked"`
	Allowed      bool   `json:"allowed"`
	Class        string `json:"class"`
	Action       string `json:"action"`
	Reason       string `json:"reason"`
	SubjectHash  string `json:"subject_hash"`
	RetryAfterMS int64  `json:"retry_after_ms"`
}

type RetrievalTrace struct {
	Enabled   bool   `json:"enabled"`
	KBVersion string `json:"kb_version"`
	Hits      int    `json:"hits"`
}

type OutcomeTrace struct {
	TotalLatencyMS             int64 `json:"total_latency_ms"`
	TotalTokens                int   `json:"total_tokens"`
	AttemptedHallucinatedCount int   `json:"attempted_hallucinated_count"`
	EscapedHallucinatedCount   int   `json:"escaped_hallucinated_count"`
	KBConflictCount            int   `json:"kb_conflict_count"`
}

func NewWriter(opts WriterOptions) (*Writer, error) {
	dir := opts.Dir
	if dir == "" {
		dir = DefaultTraceDir
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create trace dir: %w", err)
	}
	return &Writer{dir: dir, now: now}, nil
}

func (w *Writer) Dir() string {
	return w.dir
}

func (w *Writer) Append(record TraceRecord) error {
	now := w.now()
	record = record.withDefaults(now)
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal trace record: %w", err)
	}
	path := filepath.Join(w.dir, traceFileName(now))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, DefaultTraceFilePerm)
	if err != nil {
		return fmt.Errorf("open trace file: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write trace line: %w", err)
	}
	return nil
}

func HashTracePayload(v any) (string, error) {
	data, err := canonicalTraceJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func canonicalTraceJSON(v any) ([]byte, error) {
	redacted := security.RedactForTrace(v)
	data, err := json.Marshal(redacted)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical trace payload: %w", err)
	}
	return data, nil
}

func traceFileName(t time.Time) string {
	return "agent-trace-" + t.Format("2006-01-02") + ".jsonl"
}

func Cleanup(dir string, retentionDays int, now time.Time) error {
	if dir == "" {
		dir = DefaultTraceDir
	}
	if retentionDays <= 0 {
		retentionDays = DefaultTraceRetentionDays
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read trace dir: %w", err)
	}
	cutoff := dateOnly(now).AddDate(0, 0, -retentionDays)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		traceDate, ok := traceFileDate(entry.Name())
		if !ok || !traceDate.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("remove expired trace file %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func traceFileDate(name string) (time.Time, bool) {
	const prefix = "agent-trace-"
	const suffix = ".jsonl"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return time.Time{}, false
	}
	dateText := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	traceDate, err := time.Parse("2006-01-02", dateText)
	if err != nil {
		return time.Time{}, false
	}
	return traceDate, true
}

func dateOnly(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, t.Location())
}

func (r TraceRecord) withDefaults(now time.Time) TraceRecord {
	if r.SchemaVersion == "" {
		r.SchemaVersion = SchemaVersion
	}
	if r.Timestamp == "" {
		r.Timestamp = now.Format(time.RFC3339)
	}
	if r.Runtime.CutoverIntents == nil {
		r.Runtime.CutoverIntents = []string{}
	}
	if r.Planner.Slots.TargetRefs == nil {
		r.Planner.Slots.TargetRefs = []any{}
	}
	if r.Planner.Slots.Metrics == nil {
		r.Planner.Slots.Metrics = []string{}
	}
	if r.ToolCalls == nil {
		r.ToolCalls = []ToolCallTrace{}
	}
	if r.Renderer.InputToolCallIDs == nil {
		r.Renderer.InputToolCallIDs = []string{}
	}
	if r.Renderer.InputToolArgHashes == nil {
		r.Renderer.InputToolArgHashes = []string{}
	}
	if r.EntityRegistry.SyncEvent == "" {
		r.EntityRegistry.SyncEvent = "unavailable"
	}
	return r
}
