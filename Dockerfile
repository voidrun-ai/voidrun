# syntax=docker/dockerfile:1.7
# Multi-stage Dockerfile for voidrun
# Stage 1: Build stage
FROM golang:1.24.11-alpine AS builder

# Speed up module downloads and allow caching
ENV GOPROXY=https://proxy.golang.org,direct

# Install build dependencies
RUN apk add --no-cache \
    git \
    gcc \
    musl-dev \
    ca-certificates \
    tzdata

# Set working directory
WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download && \
    go mod verify

# Copy source code
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg

# Build the application with optimizations
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
    -trimpath \
    -ldflags="-s -w" \
    -o voidrun \
    ./cmd/server/main.go

# Build setup-net CLI
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
    -trimpath \
    -ldflags="-s -w" \
    -o voidrun-setup-net \
    ./cmd/setup-net/main.go

# Verify binary was created
RUN ls -lh /build/voidrun

# Stage 2: Runtime stage
FROM alpine:3.20

# Install runtime dependencies
# Note: cloud-hypervisor must be installed on the host at /usr/local/bin/cloud-hypervisor
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    bash \
    curl \
    jq \
    socat \
    iproute2 \
    iptables \
    qemu-img

# Create non-root user (optional, for security)
# RUN addgroup -g 1000 voidrun && \
#     adduser -D -u 1000 -G voidrun voidrun

# Copy binary from builder
COPY --from=builder /build/voidrun /usr/local/bin/voidrun
COPY --from=builder /build/voidrun-setup-net /usr/local/bin/voidrun-setup-net

# Copy static files (dashboard)
COPY static /app/static

# Create necessary directories
RUN mkdir -p /app/logs && \
    chmod +x /usr/local/bin/voidrun

# Set working directory
WORKDIR /app

# Expose the API port
EXPOSE 8080

# Run the server
CMD ["voidrun"]
