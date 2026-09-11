# Official Substrate pin

`upstream.env` pins the unmodified provider commit and the SHA-256 of its
`ateapi.proto`. Orka does not apply provider patches or consume fork-only APIs.

`scripts/lib/substrate-upstream.sh` verifies the official repository, commit,
and clean tracked source before installation. It uses a dedicated gVisor kind
cluster and a scoped kubeconfig. Existing clusters require explicit reuse;
a shared registry on another port is never replaced.

Regenerate or verify the vendored protocol with protoc 36.0 and the pinned Go
generators installed by the script:

```bash
bash scripts/generate-substrate-proto.sh
bash scripts/generate-substrate-proto.sh verify
```

The upstream Apache 2.0 license is retained in `internal/substratepb/LICENSE`.
Updating the pin requires source/digest review, regenerated protocol, native
transport tests, and `scripts/agent-substrate-e2e.sh`. See ADR 0031 and
`website/docs/concepts/substrate.md` for the supported lifecycle and trust
boundary. Do not interpret a source match or doctor result as live conformance.
