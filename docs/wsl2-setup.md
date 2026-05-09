# Running VoidRun on WSL2

This guide covers setting up and running the VoidRun sandbox server on **Windows Subsystem for Linux 2 (WSL2)** with Ubuntu 24.04.

## Prerequisites

- Windows 10/11 with WSL2 enabled
- Ubuntu 24.04 LTS (or similar) WSL2 distro
- At least 8 GB RAM allocated to WSL2
- KVM support enabled in WSL2 (see below)

---

## 1. Enable KVM in WSL2

WSL2 supports nested virtualization on Windows 11 (and recent Windows 10 builds). Verify KVM is available:

```bash
ls /dev/kvm
```

If `/dev/kvm` exists, you're good. If not, ensure you're on a recent Windows build and add to your `.wslconfig` (in `%USERPROFILE%\.wslconfig` on Windows):

```ini
[wsl2]
nestedVirtualization=true
memory=8GB
processors=4
```

Then restart WSL: `wsl --shutdown` from PowerShell.

---

## 2. Install Go 1.24

Install Go to your home directory (no sudo required):

```bash
curl -fsSL https://go.dev/dl/go1.24.1.linux-amd64.tar.gz -o /tmp/go.tar.gz
tar -C ~ -xzf /tmp/go.tar.gz
echo 'export PATH=~/go/bin:~/bin:$PATH' >> ~/.bashrc
source ~/.bashrc
go version
# Expected: go version go1.24.x linux/amd64
```

---

## 3. Install Cloud Hypervisor

Download the static binary (no package manager needed):

```bash
mkdir -p ~/bin
curl -fsSL https://github.com/cloud-hypervisor/cloud-hypervisor/releases/latest/download/cloud-hypervisor-static \
  -o ~/bin/cloud-hypervisor
chmod +x ~/bin/cloud-hypervisor
~/bin/cloud-hypervisor --version
```

Make it available under `sudo`:

```bash
sudo ln -sf ~/bin/cloud-hypervisor /usr/local/bin/cloud-hypervisor
```

---

## 4. Install qemu-img

Extract `qemu-img` from the Ubuntu package (no `sudo apt install` needed):

```bash
cd /tmp
apt download qemu-utils
mkdir -p /tmp/qemu-extract
dpkg-deb -x qemu-utils*.deb /tmp/qemu-extract
cp /tmp/qemu-extract/usr/bin/qemu-img ~/bin/qemu-img
chmod +x ~/bin/qemu-img
```

Install shared library dependencies system-wide (requires sudo once):

```bash
# Extract required libraries
for pkg in liburing2 libaio1t64; do
  apt download $pkg
  dpkg-deb -x ${pkg}*.deb /tmp/qemu-extract/
done

# Install libraries system-wide so they work under sudo
sudo cp /tmp/qemu-extract/lib/x86_64-linux-gnu/liburing.so* /usr/local/lib/
sudo cp /tmp/qemu-extract/lib/x86_64-linux-gnu/libaio.so* /usr/local/lib/
sudo ldconfig

# Install qemu-img system-wide
sudo cp ~/bin/qemu-img /usr/local/bin/qemu-img

# Verify
sudo qemu-img --version
# Expected: qemu-img version 8.x.x
```

---

## 5. Install & Start MongoDB

Download and run MongoDB in userspace:

```bash
cd /tmp
curl -fsSL https://fastdl.mongodb.org/linux/mongodb-linux-x86_64-ubuntu2404-8.0.10.tgz -o mongodb.tgz
tar xzf mongodb.tgz
cp mongodb-linux-x86_64-ubuntu2404-8.0.10/bin/mongod ~/bin/

# Create data directories
mkdir -p ~/voidrun-data/mongo ~/voidrun-data/logs

# Start MongoDB (forked to background)
~/bin/mongod --dbpath ~/voidrun-data/mongo \
             --logpath ~/voidrun-data/logs/mongod.log \
             --port 27017 \
             --fork
```

> **Note:** MongoDB needs to be running before starting VoidRun. To stop it later: `kill $(pgrep mongod)`

---

## 6. Grant Network Capabilities to the Binary

The VoidRun server needs `CAP_NET_ADMIN` and `CAP_NET_RAW` to create TAP interfaces. Use `setcap` to grant these to the binary instead of running as root:

```bash
# Build first, then set capabilities
cd /path/to/voidrun
go build -o voidrun ./cmd/server/main.go
sudo setcap cap_net_admin,cap_net_raw+ep ./voidrun

# Verify
getcap ./voidrun
# Expected: ./voidrun cap_net_admin,cap_net_raw=ep
```

> **Note:** `setcap` is reset every time you rebuild the binary. Always re-run `sudo setcap` after rebuilding.

---

## 7. Setup Network Bridge

Create the virtual network bridge that VMs use for connectivity:

```bash
cd /path/to/voidrun
sudo env PATH=$PATH:/usr/sbin go run cmd/setup-net/main.go
```

This creates bridge `vmbr0` with gateway `192.168.100.1/22` and sets up NAT/iptables rules.

> **Note:** If `iptables` is not found, install it: `sudo apt install -y iptables`

---

## 8. Prepare Base Images

Place your VM base image and kernel in the base images directory:

```bash
mkdir -p ~/voidrun-data/base-images ~/voidrun-data/instances

# Copy your kernel and base disk image:
cp /path/to/vmlinux ~/voidrun-data/base-images/
cp /path/to/debian-base.qcow2 ~/voidrun-data/base-images/
```

Expected files:
```
~/voidrun-data/base-images/
├── vmlinux               # Cloud Hypervisor compatible kernel
└── debian-base.qcow2     # Base disk image (qcow2 format)
```

---

## 9. Configure the .env File

Create `.env` in the project root:

```bash
cd /path/to/voidrun
cat > .env << 'EOF'
# Server
SERVER_PORT=9999
SERVER_HOST=0.0.0.0

# MongoDB (local, no auth)
MONGO_URI=mongodb://localhost:27017/vr-db
MONGO_DB=vr-db

# Paths — update these to your actual home directory
BASE_IMAGES_DIR=/home/<your-user>/voidrun-data/base-images
INSTANCES_DIR=/home/<your-user>/voidrun-data/instances
KERNEL_PATH=/home/<your-user>/voidrun-data/base-images/vmlinux

# Network
BRIDGE_NAME=vmbr0
GATEWAY_IP=192.168.100.1/22
NETWORK_CIDR=192.168.100.0/22

# Sandbox defaults
SANDBOX_DEFAULT_VCPUS=1
SANDBOX_DEFAULT_MEMORY_MB=1024
SANDBOX_DEFAULT_DISK_MB=5120
SANDBOX_DEFAULT_IMAGE=debian

# Disk format — IMPORTANT for WSL2
# "qcow2"      = thin overlay with backing file (native Linux only)
# "qcow2-flat" = standalone qcow2, no backing file (works on WSL2)
# "raw"         = raw image copy, auto-converts from qcow2 (recommended for WSL2)
SANDBOX_DISK_FORMAT=qcow2-flat

# System
SYSTEM_USER_NAME=System
SYSTEM_USER_EMAIL=system@local

# Health
HEALTH_ENABLED=true
HEALTH_INTERVAL_SEC=60
HEALTH_CONCURRENCY=16

# Disable auto-lifecycle for dev
AUTO_LIFECYCLE_ENABLED=false

# Metrics
METRICS_ENABLED=true

# CORS
CORS_ENABLED=true
CORS_ALLOW_ORIGINS=*

# Authentication — Clerk (optional, for dashboard/JWT auth)
# Get these from https://dashboard.clerk.com → your app → API Keys
# CLERK_ENABLED=true
# CLERK_SECRET_KEY=sk_test_xxxxx
# CLERK_PUBLISHABLE_KEY=pk_test_xxxxx
# CLERK_JWKS_URL=
EOF
```

### Clerk Authentication Setup (Optional)

VoidRun supports two auth methods:
- **API Key** (`X-API-Key` header) — always available, no external setup needed
- **Clerk JWT** (`Authorization: Bearer <token>`) — for dashboard/frontend auth via [Clerk](https://clerk.com)

To enable Clerk:

1. Create a free account at [clerk.com](https://clerk.com) and create an application
2. Go to **API Keys** in the Clerk dashboard
3. Copy your keys and add them to `.env`:

```env
CLERK_ENABLED=true
CLERK_SECRET_KEY=sk_test_your_secret_key_here
CLERK_PUBLISHABLE_KEY=pk_test_your_publishable_key_here
```

4. (Optional) Set `CLERK_JWKS_URL` if you need custom JWKS endpoint. If left empty, the SDK auto-discovers it from the secret key.

> **Without Clerk:** If `CLERK_ENABLED` is not set or `false`, only API key auth works. However, generating the first API key requires authentication — so **Clerk is needed for initial bootstrap**. The flow is:
>
> 1. Enable Clerk and sign in through your frontend/dashboard to get a JWT
> 2. Use the Clerk Bearer token to call `POST /api/orgs/apikeys` to generate your first API key:
>    ```bash
>    curl -X POST http://localhost:9999/api/orgs/apikeys \
>      -H "Authorization: Bearer <your-clerk-jwt>" \
>      -H "Content-Type: application/json" \
>      -d '{"name": "dev-key"}'
>    ```
> 3. Save the returned API key — it's only shown once
> 4. After that, you can use `X-API-Key: vr_xxxxx` for all API calls and optionally disable Clerk

### Disk Format Options (WSL2)

| Format | Description | WSL2 Compatible | Speed |
|--------|-------------|-----------------|-------|
| `qcow2` | Overlay with backing file (thin provisioning) | **No** — CH static binary lacks backing file support | Instant |
| `qcow2-flat` | Full standalone qcow2 copy | **Yes** | ~5-10s per sandbox (copies full image) |
| `raw` | Raw disk copy, auto-converts qcow2→raw on first use | **Yes** | ~5-10s per sandbox (first run also converts base) |

> **Recommendation:** Use `qcow2-flat` for WSL2. If you experience issues, try `raw`.

---

## 10. Build & Run the Server

### Development Mode

```bash
cd /path/to/voidrun
sudo env PATH=$PATH go run ./cmd/server/main.go
```

> `sudo env PATH=$PATH` preserves your user's `PATH` so `go` is found. No build or `setcap` step needed each time.

### Production Build

```bash
cd /path/to/voidrun
go build -o voidrun ./cmd/server/main.go
sudo setcap cap_net_admin,cap_net_raw+ep ./voidrun
./voidrun
```

Or as a one-liner script:

```bash
go build -o voidrun ./cmd/server/main.go && sudo setcap cap_net_admin,cap_net_raw+ep ./voidrun && ./voidrun
```

You should see:

```
🚀 VoidRun Server 0.1.0 running on 0.0.0.0:9999
```

### Verify the server is running:

```bash
curl http://localhost:9999/api/version
# {"version":"0.1.0","commit":"","buildTime":""}
```

---

## Troubleshooting

### `qemu-img: error while loading shared libraries: liburing.so.2`

Install the libraries system-wide:
```bash
sudo cp ~/lib/liburing.so* /usr/local/lib/
sudo cp ~/lib/libaio.so* /usr/local/lib/
sudo ldconfig
```

### `Failed to create bridge: ip [link add name vmbr0 type bridge]: exit status 2`

Bridge creation requires root:
```bash
sudo env PATH=$PATH:/usr/sbin go run cmd/setup-net/main.go
```

### `failed to generate unique tap` / `TUNSETIFF operation not permitted`

TAP interface creation requires `CAP_NET_ADMIN`. Grant it to the binary:
```bash
go build -o voidrun ./cmd/server/main.go
sudo setcap cap_net_admin,cap_net_raw+ep ./voidrun
./voidrun
```

> **Note:** These capabilities are stripped on every rebuild — re-run `sudo setcap` after each `go build`.

### `Backing file support is disabled`

Cloud Hypervisor's static binary doesn't support qcow2 backing files. Set in `.env`:
```
SANDBOX_DISK_FORMAT=qcow2-flat
```

### `Cannot open kernel file: No such file or directory`

Check `KERNEL_PATH` in `.env` matches your actual kernel file:
```bash
ls -la ~/voidrun-data/base-images/vmlinux*
```
The default expected name is `vmlinux`. If your kernel file has a different name (e.g. `vmlinux-clh`), update `KERNEL_PATH` accordingly.

### `context deadline exceeded` on boot

The CLH client timeout may be too short for large disk images. The default is 30 seconds. If your base image is very large, the flat copy may take longer. Check logs for details.

### `iptables: command not found` during network setup

```bash
sudo apt install -y iptables
```

---

## Quick Start Summary

```bash
# 1. Start MongoDB
~/bin/mongod --dbpath ~/voidrun-data/mongo --logpath ~/voidrun-data/logs/mongod.log --port 27017 --fork

# 2. Setup network (first time or after WSL restart)
cd ~/voidrun
sudo env PATH=$PATH:/usr/sbin go run cmd/setup-net/main.go

# 3. Run the server
sudo env PATH=$PATH go run ./cmd/server/main.go
```

The server will be available at `http://localhost:9999`.
