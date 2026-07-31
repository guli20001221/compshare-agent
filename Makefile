ADDR ?= 0.0.0.0:7429

.PHONY: run build linux deploy deploy-feishu deploy-all test stop

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

# Register the prebuilt root binary with ally.
deploy:
	./deploy/scripts/deploy.sh

# Register the Feishu long-connection adapter as a second ally service.
deploy-feishu:
	./deploy/scripts/deploy-feishu.sh

# Register the main Agent and Feishu adapter on the same host.
deploy-all:
	./deploy/scripts/deploy.sh
	./deploy/scripts/deploy-feishu.sh

# Run all Go tests.
test:
	go test ./... -count=1

# Kill any running server on the configured addr.
stop:
	-pkill -f 'cmd server --addr $(ADDR)'
