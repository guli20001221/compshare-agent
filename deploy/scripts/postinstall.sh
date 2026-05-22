#!/bin/sh
# Auto-register compshare-agent with ally on each install / upgrade.
#
# Layout (everything lives under /data/yuanpeng.wei/compshare-agent/):
#   compshare-agent              binary
#   deploy/kb/...                RAG corpus + embedding sidecar
#   scripts/rag_w0/staff_names.txt
#   agent.yaml.example           shipped template (always replaced on upgrade)
#   agent.yaml                   operator-edited config (NOT shipped; copied
#                                from the example on first install)
#   env                          shell-format secrets file; operator-managed
#
# The `env` file MUST exist before this script runs ally invite. If it does
# not, we print bootstrap instructions and exit 0 so the rpm install itself
# still succeeds; operator runs `rpm --reinstall ...` after writing `env`.

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

 After installing ally, re-run this step manually with:
   sudo rpm --reinstall <this rpm>
================================================================================

EOF
    exit 0
fi

if [ ! -f "$ENV_FILE" ]; then
    cat <<EOF

================================================================================
 compshare-agent files installed under $APP_DIR

 Next step: create the secrets file, then re-run this install to register
 the process with ally.

   sudo install -m 0600 /dev/stdin $ENV_FILE <<'ENV'
   LLM_API_KEY=...
   COMPSHARE_SERVICE_PUBLIC_KEY=...
   COMPSHARE_SERVICE_PRIVATE_KEY=...
   COMPSHARE_DEFAULT_ROLE_URN=ucs:iam::<top_org_id>:role/ucs-service-role/ServiceRoleForCompshare
   MYSQL_DSN='root:<password>@tcp(<host>:<port>)/compshare_agent?parseTime=true&loc=UTC&charset=utf8mb4'
   ADDR=10.182.45.17:10100
   ENV

   sudo rpm --reinstall <this rpm>     # re-runs ally invite
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
