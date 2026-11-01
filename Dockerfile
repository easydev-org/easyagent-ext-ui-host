# syntax=docker/dockerfile:1
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

# THE INSTALLED PANELS ARE THE ONLY STATE, and without a volume they vanish on restart — which reads,
# from the console, as every extension losing its UI at once. The service refuses to start when this
# directory does not exist, so a missing mount is a crash loop rather than an empty host.
VOLUME ["/data"]
ENV UI_DATA_DIR=/data PORT=8080

EXPOSE 8080
ENTRYPOINT ["/app/ext-ui-host"]
