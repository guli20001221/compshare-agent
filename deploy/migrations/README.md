# HTTP/SSE PostgreSQL migrations

Apply these migrations manually before starting `compshare-agent server`.
The binary verifies required tables and columns at startup, but it does not auto-migrate.

Backend is **PostgreSQL** (migrated from MySQL/TiDB). These are PG-dialect DDL
(JSONB / TIMESTAMPTZ / `plpgsql` triggers) — apply with `psql`, **not** the mysql
client. The mysql client will error on this SQL.

**Deploy order is mandatory, and this is a two-stage cluster cutover.** Schema
compatibility does not make the old in-process executor compatible with the new
database-fenced executor. Never let both execute chat traffic for the same
conversation.

1. Apply every migration through `0010` while the existing backend keeps serving.
2. Deploy the new backend to every replica with `durable_turns: false`.
3. Verify no old backend replica remains, then drain chat traffic and stop all
   backend replicas.
4. Enable `durable_turns`, start all backend replicas, and release the matching
   durable-protocol frontend before restoring traffic.

Before step 4, set `COMPSHARE_TURN_SECRET_KEY` on every backend replica to the
same base64-encoded 32 random bytes. It encrypts short-lived workflow secrets
needed for crash recovery; never commit this value to YAML. Rotate it only after
all recoverable turns have finished.

Do not roll the flag one replica at a time. If a zero-downtime cutover is
required, isolate old and new traffic by protocol and conversation identity.

```bash
# DSN is the same libpq URL as the server's MYSQL_DSN (the env var name is kept
# for compatibility; the value is a PostgreSQL connection string). The target
# database must already exist (e.g. CREATE DATABASE compshare_agent;).
DSN='postgresql://user:pass@host:5432/compshare_agent?sslmode=disable'

psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0001_init.sql
psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0002_create_agent_traces.sql
psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0003_add_session_context_version.sql
psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0004_add_agent_traces_outcome_columns.sql
psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0005_create_turn_execution.sql
psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0006_create_turn_protocol.sql
psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0007_add_turn_recovery_context.sql
psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0008_add_turn_retry_policy.sql
psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0009_add_interaction_supersession.sql
psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0010_add_action_abandonment.sql

# 0011 belongs to the OPTIONAL SSH-ops lane, not to the cutover above. Its audit
# table is fail-closed, so a missing 0011 disables the lane silently — the server
# starts and logs the reason; it does not error.
psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0011_create_ssh_ops_audit.sql
psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0012_create_feishu_oauth_tokens.sql
psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0013_add_ssh_ops_context_observability.sql
```

`0012` is required only before enabling `agent.feishu.external_image_oauth`. It
creates `feishu_oauth_tokens`, which holds AES-GCM ciphertext for the rotating
delegated Feishu user token; it contains no plaintext access or refresh token.

`0013` is required with the contextual SSH-ops harness. It adds only aggregate
schema/coverage and command-class columns to `ssh_ops_audit`; it does not store
the injected user reports, platform fact values, credentials, or raw commands.

In the current production GitLab pipeline, clicking `deploy` does **not** run
SQL migrations. Before deploying a binary that writes `0013`'s columns, run the
manual `migrate-feishu-oauth` job from the same `main` pipeline first (the name
is historical; its migration pod applies every idempotent `*.sql` file from that
commit's image), then run `deploy`. The ordering matters: without `0013`, the
fail-closed SSH-ops audit INSERT refuses contextual diagnosis rather than
entering an unrecorded instance.

Every file is idempotent, so the whole list can be applied to any database
regardless of how far it already is — a new one, or a deployment still on 0004.
`TestMigrationsApplyTwiceCleanly` (internal/store, needs
`COMPSHARE_TEST_MYSQL_DSN`) applies them all twice and compares the resulting
schema, so a new migration that drops an `IF NOT EXISTS` fails the build rather
than the next upgrade. Keep it that way: `CREATE TRIGGER` has no `IF NOT EXISTS`
before PG 14, so precede one with `DROP TRIGGER IF EXISTS`, and precede every
`ADD CONSTRAINT` with `DROP CONSTRAINT IF EXISTS`.

For a throwaway local Docker PostgreSQL (no host `psql` needed), see README §3 —
it pipes each file through `docker exec -i cs-pg psql ...`.
