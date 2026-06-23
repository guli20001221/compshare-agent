# Default port for the HTTP server.
addr := ":8236"

# Build + run the HTTP server.
# Usage: just run [addr=":7777"]
run addr=addr:
    go run ./cmd server --addr {{addr}}

# Build only.
build:
    go build -o compshare-agent ./cmd

# Cross-compile a Linux amd64 binary for server deployment.
linux:
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o compshare-agent ./cmd

# Build the deploy binary and register it with ally.
deploy: linux
    ./deploy/scripts/deploy.sh

# Run all Go tests.
test:
    go test ./... -count=1

# Kill any running server on the configured addr.
stop addr=addr:
    -pkill -f 'cmd server --addr {{addr}}'
