# syntax=docker/dockerfile:1.7

# ── Builder ───────────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# Cache module download as its own layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# Strip symbol table + DWARF, embed version/commit, fully static.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-w -s -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
    -o /out/server \
    ./cmd/server

# ── Runtime ───────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="payment-gateway" \
      org.opencontainers.image.source="https://github.com/Natthyx/payment-gateway" \
      org.opencontainers.image.licenses="Proprietary" \
      org.opencontainers.image.description="Multi-provider payment orchestrator"

# Bring migration files into the image so the in-app migrator can find them.
COPY --from=builder /src/migrations /migrations
COPY --from=builder /out/server     /server

USER nonroot:nonroot
EXPOSE 8080
ENV MIGRATIONS_PATH=file:///migrations

ENTRYPOINT ["/server"]
