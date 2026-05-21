# Load env vars from .env (not committed; copy from .env.example).
set dotenv-load := true
set dotenv-required := true

# Default port for the HTTP server.
addr := ":8236"

# Build + run the HTTP server with env from .env.
# Usage: just run [addr=":7777"]
run addr=addr:
    go build -o agent ./cmd
    ./agent server --addr {{addr}}

# Build only.
build:
    go build -o agent ./cmd

# Run all Go tests.
test:
    go test ./... -count=1

# Kill any running ./agent server (matching --addr :8080 by default).
stop addr=addr:
    -pkill -f 'agent server --addr {{addr}}'
