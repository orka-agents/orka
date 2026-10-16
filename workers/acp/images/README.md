# Immutable ACP runtime images

These definitions build separate Codex, Claude, GitHub Copilot, and OpenCode
ACP runtime images, plus opt-in AgentKit and Foundry composition images. The build context must
be the repository root so each image can compile
`cmd/orka-acp-runtime` and verify its duplicated supply-chain values against
`internal/acp/pins.go`.

The four built-in final images intentionally:

- run the Orka supervisor as root so it can create private session trees and
  launch each ACP child under a unique, non-reused UID/GID;
- contain no provider, controller, SCM, Git, or MCP credential;
- remove npm, npx, Corepack, and Yarn from the runtime image;
- contain no `git`, `ssh`, `curl`, or `wget` client;
- use `/sessions` as the writable session root and otherwise support a
  read-only root filesystem;
- rely on the supervisor's empty child-environment allowlist and provider
  update-disable controls;
- support only `linux/amd64` and `linux/arm64`.

Runtime credentials must be mounted as files and referenced through the
`ORKA_ACP_*_TOKEN_FILE` settings. Do not pass secrets as Docker build arguments,
labels, or environment defaults.

## AgentKit composition image

`workers/acp/images/agentkit/Dockerfile` layers the static Orka supervisor and
exec helper onto an operator-supplied AgentKit agent image. It requires the
source image by immutable digest and validates the expected
`/opt/agentkit/bin/agentkit-serve` executable and `/agent/agent.yaml`. The
advertised adapter digest must equal the digest-pinned source image identity,
so a stale or unrelated value cannot pass composition. The build does not
rebuild or sanitize the source image.

The source-image operator remains responsible for its packages and clients
(including package managers, Git, SSH, curl, or wget), embedded defaults and
credentials, writable paths, supported architectures, vulnerability posture,
SBOM and signature, and third-party licensing. Deployment must apply the same
read-only filesystem, credential-mount, service-account, egress, and supervisor
capability controls described below.

## Foundry composition image

`workers/acp/images/foundry/Dockerfile` layers the static Orka supervisor and
exec helper onto an operator-supplied Foundry ACP image. Set
`FOUNDRY_RUNTIME_IMAGE` to a digest-pinned image containing the executable
`/agent-runtime-foundry` and a nonempty `/agent/foundry.json`. Set
`FOUNDRY_ADAPTER_DIGEST` to that same source image's `sha256:` digest. The build
rejects a mismatched adapter digest and copies both artifacts with root
ownership and read-only permissions. It supports `linux/amd64` and
`linux/arm64` and starts the supervisor as root to allocate isolated child
identities.

The source image supplies the immutable Foundry target, model, and tool policy.
The composition does not rebuild or sanitize that image; its operator owns the
same source-content responsibilities listed for AgentKit above. Azure identity
belongs to the separate broker sidecar and must not be embedded in the source
image or passed to the ACP child. Use the explicit
`docker-build-acp-foundry-runtime` and `docker-push-acp-foundry-runtime` Make
targets; Foundry is excluded from the aggregate built-in image targets.

## Frozen inputs

Base images are immutable multi-platform manifest-list references:

| Input | Immutable reference |
| --- | --- |
| Dockerfile frontend | `docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e` |
| Go builder | `golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61` |
| Node adapter builder (build platform only) | `node:22.22.0-bookworm-slim@sha256:dd9d21971ec4395903fa6143c2b9267d048ae01ca6d3ea96f16cb30df6187d94` |
| Node runtime (Codex, Claude, Copilot final stages and the Claude layout stage) | `node:22.22.0-trixie-slim@sha256:465a8c8f0f4103861bcbcf3e512608394b7155eccb1955425f4ea3f672ddc53e` |
| Debian OpenCode runtime | `debian:trixie-slim@sha256:020c0d20b9880058cbe785a9db107156c3c75c2ac944a6aa7ab59f2add76a7bd` |

## Runtime base

Every stage that runs on the target platform (`--platform=$TARGETPLATFORM`)
uses Debian 13 ("trixie"). Build-only stages keep the bookworm builders so the
adapter artifact digests pinned in `internal/acp/pins.go` are unchanged.
[Toolbox](../../../website/docs/guides/toolboxes.md) authors build against this
baseline: a toolbox binary must run with the runtime's glibc.

| Runtime | Final base | Debian | glibc |
| --- | --- | --- | --- |
| Codex | `node:22.22.0-trixie-slim` | 13 (trixie) | 2.41 |
| Claude | `node:22.22.0-trixie-slim` | 13 (trixie) | 2.41 |
| Copilot | `node:22.22.0-trixie-slim` | 13 (trixie) | 2.41 |
| OpenCode | `debian:trixie-slim` | 13 (trixie) | 2.41 |

Verify after a rebuild with `docker run --rm --entrypoint ldd <image> --version`.

Codex inputs:

| Input | Pin |
| --- | --- |
| Codex ACP source | commit `68d7d2d5ddfc0ed5746f9f6130892dda685e65dd`; codeload archive SHA-256 `51ba8d1d302f8a854c0501887ffbe12fb267f6e8fdae9ad2b58e2c8fc9513b1e` |
| Codex ACP release | `2.1.1`; npm tar SHA-256 `9da0d580518d006d257609a4b64b7c5bd96a7f36f2d86db5c8f2cb9385de5872` |
| Orka Codex ACP external-sandbox patch | `workers/acp/images/codex/patch-agent-mode.mjs`; SHA-256 `538ac5d7ae6e3d46f3f0961439f914bfb9e427be6aea83c9b1025548ef8cac1d` |
| Patched Codex ACP `dist/index.js` | SHA-256 `6792f51a6af4e1ae6317ad2197240836038bc53cb9f0fc9152a62d31b132d5a2` |
| Codex CLI | `0.160.0`; source commit `a956835d020762cb2b570053af06f643a11c0ecc` |
| Codex CLI `linux/amd64` | npm tar SHA-256 `37a41d61c3399182b8c727b77090cc7a1566bd849d0f09070a0bbc6fec4c58dc` |
| Codex CLI `linux/arm64` | npm tar SHA-256 `9286a7e01d500ab224c9e5b4b223b7adf5dd901fba23efd6a438316426798b17` |

Claude inputs:

| Input | Pin |
| --- | --- |
| Claude Agent ACP source | commit `4e1fc1e8df2b9af3b1b2d6880ba90c5862283ba8`; codeload archive SHA-256 `533a654f100d54b86ce457fc128e7b1f3e85e3478474e536968bca6999953242` |
| Claude Agent ACP release | `0.89.1`; npm tar SHA-256 `0df9fa22a5e1ccc39bfba37c0c58f272febed6884fb51a2df970840796ea2acb` |
| Claude Agent SDK | `0.3.293`; npm tar SHA-256 `395bfbda4294c6334fa18be3706a46ff31851b4f4a6f74ba11ac96c9d97ced9f` |
| Claude Code | `2.1.293`, carried by the SDK native package |
| Claude native `linux/amd64` | npm tar SHA-256 `bdcb3a8e653e96aace67adb5ce6a50d20753c62093ec45396882fdb327535a76` |
| Claude native `linux/arm64` | npm tar SHA-256 `7a896de19be15b52b0b0ede2befe58c11ff2321064c3333ccac3f84eadafc36c` |

GitHub Copilot inputs:

| Input | Pin |
| --- | --- |
| Copilot CLI | `1.0.96`; source commit `dcdc0273c0250d8b32677ca99bc0e1f5006daa89` |
| Copilot CLI `linux/amd64` | official `copilot-linux-x64.tar.gz`; SHA-256 `66b60b58b14d72390f30d6d7f8480d18a0f05c111d132e4ff8f92ae637d413f8` |
| Copilot CLI `linux/arm64` | official `copilot-linux-arm64.tar.gz`; SHA-256 `5073e1ee39fab0c724d10e6fc852977830b5196aa9b66cde934a65faa8010d6b` |
| Copilot CLI license | `LICENSE.md` at the pinned source commit; SHA-256 `c694e2d9b3f3bb40704ca9bdc823dc90269999ab4c013dfd8a52c407046074b4` |

Copilot CLI `1.0.96` is newer than the `1.0.76-0` boundary that fixed
credentialless custom-provider session creation in ACP mode. Orka therefore
keeps the real upstream credential behind its provider proxy and starts
Copilot ACP sessions without GitHub login or an ACP authentication exchange.

OpenCode inputs:

| Input | Pin |
| --- | --- |
| OpenCode release | `1.19.0` |
| OpenCode source | tag commit `733b2c627e5aec39cb33cc22373bdce33053ef2c`; codeload archive SHA-256 `ab433f7a5fc9e54c90da18bd3457298009f426424805f043aa32c03543b4a192` |
| OpenCode `linux/amd64` | baseline `opencode-linux-x64-baseline-1.19.0.tgz` (no AVX2 scheduling assumption); npm tar SHA-256 `6bdf946045ebfa5773811d0037c1ca8c8a3831880434289b9e4a40c2f1fe8432`; extracted binary SHA-256 `135ac3f86c43f2a22328a3554fec5f0bc57d71576077902661bc38dac34871fd` |
| OpenCode `linux/arm64` | `opencode-linux-arm64-1.19.0.tgz`; npm tar SHA-256 `8481102c5d475013302d2f4766cea02b7390e76e74c146408a8cebe2b7f6778f`; extracted binary SHA-256 `1e392db1d558a9eebaef618d4da3927f8b61fbcbd9a19513ed77f911142415df` |
| ripgrep `linux/amd64` | Debian `15.2.0-1` glibc package from immutable snapshot file `096560a159a8be70155f16209d91777019011677` with a versioned Debian-pool fallback; package SHA-256 `d9d86586969e925b8627936ec4387d10cc65baf7d42195258743a49e711b6157`; extracted `rg` SHA-256 `af751db9b06d308fd5078aa973880cc8c75678289386bf1a6ee5faa614aa6f2d` |
| ripgrep `linux/arm64` | Debian `15.2.0-1` glibc package from immutable snapshot file `482bbe93dc82997d7d84c901990e8d9f4327457c` with a versioned Debian-pool fallback; package SHA-256 `8f18379cd499dd890d27190a359fe194d0bce92d7efad8e47b0f636e64bf4b46`; extracted `rg` SHA-256 `d3febe5d02e2a15da0909200caeb5af0be65e58736331c2ab88b7086d62e942b` |
| Orka root instruction | `workers/acp/images/opencode/AGENTS.md`; SHA-256 `b8e691e210a9502c9689ba20705806dfb1b5b10b5ca1062f73f9a85b1043a812` |
| Image third-party notice | `workers/acp/images/opencode/NOTICE.md` (mirrors root `NOTICE.md`); SHA-256 `0f876edfd5c73c4a99ffb0565c8ea3350a4db1167cc3ded7d8b50cc0fa061ff0` |

Codex and Claude adapters are compiled from exact GitHub source archives after
frozen `package-lock.json` installs with `npm ci --ignore-scripts`. Their
unmodified adapter outputs must first byte-match separately checksum-verified
published npm releases. The Codex build then applies the checksum-pinned Orka
mode patch, rebuilds, and verifies the exact patched bundle digest. That mode
selects Codex's `externalSandbox` policy with restricted network so the
RuntimeSession and Pod security boundary is authoritative without nested
namespaces.

Codex ACP 2.1.1 still negotiates ACP `protocolVersion: 1`; its package version
is independent of the ACP wire version. The Orka mode passes `standard` and
`user` for the adapter's mode kind and approvals reviewer, keeping `on-request`
approvals under the RuntimeSession security boundary.

The adapter returns JSON-RPC `invalid_request` (`-32600`) with
`data.reason: "thread_active_writer"` when another Codex client owns a thread.
A caller must release that writer before retrying. For an unmaterialized
thread, the adapter falls back to `thread/read` in the same live app-server;
a missing persisted thread still fails. For new conversations, Orka uses
`session/new` and continues with `session/prompt`.

For native Codex restoration, the supervisor installs a verified SessionKit
bundle into the child's fresh private `CODEX_HOME` before starting codex-acp.
Orka requires the adapter's advertised `session/resume` capability and resumes
the original thread UUID with the current working directory, provider, MCP
tools, approvals, and sandbox settings. Missing resume support or a failed
resume rejects creation without falling back to `session/new`.
The supervisor also sets `MODEL_PROVIDER` to the destination provider ID;
`CODEX_CONFIG.model_provider` alone does not select the provider passed to
`thread/resume` in this adapter release. See the
[Codex session migration guide](../../../website/docs/guides/codex-session-migration.md)
for supported history, import/export commands, and recovery limits.

When the agent advertises `sessionCapabilities.close`, Orka sends
`session/close` before the bounded process stop on deletion. When an adapter
child exits while its RuntimeSession is idle and the agent advertises
`sessionCapabilities.resume`, the supervisor restarts the adapter under the
same session identity and reconnects it with `session/resume` (no history
replay) before the next prompt. The restart finishes before that prompt gets
provider or MCP access, so a model request the agent makes before answering
`session/resume` is refused instead of counting against the prompt's
`maxTurns`; Claude Agent ACP 0.89.1 completes its one such request before it
answers. Notifications received while the resume
handshake is in flight are discarded. Session-control updates such as
`available_commands_update` are excluded from harness prompt streams
regardless of arrival order. This resume is same-Pod only: the agent's state
lives in the ephemeral session home, so a cold resume after suspension or a
pool replacement still recreates the RuntimeSession from Orka's canonical
transcript, and an idle exit the agent cannot resume retires the session the
same way. Before any ACP prompt write, the supervisor returns non-retryable
`prompt_not_accepted` for that idle-exit retirement; the controller records
`Failed`/`RuntimeLost` without automatic replay, and the next continuation
rebuilds from the canonical transcript. Orka does not use `session/load`
(removed in the ACP v2 draft in favor of `session/resume` with `replayFrom`),
`session/list`, or the still-unstable `session/fork`.

Session-lifecycle capabilities advertised by the pinned runtimes, verified by
driving each adapter over stdio (`initialize`, `session/new`, `session/prompt`,
process kill, fresh process, `session/resume` or `session/load`, `session/close`)
when each pin last changed: Codex on 2026-10-06, and Claude, Copilot and
OpenCode on 2026-10-12:

| Runtime | `loadSession` | `resume` | `close` | `list` | `fork` | Resume across adapter restart |
| --- | --- | --- | --- | --- | --- | --- |
| Codex ACP 2.1.1 (Codex CLI 0.160.0) | yes | yes | yes | yes | yes (unstable) | works; only `available_commands_update` follows |
| Claude Agent ACP 0.89.1 (Claude Code 2.1.293) | yes | yes | yes | yes | yes (unstable) | works; sends one model request before the next prompt |
| Copilot CLI 1.0.96 | yes | no (`Method not found`) | yes | yes | no | `session/load` only, replays full history |
| OpenCode 1.19.0 | yes | yes | yes | yes | yes (unstable) | works |

Re-run the probe when any of these pins change; Copilot is the only built-in
runtime whose idle adapter exit cannot be resumed in place.

The Copilot image instead installs the unmodified official per-architecture
release executable. Its tar asset is checksum-verified, must contain exactly one
`copilot` entry, and is never downloaded at runtime. The four built-in final
images perform no package installation or network download.

OpenCode is not wrapped by an adapter: the supervisor launches the
checksum-verified native binary as `opencode --pure acp`. Its per-session config
permits only the explicit Orka OpenAI-compatible proxy and disables project
config, model fetching, external skills/plugins, LSP downloads, formatters,
sharing, snapshots, and auto-update. It explicitly loads the immutable
`/opt/opencode/AGENTS.md` guardrail followed by the session workspace root
`AGENTS.md`, while ambient project discovery remains disabled. Native tool
permissions are deterministic `allow`/`deny` rules derived from the session's
Orka tool policy; read-intent sessions additionally force shell, mutation,
and Grep tools off. Grep cannot carry the path-specific secret-file exclusions
applied to Read, and the current dispatcher does not admit interactive ACP
permission prompts. The image vendors the checksum-pinned Debian glibc ripgrep 15.2.0 binary at
`/usr/local/bin/rg`, avoiding the x86_64 musl allocator failure mode while
preventing OpenCode's Glob/Grep tools from
attempting a runtime GitHub download. The private config directory is seeded
only with empty `node_modules` plus root package/lock dependency metadata for
`@opencode-ai/plugin` 1.19.0 so OpenCode's shallow dependency check does not
launch a background network install; no plugin implementation is vendored. As
with the other built-in runtime images, Git is intentionally absent: the ACP
child sees a materialized workspace tree without repository credentials, and
Orka's clean-room Publisher owns Git staging and publication after delta
validation.

## Build with the remote builder

Use the repository root as the final argument:

```bash
docker buildx build \
  --builder remote-vm \
  --platform linux/amd64,linux/arm64 \
  --file workers/acp/images/codex/Dockerfile \
  --tag docker.io/sozercan/orka-acp-codex:2.1.1-codex-0.160.0 \
  --provenance=mode=max \
  --sbom=true \
  --push \
  .
```

```bash
docker buildx build \
  --builder remote-vm \
  --platform linux/amd64,linux/arm64 \
  --file workers/acp/images/claude/Dockerfile \
  --tag docker.io/sozercan/orka-acp-claude:0.89.1-sdk-0.3.293 \
  --provenance=mode=max \
  --sbom=true \
  --push \
  .
```

```bash
docker buildx build \
  --builder remote-vm \
  --platform linux/amd64,linux/arm64 \
  --file workers/acp/images/copilot/Dockerfile \
  --tag docker.io/sozercan/orka-acp-copilot:1.0.96 \
  --provenance=mode=max \
  --sbom=true \
  --push \
  .
```

```bash
docker buildx build \
  --builder remote-vm \
  --platform linux/amd64,linux/arm64 \
  --file workers/acp/images/opencode/Dockerfile \
  --tag docker.io/sozercan/orka-acp-opencode:1.19.0-rg-15.2.0 \
  --provenance=mode=max \
  --sbom=true \
  --push \
  .
```

For a non-publishing verification build, replace `--push` and `--tag` with:

```bash
--output=type=cacheonly
```

After publishing, record the returned multi-platform image digest and sign that
immutable digest rather than a mutable tag.

## Runtime contract

Each built-in image defaults only `ORKA_ACP_PROVIDER`. Deployment configuration
must supply the model, runtime-pool and controller fence, profile digests,
workspace intent, credential role/scope, and absolute Secret-mounted token-file
paths.
Mount a writable volume at `/sessions`; do not rely on the image layer for
session durability. Kubernetes packaging must also enforce the read-only root
filesystem, no service-account token, default-deny egress, and only the
explicit supervisor capabilities needed for UID/GID and process management.

The images omit an SCM client on purpose. Clone and publication belong to the
separate clean-room Workspace/Publisher identity, not the ACP runtime process
tree.

## Licensing caveat

Orka's license and notice, adapter licenses, and provider license/notice files
are retained under `/usr/share/licenses` in the built-in final images.

Codex ACP and the pinned Codex source are Apache-2.0. OpenCode is MIT, and
ripgrep is dual-licensed under MIT or the Unlicense. Claude Agent ACP is
Apache-2.0, but the Claude Agent SDK and bundled native Claude Code executable
are proprietary Anthropic artifacts whose package license says use is subject
to Anthropic's legal agreements. GitHub Copilot CLI is distributed unmodified
under the GitHub Copilot CLI License included in the image and in the repository
`NOTICE.md`. Building these Dockerfiles does not independently grant service or
provider access. Obtain legal approval for the intended distribution and
service model before pushing or sharing the Claude or Copilot images.
