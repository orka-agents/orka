#!/usr/bin/env bash
# Runs the pinned native OpenCode restore tests without provider credentials or
# external network access. Requires Docker and the Go toolchain from go.mod.
# Optional: ORKA_NATIVE_RESTORE_IMAGE, ORKA_NATIVE_RESTORE_SKIP_BUILD=1.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

native_restore_image="${ORKA_NATIVE_RESTORE_IMAGE:-orka-opencode-native-restore:local}"
native_restore_arch="$(docker version --format '{{.Server.Arch}}')"
case "$native_restore_arch" in
  amd64|arm64) ;;
  *) printf 'Unsupported Docker architecture: %s\n' "$native_restore_arch" >&2; exit 1 ;;
esac

if [[ "${ORKA_NATIVE_RESTORE_SKIP_BUILD:-0}" != 1 ]]; then
  docker build --platform "linux/$native_restore_arch" \
    --file workers/acp/images/opencode/Dockerfile --tag "$native_restore_image" .
fi

mkdir -p bin
native_restore_build="$(mktemp -d "$repo_root/bin/native-restore-e2e.XXXXXX")"
trap 'rm -rf "$native_restore_build"' EXIT
CGO_ENABLED=0 GOOS=linux GOARCH="$native_restore_arch" \
  go test -c -o "$native_restore_build/supervisor.test" ./workers/acp/supervisor

# Match the runtime's identity/process capability set, with no external
# network and only ephemeral writable directories. The test starts local
# provider/MCP fixtures inside this container and verifies native binary pins.
docker run --rm --init --network=none --read-only \
  --cap-drop=ALL --cap-add=CHOWN --cap-add=KILL --cap-add=SETGID --cap-add=SETUID \
  --tmpfs /tmp:rw,exec,nosuid,size=512m --tmpfs /home:rw,nosuid,size=16m \
  --mount "type=bind,src=$native_restore_build/supervisor.test,dst=/native-restore.test,readonly" \
  --env ORKA_OPENCODE_NATIVE_RESTORE_E2E=1 \
  --entrypoint /native-restore.test "$native_restore_image" \
  -test.run '^TestOpenCodeNativeRestoreE2E$' -test.v -test.count=1 -test.timeout=7m
