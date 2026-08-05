#!/usr/bin/env bash
# Shared helpers for publishing immutable test images to a registry reachable by
# both the host and nodes in one Kind cluster.

orka_kind_registry_name() {
  local cluster="$1"
  local sanitized
  sanitized="$(printf '%s' "${cluster}" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9_.-' '-')"
  sanitized="${sanitized#[-_.]}"
  sanitized="${sanitized%[-_.]}"
  printf '%s-orka-registry\n' "${sanitized}"
}

orka_kind_registry_start() {
  local cluster="$1"
  local kind_bin="${KIND:-kind}"
  ORKA_KIND_REGISTRY_NAME="$(orka_kind_registry_name "${cluster}")"

  docker rm -f "${ORKA_KIND_REGISTRY_NAME}" >/dev/null 2>&1 || true
  docker run -d --name "${ORKA_KIND_REGISTRY_NAME}" --network kind \
    -p 127.0.0.1::5000 registry:2 >/dev/null

  local port_output host_port
  port_output="$(docker port "${ORKA_KIND_REGISTRY_NAME}" 5000/tcp)"
  port_output="${port_output%%,*}"
  host_port="${port_output##*:}"
  [[ "${host_port}" =~ ^[0-9]+$ ]] || {
    echo "unexpected Kind registry port mapping: ${port_output}" >&2
    return 1
  }
  ORKA_KIND_REGISTRY_ADDR="localhost:${host_port}"

  local deadline=$((SECONDS + 30))
  until curl -fsS "http://${ORKA_KIND_REGISTRY_ADDR}/v2/" >/dev/null 2>&1; do
    if (( SECONDS >= deadline )); then
      echo "Kind registry ${ORKA_KIND_REGISTRY_ADDR} did not become ready" >&2
      return 1
    fi
    sleep 1
  done

  local node registry_dir
  while IFS= read -r node; do
    [[ -n "${node}" ]] || continue
    registry_dir="/etc/containerd/certs.d/${ORKA_KIND_REGISTRY_ADDR}"
    docker exec "${node}" mkdir -p "${registry_dir}"
    docker exec -i "${node}" sh -c "cat > '${registry_dir}/hosts.toml'" <<HOSTS
server = "http://${ORKA_KIND_REGISTRY_ADDR}"
[host."http://${ORKA_KIND_REGISTRY_NAME}:5000"]
  capabilities = ["pull", "resolve"]
HOSTS
  done < <("${kind_bin}" get nodes --name "${cluster}")

  export ORKA_KIND_REGISTRY_NAME ORKA_KIND_REGISTRY_ADDR
}

orka_kind_registry_push() {
  local source_image="$1"
  local repository="$2"
  local target="${ORKA_KIND_REGISTRY_ADDR}/${repository}:e2e"
  docker tag "${source_image}" "${target}"
  docker push "${target}" >/dev/null

  local prefix="${target%:e2e}@sha256:"
  local digest_ref
  digest_ref="$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "${target}" | \
    awk -v prefix="${prefix}" 'index($0, prefix) == 1 { print; exit }')"
  [[ "${digest_ref}" =~ @sha256:[0-9a-f]{64}$ ]] || {
    echo "no immutable digest found after pushing ${target}" >&2
    return 1
  }
  printf '%s\n' "${digest_ref}"
}

orka_kind_registry_stop() {
  local name="${ORKA_KIND_REGISTRY_NAME:-}"
  if [[ -z "${name}" && $# -gt 0 ]]; then
    name="$(orka_kind_registry_name "$1")"
  fi
  if [[ -n "${name}" ]]; then
    docker rm -f "${name}" >/dev/null 2>&1 || true
  fi
}
