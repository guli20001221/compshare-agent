ADDR ?= 0.0.0.0:7429
IMAGE ?= compshare-agent:local
PLATFORM ?= linux/amd64

.PHONY: run build linux docker-build docker-smoke docker-push-legacy deploy deploy-feishu deploy-all test stop

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

# Push a single-platform Docker-v2/gzip image consumable by production Docker 1.12.6.
docker-push-legacy:
	./deploy/docker/build-push-docker-1.12.sh "$(IMAGE)"

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
