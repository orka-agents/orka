# Execution enforcement protocol

`orka_enforcement/v1/enforcement.proto` is the provider-neutral contract.
Providers are separately installed operator-trusted services, authenticated with
per-node mTLS where physical enforcement is node-local. Policy bytes are opaque:
Orka does not translate, merge or replace the submitted policy.

An immutable binding belongs to one complete requirement and one physical
container incarnation, not a consuming Task. Pending, refused, malformed,
mismatched, or failed RPC results never authorize execution. Every mandatory
provider must return current enforced evidence, and their capabilities together
must cover the requirement. Runtime-owned stop precedes terminal idempotent
retirement. An RPC deadline does not expire a Task, container or policy.

Requirement and target-facts identities are SHA-256 over the ASCII domain,
one NUL byte, then deterministic protobuf encoding. Domains are respectively
`orka.enforcement.requirement/v1` and `orka.enforcement.target-facts/v1`.
The result is lowercase hex with a `sha256:` prefix. Capability order is part
of the submitted requirement identity. Do not parse or reserialize policy bytes.

Regenerate Go bindings with `protoc-gen-go v1.36.12` and
`protoc-gen-go-grpc v1.5.1`:

```sh
protoc -Iproto \
  --go_out=. --go_opt=module=github.com/orka-agents/orka \
  --go-grpc_out=. --go-grpc_opt=module=github.com/orka-agents/orka \
  proto/orka_enforcement/v1/enforcement.proto
```

External Python providers can generate the same schema with
`grpcio-tools==1.74.0`, `--python_out=<src>` and
`--grpc_python_out=<src>`. Generated modules retain the independent
`orka_enforcement.v1` namespace.
