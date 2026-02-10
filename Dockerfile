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
    -o hyper-server \
    ./cmd/server/main.go

# Build setup-net CLI
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
    -trimpath \
    -ldflags="-s -w" \
    -o hyper-setup-net \
    ./cmd/setup-net/main.go

# Verify binary was created
RUN ls -lh /build/hyper-server

# Stage 2: Runtime stage
FROM alpine:3.20

# Install runtime dependencies
# Note: cloud-hypervisor must be installed on the host at /usr/local/bin/cloud-hypervisor
# This keeps VM processes outside pod memory limits
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
COPY --from=builder /build/hyper-server /usr/local/bin/hyper-server
COPY --from=builder /build/hyper-setup-net /usr/local/bin/hyper-setup-net

# Copy static files (dashboard)
COPY static /app/static

# Create necessary directories
RUN mkdir -p /app/logs && \
    chmod +x /usr/local/bin/hyper-server

# Set working directory
WORKDIR /app

# # Health check
# HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
#     CMD curl -f http://localhost:8080/vms || exit 1

# Expose the API port
EXPOSE 8080

# Set environment variables with defaults
ENV SERVER_PORT=8080
ENV SERVER_HOST=0.0.0.0
ENV MONGO_URI=mongodb://root:Qaz123wsx123@mongo:27017/vr-db?authSource=admin
ENV MONGO_DB=vr-db
ENV BASE_IMAGES_DIR=/root/void-run-test/base-images
ENV INSTANCES_DIR=/root/void-run-test/instances
ENV KERNEL_PATH=/root/void-run-test/base-images/vmlinux
ENV BRIDGE_NAME=vmbr0
ENV GATEWAY_IP=10.20.0.1/22
ENV NETWORK_CIDR=10.20.0.0/22
ENV SUBNET_PREFIX=10.20.0.
ENV SYSTEM_USER_NAME=System
ENV SYSTEM_USER_EMAIL=system@local
ENV SANDBOX_DEFAULT_VCPUS=1
ENV SANDBOX_DEFAULT_MEMORY_MB=1024
ENV SANDBOX_DEFAULT_DISK_MB=5120
ENV SANDBOX_DEFAULT_IMAGE=debian
ENV HEALTH_ENABLED=true
ENV HEALTH_INTERVAL_SEC=60
ENV HEALTH_CONCURRENCY=16
ENV API_KEY_CACHE_TTL_SECONDS=3600

# Run the server
CMD ["hyper-server"]
