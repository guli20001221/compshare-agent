# PostgreSQL migrations

The binary never auto-migrates. Apply every file in lexical order with `psql`
before deploying the matching binary:

```bash
for migration in deploy/migrations/*.sql; do
  psql "$DSN" -X -v ON_ERROR_STOP=1 -f "$migration"
done
```

The `mysql` config key and `MYSQL_DSN` environment name are historical; the
value is a PostgreSQL/libpq URL. All migrations are idempotent and remain in the
sequence after their runtime consumer is retired, because deployed databases
may already contain their schema.

| Migration | Purpose |
|---|---|
| `0001_init.sql` | sessions and messages |
| `0002_create_agent_traces.sql` | completed-turn traces |
| `0003_add_session_context_version.sql` | optimistic session-context version |
| `0004_add_agent_traces_outcome_columns.sql` | trace outcome columns |
| `0005_create_turn_execution.sql` | retired durable-turn schema (history only) |
| `0006_create_turn_protocol.sql` | retired durable-turn schema (history only) |
| `0007_add_turn_recovery_context.sql` | retired durable-turn schema (history only) |
| `0008_add_turn_retry_policy.sql` | retired durable-turn schema (history only) |
| `0009_add_interaction_supersession.sql` | retired durable-turn schema (history only) |
| `0010_add_action_abandonment.sql` | retired durable-turn schema (history only) |
| `0011_create_ssh_ops_audit.sql` | SSH-ops audit |
| `0012_create_feishu_oauth_tokens.sql` | encrypted Feishu delegated tokens |
| `0013_add_ssh_ops_context_observability.sql` | SSH context/audit aggregates |
| `0014_add_ssh_ops_step_detail.sql` | redacted SSH step summaries |

SSH-ops requires `0011`, `0013` and `0014`. At boot the lane probes every
column its writer uses; an incomplete audit schema disables only SSH-ops and
keeps chat serving. After applying a missing migration, restart or redeploy the
same image because the probe is boot-only. `0012` is required before enabling
Feishu external-image OAuth.

In GitLab, run the `migrate-feishu-oauth` manual job before `deploy`. Its name is
retained for pipeline compatibility; the migration pod applies every `*.sql`
file from the current image.

`TestMigrationsApplyTwiceCleanly` applies the complete sequence twice against a
real PostgreSQL when `COMPSHARE_TEST_MYSQL_DSN` is set. New migrations must keep
that property.
