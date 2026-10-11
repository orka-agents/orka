#!/usr/bin/env bash
# All Docker, dockerd, sudo and systemctl calls are allowlisted mocks.
set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT
mkdir -p "${tmp_dir}/mock-bin"

cat > "${tmp_dir}/mock-bin/sudo" <<'MOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
case "$1" in
  cp)
    if [[ "${*: -1}" == "${MOCK_CONFIG}" ]]; then
      if [[ "$*" == *original.json* ]]; then
        echo restore >> "${MOCK_LOG}"
      else
        echo write >> "${MOCK_LOG}"
      fi
    fi
    ;;
  test|cat|mkdir|rm|dockerd|systemctl) ;;
  *) echo "unexpected sudo command: $1" >&2; exit 90 ;;
esac
exec "$@"
MOCK
cat > "${tmp_dir}/mock-bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "$1" == --host && "$2" == unix:///var/run/docker.sock ]] || exit 90
shift 2
case "$*" in
  'ps --quiet')
    echo ps >> "${MOCK_LOG}"
    [[ "${MOCK_PS_FAIL:-0}" == 0 ]] || exit 1
    printf '%s' "${MOCK_RUNNING:-}"
    ;;
  'info --format {{json .RegistryConfig.Mirrors}}')
    echo info >> "${MOCK_LOG}"
    [[ "${MOCK_INFO_FAIL:-0}" == 0 ]] || exit 1
    if [[ "${MOCK_MISSING_MIRROR:-0}" == 1 ]]; then
      echo '[]'
    else
      jq '."registry-mirrors" | map(. + "/")' "${MOCK_CONFIG}"
    fi
    ;;
  *) echo "unexpected Docker command: $*" >&2; exit 90 ;;
esac
MOCK
cat > "${tmp_dir}/mock-bin/dockerd" <<'MOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "$1" == --validate && "$2" == --config-file && "$#" == 3 ]] || exit 90
echo validate >> "${MOCK_LOG}"
[[ "${MOCK_VALIDATE_FAIL:-0}" == 0 ]] || exit 1
jq -e '."registry-mirrors"[0] == "https://mirror.gcr.io"' "$3" >/dev/null
MOCK
cat > "${tmp_dir}/mock-bin/systemctl" <<'MOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
case "$*" in
  'restart docker')
    echo restart >> "${MOCK_LOG}"
    if [[ "${MOCK_RESTART_FAIL:-0}" == 1 && ! -e "${MOCK_LOG}.restarted" ]]; then
      touch "${MOCK_LOG}.restarted"
      exit 1
    fi
    ;;
  'is-active --quiet docker')
    echo active >> "${MOCK_LOG}"
    [[ "${MOCK_ACTIVE_FAIL:-0}" == 0 ]] || exit 1
    ;;
  *) echo "unexpected systemctl command: $*" >&2; exit 90 ;;
esac
MOCK
chmod +x "${tmp_dir}/mock-bin/"*

new_case() {
  case_dir="${tmp_dir}/$1"
  mkdir -p "${case_dir}"
  export MOCK_CONFIG="${case_dir}/docker/daemon.json"
  export MOCK_LOG="${case_dir}/calls"
  : > "${MOCK_LOG}"
  export RUNNER_ENVIRONMENT=github-hosted RUNNER_OS=Linux
  unset MOCK_RUNNING MOCK_PS_FAIL MOCK_INFO_FAIL MOCK_MISSING_MIRROR
  unset MOCK_VALIDATE_FAIL MOCK_RESTART_FAIL MOCK_ACTIVE_FAIL
}
seed_config() {
  mkdir -p "$(dirname "${MOCK_CONFIG}")"
  printf '%s\n' "$1" > "${MOCK_CONFIG}"
  cp "${MOCK_CONFIG}" "${case_dir}/before.json"
}
run_action() {
  PATH="${tmp_dir}/mock-bin:${PATH}" bash -c \
    'source "$1"; configure_docker_hub_mirror "$2"' \
    _ "${script_dir}/configure.sh" "${MOCK_CONFIG}" > "${case_dir}/output" 2>&1
}
expect_success() {
  if run_action; then
    :
  else
    cat "${case_dir}/output" >&2
    echo "unexpected failure: ${case_dir}" >&2
    exit 1
  fi
}
expect_failure() {
  if run_action; then
    echo "unexpected success: ${case_dir}" >&2
    exit 1
  fi
}
expect_calls() {
  printf '%s\n' "$@" > "${case_dir}/expected-calls"
  if [[ "$#" == 0 ]]; then : > "${case_dir}/expected-calls"; fi
  diff -u "${case_dir}/expected-calls" "${MOCK_LOG}"
}
expect_unchanged() {
  cmp "${case_dir}/before.json" "${MOCK_CONFIG}"
}

new_case missing-config
expect_success
jq -e '. == {"registry-mirrors": ["https://mirror.gcr.io"]}' "${MOCK_CONFIG}" >/dev/null
expect_calls ps validate write restart active info

new_case preserve-settings
seed_config '{"debug":false,"features":{"containerd-snapshotter":true},"log-opts":{"max-size":"10m"},"registry-mirrors":["https://existing.example","https://mirror.gcr.io/","https://mirror.gcr.io"]}'
expect_success
jq -e '. == {"debug":false,"features":{"containerd-snapshotter":true},"log-opts":{"max-size":"10m"},"registry-mirrors":["https://mirror.gcr.io","https://existing.example"]}' "${MOCK_CONFIG}" >/dev/null
cp "${MOCK_CONFIG}" "${case_dir}/merged.json"
expect_success
cmp "${case_dir}/merged.json" "${MOCK_CONFIG}"
expect_calls ps validate write restart active info ps validate write restart active info

for invalid in 'not-json' '[]' 'null' '{"registry-mirrors":null}' '{"registry-mirrors":"https://other.example"}' '{"registry-mirrors":[42]}'; do
  new_case invalid-config
  seed_config "${invalid}"
  expect_failure
  expect_unchanged
  expect_calls ps
done

new_case validation-failure
seed_config '{"debug":false}'
export MOCK_VALIDATE_FAIL=1
expect_failure
expect_unchanged
expect_calls ps validate

for flag in MOCK_RESTART_FAIL MOCK_ACTIVE_FAIL MOCK_INFO_FAIL MOCK_MISSING_MIRROR; do
  new_case "${flag}"
  seed_config '{"debug":false}'
  export "${flag}=1"
  expect_failure
  expect_unchanged
  case "${flag}" in
    MOCK_RESTART_FAIL) expect_calls ps validate write restart restore restart ;;
    MOCK_ACTIVE_FAIL) expect_calls ps validate write restart active restore restart ;;
    *) expect_calls ps validate write restart active info restore restart ;;
  esac
done

new_case rollback-created-config
export MOCK_RESTART_FAIL=1
expect_failure
[[ ! -e "${MOCK_CONFIG}" ]]
expect_calls ps validate write restart restart

for flag in MOCK_RUNNING MOCK_PS_FAIL; do
  new_case "${flag}"
  seed_config '{"debug":false}'
  export "${flag}=1"
  expect_failure
  expect_unchanged
  expect_calls ps
done

for runner in self-hosted macOS; do
  new_case "${runner}"
  seed_config '{"debug":false}'
  if [[ "${runner}" == self-hosted ]]; then
    export RUNNER_ENVIRONMENT=self-hosted
  else
    export RUNNER_OS=macOS
  fi
  expect_failure
  expect_unchanged
  expect_calls
done

echo 'Docker Hub mirror shell contracts passed; only mocked Docker/service commands ran'
