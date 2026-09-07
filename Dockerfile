########################
# Build Stage
########################
FROM golang:1.26.8-alpine3.24 AS builder

ARG VERSION=dev
ENV CGO_ENABLED=0 \
    GOFLAGS="-trimpath" \
    LDFLAGS="-s -w -X main.version=${VERSION}"

WORKDIR /src

# Better layer caching for deps
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Copy the rest and build
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags "${LDFLAGS}" -o /out/cbom-repository ./cmd/cbom-repository

########################
# Run Stage
########################
FROM alpine:3.24.1

# Pull the latest security fixes for the base packages (e.g. libcrypto3/libssl3;
# CVE-2026-22184 and CVE-2026-14456 arrived this way). BuildKit caches this layer by
# its instruction text, so a cached layer would keep shipping old packages long after
# alpine published fixes (that is what failed the Trivy gate on every PR in 2026-09).
# The build-arg below changes on every CI run and re-run (the workflows pass the run
# id and attempt), which invalidates the layer and re-runs the upgrade; the default
# keeps the layer cache stable for local builds. Drop the arg once the base image tag
# itself carries the fixes.
ARG APK_UPGRADE_STAMP=2026-09-04
RUN echo "apk upgrade stamp: ${APK_UPGRADE_STAMP}" && apk update && apk upgrade --no-cache

LABEL org.opencontainers.image.authors="ILM <ilm@omnitrust.com>"

# add non root user ilm
RUN addgroup --system --gid 10001 ilm && adduser --system --home /opt/ilm --uid 10001 --ingroup ilm ilm

COPY --from=builder /out/cbom-repository /usr/local/bin/cbom-repository

ENV APP_LOG_LEVEL=INFO

EXPOSE 8080

USER 10001

ENTRYPOINT ["/usr/local/bin/cbom-repository"]
