#!/bin/sh
# Run after extracting the deploy tarball into /data/yuanpeng.wei/compshare-agent/.
# Reads ./env (sibling file) and registers the agent with ally.

set -e

APP_DIR="$(cd "$(dirname "$0")" && pwd)"
ENV_FILE="$APP_DIR/env"
CONFIG_FILE="$APP_DIR/agent.yaml"

if [ ! -f "$ENV_FILE" ]; then
    echo "missing $ENV_FILE — edit it first (LLM_API_KEY, COMPSHARE_*, MYSQL_DSN, ADDR)" >&2
    exit 1
fi

# shellcheck disable=SC1090
. "$ENV_FILE"

: "${LLM_API_KEY:?env: LLM_API_KEY missing}"
: "${COMPSHARE_SERVICE_PUBLIC_KEY:?env: COMPSHARE_SERVICE_PUBLIC_KEY missing}"
: "${COMPSHARE_SERVICE_PRIVATE_KEY:?env: COMPSHARE_SERVICE_PRIVATE_KEY missing}"
: "${COMPSHARE_DEFAULT_ROLE_URN:?env: COMPSHARE_DEFAULT_ROLE_URN missing}"
: "${MYSQL_DSN:?env: MYSQL_DSN missing}"
: "${ADDR:?env: ADDR missing (e.g. 10.182.45.17:10100)}"

# Write-operations switch. Optional and read-only by default (code default is off):
# unless the env file sets it to 1, write ops (create/start/stop/reboot/reset-
# password/resize-disk) stay hidden/blocked. Forwarded explicitly because the binary
# only reads it from its OWN process env — sourcing the env file here is not enough,
# `ally invite` only passes the --app-env vars below to the spawned server.
# Destructive actions (delete / L2) stay refused even when this is 1.
MUTATING_TOOLS="${COMPSHARE_ENABLE_MUTATING_TOOLS:-0}"

# Context-engineering optimizations. Shipped ON via .env.example by deliberate
# decision; the Go code default stays off, so they only take effect because they are
# forwarded here (same plumbing reason as the write-ops switch above). Default to 1
# so a fresh env file still ships them on. Structured-output is plumbed but defaults
# off (no measured benefit on ds-v4-flash) — set COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT=json_object
# in the env file to enable once validated.
SESSION_FACT_CONTEXT="${USE_SESSION_FACT_CONTEXT:-1}"
REACT_RESULT_PROJECTION="${USE_REACT_RESULT_PROJECTION:-1}"
REACT_HISTORY_COMPACTION="${USE_REACT_HISTORY_COMPACTION:-1}"
COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT_MODE="${COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT:-}"

# Trace / observability sink (Phase-0 of the trace-observability rollout). Forwarded
# for the same reason as the switches above — the binary reads COMPSHARE_TRACE_* only
# from its OWN process env, and `ally invite` passes only the --app-env vars below.
# Shipped ON writing to MySQL by deliberate decision: every turn writes one
# agent_traces row (outcome/state/retrieval attribution). PREREQUISITE: ops must run
# the deploy/migrations/0002 + 0004 DDL on the MYSQL_DSN database first; the writer
# probes for the promoted columns and degrades to the trace_json blob if 0004 is not
# applied. The Go code default is OFF (nil writer), so traces flow only because these
# are shipped here and forwarded. Set COMPSHARE_TRACE_ENABLED=0 to disable, or
# COMPSHARE_TRACE_SINK=file|both to change where traces land.
TRACE_ENABLED="${COMPSHARE_TRACE_ENABLED:-1}"
TRACE_SINK="${COMPSHARE_TRACE_SINK:-mysql}"
TRACE_DIR="${COMPSHARE_TRACE_DIR:-}"

ally invite compshare-agent \
    --app-bin "$APP_DIR/compshare-agent" \
    --app-pwd "$APP_DIR" \
    --app-env "LLM_API_KEY=$LLM_API_KEY" \
    --app-env "COMPSHARE_SERVICE_PUBLIC_KEY=$COMPSHARE_SERVICE_PUBLIC_KEY" \
    --app-env "COMPSHARE_SERVICE_PRIVATE_KEY=$COMPSHARE_SERVICE_PRIVATE_KEY" \
    --app-env "COMPSHARE_DEFAULT_ROLE_URN=$COMPSHARE_DEFAULT_ROLE_URN" \
    --app-env "MYSQL_DSN=$MYSQL_DSN" \
    --app-env "COMPSHARE_ENABLE_MUTATING_TOOLS=$MUTATING_TOOLS" \
    --app-env "USE_SESSION_FACT_CONTEXT=$SESSION_FACT_CONTEXT" \
    --app-env "USE_REACT_RESULT_PROJECTION=$REACT_RESULT_PROJECTION" \
    --app-env "USE_REACT_HISTORY_COMPACTION=$REACT_HISTORY_COMPACTION" \
    --app-env "COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT=$COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT_MODE" \
    --app-env "COMPSHARE_TRACE_ENABLED=$TRACE_ENABLED" \
    --app-env "COMPSHARE_TRACE_SINK=$TRACE_SINK" \
    --app-env "COMPSHARE_TRACE_DIR=$TRACE_DIR" \
    -- server \
    --config "$CONFIG_FILE" \
    --addr "$ADDR"

echo
echo "registered. useful: ally status compshare-agent / ally logs compshare-agent"
