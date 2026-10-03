# syntax=docker/dockerfile:1.7
FROM golang:1.27.1-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=secret,id=proxy_ca,required=false \
    set -eu; \
    if [ -s /run/secrets/proxy_ca ]; then \
      cat /etc/ssl/certs/ca-certificates.crt /run/secrets/proxy_ca > /tmp/session-ca-bundle.crt; \
      SSL_CERT_FILE=/tmp/session-ca-bundle.crt go mod download; \
    else \
      go mod download; \
    fi

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/gsm2sip-api ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/gsm2sip-worker ./cmd/worker && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/gsm2sip-admin ./cmd/admin

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=build /out/gsm2sip-api /usr/local/bin/gsm2sip-api
COPY --from=build /out/gsm2sip-worker /usr/local/bin/gsm2sip-worker
COPY --from=build /out/gsm2sip-admin /usr/local/bin/gsm2sip-admin
COPY --chown=nonroot:nonroot migrations/ /app/migrations/

ENV MIGRATIONS_DIR=/app/migrations
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/gsm2sip-api"]
