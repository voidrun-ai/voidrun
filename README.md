# VoidRun API Server

VoidRun is a Go API server for managing sandboxes with file operations, command execution, PTY sessions, and organization/API key management. It exposes a REST API (with WebSocket streams for PTY and file watching) and persists state in MongoDB.

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
2. Authenticate with either:
   - `X-API-Key` (org is derived from the key and cannot be overridden), or
   - `Authorization: Bearer <jwt>` plus `X-Org-ID` for org context.
3. All resource access is tenant-scoped by validated `orgId`.

```bash
curl -X POST http://localhost:8080/api/register \
	-H 'Content-Type: application/json' \
	-d '{"name":"Admin","email":"admin@example.com"}'
```

```bash
curl http://localhost:8080/api/sandboxes \
	-H 'X-API-Key: hf_your_key_here'
```

```bash
curl http://localhost:8080/api/sandboxes \
	-H 'Authorization: Bearer <jwt_token>' \
	-H 'X-Org-ID: <org_object_id>'
```

## API Base URL

- Docker Compose: `http://localhost:8080/api`
- Local default: `http://localhost:33944/api`

The full OpenAPI spec is in [openapi.yml](openapi.yml).

## Model Context Protocol (MCP)

The API exposes a **Streamable HTTP** MCP endpoint at **`POST /api/mcp`** (same auth as the REST API: **`X-API-Key`** or Bearer JWT + **`X-Org-ID`**). Tool handlers manage sandboxes, exec, and files in ```14:54:voidrun/mcp/server.go```.

### Use VoidRun MCP as a tool in Cursor

1. Start the API (Docker Compose on **8080**, or local `go run` and set **`SERVER_PORT`** if not using 8080).
2. Export a valid API key in the environment Cursor inherits (desktop launchers often do not load shell rc files):

   ```bash
   export VOIDRUN_API_KEY="hf_your_key_here"
   ```

3. Project config lives at **[`.cursor/mcp.json`](../.cursor/mcp.json)** in this repo. Edit the **`url`** if your server is not at `http://127.0.0.1:8080/api/mcp` (for example `http://127.0.0.1:33944/api/mcp` for the default local port).
4. Reload MCP in Cursor (**Settings → MCP**) and confirm **voidrun** connects. Check **Output → MCP** if initialization fails.

Secrets should stay in **`${env:VOIDRUN_API_KEY}`**; do not commit real keys. For **Clerk JWT** instead of an API key, Cursor’s static **`headers`** map is awkward for short-lived tokens; prefer an API key for this integration.

### Other ways to test MCP

1. **MCP Inspector (recommended)** — Official UI and CLI for Streamable HTTP. Requires Node **^22.7.5** per [modelcontextprotocol/inspector](https://github.com/modelcontextprotocol/inspector).

   - **UI:** `npx @modelcontextprotocol/inspector` → open **http://localhost:6274**, choose transport **streamable-http**, set server URL to your **`/api/mcp`** endpoint (query shortcut: `?transport=streamable-http&serverUrl=http://127.0.0.1:8080/api/mcp`). Configure auth so **`X-API-Key`** is sent (use the sidebar auth / header options; Inspector documents Bearer for SSE, but **CLI** below supports arbitrary headers).
   - **CLI (scriptable / CI-friendly):**

     ```bash
     npx @modelcontextprotocol/inspector --cli http://127.0.0.1:8080/api/mcp \
       --transport http \
       --header "X-API-Key: hf_your_key_here" \
       --method tools/list
     ```

     Use **`--method tools/call`** with **`--tool-name`** / **`--tool-arg`** to exercise a specific tool.

2. **Raw HTTP** — Any client that can send **`Content-Type: application/json`** JSON-RPC to **`POST /api/mcp`**, plus **`X-API-Key`**, then reuse **`Mcp-Session-Id`** from the **`initialize`** response on later POSTs (see [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go) **`streamable_http_test.go`** for request shapes).

3. **API platforms** — **Bruno**, **Insomnia**, or **Postman**: run **`initialize`**, capture **`Mcp-Session-Id`**, then **`tools/list`** / **`tools/call`** in separate requests with the same header.

4. **Automated tests in-repo** — There is no dedicated **`voidrun/mcp/*_test.go`** yet; you can add an integration test that builds the Gin router (or uses **`httptest`**) and posts the same JSON-RPC sequence, or call the Inspector CLI from a shell script in CI.

## Package Layout (Public)

All previously `internal/*` and `pkg/*` packages are now top-level public packages for extension use-cases (for example from `voidrun-ee`):

- `voidrun/config`
- `voidrun/server`
- `voidrun/service`
- `voidrun/repository`
- `voidrun/middleware`
- `voidrun/model`
- `voidrun/machine`
- `voidrun/storage`
- `voidrun/util`

## Environment Variables

The server reads configuration from environment variables. Common options:

```text
SERVER_PORT=33944
SERVER_HOST=
MONGO_URI=mongodb://root:Qaz123wsx123@localhost:27017/vr-db?authSource=admin
MONGO_DB=vr-db
BASE_IMAGES_DIR=/var/lib/voidrun/base-images
INSTANCES_DIR=/var/lib/voidrun/instances
KERNEL_PATH=/var/lib/voidrun/base-images/vmlinux
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
JWT_SECRET=change-me-in-production
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
