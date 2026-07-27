package sshops

import (
	"context"
	"fmt"
)

// AuditEvent is one consented SSH-ops attempt. It NEVER carries the credential — only the tenant
// identity, the target instance, the task text, and the outcome. The persisted row is the
// fail-closed record that a human-consented, read-only in-instance access happened.
type AuditEvent struct {
	RequestUUID string
	// TurnID is the server-side turn identity; TaskHash is the sha256 of the raw task text.
	// Together they are the (turn_id, task_hash) UNIQUE key that stops a durable replay of the
	// SAME turn from re-entering the box (INV-9). The engine-side per-turn gate (INV-11) is the
	// primary defense against a one-word Task tweak; this DB key covers cross-turn/replay only.
	TurnID            string
	TaskHash          string
	TopOrganizationID uint32
	OrganizationID    uint32
	InstanceID        string
	Task              string
	Phase             string // "read_only" in Phase 1
	ExitCode          int
	TimedOut          bool
	OutputBytes       int
	Disposition       string // "started" | "ok" | "error"
	ErrClass          string // credential-free error class (e.g. "auth_failed"); "" on success
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
