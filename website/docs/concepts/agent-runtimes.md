---
slug: /agent-runtimes
description: "How Orka runs coding agents: ACP RuntimePools, sessions, and the pool lifecycle."
---

# Agent runtimes

A `type: agent` Task runs a real coding-agent CLI — Codex, Claude Code, GitHub Copilot CLI,
or OpenCode — against a git repository. This page explains how Orka runs them: warm pools of
runtime Pods, private sessions inside them, a strict separation between the agent and your
credentials, and a durable record of every attempt.

In short:

- **Pools, not Jobs.** Orka keeps a controller-owned `RuntimePool` per runtime profile. Its
  Pod scales to zero when idle and hosts several private **RuntimeSessions** when busy, so a
  Task usually starts in seconds. There is no per-Task Job and no fallback path.
- **The agent edits; Orka publishes.** The CLI works on a sanitized checkout with no Git
  credentials. When it finishes, Orka validates the change, and a separate
  Workspace/Publisher commits, pushes, and verifies it, optionally opening a pull request.
- **Every attempt is durable.** Orka records each prompt attempt, so a controller restart
  never sends the same prompt twice and an uncertain outcome is reported as uncertain.
- **Models through a proxy.** Runtimes reach your model gateway through Orka's authenticated
  [provider proxy](../operations/provider-proxy.md); no provider key enters the agent.

Orka harness v2 (`orka.harness.v2`) is the controller-to-supervisor contract, carried over
HTTP with NDJSON prompt streams. The supervisor in each runtime Pod drives the agent CLI over
the [Agent Client Protocol](https://agentclientprotocol.com) (ACP) on stdin/stdout. These are
separate protocols with independent versions. Orka owns the durable Task attempt,
RuntimeSession identity, queueing, workspace validation, delivery receipt, transcript, and
result projection; the runtime Pod owns only the short-lived provider process and ACP session
for a fenced RuntimeSession.

The built-in set is intentionally closed: `codex`, `claude`, `copilot`, and `opencode`.
Operators can also register and conformance-test an external `orka.harness.v2`
`AgentRuntime`. A ready strict-governed registration selected through `runtimeRef` uses the
same durable Task and RuntimeSession state machines, while the operator remains responsible
for the external service lifecycle and capacity.

## Supported runtime profiles

| Runtime | `Agent.spec.runtime.type` | Packaging status |
| --- | --- | --- |
| Codex | `codex` | Immutable Codex ACP image definition is included. Configure a digest-pinned image. |
| Claude | `claude` | Immutable Claude ACP image definition is included. Configure a digest-pinned image. |
| Copilot | `copilot` | Immutable Copilot ACP image definition is included. Configure a digest-pinned image. |
| OpenCode | `opencode` | Immutable OpenCode ACP image definition is included. Configure a digest-pinned image. |

Built-in runtimes can mount extra command-line tools from read-only toolbox
images declared in `Agent.spec.runtime.toolboxes`; see
[Toolboxes](../guides/toolboxes.md).

External runtimes use an `AgentRuntime` registration with `contractVersion: orka.harness.v2`. Current-generation conformance proves the exact instance, profile, cancellation, duplicate, and workspace-governance claims. Dispatch revalidates the frozen registration, authentication authority, and observed runtime identity before every mutation. See [Bring your own AgentRuntime](../guides/bring-your-own-agent-runtime.md) and the [adapter contract](../development/agent-runtime-adapter-contract.md).

## Architecture

```text
Task (type: agent)
  -> durable ACP attempt and queue reservation
  -> built-in path: central authenticated provider proxy -> Vekil/model backend
     -> controller-owned RuntimePool for one trust domain/profile
        -> one exact runtime Pod (0 or 1 replica)
           -> private RuntimeSession HOME/workspace/UID + provider process
           -> per-session loopback MCP proxy -> controller prompt broker
  -> external path: authenticated operator-owned AgentRuntime endpoint
     -> exact fenced RuntimeSession + non-reconnectable prompt stream
     -> registered provider, MCP, and workspace-governance profile
  -> workspace delta validation
  -> artifact + credential brokers -> separate Workspace/Publisher identity
  -> clean-room clone, prepare, push, verify, and optional PR reconciliation
  -> durable execution and delivery status projected onto the Task
```

A shared runtime Pod is a same-trust-domain density optimization, not a tenant isolation boundary. Different namespaces/profile digests are assigned different logical pools.

## RuntimePools

RuntimePools are controller-owned implementation resources. Do not hand-author them for normal built-in Tasks. The controller derives a pool from the Task namespace, runtime type, model, immutable adapter/image digests, tool and approval policy, workspace intent, proxy credential scope, and resource class.

The first-release defaults are:

- one active Pod per pool;
- up to 10 resident RuntimeSessions;
- up to 4 concurrently running prompts;
- scale from zero on durable demand;
- admit new sessions only while lifecycle is `Serving` and admission is `Accepting`;
- drain to `Quiescent` before scale-down or replacement.

Pool lifecycle is:

```mermaid
stateDiagram-v2
    direction LR
    [*] --> Stopped
    Stopped --> Starting: a Task needs this pool
    Starting --> Serving: Pod ready
    Serving --> Draining: replacement or scale-down
    Draining --> Quiescent: last session finished
    Quiescent --> Stopping
    Stopping --> Stopped
```

`Serving` is the only lifecycle that admits a new RuntimeSession, and only while
`status.admissionState` is also `Accepting`. Two more lifecycles are not shown above because the
controller can enter them from anywhere: **`Degraded`** (something is wrong — rollout failed,
scheduling failed, quota denied) and **`Ambiguous`** (the controller cannot prove what the pool is
doing). Both close admission, so new work waits instead of landing on a pool nobody can vouch for.

Capacity reservations live in `RuntimePool.status` and use Kubernetes
`resourceVersion` compare-and-swap. The dispatcher claims a resident/prompt slot
before changing an attempt to `Reserved`, renews or converts it as work moves
through the session lifecycle, and reclaims expired reservations after restart.

Inspect pools with:

```bash
orka runtime-pool list
orka runtime-pool get '<pool-name>' -o yaml
kubectl get runtimepools
```

## Orka harness v2 Task lifecycle {#acp-v2-task-lifecycle}

The durable Task execution state is independent from the top-level compatibility phase. It is
drawn in full under [Task lifecycle](architecture.md#task-lifecycle). Two states are worth calling
out here:

- `SubmittedUnknown` records a loss in the submit acknowledgement window — Orka sent the prompt and
  never learned whether the runtime received it.
- `OutcomeUnknown` is terminal for that attempt. Do not treat it as a generic retryable failure;
  the whole point is that a retry might run your prompt a second time.

For built-in runtimes, the RuntimeSession lifecycle is:

```mermaid
stateDiagram-v2
    [*] --> creating
    creating --> idle
    idle --> prompt_running: prompt admitted
    prompt_running --> validating: prompt settled
    validating --> idle: read Task, or no changes
    validating --> preparing_publication: write Task with changes

    preparing_publication --> publication_prepared
    publication_prepared --> finalizing: publication reconciled
    finalizing --> deleting

    idle --> deleting
    deleting --> deleted
    deleted --> [*]

    prompt_running --> cancelling: cancellation requested
    cancelling --> poisoned
    poisoned --> deleting
```

Reading it:

- **Only an `idle` session can accept a prompt or be evicted.** Everything else is busy holding
  state that must not be interrupted.
- The session freezes the validated workspace delta and waits in `publication_prepared`.
  The controller and clean-room Publisher prepare the commit, push, and verify the remote,
  recording progress in a `Publication`. The controller then asks the supervisor to finalize
  the session using the terminal publication receipt.
- **`poisoned`** is the escape hatch for uncertainty. A session that cannot prove it cleaned up its
  processes, that its workspace is intact, or that publication was safe is poisoned and deleted
  rather than reused. The pool may be recycled with it. Any state above can reach `poisoned`; the
  arrows are omitted so the main flow stays readable.
- Validation, publication, finalization, and the release of the Session lease all complete before
  the Task shows a terminal status. A `Succeeded` Task means the work is done and verified, not
  merely started.

## Durable control authority

ACP control decisions are Kubernetes-authoritative:

- `ControllerEpoch`, `PromptAttempt`, `RuntimeSessionControl`, `BranchClaim`,
  `Publication`, and `ExternalEffect` status transitions use
  `resourceVersion` compare-and-swap;
- coordination Leases serialize controller-epoch mutation and one logical
  Session mutation at a time;
- `RuntimePool.status` owns exact-instance lifecycle, demand, capacity, and
  reservation state.

SQLite is not a second ACP control authority. It persists transcript and
SessionTurn payloads, deferred outbox projections, and artifacts
behind the Kubernetes fences. Cross-store finalization binds the exact outbox
projection before releasing the Session lease.

### What a fence is, and why every request carries one

A **fence** is a set of identifiers attached to every mutating request. The receiver compares
them against what it believes to be true and rejects anything that does not match exactly.

The problem it solves: a controller can be network-partitioned, lose leadership, come back, and
try to finish work it started — while a *new* controller is already doing that work. Without a
fence, the old controller's late request looks identical to a legitimate one. With a fence, its
stale controller epoch does not match and the request is refused.

The identifiers nest, from the widest scope to the narrowest:

```mermaid
flowchart TD
    Epoch["<b>controllerEpoch</b><br/>which controller is in charge"]
    Pool["<b>runtimePoolUID</b> + <b>runtimePoolGeneration</b><br/>which pool, at which version of its spec"]
    Inst["<b>runtimeInstanceID</b> + <b>supervisorBootID</b><br/>which Pod, and which boot of the process inside it"]
    Profile["<b>runtimeProfileDigest</b><br/>which immutable image + profile is running"]
    Sess["<b>runtimeSessionUID</b> + <b>runtimeSessionGeneration</b><br/>which session, at which version"]
    Op["<b>operationID</b> + <b>requestDigest</b> + <b>expiresAt</b><br/>this one request, unmodified, still in date"]

    Epoch --> Pool --> Inst --> Profile --> Sess --> Op

    style Epoch fill:#f3f0ff,stroke:#7048e8
    style Op fill:#fff4e6,stroke:#d9822b
```

| Field | Rejects | Because |
| --- | --- | --- |
| `controllerEpoch` | a controller that lost leadership | a new controller has taken over |
| `runtimePoolUID` / `runtimePoolGeneration` | a request for a deleted or reconfigured pool | the pool it was written for no longer exists in that shape |
| `runtimeInstanceID` / `supervisorBootID` | a request aimed at a Pod that has since restarted | the process that held the session state is gone |
| `runtimeProfileDigest` | a request assuming a different image or profile | the runtime is not the one the request was planned against |
| `runtimeSessionUID` / `runtimeSessionGeneration` | a request for a session that was recreated | the new session is not the old one, even under the same name |
| `operationID` | reuse of an operation ID with a different request digest | one operation ID identifies one immutable request |
| `requestDigest` | a request whose body was altered in transit | the digest covers the whole request |
| `expiresAt` | a request that sat in a queue too long | the world may have moved on |

Session-scoped fields are omitted for pool-wide operations such as drain, which have no session.

Identical retries with the same `operationID` and `requestDigest` replay the recorded response
without repeating the effect, provided the fences and expiry are still valid. These replays
return HTTP 200; reusing the operation ID with a different digest returns `digest_conflict`
with HTTP 409.

If an authenticated drain request or status probe fails during rollout or scale-down, the
controller marks the `RuntimePool` as `Degraded`, closes admission, and preserves the old Pod.
Later reconciles recheck the drain, and replacement or scale-down requires authenticated
quiescence. Prompt or publication outcomes that cannot be proven may put a Task in
`OutcomeUnknown`; see [Task lifecycle](architecture.md#task-lifecycle).

## Create an Agent

Provider credentials are separate from Git credentials. Built-in ACP Agents do
not set `secretRef`: the controller scopes each RuntimePool to the central
authenticated provider proxy, which injects the upstream Vekil/model credential
outside the ACP child process.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: codex-reviewer
spec:
  runtime:
    type: codex
    contractVersion: orka.harness.v2
    defaultMaxTurns: 20
    defaultAllowBash: true
    defaultAllowedTools:
      - Read
      - Glob
      - Grep
  model:
    name: gpt-6-astra
```

OpenCode model IDs use provider/model form, for example `openai/gpt-6-astra`.
OpenCode selects `apply_patch` for GPT-family models and `edit`/`write` for
others; Orka normalizes those aliases into one governed mutation capability, so
allowing one exposes the group and disallowing one closes the group. For
read-intent workspaces, Orka also disables Bash and OpenCode Grep because Grep
cannot carry the secret-file exclusions applied to OpenCode Read; Read and Glob
remain available when allowed by policy.

For built-in RuntimePools, Task-level `spec.agentRuntime` contains runtime overrides such as `maxTurns`, `allowedTools`, `disallowedTools`, and `allowBash`. For `runtimeRef`, the registered external profile owns provider, model, prompt, skill, tool, and runtime defaults. Its `capabilities.mcpPolicy` stores the exact tool and approval policy represented by the profile digests, and Orka uses that same policy for conformance and dispatch. The Agent must omit `spec.model`, `spec.systemPrompt`, `spec.skills`, enabled `spec.tools`, `defaultMaxTurns`, `defaultAllowedTools`, `defaultAllowBash`, and `defaultReasoningEffort`. Disabled Agent tool entries are inert and accepted. Task-level `allowedTools` must equal the registered allowlist when brokered tools are exposed. Orka rejects `maxTurns`, `disallowedTools`, and `allowBash` because those values are fixed by the registration. Repository configuration belongs at top-level `spec.workspace`.

## Read-only workspace Task

Agent Tasks default to `intent: read` when `workspace.intent` is omitted, but setting it explicitly makes review intent clear.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: review-repository
spec:
  type: agent
  agentRef:
    name: codex-reviewer
  prompt: Review the authentication package. Do not modify files.
  workspace:
    intent: read
    gitRepo: https://github.com/example/project.git
    ref: 0123456789abcdef0123456789abcdef01234567
    readCredentialRef:
      name: project-read
    subPath: services/auth
  agentRuntime:
    maxTurns: 20
    allowBash: true
  timeout: 20m
```

The clean-room workspace boundary resolves `project-read` for one clone/read operation, verifies the source baseline, removes credentials, and stores a sanitized workspace artifact. The runtime downloads that artifact without SCM credentials or direct SCM publication access.

A read Task succeeds only after validation proves the final tree still matches the verified baseline. Unexpected changes are classified `ReadOnlyWorkspaceModified` and poison the RuntimeSession.

## Write and publication Task

:::tip[Video demo]
Watch [From chat to a GitHub pull request](https://www.youtube.com/watch?v=FlLgdh2lKMk).
:::

Source read, target read, target write, and forge API credentials are distinct
roles, even when an operator deliberately backs multiple roles with the same
Secret.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: update-documentation
spec:
  type: agent
  agentRef:
    name: codex-reviewer
  prompt: Update the authentication documentation and run its focused checks.
  workspace:
    intent: write
    gitRepo: https://github.com/example/project.git
    branch: main
    readCredentialRef:
      name: project-read
    publicationGitRepo: https://github.com/example/project.git
    publicationReadCredentialRef:
      name: project-publication-read
    publicationCredentialRef:
      name: project-publish
    forgeCredentialRef:
      name: project-forge
    pushBranch: orka/update-auth-docs
    prBaseBranch: main
    createPR: true
    prTitle: "fix: handle empty filters"
    prBody: "Reject empty filters before querying."
  agentRuntime:
    maxTurns: 40
    allowBash: true
  timeout: 40m
```

The ACP child may edit the materialized checkout, but its Git configuration, refs, hooks, filters, remotes, and history are never trusted for publication. After prompt settlement, the supervisor freezes the session process tree and produces a bounded, content-addressed workspace delta. The separate Workspace/Publisher then:

1. creates a clean clone at the persisted baseline with the source-read credential;
2. applies the validated delta;
3. prepares a deterministic Orka-owned commit;
4. records the exact tree, commit, and artifact digests;
5. compare-and-swap publishes the exact commit with the target-write credential;
6. independently verifies the remote ref with the target-read credential;
7. reconciles a pull request with the forge-only credential when `createPR: true`.

At reservation, Orka freezes the selected Secret UID/resourceVersion and key for
each role. The credential broker releases a value only to the
Workspace/Publisher for the exact active operation. No Git or forge credential
enters the runtime Pod. Branch push is the minimum durable delivery;
pull-request reconciliation is opt-in.

Workspace deltas and prepared publication artifacts cross the controller API
through short-lived artifact capabilities bound to the operation, method,
path, digest, size, media type, identity, and expiry. The Publisher does not
hold the artifact signing key.

Every `spec.workspace` field, with defaults, is in the
[Resource reference](../reference/resources.md#workspace).

## Pull request text

Pull-request text comes from the frozen Task spec and prompt, never from files or output produced by the agent. Orka rejects secret-like titles and bodies at runtime before publication, including prompt-derived titles. Orka reserves comments beginning with `<!-- orka.publisher.pr-` for reconciliation. Remove these comments when copying an existing PR body into `prBody`. The publisher keeps the original title and body when reconciling an existing pull request. A continuation does not overwrite a person's edits.

`prTitle` and `prBody` apply only when `createPR: true`. Supplying presentation text alone does not request a pull request; it can remain configured on branch-only Tasks.

Explicit `prTitle` or `prBody` overrides require a publisher that advertises `pull-request-presentation.v1`. During a rolling update, Tasks with overrides wait before prompt admission. Tasks without overrides can finish with an older publisher's generic PR text.

Do not embed credentials, query strings, or fragments in repository URLs.

## Delivery status

`status.delivery` is the authoritative non-secret receipt. Intermediate states include `Validating`, `Preparing`, `Prepared`, `Publishing`, and `Verifying`. Terminal outcomes include:

- `ReadValidated` — read-only tree matches the verified baseline;
- `NoChange` — a write Task produced no validated change;
- `VerifiedExact` — the remote branch equals the prepared commit;
- `DeliveredSuperseded` — the prepared commit is present and a verified descendant now leads the branch;
- `ReadOnlyWorkspaceModified` — a read Task changed the workspace;
- `CredentialBlocked` — a required credential could not be resolved or used;
- `DeliveryConflict` — branch ownership or exact-ref reconciliation failed;
- `PublicationOutcomeUnknown` — the remote could not be observed after bounded reconciliation;
- `CancelledBeforePublish` — cancellation won before publication.

Use `orka task status <name>` or inspect the Task YAML to see execution, RuntimePool, and delivery data.

## Session continuity

Use `spec.sessionRef` for canonical transcript continuity and serialized mutation of one logical session:

```yaml
spec:
  sessionRef:
    name: repository-review-session
  workspace:
    intent: read
    gitRepo: https://github.com/example/project.git
```

A RuntimeSession is ephemeral. If its Pod is replaced, Orka creates a fresh
provider process from the verified workspace baseline. Codex runtimes advertising
native Session support can restore a private SessionKit checkpoint into an
isolated home, then load its original UUID with current credentials and policy.
Other sessions use the canonical transcript. Neither path replays an accepted
prompt. See [Codex session migration](../guides/codex-session-migration.md) for
the supported format and import/export commands.

## Runtime and credential boundaries

A built-in runtime Pod uses a digest-pinned image, a read-only root filesystem, and no
service-account token. It holds no Git credential and has no route to source control. Its
model traffic goes only to the central provider proxy, and each RuntimeSession reaches Orka
tools only through a credential-protected loopback MCP proxy that the controller rechecks on
every call. A call to a tool outside the frozen policy, such as one the model names on its own,
is refused there as a failed tool call and the prompt continues. The Workspace/Publisher, which does hold the Git credentials, has no access to
models or tools. [Security](security.md#acp-runtimepool-pods) describes each boundary.

## Controller configuration

ACP is active when the controller runs in `harness-v2` mode; there is no
separate ACP enablement flag. At minimum, set the controller mode and configure
immutable images:

```text
--controller-mode=harness-v2
--acp-runtime-namespace=orka-runtimes
--acp-provider-proxy-namespace=orka-system
--acp-provider-proxy-base-url=http://provider-auth-proxy.orka-system.svc:8080
--acp-codex-runtime-image=<repository>@sha256:<digest>
--acp-claude-runtime-image=<repository>@sha256:<digest>
--acp-copilot-runtime-image=<repository>@sha256:<digest>
--acp-opencode-runtime-image=<repository>@sha256:<digest>
```

:::warning[The two provider-proxy flags must name the same namespace]
`--acp-provider-proxy-base-url` is the address runtime Pods call.
`--acp-provider-proxy-namespace` is what the generated NetworkPolicy allows them to reach.
If they disagree, every provider call is dropped by default-deny egress with no useful error.
The flag defaults to `vekil-system`, but the Helm chart overrides it to the release namespace
(`orka-system` on a default install), which is where the chart puts `provider-auth-proxy`.
:::

Mutable image tags are rejected for built-in pools. For Kustomize deployment,
apply `config/acp-production`; `config/default` alone omits the required
cross-namespace Vekil ingress boundary.

## Troubleshooting

- **Task fails before queueing**: for a built-in runtime, verify the Agent has `runtime.type: codex`, `claude`, `copilot`, or `opencode`, ACP is enabled, and the matching digest-pinned image is configured. For `runtimeRef`, verify the registration is current-generation, ready, strict-governed, and matches the Task workspace intent.
- **Task remains queued**: inspect the selected RuntimePool lifecycle/admission/capacity and the runtime Pod scheduling conditions.
- **Agent warnings in Task results**: Orka advertises ACP session notices, so Codex and Claude Code send their own warnings, such as Codex's missing model metadata, as notices, which Orka keeps out of the result and does not log. Copilot CLI 1.0.96 ignores notices and still writes its `Info:` lines into the reply.
- **Task reports `OutcomeUnknown`**: do not retry the same attempt automatically. Inspect the recorded attempt, controller epoch, runtime instance, and event timeline.
- **Read Task fails validation**: inspect `status.delivery` for `ReadOnlyWorkspaceModified` and treat the session as poisoned.
- **Write Task is blocked**: verify all required source-read, target-read, target-write, and forge credential references, the publication repository/branch, and the Workspace/Publisher broker configuration. Require a terminal verified delivery receipt before claiming success.
- **`runtimeRef` Task is rejected**: inspect `AgentRuntime.status` for current-generation conformance, exact profile and instance identity, strict workspace governance, and valid authentication Secret bindings. Remove unsupported Task overrides such as `maxTurns`, `disallowedTools`, or `allowBash`.
- **`spec.execution.workspace` Task is rejected**: enable the workspace provider API and ACP dispatch with the required admission webhooks, then check the selected `classRef`, live `use` authorization, provider heartbeat/contracts, and immutable profile binding. Provider-specific Task selectors are removed. An ACP class must require `acp.runtime.v2`; its allowed reuse and detach actions must match the request. Core also needs read-only access to native worker Pods when applicable. The rejection reason appears in `Task.status.executionWorkspace`. Repository access uses top-level `spec.workspace`; see [Agent Sandbox](agent-sandbox.md) and [Substrate](substrate.md).

## Agent persona

Built-in harness v2 runtimes support an optional [`Agent.spec.soul`](./soul.md)
source, independent of the role system prompt. The controller resolves and pins
its content before dispatch. Codex and Claude use native instruction settings;
Copilot and OpenCode use protected per-session instruction files. These files are
not repository content and are not published or restored from a repository
workspace checkpoint. External `runtimeRef` registrations do not accept souls.
