#!/usr/bin/env bash
set -Eeuo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=hack/agent-substrate/upstream.env
source "${root_dir}/hack/agent-substrate/upstream.env"
mode="${1:-generate}"
case "${mode}" in generate|verify) ;; *) printf 'usage: %s [generate|verify]\n' "$0" >&2; exit 2 ;; esac

task_generate_dir="$(mktemp -d "${TMPDIR:-/tmp}/orka-substrate-proto.XXXXXX")"
trap 'rm -rf "${task_generate_dir}"' EXIT
if [[ -n "${SUBSTRATE_SOURCE_DIR:-}" ]]; then
  source_commit="$(git -C "${SUBSTRATE_SOURCE_DIR}" rev-parse HEAD)"
  [[ "${source_commit}" == "${SUBSTRATE_UPSTREAM_COMMIT}" ]] || { printf 'Substrate source commit does not match the pinned upstream commit\n' >&2; exit 1; }
  cp "${SUBSTRATE_SOURCE_DIR}/pkg/proto/ateapipb/ateapi.proto" "${task_generate_dir}/ateapi.proto"
else
  curl --fail --show-error --silent --location \
    "https://raw.githubusercontent.com/agent-substrate/substrate/${SUBSTRATE_UPSTREAM_COMMIT}/pkg/proto/ateapipb/ateapi.proto" \
    --output "${task_generate_dir}/ateapi.proto"
fi
source_digest="$(shasum -a 256 "${task_generate_dir}/ateapi.proto" | awk '{print $1}')"
[[ "${source_digest}" == "${SUBSTRATE_UPSTREAM_PROTO_SHA256}" ]] || { printf 'Substrate protocol source digest mismatch\n' >&2; exit 1; }

command -v protoc >/dev/null || { printf 'protoc is required\n' >&2; exit 1; }
[[ "$(protoc --version)" == "libprotoc ${SUBSTRATE_PROTOC_VERSION}" ]] || {
  printf 'Protocol generation requires protoc %s\n' "${SUBSTRATE_PROTOC_VERSION}" >&2
  exit 1
}
mkdir -p "${root_dir}/bin"
GOBIN="${root_dir}/bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
GOBIN="${root_dir}/bin" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
PATH="${root_dir}/bin:${PATH}" protoc --proto_path="${task_generate_dir}" \
  --go_out="${task_generate_dir}" --go_opt=paths=source_relative \
  '--go_opt=Mateapi.proto=github.com/orka-agents/orka/internal/substratepb;ateapipb' \
  --go-grpc_out="${task_generate_dir}" --go-grpc_opt=paths=source_relative \
  '--go-grpc_opt=Mateapi.proto=github.com/orka-agents/orka/internal/substratepb;ateapipb' \
  "${task_generate_dir}/ateapi.proto"

for filename in ateapi.proto ateapi.pb.go ateapi_grpc.pb.go; do
  if [[ "${mode}" == verify ]]; then
    cmp "${task_generate_dir}/${filename}" "${root_dir}/internal/substratepb/${filename}"
  else
    cp "${task_generate_dir}/${filename}" "${root_dir}/internal/substratepb/${filename}"
  fi
done
