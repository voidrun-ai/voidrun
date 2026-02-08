# VoidRun API Server

VoidRun is a Go API server for managing sandboxed VMs with file operations, command execution, PTY sessions, and organization/API key management. It exposes a REST API (with WebSocket streams for PTY and file watching) and persists state in MongoDB.

## Highlights

- Sandbox lifecycle management (create, list, delete)
- Command execution (sync + streaming) and background processes
- File system operations (upload, download, list, compress, watch)
- PTY sessions (ephemeral and persistent)
- Org + API key management
- OpenAPI spec included

## Requirements

- Linux host with KVM support
- `cloud-hypervisor` installed on the host at `/usr/local/bin/cloud-hypervisor`
- MongoDB (Docker Compose provided)
- `iptables` and bridge networking tools available on the host

## Quick Start (Docker Compose)

```bash
cd /root/workspace/vr-work/voidrun

docker compose up --build
```

The API is exposed on `http://localhost:8080/api` when using Docker Compose.

## Local Development

### 1) Configure networking for sandboxes

The server expects a Linux bridge for sandbox networking. The `setup-net` tool configures the bridge and NAT.

```bash
go run ./cmd/setup-net
```

### 2) Run MongoDB

```bash
docker run -d --name voidrun-mongo -p 27017:27017 \
	-e MONGO_INITDB_ROOT_USERNAME=root \
	-e MONGO_INITDB_ROOT_PASSWORD=Qaz123wsx123 \
	-e MONGO_INITDB_DATABASE=vr-db \
	mongo:7.0-alpine
```

### 3) Run the API server

```bash
go run ./cmd/server
```

By default the server listens on `:33944`. Set `SERVER_PORT` to change it.

## Authentication Flow

1. Register a user and get a default org + API key.
2. Use the API key for all subsequent requests in the `X-API-Key` header.

```bash
curl -X POST http://localhost:8080/api/register \
	-H 'Content-Type: application/json' \
	-d '{"name":"Admin","email":"admin@example.com"}'
```

```bash
curl http://localhost:8080/api/sandboxes \
	-H 'X-API-Key: hf_your_key_here'
```

## API Base URL

- Docker Compose: `http://localhost:8080/api`
- Local default: `http://localhost:33944/api`

The full OpenAPI spec is in [openapi.yml](openapi.yml).

## Environment Variables

The server reads configuration from environment variables. Common options:

```text
SERVER_PORT=33944
SERVER_HOST=
MONGO_URI=mongodb://root:Qaz123wsx123@localhost:27017/vr-db?authSource=admin
MONGO_DB=vr-db
BASE_IMAGES_DIR=/var/lib/hyper-fleet/base-images
INSTANCES_DIR=/var/lib/hyper-fleet/instances
KERNEL_PATH=/var/lib/hyper-fleet/base-images/vmlinux
BRIDGE_NAME=vmbr0
GATEWAY_IP=192.168.100.1/22
NETWORK_CIDR=192.168.100.0/22
SUBNET_PREFIX=192.168.100.
SYSTEM_USER_NAME=System
SYSTEM_USER_EMAIL=system@local
SANDBOX_DEFAULT_VCPUS=1
SANDBOX_DEFAULT_MEMORY_MB=1024
SANDBOX_DEFAULT_DISK_MB=5120
SANDBOX_DEFAULT_IMAGE=debian
HEALTH_ENABLED=true
HEALTH_INTERVAL_SEC=60
HEALTH_CONCURRENCY=16
API_KEY_CACHE_TTL_SECONDS=3600
```

## Key Endpoints (Summary)

- `POST /api/register` - create user, org, and API key
- `GET /api/sandboxes` - list sandboxes
- `POST /api/sandboxes` - create sandbox
- `GET /api/sandboxes/{id}` - get sandbox
- `DELETE /api/sandboxes/{id}` - delete sandbox
- `POST /api/sandboxes/{id}/exec` - execute command
- `POST /api/sandboxes/{id}/exec-stream` - stream exec output
- `POST /api/sandboxes/{id}/pty/sessions` - create PTY session
- `GET /api/sandboxes/{id}/files` - list files
- `POST /api/sandboxes/{id}/files/upload` - upload file
- `GET /api/sandboxes/{id}/files/watch/{sessionId}/stream` - watch file events (WS)

See [openapi.yml](openapi.yml) for full details.

## Troubleshooting

- If the server fails to start, verify MongoDB connectivity and KVM support.
- PTY and file watch use WebSockets; ensure your proxy allows WS upgrades.
- Sandbox networking issues usually indicate missing bridge or iptables rules.

## License

Proprietary
