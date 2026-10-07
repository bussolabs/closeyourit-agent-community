# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w -X github.com/bussolabs/closeyourit-agent/internal/version.Version=${VERSION}" \
      -o /closeyourit-agent ./cmd/closeyourit-agent

FROM alpine:3.22
# Links the ghcr.io package to the public repository, which makes its page and visibility follow it (CYRA-914 P23).
LABEL org.opencontainers.image.source="https://github.com/bussolabs/closeyourit-agent-community" \
      org.opencontainers.image.description="CloseYourIt server monitoring agent" \
      org.opencontainers.image.licenses="MIT"
# smartmontools for SMART (the agent shells out to smartctl); tzdata for correct timestamps.
RUN apk add --no-cache smartmontools tzdata
COPY --from=build /closeyourit-agent /usr/local/bin/closeyourit-agent
# Persistent data (fingerprint id): mount a volume on /var/lib/closeyourit-agent.
VOLUME /var/lib/closeyourit-agent
HEALTHCHECK --interval=120s --timeout=10s --start-period=90s \
  CMD ["/usr/local/bin/closeyourit-agent", "health"]
ENTRYPOINT ["/usr/local/bin/closeyourit-agent"]
