#!/bin/bash
set -euo pipefail

echo "Installing Kubebuilder development tools..."

ARCH=$(go env GOARCH)

# Install Bun (pinned to the version used by the UI test job)
PINNED_BUN_VERSION="1.3.13"
if [ ! -x /usr/local/bin/bun ] || [ "$(/usr/local/bin/bun --version)" != "${PINNED_BUN_VERSION}" ]; then
  # Digests from the pinned release's SHASUMS256.txt.
  case "${ARCH}" in
    amd64)
      BUN_ARCH=x64
      BUN_SHA256=79c0771fa8b92c33aae41e15a0e0d307ea99d0e2f00317c71c6c53237a78e25a
      if ! grep -qw avx2 /proc/cpuinfo; then
        BUN_ARCH=x64-baseline
        BUN_SHA256=9d8a24292a7068090205daac0a5a223f5f69736f5287e37bf88d3b4031edc750
      fi
      ;;
    arm64)
      BUN_ARCH=aarch64
      BUN_SHA256=70bae41b3908b0a120e1e58c5c8af30e74afae3b8d11b0d3fdd8e787ddfb4b22
      ;;
    *) echo "ERROR: Bun is not supported on linux-${ARCH}" >&2; exit 1 ;;
  esac
  if ! command -v unzip &> /dev/null; then
    apt-get update >/dev/null
    apt-get install -y --no-install-recommends unzip >/dev/null
  fi
  echo "Installing Bun ${PINNED_BUN_VERSION} (linux-${BUN_ARCH})..."
  curl -fsSL "https://github.com/oven-sh/bun/releases/download/bun-v${PINNED_BUN_VERSION}/bun-linux-${BUN_ARCH}.zip" -o /tmp/bun.zip \
    || { echo "ERROR: failed to download Bun ${PINNED_BUN_VERSION} for linux-${BUN_ARCH}" >&2; exit 1; }
  printf '%s  /tmp/bun.zip\n' "${BUN_SHA256}" | sha256sum --check --status \
    || { echo "ERROR: Bun archive checksum verification failed" >&2; exit 1; }
  unzip -o /tmp/bun.zip -d /usr/local
  chmod +x "/usr/local/bun-linux-${BUN_ARCH}/bun"
  ln -sf "/usr/local/bun-linux-${BUN_ARCH}/bun" /usr/local/bin/bun
  rm -f /tmp/bun.zip
fi
if [ "$(/usr/local/bin/bun --version)" != "${PINNED_BUN_VERSION}" ]; then
  echo "ERROR: Bun ${PINNED_BUN_VERSION} installation verification failed" >&2
  exit 1
fi
if ! command -v bun &> /dev/null || [ "$(bun --version)" != "${PINNED_BUN_VERSION}" ]; then
  echo "ERROR: Bun ${PINNED_BUN_VERSION} is installed at /usr/local/bin/bun but is not active on PATH" >&2
  echo "Move /usr/local/bin before other Bun installations on PATH and rerun setup." >&2
  exit 1
fi

# Install kind
if ! command -v kind &> /dev/null; then
  curl -Lo ./kind "https://kind.sigs.k8s.io/dl/latest/kind-linux-${ARCH}"
  chmod +x ./kind
  mv ./kind /usr/local/bin/kind
fi

# Install kubebuilder
if ! command -v kubebuilder &> /dev/null; then
  curl -L -o kubebuilder "https://go.kubebuilder.io/dl/latest/linux/${ARCH}"
  chmod +x kubebuilder
  mv kubebuilder /usr/local/bin/
fi

# Install kubectl
if ! command -v kubectl &> /dev/null; then
  KUBECTL_VERSION=$(curl -L -s https://dl.k8s.io/release/stable.txt)
  curl -LO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl"
  chmod +x kubectl
  mv kubectl /usr/local/bin/kubectl
fi

# Wait for Docker to be ready
for i in {1..30}; do
  if docker info >/dev/null 2>&1; then
    break
  fi
  if [ $i -eq 30 ]; then
    echo "WARNING: Docker not ready after 30s"
  fi
  sleep 1
done

# Create kind network, ignore errors if exists or conflicts
docker network inspect kind >/dev/null 2>&1 || docker network create kind || true

# Verify installations
echo "Installed versions:"
bun --version
kind version
kubebuilder version
kubectl version --client
docker --version
go version

echo "DevContainer ready!"
