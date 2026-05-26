# Load env vars from .env (not committed; copy from .env.example).
set dotenv-load := true
set dotenv-required := true

# Default port for the HTTP server.
addr := ":8236"

# Build + run the HTTP server with env from .env.
# Usage: just run [addr=":7777"]
run addr=addr:
    go run ./cmd server --addr {{addr}}

# Build only.
build:
    go build -o compshare-agent ./cmd

# Cross-compile a Linux amd64 binary for server deployment.
linux:
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o compshare-agent ./cmd

# Pack the linux binary + kb + env + invite.sh into a tarball for one-shot
# upload. Extract the tarball INTO /data/yuanpeng.wei/compshare-agent/ on the
# server (files land at the root of that dir, no version-suffixed wrapper).
# Usage:  just pack [version="0.1.0"]
# Output: dist/compshare-agent-<version>.tar.gz
# Deploy: scp dist/compshare-agent-<version>.tar.gz ucloud@<host>:/data/yuanpeng.wei/compshare-agent/
#         ssh ucloud@<host> 'cd /data/yuanpeng.wei/compshare-agent && tar -xzf compshare-agent-*.tar.gz && ./invite.sh'
pack version="0.1.0": linux
    @rm -rf dist/staging
    @mkdir -p dist/staging/deploy/kb dist/staging/scripts/rag_w0
    @cp -p compshare-agent dist/staging/
    @cp deploy/kb/stage2b_w0.jsonl dist/staging/deploy/kb/
    @cp deploy/kb/embeddings_*_qwen3-embedding-8b.jsonl dist/staging/deploy/kb/
    @cp scripts/rag_w0/staff_names.txt dist/staging/scripts/rag_w0/
    @cp deploy/conf/agent.yaml.example dist/staging/agent.yaml
    @cp deploy/scripts/invite.sh dist/staging/
    @chmod 0755 dist/staging/invite.sh dist/staging/compshare-agent
    @cp .env dist/staging/env
    @chmod 0600 dist/staging/env
    @tar -czf dist/compshare-agent-{{version}}.tar.gz -C dist/staging .
    @rm -rf dist/staging
    @ls -lh dist/compshare-agent-{{version}}.tar.gz

# Run all Go tests.
test:
    go test ./... -count=1

# Kill any running ./agent server (matching --addr :8080 by default).
stop addr=addr:
    -pkill -f 'cmd server --addr {{addr}}'
