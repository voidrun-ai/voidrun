# Snapshot/Restore Scale and Security Review

Date: 2026-06-19
Branch reviewed: `feat/ch-snap-restore`
Scope: local working-tree changes in `voidrun`

## Executive Summary

The snapshot/restore redesign is moving in a useful direction for startup latency and fleet efficiency, but it is not yet ready to be called optimized for scale and security.

The strongest positives are:

- `singleflight` deduplication for concurrent auto-restore calls
- persisted network metadata (`macAddress`, `netnsName`, `tapName`) to make restore deterministic
- bounded lifecycle concurrency for snapshot/delete sweeps

The main blockers are:

1. Restored VMs lose part of the host-side confinement that fresh boots still have.
2. The new DNS firewall rules weaken network isolation because they are inserted before the private-range drops.
3. Auto-restore work is tied to the first caller's request context, which can cause shared restores to fail under load.
4. The public API contract was not updated to match the lifecycle rewrite.
5. The new memory settings may reduce VM density, and there is no evidence in this branch that the trade-off was measured.
6. The repo is not currently green under `go test ./...`.

Verdict: good prototype progress, but not yet production-ready from a scale/security standpoint.

## What Changed

This branch replaces the old `start/stop/pause/resume` flow with a `snapshot/restore` model and updates the service layer to auto-restore snapshotted sandboxes on demand.

Major themes in the diff:

- lifecycle state model changes from `running/paused/stopped` to `running/snapshotted/killed/deleted`
- runtime snapshot creation and restore support added in `runtime/lifecycle.go`
- sandbox service updated to auto-restore via `singleflight`
- lifecycle manager updated to auto-snapshot idle sandboxes and auto-delete old snapshotted sandboxes
- network namespace setup updated to allow DNS only to configured nameservers
- router changed from `/start`, `/stop`, `/pause`, `/resume` to `/snapshot`, `/restore`

## Findings

### 1. High: restore path drops Landlock confinement

Fresh boots still enable both seccomp and Landlock, but the production restore path only re-enables seccomp. That means a restored Cloud Hypervisor process can end up with broader filesystem access than a newly created VM.

Why it matters:

- security posture becomes inconsistent by lifecycle state
- a sandbox that was safe at create-time becomes less isolated after restore
- this is the kind of regression that can be missed in functional testing but matters in a multi-tenant environment

Evidence:

- `runtime/lifecycle.go` fresh create path appends `--seccomp` and `--landlock`
- `runtime/lifecycle.go` restore path appends only `--seccomp`

Recommended fix:

- make restore use the same Landlock policy builder as create
- avoid maintaining two separate security configurations for the same VMM role
- add an automated test that asserts restore and create both include the same confinement flags

### 2. High: DNS allow rules are ordered before the private-range drops

The new rules allow DNS to configured nameservers before the branch drops traffic to metadata and RFC1918 ranges. If a configured nameserver lives in link-local or private space, that allow rule wins.

Why it matters:

- it weakens the current "deny internal networks from the guest" model
- metadata or internal resolver access could be reintroduced through configuration
- the new test already shows the rule order is opposite of the intended policy

Evidence:

- `runtime/network.go` inserts DNS `ACCEPT` rules before the `169.254.169.254`, `10/8`, `172.16/12`, and `192.168/16` drops
- `runtime/network_test.go` fails with `DNS rules should be AFTER the drops`

Recommended fix:

- move DNS allow rules after the metadata/private-network drops, or
- explicitly reject private/link-local nameserver addresses at config validation time
- keep the regression test and require it to pass before merge

### 3. Medium: shared auto-restore is coupled to a caller request context

The `singleflight` dedupe is a good idea, but the shared restore still runs inside the first caller's request context. If that caller disconnects or times out, the restore can be canceled and rolled back for every concurrent waiter.

Why it matters:

- burst traffic to the same sandbox can fail together
- tail latency becomes sensitive to client disconnects and gateway timeouts
- this turns a scale optimization into a reliability hazard under load

Evidence:

- `service/sandbox.go` calls `s.restoreGroup.Do(id, func() { return s.Restore(ctx, orgID, id) })`
- `service/sandbox.go` then uses that same `ctx` in `waitForAgent()`

Recommended fix:

- decouple the restore worker from the first request by using a fresh bounded internal context
- let callers wait on the shared work result, but do not let one caller cancel the whole restore
- consider a per-sandbox in-flight state machine if restore behavior keeps growing

### 4. Medium: API docs and route contract drifted apart

The router now exposes `/snapshot` and `/restore`, but the OpenAPI spec still documents `/start`, `/stop`, `/pause`, and `/resume`. The schema enum also still advertises old states.

Why it matters:

- generated SDKs and external clients will be wrong
- support and product teams can share outdated lifecycle behavior
- integration breakage is likely even if the server code works

Evidence:

- `server/server.go` registers `/snapshot` and `/restore`
- `openapi.yml` still documents `/sandboxes/{id}/start`, `/stop`, `/pause`, `/resume`
- `openapi.yml` still lists lifecycle states including `stopped` and `paused`, not `snapshotted`

Recommended fix:

- update `openapi.yml` in the same change set as route changes
- regenerate any downstream clients after the spec is corrected
- add a lightweight check that route names and OpenAPI paths stay in sync

### 5. Medium: memory settings may reduce density, with no proof of the trade-off

The branch changes memory configuration from shared memory mode to private memory mode on both the API and CLI paths.

Why it matters:

- memory sharing is often important for VM density when many guests share the same base image
- disabling it may be the right compatibility decision for snapshots, but it can reduce host efficiency
- the branch does not include benchmark evidence showing the fleet-level impact is acceptable

Evidence:

- `runtime/lifecycle.go` changes `Shared: true` to `Shared: false`
- `runtime/lifecycle.go` changes CLI memory flags from `size=%dM,shared=on,mergeable=off` to `size=%dM`

Recommended fix:

- document why shared memory had to be disabled
- run before/after density and memory-pressure measurements
- if the change is required for restore correctness, call that out explicitly in docs and rollout notes

### 6. Medium: current branch is not test-clean

The branch currently fails `go test ./...`.

Why it matters:

- merge confidence is lower when a lifecycle rewrite is not validated end to end
- one failure is directly tied to the new network policy behavior
- another failure comes from a helper program that no longer matches current interfaces

Observed failures:

- `runtime/network_test.go` fails because DNS rules are ordered before the deny rules
- `cmd/test-sandbox/main.go` does not compile against the current repository APIs

Recommended fix:

- make the full Go test suite green before merge
- either update `cmd/test-sandbox/main.go` to current interfaces or exclude it from normal package builds if it is only a local experiment

## Scale Assessment

### Improvements

- `singleflight` is the right direction for preventing restore stampedes
- lifecycle manager concurrency caps are a good guardrail for bulk snapshot/delete work
- storing MAC and NetNS metadata should reduce restore-time recomputation and edge cases

### Remaining scale concerns

- restore cancellation is still fragile because it depends on request-scoped context
- restore readiness still relies on tight polling loops and serial post-restore steps
- memory density impact is unknown after disabling shared guest memory
- API contract drift increases rollout cost across SDKs and automation

Overall scale verdict: improved architecture, but not yet proven or hardened for high-concurrency production use.

## Security Assessment

### Improvements

- DNS is now restricted to configured nameservers instead of broad outbound UDP/TCP allowances
- sandbox network metadata is persisted, reducing restore-time guessing
- Cloud Hypervisor lifecycle handling appears more explicit than the earlier warm-start model

### Remaining security concerns

- restore path loses Landlock parity with fresh create
- DNS rule order weakens isolation if nameservers are internal or link-local
- configuration should validate nameservers against forbidden ranges instead of relying only on iptables ordering
- route/spec drift makes it easier for external callers to rely on outdated lifecycle assumptions

Overall security verdict: not ready to claim secure-by-default until restore confinement and firewall ordering are fixed.

## Recommended Next Steps

1. Fix restore-path security parity by reusing the same Landlock policy generation as create.
2. Reorder DNS firewall rules or reject unsafe nameserver addresses during config validation.
3. Decouple `singleflight` restore execution from request-scoped cancellation.
4. Update `openapi.yml` and any generated clients to the new lifecycle model.
5. Benchmark memory density and restore latency before and after the shared-memory change.
6. Get `go test ./...` green and keep the new network regression test in CI.

## Merge Recommendation

Do not merge as-is if the goal is a production-ready scale/security improvement.

This branch is close enough to keep iterating on, but it should clear the restore confinement issue, the firewall ordering issue, and the current test failures before being treated as ready to share as a completed solution rather than an in-progress design.
