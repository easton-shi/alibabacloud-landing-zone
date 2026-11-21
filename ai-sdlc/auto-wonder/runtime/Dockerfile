FROM golang:1.27-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/autowonder-runtime ./cmd/autowonder-runtime

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git openssh-client tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 autowonder \
    && useradd --uid 10001 --gid autowonder --create-home --shell /bin/bash autowonder \
    && install -d -o autowonder -g autowonder /var/lib/autowonder

COPY --from=build /out/autowonder-runtime /usr/local/bin/autowonder-runtime

USER autowonder
ENV HOME=/home/autowonder \
    AUTOWONDER_WORKSPACE_ROOT=/var/lib/autowonder/runtime
VOLUME ["/var/lib/autowonder"]
ENTRYPOINT ["/usr/local/bin/autowonder-runtime"]
