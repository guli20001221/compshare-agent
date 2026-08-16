package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/sshops"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These run against a REAL PostgreSQL (the postgres-and-race job, or any local server exported as
// COMPSHARE_TEST_MYSQL_DSN). The unit tests beside them cover JSON encoding, redaction and the size
// ceiling — none of which touch a database — so until now nothing had ever executed the statement
// that actually stores the step detail. That gap hides two classes of defect a pure-Go test cannot
// see: a parameter whose driver encoding PostgreSQL will not accept for a JSONB column, and a
// migration that does not define what the writer names.

// openIsolatedSSHOpsAuditDB applies the named migrations into a throwaway schema and returns a
// connection scoped to it. The migration list is a PARAMETER rather than a fixed set because half of
// what these tests assert is what happens when part of it is missing.
func openIsolatedSSHOpsAuditDB(t *testing.T, migrations ...string) *sql.DB {
	t.Helper()
	dsn := os.Getenv("COMPSHARE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("COMPSHARE_TEST_MYSQL_DSN not set — skipping real PostgreSQL ssh_ops_audit integration test")
	}
	if strings.Contains(dsn, "117.50.198.43") {
		t.Fatal("refusing to run the integration test against the production PostgreSQL host")
	}

	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	require.NoError(t, admin.Ping())
	schema := "sshops_it_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
		_ = admin.Close()
	})

	u, err := url.Parse(dsn)
	require.NoError(t, err)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { _ = db.Close() })

	for _, name := range migrations {
		data, readErr := os.ReadFile(filepath.Join("..", "..", "deploy", "migrations", name))
		require.NoError(t, readErr)
		_, execErr := db.Exec(string(data))
		require.NoError(t, execErr, "apply %s", name)
	}
	return db
}

// sshOpsAuditMigrations is every migration touching the table, in order. 0012 is unrelated (Feishu
// tokens) and deliberately absent.
var sshOpsAuditMigrations = []string{
	"0011_create_ssh_ops_audit.sql",
	"0013_add_ssh_ops_context_observability.sql",
	"0014_add_ssh_ops_step_detail.sql",
}

func sshOpsTestEvent(turnID string) sshops.AuditEvent {
	return sshops.AuditEvent{
		RequestUUID: turnID, TurnID: turnID, TaskHash: strings.Repeat("a", 64),
		TopOrganizationID: 66391350, OrganizationID: 64404856,
		InstanceID: "uhost-test", Task: "ComfyUI 打不开", Phase: "read_write",
		ContextSchemaVersion: 2, ContextFactCoverage: 7,
	}
}

// TestSSHOpsAuditStore_StepDetailLandsAsRealJSONB executes the write nothing else does. The Go-side
// tests prove the payload is well-formed JSON; only PostgreSQL can say whether the parameter arrives
// in a form a JSONB column accepts, and a []byte parameter is exactly the shape a driver may choose
// to send as a bytea hex escape — which would store nothing, or fail the whole Finish, while every
// unit test stayed green.
func TestSSHOpsAuditStore_StepDetailLandsAsRealJSONB(t *testing.T) {
	db := openIsolatedSSHOpsAuditDB(t, sshOpsAuditMigrations...)
	ctx := context.Background()
	audit := NewSSHOpsAuditStore(db)

	id, err := audit.Begin(ctx, sshOpsTestEvent("turn-jsonb"))
	require.NoError(t, err)

	exit := 0
	done := sshOpsTestEvent("turn-jsonb")
	done.Disposition = "ok"
	done.CommandsRan, done.CommandsRefused, done.FirstCommandClass = 2, 1, "targeted_validation"
	done.Steps = []sshops.PersistedStepSummary{
		{Command: "df -h /", Tier: "read_only", Disposition: "ran", ExitCode: &exit, Bytes: 128},
		{Command: "rm -rf /root/.cache/pip", Tier: "mutating", Disposition: "ran", ExitCode: &exit, Bytes: 0},
		{Command: "systemctl restart vllm", Tier: "mutating", Disposition: "refused", Reason: "refused_client_disconnect"},
	}
	require.NoError(t, audit.Finish(ctx, id, done))

	// Ask PostgreSQL to interpret the column, not just hand it back. jsonb_array_length and the ->>
	// path operators only work if the server parsed it AS json — a bytea-escaped or double-encoded
	// value fails here even though SELECT steps::text would have looked plausible.
	var (
		typ      string
		length   int
		firstCmd string
		reason   string
		exitCode int
		hasExit  bool
	)
	require.NoError(t, db.QueryRowContext(ctx, `
SELECT pg_typeof(steps)::text,
       jsonb_array_length(steps),
       steps->0->>'cmd',
       steps->2->>'reason',
       (steps->0->>'exit')::int,
       jsonb_exists(steps->2, 'exit')
FROM ssh_ops_audit WHERE id = $1`, id).Scan(&typ, &length, &firstCmd, &reason, &exitCode, &hasExit))

	require.Equal(t, "jsonb", typ)
	require.Equal(t, 3, length)
	require.Equal(t, "df -h /", firstCmd)
	require.Equal(t, "refused_client_disconnect", reason, "the fine-grained refusal reason must survive the write")
	require.Equal(t, 0, exitCode)
	require.False(t, hasExit, "a refused command never produced an exit status; storing one would read as success")

	// And the whole thing decodes back into the type it was written from, which is what any future
	// reader will actually do.
	var raw []byte
	require.NoError(t, db.QueryRowContext(ctx, `SELECT steps FROM ssh_ops_audit WHERE id = $1`, id).Scan(&raw))
	var back []sshops.PersistedStepSummary
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, done.Steps, back)

	// The counts must agree with the detail in the SAME row: two records of one run that disagree are
	// worse than one, because a reader cannot tell which is stale.
	var ran, refused int
	var disposition string
	var finished sql.NullTime
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT commands_ran, commands_refused, disposition, finished_at FROM ssh_ops_audit WHERE id = $1`, id).
		Scan(&ran, &refused, &disposition, &finished))
	require.Equal(t, 2, ran)
	require.Equal(t, 1, refused)
	require.Equal(t, "ok", disposition)
	require.True(t, finished.Valid)
}

// A run that recorded no steps must store SQL NULL, not '[]'. "No steps" and "this row predates the
// column" should look the same on read — commands_ran already separates them — and a query written
// against one spelling breaks silently on the other.
func TestSSHOpsAuditStore_NoStepsStoresNullNotEmptyArray(t *testing.T) {
	db := openIsolatedSSHOpsAuditDB(t, sshOpsAuditMigrations...)
	ctx := context.Background()
	audit := NewSSHOpsAuditStore(db)

	id, err := audit.Begin(ctx, sshOpsTestEvent("turn-nosteps"))
	require.NoError(t, err)
	done := sshOpsTestEvent("turn-nosteps")
	done.Disposition = "error"
	done.ErrClass = "auth_failed"
	require.NoError(t, audit.Finish(ctx, id, done))

	var isNull bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT steps IS NULL FROM ssh_ops_audit WHERE id = $1`, id).Scan(&isNull))
	require.True(t, isNull, "no steps must be NULL, not an empty JSON array")
}

// TestSSHOpsAuditSchemaProbeStopsTheLaneBeforeAnUnrecordableRun demonstrates the failure the boot
// probe prevents, against a real un-migrated database rather than a description of one.
//
// The dangerous property is that Begin names only 0011 columns: on a database missing 0013 or 0014
// the fail-closed record is written and the harness runs — with allow_writes it can change the
// instance — and only Finish fails, taking the disposition, err_class and counts with it and leaving
// the row at 'started'. The probe is what turns that into a boot error.
func TestSSHOpsAuditSchemaProbeStopsTheLaneBeforeAnUnrecordableRun(t *testing.T) {
	ctx := context.Background()

	t.Run("no table at all", func(t *testing.T) {
		db := openIsolatedSSHOpsAuditDB(t)
		err := VerifySSHOpsAuditSchema(ctx, db)
		require.Error(t, err)
		require.Contains(t, err.Error(), "ssh_ops_audit")
	})

	t.Run("0013 missing", func(t *testing.T) {
		// Not only about `steps`: a probe that covered the new column alone would pass on a database
		// the writer still cannot use.
		db := openIsolatedSSHOpsAuditDB(t, "0011_create_ssh_ops_audit.sql", "0014_add_ssh_ops_step_detail.sql")
		require.Error(t, VerifySSHOpsAuditSchema(ctx, db))
	})

	t.Run("0014 missing leaves a started row nobody can finish", func(t *testing.T) {
		db := openIsolatedSSHOpsAuditDB(t, "0011_create_ssh_ops_audit.sql", "0013_add_ssh_ops_context_observability.sql")

		err := VerifySSHOpsAuditSchema(ctx, db)
		require.Error(t, err, "the probe must refuse the boot")
		require.Contains(t, err.Error(), "0014", "the operator has to be told which migration to run")

		// Now the failure itself, with the probe bypassed as an un-probed binary would.
		audit := NewSSHOpsAuditStore(db)
		id, beginErr := audit.Begin(ctx, sshOpsTestEvent("turn-unmigrated"))
		require.NoError(t, beginErr, "Begin succeeds — which is why this is not self-correcting")

		exit := 0
		done := sshOpsTestEvent("turn-unmigrated")
		done.Disposition = "ok"
		done.CommandsRan = 1
		done.Steps = []sshops.PersistedStepSummary{{Command: "rm -rf /root/.cache/pip", Tier: "mutating", Disposition: "ran", ExitCode: &exit}}
		require.Error(t, audit.Finish(ctx, id, done))

		var disposition string
		var finished sql.NullTime
		var ran sql.NullInt64
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT disposition, finished_at, commands_ran FROM ssh_ops_audit WHERE id = $1`, id).
			Scan(&disposition, &finished, &ran))
		require.Equal(t, "started", disposition, "the box was entered and the row still says the run never ended")
		require.False(t, finished.Valid)
		require.Zero(t, ran.Int64, "even the counts are lost — the detail column takes the whole UPDATE down with it")
	})

	t.Run("fully migrated", func(t *testing.T) {
		db := openIsolatedSSHOpsAuditDB(t, sshOpsAuditMigrations...)
		require.NoError(t, VerifySSHOpsAuditSchema(ctx, db))
	})
}
