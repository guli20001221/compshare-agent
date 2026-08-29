package sshops

import (
	"context"
	"fmt"
)

// AuditEvent is one authorized SSH-ops attempt. It NEVER carries the credential — only the tenant
// identity, the target instance, the task text, and the outcome. The persisted row is the
// fail-closed record that an authorized in-instance repair session happened.
type AuditEvent struct {
	RequestUUID string
	// TurnID is the server-side turn identity; TaskHash is the sha256 of the raw task text.
	// Together they are the (turn_id, task_hash) UNIQUE key that stops a retry of the
	// SAME turn from re-entering the box (INV-9). The engine-side per-turn gate (INV-11) is the
	// primary defense against a one-word Task tweak; this DB key covers cross-turn/replay only.
	TurnID            string
	TaskHash          string
	TopOrganizationID uint32
	OrganizationID    uint32
	InstanceID        string
	Task              string
	Phase             string // "read_only" inspection or "read_write" repair authority
	// ContextSchemaVersion and ContextFactCoverage are aggregate observability
	// only. Begin records the requested context; Finish retains them only after
	// the harness confirms it constructed a model prompt containing that context. Neither stores
	// user reports or platform fact values in the audit table.
	ContextSchemaVersion int
	ContextFactCoverage  uint32
	CommandsRan          int
	CommandsRefused      int
	FirstCommandClass    string
	// Steps is the redacted, bounded per-command detail behind those counts, so an interrupted
	// run can be described by name rather than only by number. It is redacted by the producer
	// (summarizeAuditStepDetail), never by a writer, so no AuditWriter implementation — including
	// the in-memory one — ever holds a raw command. See PersistedStepSummary for what it is not.
	Steps       []PersistedStepSummary
	ExitCode    int
	TimedOut    bool
	OutputBytes int
	Disposition string // "started" | "ok" | "error"
	ErrClass    string // credential-free error class (e.g. "auth_failed"); "" on success
}

// AuditWriter records SSH-ops attempts. Begin MUST durably persist the attempt BEFORE the harness
// runs (fail-closed: no run without a record); Finish enriches the same row with the outcome.
type AuditWriter interface {
	Begin(ctx context.Context, ev AuditEvent) (id string, err error)
	Finish(ctx context.Context, id string, ev AuditEvent) error
}

// MemAuditWriter is an in-memory AuditWriter for unit tests and the live simulation. It records
// every Begin/Finish; FailBegin forces Begin to error so the fail-closed path can be exercised.
type MemAuditWriter struct {
	Events    []AuditEvent
	FailBegin bool
	seq       int
}

func (m *MemAuditWriter) Begin(_ context.Context, ev AuditEvent) (string, error) {
	if m.FailBegin {
		return "", fmt.Errorf("mem audit: forced begin failure")
	}
	m.seq++
	ev.Disposition = "started"
	m.Events = append(m.Events, ev)
	return fmt.Sprintf("mem-%d", m.seq), nil
}

func (m *MemAuditWriter) Finish(_ context.Context, _ string, ev AuditEvent) error {
	m.Events = append(m.Events, ev)
	return nil
}
