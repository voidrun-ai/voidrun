# Firecracker VM Support — Implementation Plan

## 1. Overview

This document is the complete engineering plan for adding [Firecracker](https://firecracker-microvm.github.io/) as a second hypervisor backend alongside the existing Cloud Hypervisor (CLH). Both hypervisors will be supported simultaneously; the active one is selected per-deployment via an environment variable.

The guest agent, vsock transport, networking (netns + tap), disk tooling (qemu-img), and all application-layer services (exec, PTY, file-ops, auth, MongoDB persistence, Prometheus metrics) continue to work unchanged. Only the hypervisor-facing layer changes.

---

## 2. Firecracker vs. Cloud Hypervisor — Key Differences

Understanding these differences drives every design decision below.

| Concern | Cloud Hypervisor (CLH) | Firecracker (FC) |
|---|---|---|
| **Process model** | Long-lived — process persists after `vm.shutdown`, enabling warm `vm.boot` | Exits on VM stop; every start is a cold start |
| **API style** | REST over Unix socket, `PUT /api/v1/vm.*` | REST over Unix socket, resource-oriented (`PUT /boot-source`, `PUT /machine-config`, etc.) |
| **Boot sequence** | CLI mode (all flags at launch) OR API injection (spawn empty, configure via REST) | Always REST-only: spawn empty process, configure each resource, then `PUT /actions {InstanceStart}` |
| **Pause / Resume** | Native `vm.pause` / `vm.resume` | `PATCH /vm {"state":"Paused"}` / `{"state":"Resumed"}` (FC ≥ v0.25) |
| **Stop** | `vm.shutdown` (graceful guest shutdown, CLH stays alive) | `PUT /actions {"action_type":"SendCtrlAltDel"}` (guest-initiated) or `SIGTERM` to process |
| **Delete** | `vm.delete` + SIGTERM to CLH PID | No API call; SIGTERM to FC PID suffices |
| **Disk format** | qcow2, qcow2-flat, raw | **raw only** (no qcow2 driver in FC's virtio-block) |
| **Kernel cmdline** | Passed as `--cmdline` / `PayloadConfig.Cmdline` | `PUT /boot-source {"boot_args": "..."}` |
| **Vsock** | `VsockConfig{Cid, Socket}` | `PUT /vsock {"vsock_id","guest_cid","uds_path"}` — same CID + Unix socket semantics |
| **Seccomp / Landlock** | Native `--seccomp` / `--landlock-rules` flags | Isolation via **jailer** binary (chroot + cgroup + seccomp built-in) |
| **Event monitoring** | `--event-monitor path=<file>` → structured JSON per event | Logger FIFO (`PUT /logger {"log_fifo":"..."}`) → line-oriented log text |
| **Metrics** | `vm.counters` REST endpoint on socket | Metrics FIFO (`PUT /metrics {"metrics_fifo":"..."}`) → periodic JSON snapshots |
| **State values** | `Running`, `RunningVirtualized`, `Paused`, `Loaded`, `Shutdown`, `Created` | `Not started`, `Running`, `Paused` |
| **Host isolation** | Landlock + seccomp (CLH-native) | `jailer` binary (optional but recommended for production) |

---

## 3. Architecture: Hypervisor Interface

### 3.1 Current state

`runtime/lifecycle.go` contains free functions (`Create`, `CreateCLI`, `Start`, `Stop`, `Pause`, `Resume`, `Delete`) that directly call CLH-specific APIs. `service/sandbox.go` calls these functions directly. There is no abstraction.

### 3.2 Target state

Introduce a `Hypervisor` interface in `runtime/hypervisor.go`. Both CLH and FC implement it. `service/sandbox.go` receives the active `Hypervisor` via constructor injection. A factory function selects the implementation at startup based on the `HYPERVISOR` env var.

```
┌────────────────────────────────────────────────────┐
│               service/sandbox.go                    │
│  (uses Hypervisor interface, no CLH/FC imports)    │
└──────────────┬──────────────────────────────────────┘
               │  runtime.Hypervisor interface
       ┌───────┴──────────┐
       ▼                  ▼
  CLHHypervisor      FCHypervisor
  (runtime/clh_*.go) (runtime/fc_*.go)
       │                  │
       └─────────┬────────┘
                 │  shared packages (unchanged)
       ┌─────────┴────────────────────────────────┐
       │  runtime/network.go  runtime/disk.go      │
       │  runtime/agent_client.go  runtime/client.go│
       └──────────────────────────────────────────┘
```

### 3.3 Interface definition

```go
// runtime/hypervisor.go

package runtime

import (
    "context"
    "voidrun/config"
    "voidrun/model"
)

// HypervisorType identifies which backend is in use.
type HypervisorType string

const (
    HypervisorCLH         HypervisorType = "clh"
    HypervisorFirecracker HypervisorType = "firecracker"
)

// Hypervisor is the single abstraction over CLH and Firecracker.
// Every method that touches a running VM goes through this interface.
type Hypervisor interface {
    // Type returns the HypervisorType ("clh" or "firecracker").
    Type() HypervisorType

    // Create spawns the hypervisor process, configures the VM,
    // and boots it. On return the VM is running.
    // This is used both for initial creation and for cold restarts.
    Create(ctx context.Context, cfg config.Config, spec model.SandboxSpec, overlayPath string) error

    // Start boots a VM that is already configured (warm restart — CLH only).
    // For Firecracker this is identical to Create.
    Start(ctx context.Context, id string) error

    // Stop gracefully shuts down the guest OS.
    // For CLH the hypervisor process remains alive (enabling warm Start).
    // For Firecracker the process exits; a subsequent Start is a cold Create.
    Stop(ctx context.Context, id string) error

    // Pause suspends VM execution without shutting down.
    Pause(ctx context.Context, id string) error

    // Resume resumes a paused VM.
    Resume(ctx context.Context, id string) error

    // Delete terminates the hypervisor process, destroys the network
    // namespace, and removes the PID file.
    // Disk cleanup is handled separately by runtime.Cleanup.
    Delete(ctx context.Context, id, tapName, nsName string) error

    // GetState returns a normalized state string:
    // "running", "paused", "stopped".
    // Returns an error if the hypervisor socket is unreachable.
    GetState(ctx context.Context, id string) (string, error)

    // IsRunning reports whether the hypervisor API socket is accessible.
    IsRunning(id string) bool

    // SupportsWarmRestart reports whether the hypervisor process persists
    // after Stop, making a socket-based Start possible.
    // CLH → true. Firecracker → false.
    SupportsWarmRestart() bool
}

// NewHypervisor returns the Hypervisor implementation selected by cfg.
func NewHypervisor(cfg *config.Config) Hypervisor {
    switch cfg.HypervisorType {
    case HypervisorFirecracker:
        return NewFCHypervisor(cfg)
    default:
        return NewCLHHypervisor(cfg)
    }
}
```

---

## 4. Implementation Phases

### Phase 1 — Hypervisor Interface + CLH Wrapper (no behavior change)

**Goal:** extract the existing CLH logic into a struct that implements `Hypervisor`, wire it through the service layer. Nothing changes for users.

**Files to create:**

- `runtime/hypervisor.go` — the interface and `NewHypervisor` factory (shown above)
- `runtime/clh_hypervisor.go` — `CLHHypervisor` struct implementing `Hypervisor`

**Files to modify:**

- `runtime/lifecycle.go` → keep all existing free functions **but** delegate from `CLHHypervisor` methods to them; this preserves backward compatibility while the interface is wired through.
- `config/config.go` → add `HypervisorType HypervisorType` field and `FC` config struct (see §6); also relax `resolveCHBinaryPath` so it does not `log.Fatalln` when `HYPERVISOR=firecracker` and `CH_PATH` is unset.
- `service/sandbox.go` → change constructor to accept `runtime.Hypervisor`; replace all direct `runtime.*` lifecycle calls with `s.hypervisor.*`.
- `service/lifecycle_manager.go` → same injection pattern.
- `server/server.go` (or wherever the service is wired) → call `runtime.NewHypervisor(cfg)` and inject.

**`CLHHypervisor` shape:**

```go
// runtime/clh_hypervisor.go

type CLHHypervisor struct {
    cfg *config.Config
}

func NewCLHHypervisor(cfg *config.Config) *CLHHypervisor {
    return &CLHHypervisor{cfg: cfg}
}

func (h *CLHHypervisor) Type() HypervisorType { return HypervisorCLH }
func (h *CLHHypervisor) SupportsWarmRestart() bool { return true }

func (h *CLHHypervisor) Create(ctx context.Context, cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
    return CreateCLI(cfg, spec, overlayPath)   // existing free function
}

func (h *CLHHypervisor) Start(ctx context.Context, id string) error {
    return Start(id)    // existing free function (warm boot via vm.boot)
}

func (h *CLHHypervisor) Stop(ctx context.Context, id string) error {
    return Stop(id)
}

func (h *CLHHypervisor) Pause(ctx context.Context, id string) error {
    return Pause(id)
}

func (h *CLHHypervisor) Resume(ctx context.Context, id string) error {
    return Resume(id)
}

func (h *CLHHypervisor) Delete(ctx context.Context, id, tapName, nsName string) error {
    return Delete(id, tapName, nsName)
}

func (h *CLHHypervisor) GetState(ctx context.Context, id string) (string, error) {
    client := NewAPIClientForSandbox(id)
    if !client.IsSocketAvailable() {
        return "stopped", nil
    }
    return client.GetStateWithContext(ctx)
}

func (h *CLHHypervisor) IsRunning(id string) bool {
    return NewAPIClientForSandbox(id).IsSocketAvailable()
}
```

**Service wiring change (`service/sandbox.go`):**

```go
// Before
type SandboxService struct {
    cfg     *config.Config
    // ...
}

func (s *SandboxService) stopSandbox(ctx context.Context, id string) error {
    return runtime.Stop(id)
}

// After
type SandboxService struct {
    cfg        *config.Config
    hypervisor runtime.Hypervisor
    // ...
}

func (s *SandboxService) stopSandbox(ctx context.Context, id string) error {
    return s.hypervisor.Stop(ctx, id)
}
```

**State normalization** moves from `service/sandbox.go` into `CLHHypervisor.GetState` and `FCHypervisor.GetState`, each mapping their own state strings to `"running"`, `"paused"`, `"stopped"`.

---

### Phase 2 — Firecracker API Client

**Files to create:**

- `runtime/fc_types.go` — FC REST API request/response structs
- `runtime/fc_client.go` — `FCClient` with typed methods for every used endpoint
- `runtime/fc_helpers.go` — higher-level helpers (wait for socket, configure and start)

**`fc_types.go` — key structs:**

```go
// Boot source
type FCBootSource struct {
    KernelImagePath string `json:"kernel_image_path"`
    BootArgs        string `json:"boot_args,omitempty"`
    InitrdPath      string `json:"initrd_path,omitempty"`
}

// Machine configuration
type FCMachineConfig struct {
    VcpuCount  int  `json:"vcpu_count"`
    MemSizeMib int  `json:"mem_size_mib"`
    SMT        bool `json:"smt,omitempty"`
}

// Drive (block device)
type FCDrive struct {
    DriveID      string `json:"drive_id"`
    PathOnHost   string `json:"path_on_host"`
    IsRootDevice bool   `json:"is_root_device"`
    IsReadOnly   bool   `json:"is_read_only"`
}

// Network interface
type FCNetworkInterface struct {
    IfaceID     string `json:"iface_id"`
    GuestMac    string `json:"guest_mac,omitempty"`
    HostDevName string `json:"host_dev_name"`
}

// Vsock device
type FCVsock struct {
    VsockID  string `json:"vsock_id"`
    GuestCID uint32 `json:"guest_cid"`
    UDSPath  string `json:"uds_path"`
}

// Instance action
type FCAction struct {
    ActionType string `json:"action_type"` // "InstanceStart" | "SendCtrlAltDel" | "FlushMetrics"
}

// VM state patch
type FCVMStatePatch struct {
    State string `json:"state"` // "Paused" | "Resumed"
}

// Logger configuration
type FCLogger struct {
    LogPath   string `json:"log_path"`
    Level     string `json:"level,omitempty"`    // "Error" | "Warning" | "Info" | "Debug" | "Trace"
    ShowLevel bool   `json:"show_level,omitempty"`
}

// Instance info (GET /)
type FCInstanceInfo struct {
    AppName    string `json:"app_name"`
    ID         string `json:"id"`
    State      string `json:"state"` // "Not started" | "Running" | "Paused"
    VMMVersion string `json:"vmm_version"`
}
```

**`fc_client.go` — `FCClient`:**

```go
type FCClient struct {
    socketPath string
    httpClient *http.Client
}

func NewFCClient(socketPath string) *FCClient { ... }

func (c *FCClient) IsSocketAvailable() bool { ... }
func (c *FCClient) WaitForSocket(timeout time.Duration) error { ... }

func (c *FCClient) PutBootSource(ctx context.Context, bs FCBootSource) error   { ... }
func (c *FCClient) PutMachineConfig(ctx context.Context, mc FCMachineConfig) error { ... }
func (c *FCClient) PutDrive(ctx context.Context, d FCDrive) error              { ... }
func (c *FCClient) PutNetworkInterface(ctx context.Context, ni FCNetworkInterface) error { ... }
func (c *FCClient) PutVsock(ctx context.Context, v FCVsock) error              { ... }
func (c *FCClient) PutLogger(ctx context.Context, l FCLogger) error            { ... }
func (c *FCClient) DoAction(ctx context.Context, a FCAction) error             { ... }
func (c *FCClient) PatchVMState(ctx context.Context, s FCVMStatePatch) error   { ... }
func (c *FCClient) GetInstanceInfo(ctx context.Context) (*FCInstanceInfo, error) { ... }
```

All methods use the same `http.Client` + Unix socket transport pattern already used by `CLHClient`, just with different URL paths (e.g. `http://localhost/boot-source`).

---

### Phase 3 — Firecracker Lifecycle Implementation

**File to create:** `runtime/fc_lifecycle.go` / `runtime/fc_hypervisor.go`

**`FCHypervisor` struct:**

```go
type FCHypervisor struct {
    cfg *config.Config
}

func NewFCHypervisor(cfg *config.Config) *FCHypervisor {
    return &FCHypervisor{cfg: cfg}
}

func (h *FCHypervisor) Type() HypervisorType        { return HypervisorFirecracker }
func (h *FCHypervisor) SupportsWarmRestart() bool    { return false }
```

#### 3a. `FCHypervisor.Create` — spawn + configure + boot

```
1. Resolve paths (socket, log, pid, vsock, overlay)
2. Spawn FC process:
   if cfg.FC.UseJailer:
       ip netns exec <nsName> jailer --id <sandboxID> --exec-file <fc_binary>
           --uid <uid> --gid <gid> -- --api-sock <socketPath>
   else:
       ip netns exec <nsName> <fc_binary> --api-sock <socketPath>
3. Wait for API socket to appear
4. Configure via REST (in order):
   a. PUT /logger          (log file path)
   b. PUT /machine-config  (vCPUs, memory)
   c. PUT /boot-source     (kernel path, cmdline)
   d. PUT /drives/rootfs   (overlay.raw, is_root=true)
   e. PUT /network-interfaces/eth0  (tap name, MAC)
   f. PUT /vsock           (CID derived from IP, vsock.sock path)
5. PUT /actions {"action_type": "InstanceStart"}
6. Write PID file
```

**Important:** Firecracker requires a **raw** disk image. The disk preparation path must produce `overlay.raw` when FC is active (see §4 Disk Handling).

#### 3b. `FCHypervisor.Start` — cold restart only

Because FC's process exits on stop, `Start` re-runs the same `Create` sequence using the existing overlay disk and netns (which are preserved just as they are for CLH cold restarts today).

```go
func (h *FCHypervisor) Start(ctx context.Context, id string) error {
    // Reconstruct spec from persisted sandbox metadata
    // (passed in via service layer; the interface may need ctx + sandbox record)
    // Then call h.Create(ctx, cfg, spec, overlayPath)
}
```

**Note:** The `Start` signature in the interface currently only takes `id`. Because FC needs the full spec for a cold restart, the interface will need either `spec model.SandboxSpec` added or the service layer will look up the sandbox and call `Create` directly for FC. The cleanest design is to add the spec to `Start`:

```go
// Revised Start signature in the interface
Start(ctx context.Context, id string, spec *model.SandboxSpec, overlayPath string) error
```

The `CLHHypervisor.Start` ignores `spec` and `overlayPath` (it uses the existing socket for warm boot). `FCHypervisor.Start` uses them to run a full cold boot.

#### 3c. `FCHypervisor.Stop`

Firecracker supports `SendCtrlAltDel` (soft reboot/shutdown signal to guest) via `PUT /actions`. For a clean shutdown the guest must handle it. The host-side fallback is `SIGTERM` to the FC PID.

```go
func (h *FCHypervisor) Stop(ctx context.Context, id string) error {
    client := NewFCClient(GetSocketPath(id))
    if client.IsSocketAvailable() {
        _ = client.DoAction(ctx, FCAction{ActionType: "SendCtrlAltDel"})
        // Wait briefly for process to exit, then SIGTERM
        waitForProcessExit(id, 3*time.Second)
    }
    killByPIDFile(id) // SIGTERM fallback
    return nil
}
```

#### 3d. `FCHypervisor.Pause` / `Resume`

```go
func (h *FCHypervisor) Pause(ctx context.Context, id string) error {
    return NewFCClient(GetSocketPath(id)).PatchVMState(ctx, FCVMStatePatch{State: "Paused"})
}

func (h *FCHypervisor) Resume(ctx context.Context, id string) error {
    return NewFCClient(GetSocketPath(id)).PatchVMState(ctx, FCVMStatePatch{State: "Resumed"})
}
```

#### 3e. `FCHypervisor.Delete`

```go
func (h *FCHypervisor) Delete(ctx context.Context, id, tapName, nsName string) error {
    // FC process should already be dead after Stop, but ensure
    killByPIDFile(id)
    os.Remove(GetPIDPath(id))
    if nsName != "" {
        DeleteSandboxNetNS(nsName)
    } else if tapName != "" {
        DeleteTap(tapName)
    }
    return nil
}
```

#### 3f. `FCHypervisor.GetState`

```go
func (h *FCHypervisor) GetState(ctx context.Context, id string) (string, error) {
    client := NewFCClient(GetSocketPath(id))
    if !client.IsSocketAvailable() {
        return "stopped", nil
    }
    info, err := client.GetInstanceInfo(ctx)
    if err != nil {
        return "stopped", err
    }
    switch info.State {
    case "Running":
        return "running", nil
    case "Paused":
        return "paused", nil
    default: // "Not started"
        return "stopped", nil
    }
}
```

---

### Phase 4 — Disk Handling for Firecracker

Firecracker's virtio-block driver only reads **raw** disk images. The current disk pipeline (`runtime/disk.go`) already supports raw format (used for WSL2). The following changes are needed:

1. **Force raw overlay when FC is active.** In `PrepareStorage`, if `hypervisorType == HypervisorFirecracker`, always create a raw overlay regardless of `SANDBOX_DISK_FORMAT`.

2. **Base image conversion.** If the base image on disk is qcow2 (the common case), the overlay preparation must flatten it:
   ```
   qemu-img convert -f qcow2 -O raw <base>.qcow2 <base>.raw
   qemu-img create -f raw -b <base>.raw -F raw overlay.raw <diskMB>M
   ```
   This can be done lazily (once per base image, cache the `.raw` alongside the `.qcow2`).

3. **Kernel cmdline difference.** CLH's default cmdline (`root=/dev/vda rw init=/sbin/init net.ifnames=0 biosdevname=0`) works unchanged with FC since FC also exposes the root disk as `/dev/vda`. No change needed here but the env var `SANDBOX_KERNEL_CMDLINE` remains the operator's knob.

4. **Path helper addition.** Add `GetFCSocketPath(id)` if FC uses a different socket file name (e.g. `fc.sock`) to avoid collision. Or reuse `GetSocketPath` — it returns `vm.sock` which is fine for both.

---

### Phase 5 — Jailer Support (optional, for production FC deployments)

The `jailer` binary wraps Firecracker with:
- A chroot at `/srv/jailer/firecracker/<id>/root/`
- A dedicated UID/GID
- cgroup isolation
- Built-in seccomp filter

When `FC_USE_JAILER=true`:
1. All referenced files (kernel, overlay, socket) must be **inside the jailer chroot** or bind-mounted in.
2. `GetSocketPath(id)` returns the path *outside* the jailer — the actual socket appears at a jailer-managed path.
3. The process spawn changes to:
   ```
   ip netns exec <nsName> jailer \
     --id <sandboxID> \
     --exec-file <fc_binary> \
     --uid <jailerUID> \
     --gid <jailerGID> \
     --chroot-base-dir /srv/jailer \
     -- \
     --api-sock /run/firecracker.socket
   ```
4. A helper function `GetJailerRootDir(id)` returns `/srv/jailer/firecracker/<id>/root`.

For the initial implementation, jailer support is built in but gated behind `FC_USE_JAILER=false` (default). The socket path and file paths adapt automatically when enabled.

---

### Phase 6 — Configuration Changes

**New env variables (added to `config/config.go`):**

```
HYPERVISOR              = clh        # or "firecracker"
FC_PATH                 = /usr/local/bin/firecracker
FC_JAILER_PATH          = /usr/local/bin/jailer
FC_USE_JAILER           = false
FC_JAILER_UID           = 1000
FC_JAILER_GID           = 1000
FC_JAILER_CHROOT_BASE   = /srv/jailer
```

**New config structs:**

```go
type FirecrackerConfig struct {
    BinaryPath     string
    JailerPath     string
    UseJailer      bool
    JailerUID      int
    JailerGID      int
    JailerChroot   string
}

// Added to Config:
type Config struct {
    // ... existing fields ...
    HypervisorType HypervisorType    // "clh" or "firecracker"
    FC             FirecrackerConfig
}
```

**CLH binary resolution change:** `resolveCHBinaryPath` currently `log.Fatalln` if `CH_PATH` is empty. It must be relaxed to a warning when `HYPERVISOR=firecracker`, since CLH is not required in that case.

**Model change:** Add `Hypervisor string` field to `model.Sandbox` (stored in MongoDB). This records which hypervisor created the sandbox, so that a server restart can use the correct backend for each existing sandbox even if `HYPERVISOR` is later changed. Default value: `"clh"` (backward-compatible).

---

### Phase 7 — Service Layer Adaptation

**`service/sandbox.go` changes:**

1. Constructor accepts `runtime.Hypervisor` instead of the config alone.
2. All direct `runtime.*` lifecycle calls become `s.hypervisor.*`.
3. The warm-restart vs cold-restart decision currently in `startSandbox` becomes:
   ```go
   func (s *SandboxService) startSandbox(ctx context.Context, sbx *model.Sandbox) error {
       if s.hypervisor.SupportsWarmRestart() && s.hypervisor.IsRunning(sbx.ID.Hex()) {
           return s.hypervisor.Start(ctx, sbx.ID.Hex(), nil, "")
       }
       // Cold restart (FC always, CLH when process is gone)
       overlayPath := runtime.ResolveOverlayPath(sbx.ID.Hex(), s.cfg)
       return s.hypervisor.Start(ctx, sbx.ID.Hex(), sbxToSpec(sbx), overlayPath)
   }
   ```
4. Status normalization already moves into each `Hypervisor.GetState`, so the `switch strings.ToLower(sbxState)` block in `RefreshStatuses` is removed and replaced with `s.hypervisor.GetState(ctx, id)`.

**`service/lifecycle_manager.go` changes:**

Same interface injection. No logic changes — `Pause`, `Resume`, and the delete path go through the interface.

---

### Phase 8 — Event Monitoring for Firecracker

The current `EventMonitor` tails a CLH-specific JSON event file. For FC, this file does not exist.

**Short-term (Phase 8a):** Skip event monitoring for FC sandboxes. The `EventMonitor.Start` call in `service/sandbox.go` is guarded:
```go
if s.hypervisor.Type() == runtime.HypervisorCLH && s.cfg.Monitor.Enabled {
    s.monitor.Start(ctx, sbx.ID, sbx.OrgID, sbx.CreatedBy)
}
```

**Medium-term (Phase 8b):** Add an `FCLogMonitor` that reads the FC logger FIFO and maps log lines to normalized `model.SandboxEvent` records. FC's log format is:
```
{"level":"Info","timestamp":"...","message":"Starting microVM","..."}
```
Key events to capture: `InstanceStart`, pause, resume, VM exit (process termination).

---

### Phase 9 — Metrics for Firecracker

The current metrics collector (`metrics/manager.go`) calls CLH's `vm.counters` endpoint. This endpoint does not exist in FC.

**Short-term:** The metrics collector skips FC sandboxes (checks `sbx.Hypervisor` field). Guest-agent-sourced metrics (CPU usage, memory, disk from the guest) continue to work because they use the vsock+HTTP path which is hypervisor-agnostic.

**Medium-term:** Add an `FCMetricsCollector` that reads from the FC metrics FIFO. FC emits a periodic JSON snapshot with per-device I/O counters and vCPU stats.

---

### Phase 10 — Testing

#### Unit tests

| Test file | What to test |
|---|---|
| `runtime/fc_client_test.go` | FCClient methods against a mock Unix socket HTTP server |
| `runtime/fc_hypervisor_test.go` | FCHypervisor state machine, error paths, disk path selection |
| `runtime/clh_hypervisor_test.go` | CLHHypervisor wraps existing functions correctly |
| `config/config_test.go` | FC config defaults, hypervisor type parsing |

#### Integration tests (require KVM host)

| Scenario | Steps |
|---|---|
| FC sandbox full lifecycle | create → exec cmd → pause → resume → stop → start → delete |
| FC cold restart | create → stop → start (verifying no warm socket) |
| FC disk format | create with qcow2 base → verify raw overlay is used |
| FC vsock | create → verify agent reachable on vsock port 1024 |
| CLH unchanged | run full existing lifecycle suite |

---

## 5. Complete File Change Map

### New files

| File | Purpose |
|---|---|
| `runtime/hypervisor.go` | `Hypervisor` interface, `HypervisorType`, `NewHypervisor` factory |
| `runtime/clh_hypervisor.go` | `CLHHypervisor` struct implementing the interface |
| `runtime/fc_types.go` | Firecracker API request/response structs |
| `runtime/fc_client.go` | `FCClient` — REST client over Unix socket |
| `runtime/fc_helpers.go` | Higher-level FC helpers (wait-for-socket, process kill) |
| `runtime/fc_hypervisor.go` | `FCHypervisor` struct implementing the interface |
| `docs/firecracker-setup.md` | Operator guide: installing FC binary, jailer, raw base images |

### Modified files

| File | Change |
|---|---|
| `config/config.go` | Add `HypervisorType`, `FirecrackerConfig`; relax CLH binary requirement when FC is selected |
| `runtime/lifecycle.go` | No structural change; free functions remain; `CLHHypervisor` delegates to them |
| `runtime/disk.go` | Add `PrepareRawOverlay` (FC-specific); add lazy qcow2→raw base image conversion |
| `runtime/client.go` | Minor: add `GetFCLogPath(id)` helper; no functional changes |
| `model/sandbox.go` | Add `Hypervisor string` field (bson:`hypervisor`), default `"clh"` |
| `service/sandbox.go` | Accept `runtime.Hypervisor`; replace direct `runtime.*` calls; guard event monitor |
| `service/lifecycle_manager.go` | Accept `runtime.Hypervisor`; replace direct calls |
| `server/server.go` | Call `runtime.NewHypervisor(cfg)` and inject into services |
| `metrics/manager.go` | Skip CLH counter scrape for FC sandboxes; check `sbx.Hypervisor` |
| `repository/sandbox.go` | No interface change; MongoDB handles new field transparently |
| `docker-compose.yml` | Add `FC_PATH`, `HYPERVISOR` vars with defaults |

---

## 6. Firecracker-Specific Operator Notes (for `docs/firecracker-setup.md`)

1. **Binary installation:** `firecracker` (and optionally `jailer`) must be on the host, not in the Docker image. Recommended path: `/usr/local/bin/firecracker`.
2. **KVM access:** Same as CLH — `/dev/kvm` must be accessible.
3. **Disk images:** Base images must be raw (`.raw`) or have a raw counterpart. The system auto-converts qcow2 bases lazily on first use; pre-converting with `qemu-img convert` is faster.
4. **Kernel:** Same `vmlinux` image works for both CLH and FC, as both support Linux ELF kernel format.
5. **vsock:** FC's vsock guest CID must be unique per VM. The existing `getCidFromIP` function covers this.
6. **Jailer chroot paths:** When `FC_USE_JAILER=true`, disk images and the kernel must be bind-mounted (or copied) into the jailer's chroot. The `FCHypervisor.Create` helper handles this.
7. **No qcow2 snapshot overlay:** The CLH qcow2-overlay approach (overlay backed by base) is not available for FC. Each sandbox gets a standalone raw copy. This uses more disk space. Alternatives (device-mapper thin provisioning, virtiofs) are out of scope for this plan.

---

## 7. Risk Register

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| FC vsock guest CID collision | Low | High | `getCidFromIP` already produces unique CIDs; unchanged |
| Raw disk space usage (no qcow2 backing) | Medium | Medium | Document; add `SANDBOX_DEFAULT_DISK_MB` guidance; consider dm-thin later |
| FC process exits unexpectedly (crashes) | Medium | Medium | Health monitor detects missing socket → marks `stopped`; same path as CLH |
| Jailer chroot path conflicts | Low | High | Unique per-sandbox ID; `GetJailerRootDir` enforces this |
| FC API version incompatibility | Low | Medium | Pin tested FC version in docs; client returns clear errors on unknown endpoints |
| CLH regression from interface refactor | Low | High | Phase 1 adds zero logic — existing free functions remain; CLH tests must pass before proceeding |
| Operator deploys mixed sandboxes (some CLH, some FC) | Medium | Medium | `model.Sandbox.Hypervisor` field routes each sandbox to the right backend at service restart |

---

## 8. Sequencing Summary

```
Phase 1  →  Interface abstraction + CLH wrapper (no behavior change, full test coverage)
Phase 2  →  FC API client and types
Phase 3  →  FC lifecycle implementation
Phase 4  →  Disk handling (raw overlay, qcow2→raw conversion)
Phase 5  →  Jailer support (gated by FC_USE_JAILER flag)
Phase 6  →  Configuration additions
Phase 7  →  Service layer wiring
Phase 8  →  Event monitoring (skip for FC in Phase 8a; FC log monitor in Phase 8b)
Phase 9  →  Metrics (skip CLH counters for FC in Phase 9a; FC metrics FIFO in Phase 9b)
Phase 10 →  Tests (unit after each phase; integration at end)
```

Phases 1–7 are the **minimum viable implementation** that ships a working Firecracker backend. Phases 8b and 9b are enhancements that can follow in a subsequent iteration.
