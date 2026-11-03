# syntax=docker/dockerfile:1

# ---- build ------------------------------------------------------------------
FROM golang:1.27-alpine AS build

WORKDIR /src

# No third-party modules today; the extra layer keeps the cache stable once
# dependencies appear.
COPY go.mod ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
        -ldflags "-s -w -X github.com/yusiwen/obs-adapter/internal/gateway.Version=${VERSION}" \
        -o /out/obs-adapter ./cmd/obs-adapter

# ---- runtime ----------------------------------------------------------------
FROM alpine:3.22

# CA certificates are required for the HTTPS upstream (MinIO over TLS).
RUN apk add --no-cache ca-certificates

COPY --from=build /out/obs-adapter /usr/local/bin/obs-adapter

# Unprivileged by default. The container listens on 8080, so no capability is
# needed; to terminate TLS inside the container publish 443:8443 instead of
# binding 443 directly (or grant CAP_NET_BIND_SERVICE).
USER 65532:65532

ENV OBS_ADAPTER_LISTEN=:8080 \
    OBS_ADAPTER_LOG_LEVEL=info

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=3s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/__health >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/obs-adapter"]
