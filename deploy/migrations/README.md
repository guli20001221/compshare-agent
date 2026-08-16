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

# 0011/0013/0014 belong to the OPTIONAL SSH-ops lane, not to the cutover above.
# They are required as a SET whenever the lane is enabled: since 2026-08-16 the
# server probes ssh_ops_audit for every column its writer names and DISABLES THE
# LANE (loudly, still serving chat) if any is missing — see "boot probe" below.
# With the lane off they are not probed and not needed.
psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0011_create_ssh_ops_audit.sql
psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0012_create_feishu_oauth_tokens.sql
psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0013_add_ssh_ops_context_observability.sql
psql "$DSN" -v ON_ERROR_STOP=1 -f deploy/migrations/0014_add_ssh_ops_step_detail.sql
```

`0012` is required only before enabling `agent.feishu.external_image_oauth`. It
creates `feishu_oauth_tokens`, which holds AES-GCM ciphertext for the rotating
delegated Feishu user token; it contains no plaintext access or refresh token.

`0013` is required with the contextual SSH-ops harness. It adds only aggregate
schema/coverage and command-class columns to `ssh_ops_audit`; it does not store
the injected user reports, platform fact values, credentials, or raw commands.

`0014` adds `ssh_ops_audit.steps` (JSONB): the per-command detail behind
`commands_ran` / `commands_refused`, so a run interrupted by a disconnect can be
described by name rather than only by count. It holds the REDACTED display
command (200-rune cap, marker included), tier, disposition, the fine-grained
refusal reason, exit code and byte count — never command output, and never the
raw command.

It has the **same ordering requirement as `0013`: apply it BEFORE deploying a
binary that writes it.** `Finish` is a single `UPDATE` and `steps` is one more
`SET` in it, so against a database missing the column the whole statement errors
— not just the new field. The outcome (`disposition`, `err_class`, the counts) is
then never written and the row orphans at `started`, which is precisely the state
the detached-context handling exists to prevent. The reverse direction is safe:
an older binary against a migrated database simply leaves the column NULL.

### Boot probe (SSH-ops lane)

That ordering is now **checked, not just documented**. When the lane is enabled,
`serverInstanceOpsRunner` runs `store.VerifySSHOpsAuditSchema` — one
`SELECT <every writer column> FROM ssh_ops_audit LIMIT 0`. On failure it logs

```
ssh-ops disabled: audit schema unavailable, so entering an instance could not be
recorded: ... (in-instance diagnosis stays off until the migration runs AND this
process restarts — the probe is boot-only)
```

and **disables the lane. The server still starts.**

It exists because the un-probed failure is silent, late, and on the wrong side of
the safety boundary. `Begin` names only `0011`'s columns, so on a partially
migrated table it SUCCEEDS: the record is written, the harness enters the
instance — with `allow_writes: true` it can change it — and the error appears
only at `Finish`, which loses the outcome and the counts together and leaves the
row at `started`. `TestSSHOpsAuditSchemaProbeStopsTheLaneBeforeAnUnrecordableRun`
reproduces exactly that against a real PostgreSQL before asserting the probe
refuses first.

**Which failures take the server down, and which only take the lane down:**

| failure | result | why |
|---|---|---|
| core tables (`sessions`, `messages`, turn protocol) missing | boot failure | the product cannot run; `store.VerifySchema` already refuses at `OpenMySQL` |
| SSH-ops **configuration** wrong (`harness_path`, `base_url`, key, `internal_ipv6` without `iam_url`, unparseable `public_ipv6_prefix`) | boot failure | the artifact is broken — restarting produces the same lane that can enter no box, so a silent downgrade would let a bad edit read as a failed experiment |
| `ssh_ops_audit` table or column missing | **lane disabled**, loudly | nothing is wrong with the artifact; the environment is not ready, and one migration job fixes it without a new image |

The safety boundary is *audit unavailable → do not enter a user's instance*, not
*audit unavailable → nobody may chat*. A nil runner delivers the first exactly:
the tool is absent from the model's window, the prompt does not advertise write
mode, and even a replayed or hallucinated call gets INV-10's inert refusal — no
credential is fetched and no SSH is dialled. Failing the boot instead would be a
full outage, because `deploy/k8s/deployment.yaml` is `replicas: 1` with
`strategy: Recreate`: the old Pod is gone before the new one starts, so there is
no version left serving.

**If a deploy does land before its migration**, chat keeps working and only
in-instance diagnosis is off. Recovery is: run the migration job, then redeploy
the same image (or restart the Pod). The redeploy is required — the probe runs at
boot and nothing polls for the column afterwards, deliberately: a background
re-check would add a moving part to buy an action the operator is already taking.

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
