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

- Firecracker jailer integration (follow-up)
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

Both require KVM (`/dev/kvm`). Firecracker hosts need a PVH-compatible kernel (may share `KERNEL_PATH` or use `FC_KERNEL_PATH`).

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
| `FC_PATH` | `/usr/local/bin/firecracker` | FC binary |
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

### 5.2 Firecracker (`plugins/firecracker`)

**Registration:** `compute.Register("firecracker", ...)`

**Method mapping:**

| Interface method | FC implementation |
|------------------|-------------------|
| `StartVM` | spawn FC → PUT boot-source, machine-config, drives, net, vsock → `InstanceStart` |
| `StopVM` | `SendCtrlAltDel` or kill process |
| `Snapshot` | `Pause` → `CreateSnapshot` → kill VMM |
| `Restore` | spawn FC → `LoadSnapshot` → `Resume` |
| `GetState` | `GET /` → `state` field |
| `Counters` | optional `GET /metrics` if FIFO enabled; else nil |
| `EventSource` | nil (no file stream); health poll only |

**Disk policy:**
- Reject `FormatQcow2` with backing overlay
- Prefer `FormatRaw` or standalone qcow2 copy (`qcow2-flat`)

**Vsock:** same `CONNECT <port>\n` handshake — guest agent unchanged.

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

### Phase 3 — Firecracker plugin

Tasks:
- [ ] Add `plugins/firecracker/client.go` — FC REST client over Unix socket
- [ ] Implement `StartVM`, `StopVM`, `Snapshot`, `Restore`, `GetState`
- [ ] Blank import `plugins/firecracker` in `cmd/server/main.go`
- [ ] Disk validation: reject qcow2 backing overlay for FC
- [ ] Integration test script: `HYPERVISOR=firecracker` create → probe agent → exec → snapshot → restore → delete
- [ ] `docs/firecracker-setup.md`

**Exit criteria:** full sandbox lifecycle works on FC host with `HYPERVISOR=firecracker`.

---

### Phase 4 — API, observability, hardening

Tasks:
- [ ] Expose `hypervisor` in sandbox list/get/create responses
- [ ] OpenAPI: optional `hypervisor` on create
- [ ] Health monitor: normalized `VMState` (no CH-specific strings in service)
- [ ] Lifecycle manager: load hypervisor per sandbox from DB
- [ ] README update: dual hypervisor requirements
- [ ] Optional: FC jailer wrapper (follow-up ticket)

**Exit criteria:** documented dual-backend deployment; OpenAPI updated.

---

## 10. Testing Strategy

### Unit tests

| Package | Coverage |
|---------|----------|
| `pkg/compute` | Register duplicate panic, Get unknown, List |
| `plugins/cloudhypervisor` | CLI arg builder, landlock rules, state normalization |
| `plugins/firecracker` | API payload builder, action types, state parse |
| `runtime/disk.go` | Format rejection per hypervisor |

### Integration tests (KVM host required)

```bash
# CH (default)
go test -tags=integration ./plugins/cloudhypervisor/...

# FC
HYPERVISOR=firecracker go test -tags=integration ./plugins/firecracker/...
```

Scenarios per backend:
1. Create → agent probe → exec command
2. Snapshot → verify snapshotted status → restore → agent probe
3. Delete → files removed
4. Concurrent create (10 sandboxes) — no socket collisions
5. Context cancel during boot — process killed

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
2. `HYPERVISOR=firecracker` — create, snapshot, restore, delete work end-to-end
3. No CH/FC imports in `service/` or `handler/`
4. All `Hypervisor` methods accept `context.Context`
5. `go test ./...` passes (unit); integration tests pass on KVM runners
6. OpenAPI documents optional `hypervisor` field
7. Existing sandboxes without `hypervisor` field default to `cloud_hypervisor`

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
