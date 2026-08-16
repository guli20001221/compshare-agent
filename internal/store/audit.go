package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"
	"unicode/utf8"

	"github.com/compshare-agent/internal/guardrails"
	"github.com/compshare-agent/internal/sshops"
	"github.com/google/uuid"
)

// SSHOpsAuditStore is the synchronous, fail-closed AuditWriter backing the consent-gated SSH-ops
// lane (COMPSHARE_SSH_OPS). It implements sshops.AuditWriter against the ssh_ops_audit table
// (deploy/migrations/0011 + 0013). The SSH credential is NEVER written — only tenant identity, the target
// instance, the (PII-redacted) task text, aggregate context coverage, and the outcome. Begin's row
// carries the REQUESTED context coverage; Finish overwrites it with what was applied, clearing it when
// the harness could not confirm the model turn began on that context — so only a row with finished_at
// set states delivery, and a query ignoring that over-reports. Begin must succeed before the harness
// runs, so a missing/unreachable table — or a UNIQUE(turn_id, task_hash) replay collision (INV-9) —
// refuses the diagnosis rather than running it unlogged.
type SSHOpsAuditStore struct {
	db *sql.DB
}

func NewSSHOpsAuditStore(db *sql.DB) *SSHOpsAuditStore { return &SSHOpsAuditStore{db: db} }

// VerifySSHOpsAuditSchema refuses to let the lane boot against a database missing any column Begin
// or Finish names. Callers turn its error into a boot failure (cmd/instance_ops.go), the same as
// every other SSH-ops misconfiguration.
//
// It is deliberately NOT part of VerifySchema. The lane is optional — like agent_traces and
// VerifyTraceSchema — and a deployment that never enables it must not be blocked on its migrations.
//
// The reason it exists at all is that the failure it catches is silent, late, and lands on the wrong
// side of the safety boundary. Begin names only the 0011 columns, so against a database missing 0013
// or 0014 the fail-closed record is written, the harness RUNS — and under allow_writes it can change
// the instance — and only then does Finish's single UPDATE error on the missing column. That loses
// the disposition, the error class and the counts in one go and orphans the row at 'started': the
// exact state the detached-context Finish exists to prevent, reached by a different route. A deploy
// note ordering the migration before the binary is a procedure; this is the check that it happened.
func VerifySSHOpsAuditSchema(ctx context.Context, db *sql.DB) error {
	// One statement naming every column the writer touches, in the style of VerifySchema's other
	// "contract" probes: a probe narrower than the writer is a probe that passes on a database the
	// writer cannot use.
	rows, err := db.QueryContext(ctx, `
SELECT id, request_uuid, turn_id, task_hash, top_organization_id, organization_id, instance_id,
       task, phase, disposition, exit_code, timed_out, output_bytes, err_class,
       context_schema_version, context_fact_coverage,
       commands_ran, commands_refused, first_command_class, steps,
       started_at, finished_at
FROM ssh_ops_audit LIMIT 0`)
	if err != nil {
		return fmt.Errorf("verify schema ssh_ops_audit (apply deploy/migrations 0011, 0013 and 0014 before this binary): %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("verify schema ssh_ops_audit close: %w", err)
	}
	return nil
}

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
    (id, request_uuid, turn_id, task_hash, top_organization_id, organization_id, instance_id, task, phase,
     context_schema_version, context_fact_coverage, disposition, started_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'started', now())`,
		id, ev.RequestUUID, ev.TurnID, ev.TaskHash, ev.TopOrganizationID, ev.OrganizationID, ev.InstanceID,
		truncateAuditTask(guardrails.RedactPII(ev.Task), 4000), ev.Phase, ev.ContextSchemaVersion, ev.ContextFactCoverage)
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
	steps, err := marshalAuditSteps(ev.Steps)
	if err != nil {
		// Never fail the outcome over the detail: the counts, disposition and err_class are what a
		// query relies on, and losing them to keep a nicer column would trade the record for the
		// annotation. Write NULL steps and say why.
		log.Printf("ssh-ops: audit step detail dropped for row %s (counts still written): %v", id, err)
		steps = nil
	}
	_, err = s.db.ExecContext(ctx, `
UPDATE ssh_ops_audit
SET disposition = $2, exit_code = $3, timed_out = $4, output_bytes = $5, err_class = $6,
    context_schema_version = $7, context_fact_coverage = $8,
    commands_ran = $9, commands_refused = $10, first_command_class = $11, steps = $12,
    finished_at = now()
WHERE id = $1`,
		id, ev.Disposition, ev.ExitCode, ev.TimedOut, ev.OutputBytes, ev.ErrClass,
		ev.ContextSchemaVersion, ev.ContextFactCoverage,
		ev.CommandsRan, ev.CommandsRefused, ev.FirstCommandClass, steps)
	if err != nil {
		return fmt.Errorf("ssh_ops_audit finish: %w", err)
	}
	return nil
}

// marshalAuditSteps renders the per-command detail for the JSONB column, or nil for none.
// nil becomes SQL NULL rather than '[]' so "this run recorded no steps" and "this row predates the
// column" look the same on read — which they should, since commands_ran already separates them.
func marshalAuditSteps(steps []sshops.PersistedStepSummary) ([]byte, error) {
	if len(steps) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(steps)
	if err != nil {
		return nil, fmt.Errorf("marshal ssh_ops_audit steps: %w", err)
	}
	// A last-resort ceiling on the write. The producer already bounds rows (maxAuditStepRows) and
	// each command (maxAuditStepCommandRunes); this catches the case where those bounds are later
	// loosened without anyone thinking about the UPDATE, because a Finish that fails on payload
	// size loses the OUTCOME, not just the detail.
	if len(encoded) > maxAuditStepsBytes {
		return nil, fmt.Errorf("ssh_ops_audit steps payload %d bytes exceeds %d", len(encoded), maxAuditStepsBytes)
	}
	return encoded, nil
}

// maxAuditStepsBytes is generous against the producer's own bounds (120 rows x ~200 runes is well
// under it) precisely so that reaching it means a bound moved, not that a run was unusually long.
const maxAuditStepsBytes = 256 * 1024

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
