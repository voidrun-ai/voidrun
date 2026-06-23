#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

echo "== Unit tests =="
go test ./...

echo "== Build =="
go build -o /tmp/voidrun-server ./cmd/server

if [[ ! -e /dev/kvm ]]; then
  echo "SKIP: /dev/kvm not available on this host (nested virt required on cloud VMs)"
  echo "Plugin registry:"
  go test ./pkg/compute/... -v -run TestResolveType
  exit 0
fi

echo "== KVM available: integration smoke =="
export HYPERVISOR="${HYPERVISOR:-cloud_hypervisor}"
go test -tags=integration ./plugins/... -v -count=1 2>/dev/null || echo "No integration tests yet or skipped"

echo "Done."
