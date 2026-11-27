#!/usr/bin/env bash

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
