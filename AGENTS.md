# Agent instructions

## Cursor Cloud specific instructions

- Standard setup and run commands live in `README.md` and the WSL2-specific sandbox guide lives in `docs/wsl2-setup.md`; use those as the source of truth for normal developer workflows.
- The API service can be developed and tested against MongoDB without booting sandboxes. In Cursor Cloud, full sandbox creation/execution also requires `/dev/kvm`, `cloud-hypervisor`, a configured bridge/NAT, and base image/kernel files; if those are absent, expect only API/database flows such as version, org/API-key, image listing, and sandbox listing to work.
- The current router does not mount the README's public `/api/register` example. For local API bootstrapping without Clerk credentials, start the server with `AUTH_LOCAL_MODE=true`, create an API key through `/api/orgs/apikeys`, then restart with normal auth and use that key via `X-API-Key`.
- When running API-only checks in an environment without sandbox VM support, set `HEALTH_ENABLED=false`, `MONITOR_ENABLED=false`, and `AUTO_LIFECYCLE_ENABLED=false` so startup does not try to manage unavailable VM runtime state.
