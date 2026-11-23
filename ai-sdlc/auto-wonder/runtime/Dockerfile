FROM golang:1.27-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/autowonder-runtime ./cmd/autowonder-runtime

FROM node:24-bookworm-slim

ARG OPENCODE_VERSION=latest
ARG CODEX_VERSION=latest

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       bash \
       bubblewrap \
       ca-certificates \
       curl \
       git \
       jq \
       openssh-client \
       ripgrep \
       tzdata \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g --no-audit --no-fund \
       --allow-scripts=@opencode/cli \
       "@opencode/cli@${OPENCODE_VERSION}" \
       "@openai/codex@${CODEX_VERSION}" \
    && opencode --version \
    && codex --version \
    && npm cache clean --force

RUN groupadd --gid 10001 autowonder \
    && useradd --uid 10001 --gid autowonder --create-home --shell /bin/bash autowonder \
    && install -d -m 0755 -o autowonder -g autowonder /var/lib/autowonder \
    && install -d -m 0700 -o autowonder -g autowonder \
       /home/autowonder/.config/opencode \
       /home/autowonder/.codex

COPY --from=build /out/autowonder-runtime /usr/local/bin/autowonder-runtime
COPY --chown=autowonder:autowonder --chmod=0644 \
    config/opencode.json /home/autowonder/.config/opencode/opencode.json
COPY --chown=autowonder:autowonder --chmod=0644 \
    config/codex.toml /home/autowonder/.codex/config.toml

USER autowonder
ENV HOME=/home/autowonder \
    XDG_CONFIG_HOME=/home/autowonder/.config \
    XDG_DATA_HOME=/home/autowonder/.local/share \
    XDG_CACHE_HOME=/home/autowonder/.cache \
    CODEX_HOME=/home/autowonder/.codex \
    AUTOWONDER_PROVIDER=opencode \
    AUTOWONDER_WORKSPACE_ROOT=/var/lib/autowonder/runtime \
    OPENCODE_MODEL=litellm/deepseek-v4.1-flash \
    LITELLM_BASE_URL=https://litellm.yamibuy.net/v1

VOLUME ["/var/lib/autowonder"]
ENTRYPOINT ["/usr/local/bin/autowonder-runtime"]
