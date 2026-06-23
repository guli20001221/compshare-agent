#!/bin/sh
# Build artifacts and secrets are configured in deploy/conf/config.yaml.
# This script only registers the current checkout's binary with ally.

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

ally invite compshare-agent \
    --app-bin "$APP_BIN" \
    --app-pwd "$APP_DIR" \
    -- server \
    --config "$CONFIG_FILE"

echo
echo "registered. useful: ally status compshare-agent / ally logs compshare-agent"
