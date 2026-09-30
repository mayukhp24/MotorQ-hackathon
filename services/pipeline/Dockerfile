# Build context: repository root (the image also carries db/ migrations).
# Optional build secret `extra_ca` adds a corporate TLS-inspection CA for the
# dependency download only; it is never written to any layer.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY services/pipeline/go.mod services/pipeline/go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then cat /etc/ssl/certs/ca-certificates.crt /run/secrets/extra_ca > /tmp/ca.pem; \
      export SSL_CERT_FILE=/tmp/ca.pem; fi; \
    go mod download && rm -f /tmp/ca.pem
COPY services/pipeline/ ./
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
COPY db/postgres/migrations /migrations/postgres
COPY db/clickhouse /migrations/clickhouse
COPY services/pipeline/internal/oem/specs /etc/fleetpulse/adapters
USER nonroot:nonroot
EXPOSE 8080 9101 9102 9103 9104
ENTRYPOINT ["/usr/local/bin/ingest-gateway"]
