#!/bin/sh
# Auto-register compshare-agent with ally on every install / upgrade.
#
# Layout (everything lives under /data/yuanpeng.wei/compshare-agent/):
#   compshare-agent        binary
#   deploy/kb/...          RAG corpus + embedding sidecar
#   scripts/rag_w0/...     staff_names.txt
#   agent.yaml.example     shipped template (always refreshed on upgrade)
#   agent.yaml             operator-edited config (copied from .example on
#                          first install; preserved across upgrades)
#   env                    secrets (mode 0600, baked into the RPM from the
#                          dev machine's .env)

set -e

APP_DIR=/data/yuanpeng.wei/compshare-agent
ENV_FILE="$APP_DIR/env"
CONFIG_FILE="$APP_DIR/agent.yaml"
EXAMPLE_FILE="$APP_DIR/agent.yaml.example"

if [ ! -f "$CONFIG_FILE" ] && [ -f "$EXAMPLE_FILE" ]; then
    cp "$EXAMPLE_FILE" "$CONFIG_FILE"
fi

if ! command -v ally >/dev/null 2>&1; then
    cat <<EOF

================================================================================
 compshare-agent files installed under $APP_DIR
 but 'ally' was not found in PATH — skipping process registration.

 After installing ally, re-run with:
   sudo rpm --reinstall <this rpm>
================================================================================

EOF
    exit 0
fi

# shellcheck disable=SC1090
. "$ENV_FILE"

: "${LLM_API_KEY:?env: LLM_API_KEY missing in $ENV_FILE}"
: "${COMPSHARE_SERVICE_PUBLIC_KEY:?env: COMPSHARE_SERVICE_PUBLIC_KEY missing in $ENV_FILE}"
: "${COMPSHARE_SERVICE_PRIVATE_KEY:?env: COMPSHARE_SERVICE_PRIVATE_KEY missing in $ENV_FILE}"
: "${COMPSHARE_DEFAULT_ROLE_URN:?env: COMPSHARE_DEFAULT_ROLE_URN missing in $ENV_FILE}"
: "${MYSQL_DSN:?env: MYSQL_DSN missing in $ENV_FILE}"
: "${ADDR:?env: ADDR missing in $ENV_FILE (e.g. 10.182.45.17:10100)}"

ally invite compshare-agent \
    --app-bin "$APP_DIR/compshare-agent" \
    --app-pwd "$APP_DIR" \
    --app-env "LLM_API_KEY=$LLM_API_KEY" \
    --app-env "COMPSHARE_SERVICE_PUBLIC_KEY=$COMPSHARE_SERVICE_PUBLIC_KEY" \
    --app-env "COMPSHARE_SERVICE_PRIVATE_KEY=$COMPSHARE_SERVICE_PRIVATE_KEY" \
    --app-env "COMPSHARE_DEFAULT_ROLE_URN=$COMPSHARE_DEFAULT_ROLE_URN" \
    --app-env "MYSQL_DSN=$MYSQL_DSN" \
    -- server \
    --config "$CONFIG_FILE" \
    --addr "$ADDR"

cat <<EOF

================================================================================
 compshare-agent registered with ally as 'compshare-agent'.
 Working dir: $APP_DIR
 Config:      $CONFIG_FILE
 Listen:      $ADDR

 Useful:
   ally status compshare-agent
   ally logs compshare-agent
================================================================================

EOF

exit 0
