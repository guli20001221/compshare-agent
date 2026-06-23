ADDR ?= :8236

.PHONY: run build linux deploy test stop

# Build + run the HTTP server.
# Usage: make run ADDR=:7777
run:
	go run ./cmd server --addr $(ADDR)

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
stop:
	-pkill -f 'cmd server --addr $(ADDR)'
