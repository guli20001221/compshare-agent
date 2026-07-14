# HTTP/SSE PostgreSQL migrations

Apply these migrations manually before starting `compshare-agent server`.
The binary verifies required tables and columns at startup, but it does not auto-migrate.

Backend is **PostgreSQL** (migrated from MySQL/TiDB). These are PG-dialect DDL
(JSONB / TIMESTAMPTZ / `plpgsql` triggers) — apply with `psql`, **not** the mysql
client. The mysql client will error on this SQL.

**Deploy order is mandatory: migration first, binary second.** A new binary
started against an un-migrated database will fail `VerifySchema` at boot —
do not flip the order during rolling deploys. Old binaries running against
a newer schema are compatible (they ignore unknown columns).

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
```

For a throwaway local Docker PostgreSQL (no host `psql` needed), see README §3 —
it pipes each file through `docker exec -i cs-pg psql ...`.
