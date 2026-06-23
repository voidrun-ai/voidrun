# Firecracker + Jailer Setup

VoidRun runs Firecracker **only via the jailer** (`firecracker-jailer`). Bare `firecracker` is not used.

## Requirements

- Linux with `/dev/kvm`
- Matching `firecracker` and `firecracker-jailer` binaries (same release)
- Root or capabilities for jailer, netns, and bridge setup
- PVH-enabled guest kernel at `KERNEL_PATH`

## Install (example v1.10.0)

```bash
VER=1.10.0
curl -fsSL -o /tmp/fc.tgz \
  "https://github.com/firecracker-microvm/firecracker/releases/download/v${VER}/firecracker-v${VER}-x86_64.tgz"
tar xzf /tmp/fc.tgz -C /tmp
sudo cp /tmp/release-v${VER}-x86_64/firecracker-v${VER}-x86_64 /usr/local/bin/firecracker
sudo cp /tmp/release-v${VER}-x86_64/jailer-v${VER}-x86_64 /usr/local/bin/firecracker-jailer
```

## Environment

| Variable | Default | Purpose |
|----------|---------|---------|
| `HYPERVISOR` | `cloud_hypervisor` | Set to `firecracker` for FC hosts |
| `FC_PATH` | `/usr/local/bin/firecracker` | Firecracker binary |
| `FC_JAILER_PATH` | `/usr/local/bin/firecracker-jailer` | Jailer binary |
| `FC_JAIL_UID` | `1000` | UID inside jail |
| `FC_JAIL_GID` | `1000` | GID inside jail |
| `FC_CHROOT_BASE_DIR` | `{INSTANCES_DIR}/jails` | Chroot base |
| `SANDBOX_DISK_FORMAT` | `qcow2` | Use `raw` or `qcow2-flat` for FC |

## Jailer user

```bash
sudo useradd -r -s /bin/false -u 1000 voidrun-fc 2>/dev/null || true
```

## Verify

```bash
./scripts/test-hypervisors.sh
HYPERVISOR=firecracker go test -tags=integration ./plugins/firecracker/...
```

On a DO droplet, enable **nested virtualization** (or use bare-metal) so `/dev/kvm` exists.
