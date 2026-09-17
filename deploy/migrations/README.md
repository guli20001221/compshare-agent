# PostgreSQL migrations

The binary never auto-migrates. Apply every file in lexical order with `psql`
before deploying the matching binary:

```bash
for migration in deploy/migrations/*.sql; do
  psql "$DSN" -X -v ON_ERROR_STOP=1 -f "$migration"
done
```

The `mysql` config key and `MYSQL_DSN` environment name are historical; the
value is a PostgreSQL/libpq URL. All migrations are idempotent. Schema that no
running code reads or writes is dropped by a later migration, so the sequence
never needs the files that created it.

| Migration | Purpose |
|---|---|
| `0001_init.sql` | sessions and messages |
| `0002_create_agent_traces.sql` | completed-turn traces |
| `0003_add_session_context_version.sql` | optimistic session-context version |
| `0004_add_agent_traces_outcome_columns.sql` | trace outcome columns |
| `0011_create_ssh_ops_audit.sql` | SSH-ops audit |
| `0012_create_feishu_oauth_tokens.sql` | encrypted Feishu delegated tokens |
| `0013_add_ssh_ops_context_observability.sql` | SSH context/audit aggregates |
| `0014_add_ssh_ops_step_detail.sql` | redacted SSH step summaries |
| `0015_drop_unused_storage.sql` | drops the retired durable-turn tables and four trace columns nothing writes |

Numbers 0005–0010 created the durable-turn tables `0015` drops; their files are
gone and the numbers stay unused.

SSH-ops requires `0011`, `0013` and `0014`. At boot the lane probes every
column its writer uses; an incomplete audit schema disables only SSH-ops and
keeps chat serving. After applying a missing migration, restart or redeploy the
same image because the probe is boot-only. `0012` is required before enabling
Feishu external-image OAuth.

In GitLab, run the `migrate-database` manual job before `deploy`; its pod
applies every `*.sql` file from the current image. `0015` is the one exception
to that order: it drops columns the previous binary still names in its trace
INSERT, so deploy first and migrate afterwards (migrating first only loses the
traces of the minutes in between).

`TestMigrationsApplyTwiceCleanly` applies the complete sequence twice against a
real PostgreSQL when `COMPSHARE_TEST_MYSQL_DSN` is set. New migrations must keep
that property.
