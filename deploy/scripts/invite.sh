#!/bin/sh
# Run after extracting the deploy tarball into /data/yuanpeng.wei/compshare-agent/.
# Reads ./env (sibling file, optional) and registers the agent with ally.
#
# Behavior is configured in agent.yaml (agent.features / retrieval / trace /
# planner) — NOT via env any more. The env file now holds only secrets + the
# bind address. To run fully env-free, inline the secret values into agent.yaml
# (it is gitignored) and set agent.http.listen_addr; you can then leave the env
# file empty. See deploy/conf/agent.yaml.example.

set -e

APP_DIR="$(cd "$(dirname "$0")" && pwd)"
ENV_FILE="$APP_DIR/env"
CONFIG_FILE="$APP_DIR/agent.yaml"

# Source the env file if present (optional). Secrets may instead be inlined in
# agent.yaml, and the bind address may be set as agent.http.listen_addr.
if [ -f "$ENV_FILE" ]; then
    # shellcheck disable=SC1090
    . "$ENV_FILE"
fi

# Bind address: prefer the env ADDR (operator override); otherwise fall back to
# agent.http.listen_addr in agent.yaml.
ADDR_ARG=""
if [ -n "${ADDR:-}" ]; then
    ADDR_ARG="--addr ${ADDR}"
fi

# Forward secrets so any ${ENV_VAR} placeholders in agent.yaml resolve. Empty-safe:
# if a secret is instead inlined in agent.yaml, the empty forward is ignored (the
# literal in agent.yaml wins). config.Load fails loud if a required secret is
# neither set here nor inlined. Feature flags are NO LONGER forwarded — they live
# in agent.yaml.
ally invite compshare-agent \
    --app-bin "$APP_DIR/compshare-agent" \
    --app-pwd "$APP_DIR" \
    --app-env "LLM_API_KEY=${LLM_API_KEY:-}" \
    --app-env "COMPSHARE_SERVICE_PUBLIC_KEY=${COMPSHARE_SERVICE_PUBLIC_KEY:-}" \
    --app-env "COMPSHARE_SERVICE_PRIVATE_KEY=${COMPSHARE_SERVICE_PRIVATE_KEY:-}" \
    --app-env "COMPSHARE_DEFAULT_ROLE_URN=${COMPSHARE_DEFAULT_ROLE_URN:-}" \
    --app-env "MYSQL_DSN=${MYSQL_DSN:-}" \
    -- server \
    --config "$CONFIG_FILE" \
    ${ADDR_ARG}

echo
echo "registered. useful: ally status compshare-agent / ally logs compshare-agent"
