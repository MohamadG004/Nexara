# ─── Stage 1: Build ──────────────────────────────────────────────────────────
# Use exact version for reproducible builds.
FROM golang:1.23-alpine AS builder

# Install build tools
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /build

# Copy dependency files first — Docker caches this layer unless go.mod changes.
# This means 'go mod download' only re-runs when dependencies change, not on
# every code change. Critical for fast CI builds.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Copy source and build
COPY . .

# Build flags:
# -ldflags "-s -w" strips debug info → ~30% smaller binary
# -trimpath removes local file paths from binary (security + reproducibility)
# CGO_ENABLED=0 produces a static binary that runs in scratch containers
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w -X main.version=$(git describe --tags --always --dirty)" \
    -trimpath \
    -o apex-server \
    ./cmd/server

# ─── Stage 2: Production image ───────────────────────────────────────────────
# scratch + ca-certificates = smallest possible attack surface.
# No shell, no package manager, no utilities — nothing to exploit.
FROM scratch AS production

# Copy timezone data for time.LoadLocation()
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# Copy TLS certificates for outbound HTTPS (webhooks, Slack, etc.)
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the binary
COPY --from=builder /build/apex-server /apex-server

# Copy migrations — embedded in binary at runtime
COPY --from=builder /build/migrations /migrations

# Run as non-root user (numeric UID avoids needing /etc/passwd)
USER 65532:65532

EXPOSE 8080

ENTRYPOINT ["/apex-server"]

# ─── Stage 3: Development image (hot reload with Air) ────────────────────────
FROM golang:1.23-alpine AS development

RUN apk add --no-cache git curl

# Install Air for hot reload
RUN go install github.com/air-verse/air@latest

# Install Delve for debugging
RUN go install github.com/go-delve/delve/cmd/dlv@latest

WORKDIR /app

# Delve debugger + Air hot reload
CMD ["air", "-c", ".air.toml"]
