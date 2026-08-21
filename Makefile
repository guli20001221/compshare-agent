ADDR ?= 0.0.0.0:7429
IMAGE ?= compshare-agent:local
PLATFORM ?= linux/amd64

.PHONY: run build linux docker-build docker-smoke test stop

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

# Build the self-contained production image on a current Docker/buildx host.
docker-build:
	docker buildx build --platform $(PLATFORM) --provenance=false --sbom=false \
		--build-arg VCS_REF=$$(git rev-parse HEAD) --load -t $(IMAGE) .

# Check the mixed Go/Python/Claude runtime in the locally loaded image.
docker-smoke:
	docker run --rm --platform $(PLATFORM) --entrypoint /bin/sh $(IMAGE) -ec \
		'/opt/miniforge3/envs/py313/bin/python --version; claude --version; /opt/compshare-agent/compshare-agent --help >/dev/null'

# Run all Go tests.
test:
	go test ./... -count=1

# Kill any running server on the configured addr.
stop:
	-pkill -f 'cmd server --addr $(ADDR)'
