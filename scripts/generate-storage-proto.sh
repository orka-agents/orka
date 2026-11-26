#!/usr/bin/env bash
# Generate or verify the orka.storage.v1 wire code in the nested storage module:
# the protobuf and gRPC code under pkg/storage/v1/wire/storagepb, and the
# Go-to-wire mapping that wiregen writes from the Go contract and those
# descriptors. buf compiles the .proto files, so no protoc install is needed.
set -Eeuo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
module_dir="${root_dir}/pkg/storage"
mode="${1:-generate}"
case "${mode}" in generate|verify) ;; *) printf 'usage: %s [generate|verify]\n' "$0" >&2; exit 2 ;; esac

readonly buf_version=v1.73.0
readonly protoc_gen_go_version=v1.36.12
readonly protoc_gen_go_grpc_version=v1.5.1
tool_root="${root_dir}/bin/storage-proto"

# install_tool installs one pinned Go tool into its own versioned directory and
# prints that directory.
install_tool() {
  local name="$1" package="$2" version="$3"
  local dir="${tool_root}/${name}-${version}"
  if [[ ! -x "${dir}/${name}" ]]; then
    mkdir -p "${dir}"
    GOBIN="${dir}" go install "${package}@${version}" >&2
  fi
  printf '%s\n' "${dir}"
}

buf_dir="$(install_tool buf github.com/bufbuild/buf/cmd/buf "${buf_version}")"
go_dir="$(install_tool protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go "${protoc_gen_go_version}")"
grpc_dir="$(install_tool protoc-gen-go-grpc google.golang.org/grpc/cmd/protoc-gen-go-grpc "${protoc_gen_go_grpc_version}")"
tool_path="${buf_dir}:${go_dir}:${grpc_dir}"

[[ "$(PATH="${tool_path}" buf --version)" == "${buf_version#v}" ]] || { printf 'buf %s is required\n' "${buf_version}" >&2; exit 1; }
[[ "$(PATH="${tool_path}" protoc-gen-go --version)" == "protoc-gen-go ${protoc_gen_go_version}" ]] || {
  printf 'protoc-gen-go %s is required\n' "${protoc_gen_go_version}" >&2; exit 1; }
[[ "$(PATH="${tool_path}" protoc-gen-go-grpc --version)" == "protoc-gen-go-grpc ${protoc_gen_go_grpc_version#v}" ]] || {
  printf 'protoc-gen-go-grpc %s is required\n' "${protoc_gen_go_grpc_version}" >&2; exit 1; }

# generate_into regenerates the wire code inside one copy of the module.
generate_into() {
  local dir="$1"
  (
    cd "${dir}"
    PATH="${tool_path}:${PATH}" buf lint
    rm -f v1/wire/storagepb/*.pb.go
    PATH="${tool_path}:${PATH}" buf generate
    go generate ./v1/wire
  )
}

if [[ "${mode}" == generate ]]; then
  generate_into "${module_dir}"
  exit 0
fi

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/orka-storage-proto.XXXXXX")"
trap 'rm -rf "${work_dir}"' EXIT
cp -R "${module_dir}" "${work_dir}/storage"
generate_into "${work_dir}/storage"
if ! diff -r "${module_dir}" "${work_dir}/storage"; then
  printf 'The storage wire code is stale. Run make generate-storage and commit the result.\n' >&2
  exit 1
fi
