#!/usr/bin/env bash

# https://cloud.google.com/artifact-registry/docs/pull-cached-dockerhub-images
# Cache misses fall back to Docker Hub and remain subject to its pull limits.
configure_docker_hub_mirror() (
  set -Eeuo pipefail

  if [[ "${RUNNER_ENVIRONMENT:-}" != github-hosted || "${RUNNER_OS:-}" != Linux ]]; then
    echo "::error::Docker Hub cache setup requires a GitHub-hosted Linux runner" >&2
    exit 1
  fi

  # Pin daemon operations to the runner's system service, not a remote context.
  docker_host=unix:///var/run/docker.sock
  running_containers="$(docker --host "${docker_host}" ps --quiet)"
  if [[ -n "${running_containers}" ]]; then
    echo "::error::Configure the Docker Hub cache before starting containers or kind" >&2
    exit 1
  fi

  # The caller-supplied path is only used by isolated shell-contract tests.
  config_file="$1"
  mirror=https://mirror.gcr.io
  tmp_dir="$(mktemp -d)"
  had_config=false
  changed=false
  # Invoked indirectly by the EXIT trap.
  # shellcheck disable=SC2329
  cleanup() {
    status=$?
    trap - EXIT
    if [[ "${status}" -ne 0 && "${changed}" == true ]]; then
      echo "::warning::Restoring Docker daemon configuration after cache setup failed" >&2
      if [[ "${had_config}" == true ]]; then
        sudo cp -p "${tmp_dir}/original.json" "${config_file}" || status=1
      else
        sudo rm -f "${config_file}" || status=1
      fi
      # One recovery restart with the original settings, not a pull retry.
      sudo systemctl restart docker || status=1
    fi
    rm -rf "${tmp_dir}"
    exit "${status}"
  }
  trap cleanup EXIT

  if sudo test -e "${config_file}"; then
    had_config=true
    sudo cp -p "${config_file}" "${tmp_dir}/original.json"
    # The destination belongs to the runner, inside its private temp directory.
    # shellcheck disable=SC2024
    sudo cat "${tmp_dir}/original.json" > "${tmp_dir}/input.json"
  else
    printf '{}\n' > "${tmp_dir}/input.json"
  fi

  # Preserve every other daemon setting and existing mirror, in original order.
  jq --arg mirror "${mirror}" '
    if type != "object" then
      error("Docker daemon configuration must be an object")
    elif has("registry-mirrors") and
      (."registry-mirrors" | type != "array" or any(.[]; type != "string")) then
      error("registry-mirrors must be an array of strings")
    else
      ."registry-mirrors" = ([$mirror] +
        ((."registry-mirrors" // []) | map(select(rtrimstr("/") != $mirror))))
    end
  ' "${tmp_dir}/input.json" > "${tmp_dir}/daemon.json"

  # Validate before replacing the live configuration or stopping the service.
  if ! sudo dockerd --validate --config-file "${tmp_dir}/daemon.json" >/dev/null 2>&1; then
    echo "::error::Docker rejected the merged daemon configuration" >&2
    exit 1
  fi
  sudo mkdir -p "$(dirname "${config_file}")"
  changed=true
  sudo cp "${tmp_dir}/daemon.json" "${config_file}"
  sudo systemctl restart docker
  sudo systemctl is-active --quiet docker
  if ! docker --host "${docker_host}" info --format '{{json .RegistryConfig.Mirrors}}' |
    jq -e --arg mirror "${mirror}" 'any(.[]; rtrimstr("/") == $mirror)' >/dev/null; then
    echo "::error::The running Docker daemon did not confirm the Docker Hub cache" >&2
    exit 1
  fi
  echo "Docker daemon confirmed the https://mirror.gcr.io Docker Hub cache"
)

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  configure_docker_hub_mirror /etc/docker/daemon.json
fi
