# Build stage
FROM golang:1.25-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git ca-certificates

WORKDIR /workspace

# Copy go mod files first (better caching)
COPY go.mod go.sum ./

# Use cache mount for go mod download
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

# Copy source code
COPY . .

# Build with cache mounts
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux \
    go build -a -trimpath -ldflags="-w -s" -o manager cmd/main.go

# Runtime stage - use scratch for smallest possible image
FROM scratch

# Copy SSL certificates from builder
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy binary
COPY --from=builder /workspace/manager /manager

# Create nonroot user manually (scratch doesn't have users)
USER 65532:65532

ENTRYPOINT ["/manager"]
