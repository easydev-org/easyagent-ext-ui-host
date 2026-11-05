# syntax=docker/dockerfile:1

# THE CONSOLE, TAKEN FROM ITS OWN IMAGE.
#
# The webui publishes an image, not a tarball, and /srv is its built bundle. Copying the directory is
# the only way to get the console AND the panels into one pod without building the SPA here — which
# would mean this repository needing Node, a copy of the console's source, and a second place for its
# dependencies to drift.
#
# The tag is pinned by CONSOLE_IMAGE so a build of this pod is reproducible.
ARG CONSOLE_IMAGE=ghcr.io/easydev-org/easyagent-webui:0.17.0
FROM ${CONSOLE_IMAGE} AS console

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/ext-ui-host ./cmd/ext-ui-host

FROM alpine:3.24
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/ext-ui-host /app/ext-ui-host

# THE CONSOLE'S FILES. Copied whole: its asset hashes are in its index.html, and picking files out of
# it is how a build ends up one font short.
COPY --from=console /srv /srv

# THE INSTALLED PANELS ARE THE ONLY STATE besides /srv, and without a volume they vanish on restart —
# which reads, from the console, as every extension losing its UI at once. The service refuses to start
# when UI_DATA_DIR does not exist, so a missing mount is a crash loop rather than an empty host.
VOLUME ["/data"]
ENV UI_DATA_DIR=/data \
    CONSOLE_DIR=/srv \
    PORT=8080

EXPOSE 8080
ENTRYPOINT ["/app/ext-ui-host"]
