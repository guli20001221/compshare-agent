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
CONFIRM_FORM="${COMPSHARE_CONFIRM_FORM:-0}"
GUIDED_CREATE="${COMPSHARE_GUIDED_CREATE:-0}"

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

# Editable create-flow confirmation form (server half of the double gate, create-flow
# 表单化). With this on AND the client opting in per turn (SendCSAgentChat
# Features:["confirm_form_v1"], which the AIAssistant already sends), CreateInstanceWorkflow
# confirmation frames carry a select-only Form (GPU/zone/image/charge-type) and accept
# Overrides (re-validated, <=3 edits). SAFE when the client does NOT opt in — frames stay
# byte-identical, Overrides rejected. deploy_model saga + CLI confirm unaffected either way.
# Forwarded for the same own-process-env reason as the switches above; Go code default OFF.
CONFIRM_FORM="${COMPSHARE_CONFIRM_FORM:-1}"

# Agentic-RAG answer stack. Pinned EXPLICITLY (rather than relying on the binary's
# cmd-boot default-on) so production behavior never depends on an implicit default a
# future code change could silently flip. All default ON; set any to 0 to roll back.
#   COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE            read-only SearchKnowledge registry tool (RAG as an agent tool)
#   COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP            knowledge_qa runs in the agent loop (forced SearchKnowledge first hop)
#   COMPSHARE_KNOWLEDGE_QA_DISCIPLINED_SYNTHESIS final answer written by the tight cited-synthesis prompt (anti-fab)
#   COMPSHARE_EXTERNAL_KNOWLEDGE                 merge external tool/ops corpus (vLLM/CUDA/Linux/PyTorch) into the index
AGENTIC_SEARCH="${COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE:-1}"
KQA_AGENT_LOOP="${COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP:-1}"
KQA_DISCIPLINED="${COMPSHARE_KNOWLEDGE_QA_DISCIPLINED_SYNTHESIS:-1}"
EXTERNAL_KNOWLEDGE="${COMPSHARE_EXTERNAL_KNOWLEDGE:-1}"

ally invite compshare-agent \
    --app-bin "$APP_DIR/compshare-agent" \
    --app-pwd "$APP_DIR" \
    --app-env "LLM_API_KEY=$LLM_API_KEY" \
    --app-env "COMPSHARE_SERVICE_PUBLIC_KEY=$COMPSHARE_SERVICE_PUBLIC_KEY" \
    --app-env "COMPSHARE_SERVICE_PRIVATE_KEY=$COMPSHARE_SERVICE_PRIVATE_KEY" \
    --app-env "COMPSHARE_DEFAULT_ROLE_URN=$COMPSHARE_DEFAULT_ROLE_URN" \
    --app-env "MYSQL_DSN=$MYSQL_DSN" \
    --app-env "COMPSHARE_ENABLE_MUTATING_TOOLS=$MUTATING_TOOLS" \
    --app-env "COMPSHARE_CONFIRM_FORM=$CONFIRM_FORM" \
    --app-env "COMPSHARE_GUIDED_CREATE=$GUIDED_CREATE" \
    --app-env "USE_SESSION_FACT_CONTEXT=$SESSION_FACT_CONTEXT" \
    --app-env "USE_REACT_RESULT_PROJECTION=$REACT_RESULT_PROJECTION" \
    --app-env "USE_REACT_HISTORY_COMPACTION=$REACT_HISTORY_COMPACTION" \
    --app-env "COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT=$COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT_MODE" \
    --app-env "COMPSHARE_TRACE_ENABLED=$TRACE_ENABLED" \
    --app-env "COMPSHARE_TRACE_SINK=$TRACE_SINK" \
    --app-env "COMPSHARE_TRACE_DIR=$TRACE_DIR" \
    --app-env "COMPSHARE_CONFIRM_FORM=$CONFIRM_FORM" \
    --app-env "COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE=$AGENTIC_SEARCH" \
    --app-env "COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP=$KQA_AGENT_LOOP" \
    --app-env "COMPSHARE_KNOWLEDGE_QA_DISCIPLINED_SYNTHESIS=$KQA_DISCIPLINED" \
    --app-env "COMPSHARE_EXTERNAL_KNOWLEDGE=$EXTERNAL_KNOWLEDGE" \
    -- server \
    --config "$CONFIG_FILE" \
    --addr "$ADDR"

echo
echo "registered. useful: ally status compshare-agent / ally logs compshare-agent"
