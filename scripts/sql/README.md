# scripts/sql

DDL for the CLI trace backend (`agent_traces`).

HTTP/SSE session history uses `deploy/migrations/0001_init.sql` instead:
`sessions`, `messages`, and `message_feedback`.

## Local Docker setup

```powershell
docker run -d `
    --name agent-mysql `
    -p 3306:3306 `
    -e MYSQL_ROOT_PASSWORD=devonly `
    -e MYSQL_DATABASE=compshare_agent `
    -v F:/compshare-agent-data/mysql:/var/lib/mysql `
    mysql:8.0

docker exec -i agent-mysql mysql -uroot -pdevonly compshare_agent < scripts/sql/init.sql

docker exec -i agent-mysql mysql -uroot -pdevonly compshare_agent -e "SHOW TABLES"
# expected: agent_traces
```

## DSN requirements

```
MYSQL_DSN=root:devonly@tcp(127.0.0.1:3306)/compshare_agent?parseTime=true&loc=Asia%2FShanghai&charset=utf8mb4
```
