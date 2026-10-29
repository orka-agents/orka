#!/usr/bin/env bash
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/lib/deployment-rollout.sh
. "${root}/scripts/lib/deployment-rollout.sh"

fake_grace=""
fake_kubectl() {
  local want="-n orka-system get deployment orka-controller-manager -o jsonpath={.spec.template.spec.terminationGracePeriodSeconds}"
  if [[ "$*" != "${want}" ]]; then
    printf 'unexpected kubectl call: %s\n' "$*" >&2
    return 1
  fi
  printf '%s' "${fake_grace}"
}

failing_kubectl() {
  return 1
}

expect_timeout() {
  local grace="$1" want="$2" got
  fake_grace="${grace}"
  got="$(deployment_rollout_timeout fake_kubectl -n orka-system orka-controller-manager)"
  if [[ "${got}" != "${want}" ]]; then
    printf 'grace %q: timeout %s, want %s\n' "${grace}" "${got}" "${want}" >&2
    exit 1
  fi
}

expect_timeout 360 540s
expect_timeout 600 780s
expect_timeout 121 301s
expect_timeout 120 300s
expect_timeout 30 300s
expect_timeout '' 300s

fake_grace=6m
if deployment_rollout_timeout fake_kubectl -n orka-system orka-controller-manager 2>/dev/null; then
  echo 'a non-numeric grace period was accepted' >&2
  exit 1
fi
if deployment_rollout_timeout failing_kubectl orka-controller-manager 2>/dev/null; then
  echo 'a failed Deployment read was accepted' >&2
  exit 1
fi
if deployment_rollout_timeout orka-controller-manager 2>/dev/null; then
  echo 'a call without a kubectl command was accepted' >&2
  exit 1
fi
printf '%s\n' 'ok - rollout waits cover the termination grace period plus startup, never less than 5m'
