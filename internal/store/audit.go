package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/compshare-agent/internal/guardrails"
	"github.com/compshare-agent/internal/sshops"
	"github.com/google/uuid"
)

// SSHOpsAuditStore is the synchronous, fail-closed AuditWriter backing the consent-gated SSH-ops
// lane (COMPSHARE_SSH_OPS). It implements sshops.AuditWriter against the ssh_ops_audit table
// (deploy/migrations/0011). The SSH credential is NEVER written — only tenant identity, the target
// instance, the (PII-redacted) task text, and the outcome. Begin must succeed before the harness
// runs, so a missing/unreachable table — or a UNIQUE(turn_id, task_hash) replay collision (INV-9) —
// refuses the diagnosis rather than running it unlogged.
type SSHOpsAuditStore struct {
	db *sql.DB
}

func NewSSHOpsAuditStore(db *sql.DB) *SSHOpsAuditStore { return &SSHOpsAuditStore{db: db} }

// Begin inserts the "started" row and returns its id. A short per-write timeout keeps a slow or
// down database from hanging the (already time-bounded) ops task.
func (s *SSHOpsAuditStore) Begin(ctx context.Context, ev sshops.AuditEvent) (string, error) {
	id := uuid.NewString()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// INV-4: the task is free-form operator/model text — redact PII before it is persisted. The
	// task_hash column is the raw-task sha256 the caller computed (the INV-9 dedup identity); it is
	// stored verbatim and, being a hash, carries nothing sensitive. UNIQUE(turn_id, task_hash) makes
	// a durable replay of the same turn fail here, which the fail-closed caller turns into a refusal.
	_, err := s.db.ExecContext(ctx, `
INSERT INTO ssh_ops_audit
    (id, request_uuid, turn_id, task_hash, top_organization_id, organization_id, instance_id, task, phase, disposition, started_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'started', now())`,
		id, ev.RequestUUID, ev.TurnID, ev.TaskHash, ev.TopOrganizationID, ev.OrganizationID, ev.InstanceID,
		truncateAuditTask(guardrails.RedactPII(ev.Task), 4000), ev.Phase)
	if err != nil {
		return "", fmt.Errorf("ssh_ops_audit begin: %w", err)
	}
	return id, nil
}

// Finish enriches the row with the outcome. A failure here does not lose the verdict (the access
// is already recorded by Begin), but it is returned so the caller can log it.
func (s *SSHOpsAuditStore) Finish(ctx context.Context, id string, ev sshops.AuditEvent) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `
UPDATE ssh_ops_audit
SET disposition = $2, exit_code = $3, timed_out = $4, output_bytes = $5, err_class = $6, finished_at = now()
WHERE id = $1`,
		id, ev.Disposition, ev.ExitCode, ev.TimedOut, ev.OutputBytes, ev.ErrClass)
	if err != nil {
		return fmt.Errorf("ssh_ops_audit finish: %w", err)
	}
	return nil
}

func truncateAuditTask(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Trim back to a valid UTF-8 boundary at or below n bytes. A hard s[:n] can split a multibyte
	// (CJK) task mid-rune, and PostgreSQL rejects invalid UTF-8 — that would fail the audit INSERT and,
	// because the audit is fail-closed, refuse an otherwise-valid diagnosis.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
