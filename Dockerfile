# syntax=docker/dockerfile:1

# Docker 1.12.6 is only the legacy production *runtime*. BuildKit/buildx and
# GitLab's Kaniko runner both build this image, so keep the Dockerfile free of
# BuildKit-only RUN mounts.
# Use the internal registry mirrors that the c5 Kaniko runner can reach.
# Docker Hub is not routable from that build network.
ARG GO_IMAGE=uhub.service.ucloud.cn/ucompshare-job/golang:1.25-alpine
ARG NODE_IMAGE=uhub.service.ucloud.cn/ai-mas-public/node:22-bookworm-slim
ARG PYTHON_IMAGE=uhub.service.ucloud.cn/base-images/python:3.13-slim

FROM ${GO_IMAGE} AS go-builder
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 \
    go build -trimpath -ldflags="-s -w" -o /out/compshare-agent ./cmd


FROM ${NODE_IMAGE} AS claude-cli
ARG CLAUDE_CODE_VERSION=2.1.218

# Keep the CLI on the exact version used by the direct ModelVerse Anthropic smoke.
RUN npm install --global --omit=dev --no-audit --no-fund \
      "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
    && claude --version


# The runtime base is mirrored in UHub so the production build does not depend
# on Docker Hub egress. A real run on the target engine remains a release gate
# (see deploy/docker/README.md).
FROM ${PYTHON_IMAGE} AS runtime
ARG CLAUDE_CODE_VERSION=2.1.218
ARG VCS_REF=unknown

ENV APP_HOME=/opt/compshare-agent \
    HOME=/home/compshare \
    PATH=/opt/miniforge3/envs/py313/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONUNBUFFERED=1 \
    GIN_MODE=release

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tini \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 compshare \
    && useradd --uid 10001 --gid 10001 --create-home \
         --home-dir "$HOME" --shell /usr/sbin/nologin compshare

# Preserve the production interpreter contract requested for the host deploy,
# while making the environment self-contained in the image.
COPY deploy/ssh_ops_harness/requirements.txt /tmp/ssh-ops-requirements.txt
RUN python3 -m venv /opt/miniforge3/envs/py313 \
    && /opt/miniforge3/envs/py313/bin/pip install --no-cache-dir \
         --requirement /tmp/ssh-ops-requirements.txt \
    && rm /opt/miniforge3/envs/py313/lib/python3.13/site-packages/claude_agent_sdk/_bundled/claude \
    && rm /tmp/ssh-ops-requirements.txt

# The npm package is only a build-time fetcher. Its glibc amd64 package contains
# a self-contained native CLI; copying the wrapper, node_modules and Node runtime
# would add hundreds of duplicate MiB to the image.
COPY --from=claude-cli \
     /usr/local/lib/node_modules/@anthropic-ai/claude-code-linux-x64/claude \
     /usr/local/bin/claude

WORKDIR $APP_HOME
COPY --from=go-builder /out/compshare-agent ./compshare-agent
COPY deploy/conf/config.yaml ./deploy/conf/config.yaml
COPY deploy/migrations ./deploy/migrations
COPY deploy/ssh_ops_harness ./deploy/ssh_ops_harness

# Fail the image build if the mixed Go/Python/Claude runtime is incomplete.  The
# Python suites are offline guardrail/protocol tests; the target-host smoke in
# deploy/docker/README.md covers the old daemon and its seccomp profile.
RUN test -x ./compshare-agent \
    && test -f ./deploy/ssh_ops_harness/harness.py \
    && /opt/miniforge3/envs/py313/bin/python -c \
         'import claude_agent_sdk, paramiko' \
    && test "$(claude --version | awk '{print $1}')" = "$CLAUDE_CODE_VERSION" \
    && cd ./deploy/ssh_ops_harness \
    && test "$(/opt/miniforge3/envs/py313/bin/python -c \
         'import harness; print(harness.resolve_claude_cli())')" = "/usr/local/bin/claude" \
    && for suite in test_*.py; do \
         /opt/miniforge3/envs/py313/bin/python "$suite"; \
       done

LABEL org.opencontainers.image.title="compshare-agent" \
      org.opencontainers.image.revision="$VCS_REF" \
      com.compshare.runtime.docker-min-api="1.24" \
      com.compshare.runtime.claude-code="$CLAUDE_CODE_VERSION"

USER 10001:10001
EXPOSE 7429
STOPSIGNAL SIGTERM

# tini is inside the image because Docker 1.12 predates the deployment's desired
# init behavior and this service creates Python -> Claude CLI process trees.
ENTRYPOINT ["/usr/bin/tini", "--", "/opt/compshare-agent/compshare-agent", "--config", "/opt/compshare-agent/deploy/conf/config.yaml"]
CMD ["server"]
