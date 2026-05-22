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

# Package the linux binary + kb + systemd unit into an RPM.
# Requires nfpm (https://nfpm.goreleaser.com): `go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest`
# Usage: just rpm [version="0.1.0"]
# Output: dist/compshare-agent-<version>-1.x86_64.rpm
rpm version="0.1.0": linux
    @command -v nfpm >/dev/null || { echo "nfpm not found. Install: go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest"; exit 1; }
    @mkdir -p dist
    VERSION={{version}} nfpm pkg --packager rpm --config nfpm.yaml --target dist/
    @ls -lh dist/compshare-agent-{{version}}-1.x86_64.rpm

# Run all Go tests.
test:
    go test ./... -count=1

# Kill any running ./agent server (matching --addr :8080 by default).
stop addr=addr:
    -pkill -f 'cmd server --addr {{addr}}'
