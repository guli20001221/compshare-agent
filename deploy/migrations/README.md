# HTTP/SSE MySQL migrations

Apply these migrations manually before starting `compshare-agent server`.
The binary verifies required tables at startup, but it does not auto-migrate.

```bash
mysql -uroot -p -e 'CREATE DATABASE IF NOT EXISTS compshare_agent DEFAULT CHARSET utf8mb4;'
mysql -uroot -p compshare_agent < deploy/migrations/0001_init.sql
mysql -uroot -p compshare_agent < deploy/migrations/0002_create_agent_traces.sql
```
