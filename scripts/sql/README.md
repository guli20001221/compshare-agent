# scripts/sql

DDL for the console-deployment MySQL backend (agent_traces + agent_messages).

## Local Docker setup

```powershell
# 1. Start MySQL (one-time)
docker run -d `
    --name agent-mysql `
    -p 3306:3306 `
    -e MYSQL_ROOT_PASSWORD=devonly `
    -e MYSQL_DATABASE=compshare_agent `
    -v F:/compshare-agent-data/mysql:/var/lib/mysql `
    mysql:8.0

# 2. Apply schema
docker exec -i agent-mysql mysql -uroot -pdevonly compshare_agent < scripts/sql/init.sql

# 3. Verify
docker exec -i agent-mysql mysql -uroot -pdevonly compshare_agent -e "SHOW TABLES"
# expected: agent_messages, agent_traces
```

## Production deploy

Apply `init.sql` once via your team's DB migration tool. Subsequent schema
changes MUST be additive (`ALTER TABLE ... ADD COLUMN`); column drops
require a coordinated agent-version + DB-version cutover (out of scope of
the A1-A9 plan).

## Driver / DSN requirements

The Go client (`internal/observability.MySQLWriter`) requires the DSN to
include `charset=utf8mb4` and recommends `parseTime=true`:

```
MYSQL_DSN=root:devonly@tcp(127.0.0.1:3306)/compshare_agent?parseTime=true&loc=Asia%2FShanghai&charset=utf8mb4
```

Without `charset=utf8mb4`, emoji and some Chinese characters round-trip
as `?` (silent corruption). Without `parseTime=true`, `DATETIME(3)`
columns surface as `[]byte` instead of `time.Time` and the writer panics.
