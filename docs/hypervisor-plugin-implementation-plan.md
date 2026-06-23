# Hypervisor Plugin Architecture — Implementation Plan

**Branch:** `cursor/hypervisor-plugin-plan-4063`  
**Status:** Planning  
**Date:** 2026-06-23  
**Module:** `voidrun`

## 1. Objective

Introduce a **single-binary, registry-based hypervisor plugin layer** (`pkg/compute`) that lets VoidRun orchestrate sandboxes on **Cloud Hypervisor** and **Firecracker** without the core application knowing VMM-specific details.

This plan assumes the **snapshot/restore lifecycle** from `origin/feat/ch-snap-restore` becomes the canonical model (replacing start/stop/pause/resume on `main`). The plugin interface is designed against that branch, not the current `main` pause/resume flow.

### Goals

- Core (`service/`, `handler/`) talks only to `compute.Hypervisor`
- Plugins register via `init()` + blank imports (no gRPC, no go-plugin)
- Every VM operation accepts `context.Context`
- `VMConfig` passed by value; no global VM state inside plugins
- Cloud Hypervisor plugin wraps existing CH code (including snapshot/restore)
- Firecracker plugin implements the same interface with FC-native APIs
- Backward compatible: default hypervisor remains `cloud_hypervisor`

### Non-goals (this effort)

- Cross-hypervisor snapshot migration
- Replacing Gin handlers or MongoDB persistence layer
- Merging `feat/ch-snap-restore` in this branch (prerequisite tracked separately)

---

## 2. Prerequisites

### 2.1 Merge `feat/ch-snap-restore` first

Before extracting plugins, land the snapshot lifecycle on `main`:

| Item | Branch | Action |
|------|--------|--------|
| Snapshot/restore API | `feat/ch-snap-restore` | Merge after fixing blockers below |
| Landlock parity on restore | snapshot branch | Apply same landlock builder as create |
| DNS iptables rule ordering | snapshot branch | Move DNS ACCEPT after RFC1918 drops |
| Auto-restore context | snapshot branch | Use detached ctx for `singleflight`, not caller's HTTP ctx |
| `go test ./...` green | snapshot branch | Fix failing tests before merge |

**Recommended merge order:**

```
main
  └── merge feat/ch-snap-restore  →  main (snapshot lifecycle)
        └── cursor/hypervisor-plugins-impl-4063  →  plugin extraction + FC
```

This planning branch (`cursor/hypervisor-plugin-plan-4063`) documents the design. Implementation work starts on a child branch rebased onto post-merge `main`.

### 2.2 Host requirements

| Hypervisor | Binary | Default path |
|------------|--------|--------------|
| Cloud Hypervisor | `cloud-hypervisor` | `CH_PATH` / `/usr/local/bin/cloud-hypervisor` |
| Firecracker | `firecracker` | `FC_PATH` / `/usr/local/bin/firecracker` |
| Firecracker Jailer | `firecracker-jailer` (same version as FC) | `FC_JAILER_PATH` / `/usr/local/bin/firecracker-jailer` |

Both require KVM (`/dev/kvm`). Firecracker hosts need a PVH-compatible kernel (may share `KERNEL_PATH` or use `FC_KERNEL_PATH`).

Firecracker **must always run inside the jailer** — bare `firecracker` binary invocation is not supported in production or integration tests. The jailer and Firecracker binaries must be the **same version** (static musl build).

---

## 3. Target Directory Structure

```
voidrun/
├── cmd/
│   └── server/
│       └── main.go                          # blank-import plugins
├── pkg/
│   └── compute/
│       ├── interface.go                     # Hypervisor, VMConfig, VMState
│       ├── registry.go                      # Register / Get (sync.RWMutex)
│       └── paths.go                         # shared instance path helpers (moved from runtime/client.go)
├── plugins/
│   ├── cloudhypervisor/
│   │   ├── plugin.go                        # init() → Register
│   │   ├── lifecycle.go                     # StartVM, StopVM, Snapshot, Restore
│   │   ├── client.go                        # from runtime/clh_client.go
│   │   ├── config.go                        # from runtime/clh_config.go
│   │   ├── types.go                         # from runtime/clh_types.go
│   │   ├── helpers.go                       # from runtime/clh_helpers.go
│   │   ├── events.go                        # CLH vm.evt parser
│   │   └── security.go                      # landlock + seccomp CLI builder (shared create/restore)
│   └── firecracker/
│       ├── plugin.go
│       ├── lifecycle.go
│       ├── jailer.go                        # jailer spawn, chroot prep, cleanup
│       ├── client.go                        # FC REST over Unix socket
│       ├── config.go                        # FC API types
│       └── types.go
├── runtime/                                 # orchestration only (hypervisor-agnostic)
│   ├── network.go
│   ├── disk.go
│   ├── agent_client.go
│   ├── event_monitor.go                     # dispatches to plugin EventSource
│   └── facade.go                            # thin wrappers: resolve hypervisor → delegate
├── service/
│   └── sandbox.go                           # uses runtime/facade or compute.Get
├── config/
│   └── config.go                            # HYPERVISOR, FC_PATH, HypervisorConfig
└── model/
    └── sandbox.go                           # Hypervisor string field
```

---

## 4. Core Interface Design

### 4.1 Types (`pkg/compute/interface.go`)

```go
package compute

type DiskFormat string

const (
    FormatRaw    DiskFormat = "raw"
    FormatQcow2  DiskFormat = "qcow2"
)

type Volume struct {
    Path   string
    Format DiskFormat
}

// VMConfig is passed by value to every boot/restore call.
type VMConfig struct {
    ID             string
    VCPU           int
    MemMB          int
    KernelPath     string
    InitrdPath     string
    KernelCmdline  string
    RootVolume     Volume
    EnableSecurity bool

    // Orchestration context (set by service layer, opaque to HTTP)
    InstanceDir  string
    NetNSName    string
    TapName      string
    MacAddress   string
    VsockCID     uint32
    SnapshotDir  string // non-empty only for Restore
}

type VMState string

const (
    VMStateNotStarted VMState = "not_started"
    VMStateRunning    VMState = "running"
    VMStatePaused     VMState = "paused"
    VMStateStopped    VMState = "stopped"  // guest off, VMM may be alive
    VMStateDead       VMState = "dead"     // VMM process gone
)

type Hypervisor interface {
    Name() string

    StartVM(ctx context.Context, cfg VMConfig) error
    StopVM(ctx context.Context, id string) error
    Snapshot(ctx context.Context, id string, snapshotDir string) error
    Restore(ctx context.Context, cfg VMConfig) error

    GetState(ctx context.Context, id string) (VMState, error)
    Info(ctx context.Context, id string) ([]byte, error)
    IsAvailable(id string) bool

    // Optional: nil if unsupported
    Counters(ctx context.Context, id string) ([]byte, error)
    EventSource(id string) (EventSource, error)
}

type EventSource interface {
    Poll(ctx context.Context, offset int64) (events []Event, newOffset int64, err error)
}

type Event struct {
    Name       string
    Source     string
    Properties map[string]any
    UptimeNs   int64
}
```

### 4.2 Registry (`pkg/compute/registry.go`)

```go
var (
    mu        sync.RWMutex
    factories = map[string]func() Hypervisor{}
)

func Register(name string, factory func() Hypervisor)
func Get(name string) (Hypervisor, error)
func List() []string
```

Rules:
- `Register` panics on duplicate name (fail fast at init)
- `Get` returns a **new instance** from factory (stateless plugins)
- Plugins must not store per-VM state in package globals

### 4.3 Hypervisor resolution

```go
// Priority:
// 1. sandbox.Hypervisor (DB field, set at create)
// 2. config.Hypervisor.Default (HYPERVISOR env)
// 3. "cloud_hypervisor"
func Resolve(cfg config.Config, sandbox *model.Sandbox) (compute.Hypervisor, error)
```

Env vars:

| Variable | Default | Purpose |
|----------|---------|---------|
| `HYPERVISOR` | `cloud_hypervisor` | Host default backend |
| `CH_PATH` | `/usr/local/bin/cloud-hypervisor` | CH binary |
| `FC_PATH` | `/usr/local/bin/firecracker` | FC binary (must match jailer version) |
| `FC_JAILER_PATH` | `/usr/local/bin/firecracker-jailer` | Jailer binary |
| `FC_JAIL_UID` | `1000` | UID jailer drops to before exec |
| `FC_JAIL_GID` | `1000` | GID jailer drops to before exec |
| `FC_CHROOT_BASE_DIR` | `{INSTANCES_DIR}/jails` | Base for `<chroot_base>/firecracker/<id>/root` |
| `FC_KERNEL_PATH` | `""` (falls back to `KERNEL_PATH`) | FC-specific kernel |

---

## 5. Plugin Implementations

### 5.1 Cloud Hypervisor (`plugins/cloudhypervisor`)

**Registration:**

```go
func init() {
    compute.Register("cloud_hypervisor", func() compute.Hypervisor {
        return &Provider{Binary: resolveBinary()}
    })
}
```

**Method mapping (from `feat/ch-snap-restore` lifecycle):**

| Interface method | CH implementation |
|------------------|-------------------|
| `StartVM` | `CreateCLI` — full CLI boot inside netns |
| `StopVM` | `VmmShutdown` + wait for socket gone + force-kill fallback |
| `Snapshot` | `VmPause` → `VmSnapshot` → `VmmShutdown` → prune old snaps |
| `Restore` | spawn empty CH → `VmRestore` → `VmBoot` (or `RestoreCLI`) |
| `GetState` | `vm.info` state → normalized `VMState` |
| `Counters` | `GET vm.counters` |
| `EventSource` | tail `vm.evt` JSON stream |

**Security (`EnableSecurity == true`):**
- `--seccomp true`
- `--landlock` + rules from shared `security.go`
- **Same policy on create and restore** (fixes snapshot branch gap)

**Process spawn pattern:**

```go
cmd := exec.CommandContext(ctx, "ip", append(
    []string{"netns", "exec", cfg.NetNSName, p.binary},
    args...,
)...)
```

**Socket paths** (per instance dir, not global `/run`):

```
{INSTANCES_DIR}/{id}/vm.sock
{INSTANCES_DIR}/{id}/vsock.sock
{INSTANCES_DIR}/{id}/vm.pid
{INSTANCES_DIR}/{id}/vm.log
{INSTANCES_DIR}/{id}/vm.evt
{INSTANCES_DIR}/{id}/snapshots/snap-{nanos}/
```

### 5.2 Firecracker (`plugins/firecracker`) — **always via jailer**

**Registration:** `compute.Register("firecracker", ...)`

Firecracker is **never** spawned directly. All VM processes go through `firecracker-jailer`, which provides chroot isolation, privilege dropping, cgroups, and optional netns join. This is Firecracker's recommended production model and maps to VoidRun's `EnableSecurity` semantics for the FC backend.

#### Jailer spawn pattern

```bash
firecracker-jailer \
  --id <sandbox_id> \
  --exec-file /usr/local/bin/firecracker \
  --uid <FC_JAIL_UID> \
  --gid <FC_JAIL_GID> \
  --chroot-base-dir <FC_CHROOT_BASE_DIR> \
  --netns /var/run/netns/<netns_name> \
  --daemonize \
  --new-pid-ns \
  --cgroup cpuset.cpus=<vcpu_affinity> \
  --resource-limit no-file=2048 \
  -- \
  --api-sock run/firecracker.socket \
  --log-path run/firecracker.log
```

Go equivalent:

```go
cmd := exec.CommandContext(ctx, jailerPath,
    "--id", cfg.ID,
    "--exec-file", fcBinary,
    "--uid", strconv.Itoa(jailUID),
    "--gid", strconv.Itoa(jailGID),
    "--chroot-base-dir", chrootBase,
    "--netns", filepath.Join("/var/run/netns", cfg.NetNSName),
    "--daemonize",
    "--new-pid-ns",
    "--cgroup", fmt.Sprintf("cpuset.cpus=%d", cfg.VCPU-1), // example
    "--",
    "--api-sock", "run/firecracker.socket",
    "--log-path", "run/firecracker.log",
)
```

**Notes:**
- Jailer must run as **root** (VoidRun daemon already runs privileged for netns/KVM).
- `--netns` is passed to the **jailer** (not `ip netns exec`), which joins the sandbox netns before chroot — same isolation model as CH.
- Extra FC flags after `--` are forwarded unchanged to the jailed Firecracker process.
- Jailer and Firecracker binary versions **must match** (same release artifact).

#### Chroot layout

Jailer creates:

```
{FC_CHROOT_BASE_DIR}/firecracker/{sandbox_id}/root/   ← chroot_dir
├── firecracker          # copy of exec-file (jailer does this)
├── firecracker.pid      # PID of jailed FC process (read by orchestrator)
├── run/
│   ├── firecracker.socket   # API socket (default path inside jail)
│   └── firecracker.log
├── vmlinux              # hardlink from host (orchestrator prepares)
├── rootfs.img           # hardlink of overlay disk (orchestrator prepares)
├── vsock.sock           # hardlink; vsock uds_path in API must be jail-relative
└── snapshots/           # hardlink snapshot mem/state files on restore
```

**Orchestrator responsibilities before jailer starts** (`plugins/firecracker/jailer.go`):

1. Ensure `{chroot_base}/firecracker/{id}/root` does not exist (unique ID per sandbox).
2. Let jailer create the chroot, **or** pre-create and hardlink resources into `root/` after jailer init (preferred: prepare links in a `PrepareJail(ctx, cfg)` step immediately before spawn).
3. Hardlink (not symlink) kernel, disk, vsock path, and snapshot files into chroot — jailer docs require hardlinks/copies with correct ownership for the jail UID/GID.
4. `chown` jail resources to `FC_JAIL_UID:FC_JAIL_GID` so the dropped-privilege FC process can read/write disks and create sockets.

**Host-visible API socket path** (for REST client):

```
{FC_CHROOT_BASE_DIR}/firecracker/{id}/root/run/firecracker.socket
```

Store this in `pkg/compute/paths.go` as `GetFCAPISocketPath(id)`.

**PID file** (for stop/delete):

```
{FC_CHROOT_BASE_DIR}/firecracker/{id}/root/firecracker.pid
```

#### Method mapping

| Interface method | FC + jailer implementation |
|------------------|---------------------------|
| `StartVM` | `PrepareJail` → spawn jailer → wait for API socket → PUT boot-source, machine-config, drives, net, vsock → `InstanceStart` |
| `StopVM` | `SendCtrlAltDel` via API → wait for socket gone → read `firecracker.pid` → SIGKILL fallback |
| `Snapshot` | `Pause` → `CreateSnapshot` (paths inside chroot) → kill via PID file → cleanup socket |
| `Restore` | `PrepareJail` with snapshot hardlinks → spawn jailer → `LoadSnapshot` → `Resume` |
| `Delete` | kill process → remove `{chroot_base}/firecracker/{id}/` tree + cgroup dir |
| `GetState` | `GET /` on host-visible API socket |
| `Counters` | optional `GET /metrics` if FIFO enabled; else nil |
| `EventSource` | nil (no file stream); health poll only |

#### Security mapping

| `EnableSecurity` | CH backend | FC backend |
|------------------|------------|------------|
| `true` (default) | `--seccomp` + `--landlock` | jailer (always) + cgroups + `--resource-limit` |
| `false` | seccomp/landlock off | **still use jailer** — jailer is mandatory for FC; only skip optional cgroup/resource-limit tuning |

Jailer is not optional for Firecracker even when `EnableSecurity=false`. The flag controls extra cgroup/resource-limit hardening on top of the jail.

#### Disk policy

- Reject `FormatQcow2` with backing overlay
- Prefer `FormatRaw` or standalone qcow2 copy (`qcow2-flat`)
- Disk hardlinked into chroot as `rootfs.img` (jail-relative path in `PUT /drives/root`)

#### Vsock

- `PUT /vsock` with `uds_path` set to jail-relative path (e.g. `vsock.sock`)
- Hardlink host `{instanceDir}/vsock.sock` into chroot before boot
- Guest agent `CONNECT` handshake unchanged on host side (dial host path `{instanceDir}/vsock.sock`)

#### Cleanup on delete

```go
// 1. Kill via firecracker.pid inside chroot
// 2. os.RemoveAll("{chroot_base}/firecracker/{id}/")
// 3. Best-effort: remove cgroup at /sys/fs/cgroup/*/firecracker/{id}/
```

---

## 6. What Stays Outside Plugins

| Package | Responsibility |
|---------|----------------|
| `runtime/network.go` | netns, veth, tap0, iptables isolation |
| `runtime/disk.go` | qemu-img overlay/flat/raw; hypervisor format validation |
| `runtime/agent_client.go` | vsock dial, probe, execute (hypervisor-agnostic) |
| `service/sandbox.go` | IP alloc, DB persistence, singleflight restore, agent wait |
| `service/lifecycle_manager.go` | auto-snapshot, auto-delete timers |
| `metrics/manager.go` | scrape via `Hypervisor.Counters()` + agent fallback |
| `runtime/event_monitor.go` | plugin `EventSource` or skip for FC |

---

## 7. Data Model Changes

### `model/sandbox.go`

```go
type Sandbox struct {
    // ... existing fields ...
    Hypervisor string `bson:"hypervisor,omitempty" json:"hypervisor,omitempty"`
}
```

- Missing/empty → `"cloud_hypervisor"`
- Set at create from request or host default

### `model/request.go`

```go
type CreateSandboxRequest struct {
    // ... existing fields ...
    Hypervisor string `json:"hypervisor,omitempty"` // optional override
}
```

Validation: `cloud_hypervisor` | `firecracker` only.

---

## 8. Service Layer Changes

### `service/sandbox.go` refactor map

| Current call | Replacement |
|--------------|-------------|
| `runtime.CreateCLI(cfg, spec, overlay)` | `hv.StartVM(ctx, toVMConfig(...))` |
| `runtime.Snapshot(id)` | `hv.Snapshot(ctx, id, snapshotDir)` |
| `runtime.Restore(cfg, spec, overlay, dir)` | `hv.Restore(ctx, toVMConfig(...))` |
| `runtime.Stop(id)` | `hv.StopVM(ctx, id)` |
| `runtime.Info(id)` | `hv.Info(ctx, id)` |
| `runtime.NewAPIClientForSandbox(id)` in health | `hv.GetState(ctx, id)` |

### Context propagation fixes

All VM calls from HTTP handlers must pass `r.Context()` (or a derived ctx). Auto-restore via `singleflight` must use `context.WithTimeout(context.Background(), ...)` — not the first caller's cancelable ctx.

### `EnsureRunning` (post-snapshot branch)

Auto-restore on API access calls `Restore()` when status is `snapshotted`. Hypervisor type loaded from `sandbox.Hypervisor`.

---

## 9. Implementation Phases

### Phase 0 — Prerequisites (separate PR)

**Branch:** merge `feat/ch-snap-restore` → `main`

Tasks:
- [ ] Fix Landlock parity on restore path
- [ ] Fix DNS iptables ordering + `network_test.go`
- [ ] Fix auto-restore context (`singleflight` uses independent ctx)
- [ ] Green `go test ./...`
- [ ] Update OpenAPI for `/snapshot`, `/restore` endpoints

**Exit criteria:** `main` has snapshot lifecycle; tests pass.

---

### Phase 1 — `pkg/compute` skeleton (no behavior change)

**Branch:** `cursor/hypervisor-plugins-impl-4063` (off post-merge `main`)

Tasks:
- [ ] Add `pkg/compute/interface.go`, `registry.go`, `registry_test.go`
- [ ] Add `pkg/compute/paths.go` (move path helpers from `runtime/client.go`)
- [ ] Add `plugins/cloudhypervisor/plugin.go` with stub `Hypervisor` (returns `errNotImplemented`)
- [ ] Blank import in `cmd/server/main.go`
- [ ] `config/config.go`: add `HypervisorConfig{Default, CHPath, FCPath}`
- [ ] `model/sandbox.go`: add `Hypervisor` field

**Exit criteria:** compiles; registry tests pass; zero runtime behavior change.

---

### Phase 2 — Extract CH plugin (parity with snapshot branch)

Tasks:
- [ ] Move `runtime/clh_*.go` → `plugins/cloudhypervisor/`
- [ ] Implement full `Hypervisor` interface delegating to moved code
- [ ] Extract `security.go` — shared landlock rule builder for create + restore
- [ ] Add `runtime/facade.go` — `Resolve()` + delegate to `compute.Get`
- [ ] Refactor `service/sandbox.go` to use facade
- [ ] Add `ctx` to all lifecycle calls
- [ ] Remove old `runtime/clh_*.go` and direct CH imports from service
- [ ] `runtime/event_monitor.go` — use CH plugin `EventSource`
- [ ] `metrics/manager.go` — use `Hypervisor.Counters()`

**Exit criteria:** all existing CH integration tests pass; snapshot/restore/create/delete unchanged on CH hosts.

---

### Phase 3 — Firecracker plugin (jailer required)

Tasks:
- [ ] Add `plugins/firecracker/jailer.go` — `PrepareJail`, `SpawnJailer`, `CleanupJail`, hardlink helpers
- [ ] Add `plugins/firecracker/client.go` — FC REST client over host-visible jail API socket
- [ ] Implement `StartVM`, `StopVM`, `Snapshot`, `Restore`, `Delete`, `GetState` — all via jailer
- [ ] Add jailer config: `FC_JAILER_PATH`, `FC_JAIL_UID`, `FC_JAIL_GID`, `FC_CHROOT_BASE_DIR`
- [ ] Create dedicated system user `voidrun-fc` (or configurable UID/GID) in setup docs
- [ ] Blank import `plugins/firecracker` in `cmd/server/main.go`
- [ ] Disk validation: reject qcow2 backing overlay for FC
- [ ] Integration tests (root + KVM): jailer spawn, chroot isolation, full lifecycle
- [ ] `docs/firecracker-setup.md` — jailer install, version pinning, user setup

**Exit criteria:** full sandbox lifecycle works on FC host with `HYPERVISOR=firecracker`; all FC processes run jailed as non-root UID; no bare `firecracker` spawn path exists.

---

### Phase 4 — API, observability, hardening

Tasks:
- [ ] Expose `hypervisor` in sandbox list/get/create responses
- [ ] OpenAPI: optional `hypervisor` on create
- [ ] Health monitor: normalized `VMState` (no CH-specific strings in service)
- [ ] Lifecycle manager: load hypervisor per sandbox from DB
- [ ] README update: dual hypervisor requirements (CH landlock vs FC jailer)
- [ ] Cgroup cleanup on sandbox delete (FC jailer leaves cgroup dirs)

**Exit criteria:** documented dual-backend deployment; OpenAPI updated.

---

## 10. Testing Strategy

### Unit tests

| Package | Coverage |
|---------|----------|
| `pkg/compute` | Register duplicate panic, Get unknown, List |
| `plugins/cloudhypervisor` | CLI arg builder, landlock rules, state normalization |
| `plugins/firecracker` | API payload builder, action types, state parse |
| `plugins/firecracker/jailer` | jailer CLI arg builder, chroot path resolution, hardlink manifest |
| `runtime/disk.go` | Format rejection per hypervisor |

### Integration tests (KVM + root required)

```bash
# CH (default)
sudo -E go test -tags=integration ./plugins/cloudhypervisor/...

# FC via jailer (root required for jailer + netns)
sudo -E HYPERVISOR=firecracker \
  FC_JAILER_PATH=/usr/local/bin/firecracker-jailer \
  FC_JAIL_UID=1000 FC_JAIL_GID=1000 \
  go test -tags=integration ./plugins/firecracker/...
```

Scenarios per backend:
1. Create → agent probe → exec command
2. Snapshot → verify snapshotted status → restore → agent probe
3. Delete → files removed, chroot tree removed, cgroup cleaned up
4. Concurrent create (10 sandboxes) — no socket/chroot ID collisions
5. Context cancel during boot — jailer/FC process killed, chroot cleaned up

**FC jailer-specific integration tests:**

| Test | Assert |
|------|--------|
| `TestFC_JailerProcessNotRoot` | FC process UID == `FC_JAIL_UID` |
| `TestFC_ApiSocketInsideChroot` | socket at `{chroot}/run/firecracker.socket` |
| `TestFC_HardlinksInChroot` | kernel + disk exist in chroot, not symlinks to host |
| `TestFC_JailerVersionMatch` | jailer + firecracker from same release |
| `TestFC_NetnsJoined` | TAP reachable inside jail netns |
| `TestFC_SnapshotRestore` | snapshot files created inside chroot; restore re-hardlinks |
| `TestFC_DeleteCleansChroot` | `{chroot_base}/firecracker/{id}/` gone after delete |

**Host setup for FC integration tests:**

```bash
# Create dedicated jail user (once per host)
sudo useradd -r -s /bin/false -u 1000 voidrun-fc 2>/dev/null || true

# Install matching FC + jailer from same release
curl -LO https://github.com/firecracker-microvm/firecracker/releases/download/v1.10.0/firecracker-v1.10.0-x86_64.tgz
# extract firecracker + firecracker-jailer to /usr/local/bin/
```

### CI matrix

```yaml
integration:
  matrix:
    hypervisor: [cloud_hypervisor, firecracker]
  runs-on: [self-hosted, kvm]
```

---

## 11. State Machine (post-merge, normalized)

```
                    Create (StartVM)
                          │
                          ▼
                    ┌──────────┐
         Snapshot   │ running  │◄──── Restore
         ┌──────────┤          │
         │          └──────────┘
         ▼
    ┌────────────┐
    │ snapshotted│
    └─────┬──────┘
          │ Delete
          ▼
      ┌────────┐
      │ deleted│
      └────────┘
```

`VMState` mapping:

| CH state | FC state | Normalized | App DB status |
|----------|----------|------------|---------------|
| Running | Running | `running` | `running` |
| Paused | Paused | `paused` | (unused post-snapshot) |
| Shutdown, Loaded, Created | Not started | `stopped` / `not_started` | `snapshotted` or `stopped` |
| socket gone | socket gone | `dead` | `killed` |

---

## 12. Risk Register

| Risk | Likelihood | Impact | Mitigation |
|------|------------|--------|------------|
| Snapshot branch not merged before plugin work | Medium | High — double refactor | Phase 0 gate; implement on post-merge branch |
| FC snapshot incompatibility with CH snaps | Certain | Medium | Separate snapshot dirs per backend; no cross-migrate |
| Landlock gap on restore (existing bug) | Certain | High | Fix in Phase 0; enforce in `security.go` |
| FC no `vm.counters` | Certain | Low | Agent metrics primary; FC `/metrics` optional |
| FC no event file | Certain | Low | Skip event monitor for FC sandboxes |
| Vsock reset on FC pause/snapshot | Medium | Medium | Re-probe agent after restore; document |
| Concurrent boot socket races | Low | High | Per-VM paths under `{instanceDir}` (already enforced) |
| Jailer chroot prep race | Medium | High | Unique sandbox ID; prepare hardlinks before spawn; fail if chroot exists |
| Jailer/FC version mismatch | Medium | High | Validate versions at daemon startup; document pinned releases |
| Jailer requires root | Certain | Medium | VoidRun daemon already root-capable; document requirement |
| Chroot cleanup incomplete | Medium | Medium | `CleanupJail` on delete + rollback on boot failure |
| Cgroup dirs left after delete | Medium | Low | Best-effort cgroup cleanup in `DeleteVM` |
| Parallel jailer slowdown | Low | Medium | Document; limit concurrent FC boots via semaphore if needed |

---

## 13. File Change Summary

### New files

```
pkg/compute/interface.go
pkg/compute/registry.go
pkg/compute/registry_test.go
pkg/compute/paths.go
plugins/cloudhypervisor/plugin.go
plugins/cloudhypervisor/lifecycle.go
plugins/cloudhypervisor/client.go
plugins/cloudhypervisor/config.go
plugins/cloudhypervisor/types.go
plugins/cloudhypervisor/helpers.go
plugins/cloudhypervisor/events.go
plugins/cloudhypervisor/security.go
plugins/firecracker/plugin.go
plugins/firecracker/lifecycle.go
plugins/firecracker/jailer.go
plugins/firecracker/jailer_test.go
plugins/firecracker/client.go
plugins/firecracker/config.go
plugins/firecracker/types.go
runtime/facade.go
docs/firecracker-setup.md
```

### Modified files

```
cmd/server/main.go
config/config.go
model/sandbox.go
model/request.go
service/sandbox.go
service/lifecycle_manager.go
runtime/event_monitor.go
runtime/disk.go
metrics/manager.go
openapi.yml
README.md
```

### Removed / relocated

```
runtime/clh_client.go    → plugins/cloudhypervisor/client.go
runtime/clh_config.go    → plugins/cloudhypervisor/config.go
runtime/clh_types.go     → plugins/cloudhypervisor/types.go
runtime/clh_helpers.go   → plugins/cloudhypervisor/helpers.go
runtime/lifecycle.go     → split: facade + plugin lifecycle
```

---

## 14. `cmd/server/main.go` (target)

```go
import (
    _ "voidrun/plugins/cloudhypervisor"
    _ "voidrun/plugins/firecracker"
)
```

Both plugins always compiled in; runtime selection via `HYPERVISOR` env and per-sandbox DB field.

---

## 15. Acceptance Criteria (overall)

1. `HYPERVISOR=cloud_hypervisor` (default) — identical behavior to post-merge snapshot branch
2. `HYPERVISOR=firecracker` — create, snapshot, restore, delete work end-to-end **via jailer**
3. No FC process runs outside jailer chroot; FC process UID != 0
4. No CH/FC imports in `service/` or `handler/`
5. All `Hypervisor` methods accept `context.Context`
6. `go test ./...` passes (unit); integration tests pass on KVM runners (FC tests require root)
7. OpenAPI documents optional `hypervisor` field
8. Existing sandboxes without `hypervisor` field default to `cloud_hypervisor`
9. Jailer + Firecracker versions validated at daemon startup

---

## 16. Branch & PR Strategy

| Branch | Purpose | Base | PR target |
|--------|---------|------|-----------|
| `cursor/hypervisor-plugin-plan-4063` | This plan document | `main` | `main` (docs only) |
| (upstream) `feat/ch-snap-restore` | Snapshot lifecycle | `main` | `main` |
| `cursor/hypervisor-plugins-impl-4063` | Phase 1–4 implementation | post-merge `main` | `main` |

**Do not start Phase 2+ until Phase 0 (snapshot merge) is complete.**

---

## 17. Immediate Next Actions

1. Review and merge this plan PR
2. Land `feat/ch-snap-restore` with security fixes (Phase 0)
3. Open `cursor/hypervisor-plugins-impl-4063` from updated `main`
4. Execute Phase 1 (`pkg/compute` skeleton) as first implementation PR
