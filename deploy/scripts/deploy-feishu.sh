#!/bin/sh
# Register the Feishu long-connection adapter as a separate ally service.
# Configuration is read from deploy/conf/config.yaml.

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
APP_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
CONFIG_FILE="$APP_DIR/deploy/conf/config.yaml"
APP_BIN="$APP_DIR/compshare-agent"

if [ ! -f "$APP_BIN" ]; then
    echo "missing binary: $APP_BIN" >&2
    echo "build locally and upload it to the project root as ./compshare-agent before running deploy" >&2
    exit 1
fi

if [ ! -x "$APP_BIN" ]; then
    chmod 0755 "$APP_BIN"
fi

ally invite compshare-agent-feishu \
    --app-bin "$APP_BIN" \
    --app-pwd "$APP_DIR" \
    -- feishu \
    --config "$CONFIG_FILE"

echo
echo "registered. useful: ally status compshare-agent-feishu / ally logs compshare-agent-feishu"
