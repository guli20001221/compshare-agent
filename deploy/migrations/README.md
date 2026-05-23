# HTTP/SSE MySQL migrations

Apply these migrations manually before starting `compshare-agent server`.
The binary verifies required tables and columns at startup, but it does not auto-migrate.

**Deploy order is mandatory: migration first, binary second.** A new binary
started against an un-migrated database will fail `VerifySchema` at boot —
do not flip the order during rolling deploys. Old binaries running against
a newer schema are compatible (they ignore unknown columns).

```bash
mysql -uroot -p -e 'CREATE DATABASE IF NOT EXISTS compshare_agent DEFAULT CHARSET utf8mb4;'
mysql -uroot -p compshare_agent < deploy/migrations/0001_init.sql
mysql -uroot -p compshare_agent < deploy/migrations/0002_create_agent_traces.sql
mysql -uroot -p compshare_agent < deploy/migrations/0003_add_session_context_version.sql
```
