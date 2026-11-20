#!/usr/bin/env bash
# Rollout wait helper for E2E scripts. Source this file; do not execute it.

# deployment_rollout_timeout prints a `kubectl rollout status --timeout` value
# for a Deployment: its pod termination grace period plus a 3m startup margin,
# never less than 5m. The controller-manager uses the Recreate strategy, so its
# replacement Pod starts only after the old Pod has finished the preStop upgrade
# drain, which may use the whole grace period.
#
# Arguments: the kubectl command and the arguments that select the cluster and
# namespace, followed by the Deployment name. For example:
#   deployment_rollout_timeout kubectl -n orka-system orka-controller-manager
deployment_rollout_timeout() {
  if (($# < 2)); then
    printf 'error: deployment_rollout_timeout needs a kubectl command and a Deployment name\n' >&2
    return 2
  fi
  local deployment="${!#}" grace seconds
  grace="$("${@:1:$#-1}" get deployment "${deployment}" \
    -o jsonpath='{.spec.template.spec.terminationGracePeriodSeconds}')" || return 1
  grace="${grace:-30}"
  if [[ ! "${grace}" =~ ^[0-9]+$ ]]; then
    printf 'error: Deployment %s has an invalid termination grace period %q\n' "${deployment}" "${grace}" >&2
    return 1
  fi
  seconds=$((grace + 180))
  if ((seconds < 300)); then
    seconds=300
  fi
  printf '%ss\n' "${seconds}"
}
