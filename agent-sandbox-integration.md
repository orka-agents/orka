# Agent Sandbox Integration

This document captures recommended next steps for integrating Kubernetes SIGs `agent-sandbox` with Orka. It is intentionally scoped as an integration plan and product direction, not a committed API contract.

Sources checked on 2026-05-12:

- https://github.com/kubernetes-sigs/agent-sandbox
- https://agent-sandbox.sigs.k8s.io/docs/
- https://agent-sandbox.sigs.k8s.io/docs/api/
- https://agent-sandbox.sigs.k8s.io/docs/go-client/
- https://kubernetes.io/docs/concepts/containers/runtime-class/

## Recommendation

Keep Orka's current Kubernetes `Job` execution path and `spec.execution.runtimeClassName` support as the default model for one-shot task execution and low-level runtime isolation.

Add `agent-sandbox` only for workloads where the execution environment has value beyond a single Task: durable workspaces, warm starts, interactive sessions, retained files, and reconnectable agent runtimes. It should complement RuntimeClass, not replace it.

The first Orka-facing integration should be a feature-gated, worker-as-client prototype for session-scoped coding agent workspaces. Treat sandbox-backed `code_exec` as a later opt-in backend because persistence changes the current stateless security model.

In short:

> RuntimeClass gives Orka isolated Pods.  
> `agent-sandbox` gives Orka durable, claimable, warm, policy-managed workspaces for long-running AI agents.

## Current Orka Baseline

Orka already supports runtime isolation and placement on agent and task worker pods through `ExecutionSpec`:

- `api/v1alpha1/execution_types.go`
- `api/v1alpha1/agent_types.go`
- `api/v1alpha1/task_types.go`
- `internal/controller/job_builder.go`
- `internal/tools/code_exec_kubernetes.go`

This means Orka can already run one-shot Task Jobs and Kubernetes code execution Jobs with `runtimeClassName: gvisor`, `runtimeClassName: kata-qemu`, node selectors, tolerations, and affinity.

Orka's Kubernetes `code_exec` path is also deliberately ephemeral and hardened:

- non-root user
- read-only root filesystem
- dropped Linux capabilities
- no service account token mounted
- `RuntimeDefault` seccomp
- optional AppArmor
- bounded runtime and output
- TTL fallback plus explicit cleanup

That path should remain the default because it is simple, Kubernetes-native, easy to observe, and well matched to the current model: one task produces one ephemeral worker Job.

## What Agent Sandbox Is Useful For

`agent-sandbox` is most useful when the execution environment has a lifecycle of its own.

Current Orka execution is naturally:

```text
Task -> Job -> result -> cleanup
```

That is good for one-shot tasks, but awkward for coding agents, browser agents, notebooks, and code-interpreter-style workflows.

`agent-sandbox` lets Orka model:

```text
Task -> durable workspace/session environment -> repeated commands/results
```

This unlocks:

- checked-out repositories surviving across turns
- installed dependencies surviving across tasks
- generated files, patches, screenshots, and artifacts surviving between commands
- long-running dev servers, language servers, browser sessions, or notebooks
- code-interpreter-style state
- reconnect and resume behavior after worker retries or human-in-the-loop pauses
- warm pools that avoid repeatedly hydrating expensive agent images

Good Orka fits:

- session-scoped coding agent workspaces tied to `sessionRef` or a future workspace identity
- stateful code-interpreter-style tools where a user expects imports, generated files, and installed packages to persist across calls
- agent runtime images that are expensive to cold start, such as full dev environments with Codex CLI, Claude Code CLI, Copilot CLI, browsers, language servers, package manager caches, or large dependency caches
- interactive development environments, browser/computer-use agents, and notebook-like sessions
- multi-tenant agent environments where reusable templates and default-deny network policies are platform-managed

## Product Positioning

Do not sell `agent-sandbox` primarily as "better isolation." Orka already has a solid isolation baseline through RuntimeClass and hardened worker/code execution Jobs.

The stronger message is:

> Orka uses RuntimeClass for one-shot workload isolation.  
> Orka uses `agent-sandbox` when the execution environment itself needs a lifecycle.

Top selling points for Orka:

1. **Stateful agent workspaces**: repositories, dependencies, generated files, processes, and tool caches can survive across turns or tasks.
2. **Warm starts**: clean, pre-initialized workspaces can be kept ready for heavy agent runtimes without making every Orka Task long-lived.
3. **Separated lifecycles**: Orka worker Jobs stay short-lived orchestration/control-plane units while the workspace remains durable execution infrastructure.
4. **Platform-owned templates**: operators define approved images, RuntimeClass, resource limits, network policy, storage strategy, and default tools in sandbox templates.
5. **RuntimeClass composition**: gVisor, Kata, or another runtime can still be selected inside the sandbox template pod spec.
6. **Reconnect and resume semantics**: coding, browser, and computer-use agents can keep working where they left off.
7. **Future workspace API compatibility**: Orka can expose `workspace` concepts to users while treating `agent-sandbox` as the backend implementation detail.

## When Not To Use It

Do not add `agent-sandbox` just to get gVisor or Kata isolation. Orka already supports that through `spec.execution.runtimeClassName`.

Avoid `agent-sandbox` for the default path when the workload is:

- one Task to one ephemeral Job
- stateless and short-lived
- already covered by the existing JobBuilder flow
- only trying to select a low-level runtime handler
- simpler to reason about as a normal Kubernetes Job with logs, completion status, and TTL cleanup

Also avoid making persistent `code_exec` the first integration. Stateful `code_exec` may be valuable later, but it changes the threat model because files, shell history, package caches, generated state, and secrets can survive unexpectedly. If Orka supports it later, it should be explicit opt-in, with a separate security review and clear reuse boundaries.

## Relationship To RuntimeClass

RuntimeClass and `agent-sandbox` solve different layers of the stack.

RuntimeClass selects the low-level runtime configuration used to run a Pod's containers. Orka already passes this to worker Pods through `spec.execution.runtimeClassName`.

`agent-sandbox` manages a higher-level workspace lifecycle: create or claim a sandbox from a template, optionally use a warm pool, preserve state, attach storage, apply network policy, reconnect to the same environment, and clean it up later.

The recommended model is:

- Use `runtimeClassName` for low-level isolation.
- Use `agent-sandbox` for workspace lifecycle and reuse.
- Put `runtimeClassName` inside the `SandboxTemplate` pod spec when a sandbox should run on gVisor, Kata, or another runtime.
- Do not require users to choose between RuntimeClass and `agent-sandbox`; they compose.

## Recommended Architecture

Prefer this first architecture:

```text
Orka controller
  -> creates normal Orka worker Job
       -> worker uses agent-sandbox client/router
            -> creates or claims sandbox workspace
            -> executes agent/tool command inside workspace
            -> uploads result to Orka as today
       -> worker exits
  -> sandbox is deleted, released, retained, or expired by policy
```

This preserves Orka's current controller and result-submission model while proving the value of workspace lifecycle.

Benefits:

- Existing non-sandbox Tasks are unchanged.
- Rollback is simple: disable the feature gate and return to normal Jobs.
- Orka does not need an unconditional controller dependency on optional `agent-sandbox` CRDs.
- The first implementation can use typed, generated, dynamic, or adapter-based clients without changing the rest of Orka.
- RuntimeClass still applies to the worker Job and can separately apply inside the sandbox template.

Avoid starting with this architecture:

```text
Orka controller
  -> creates only Sandbox/SandboxClaim
       -> sandbox is the worker
```

That model may be useful later, but it makes lifecycle, status, RBAC, cleanup, logs, and result upload harder before the integration has proven value.

## Phased Integration Plan

This integration should be delivered as explicit phase gates. The implementation agent must not move to the next phase until the current phase's exit criteria have been validated and the validation evidence is recorded in the PR, issue, or implementation log.

Validation evidence should include, as applicable:

- exact `agent-sandbox` version or commit tested
- exact Orka commit tested
- commands run and their pass/fail output
- Kubernetes object names created during e2e runs
- observed Task conditions/events/status
- cleanup evidence showing no unexpected retained resources
- documented reasons for any intentionally skipped environment-dependent checks, such as gVisor, Kata, or NetworkPolicy enforcement in a local Kind cluster

### Phase 0: Baseline Guardrails

Goal: Lock in what must not change before introducing sandbox-backed workspaces.

Actions:

1. Record the current default execution behavior for normal Orka Tasks, agent worker Jobs, and Kubernetes `code_exec` Jobs.
2. Confirm that `spec.execution.runtimeClassName`, node selectors, tolerations, affinity, hardened pod security settings, result submission, TTL cleanup, and explicit cleanup work as they do today.
3. Define the feature gate/config boundary for `agent-sandbox`, for example `ORKA_AGENT_SANDBOX_ENABLED=false` by default.
4. Decide which test suites are required on every later phase.
5. Add or identify a non-sandbox regression test that proves Orka can run without `agent-sandbox` CRDs installed.

Exit criteria that the implementation agent must validate before Phase 1:

- `make test` passes on the baseline branch or the current implementation branch with sandbox support disabled.
- At least one non-sandbox Task test verifies that Orka still builds and reconciles the existing worker `Job` path without any `agent-sandbox` CRDs installed.
- At least one Kubernetes `code_exec` test verifies the existing ephemeral, hardened Job behavior remains the default.
- A test or documented manifest inspection confirms `runtimeClassName` is still applied to normal worker/code execution Jobs when configured.
- The feature is explicitly disabled by default, and disabling it requires no `agent-sandbox` controller, CRDs, router, templates, or warm pools to exist in the cluster.

### Phase 1: Spike And Compatibility Check

Goal: Prove that Orka can talk to `agent-sandbox` without committing to a production API or broad code changes.

Actions:

1. Pin an `agent-sandbox` release or commit for the spike.
2. Install the CRDs, controller, and router in a disposable Kind or development cluster.
3. Create a minimal `SandboxTemplate` that runs a shell-capable image.
4. Create and claim a sandbox manually.
5. Execute a simple command inside it.
6. Write a file, read it back, and verify whether it persists according to the selected lifecycle policy.
7. Verify cleanup and retention behavior for `Delete`, `Retain`, and TTL-driven paths where supported by the pinned version.
8. Verify `runtimeClassName` inside the sandbox template with gVisor or Kata where the test cluster supports those runtime classes.
9. Check whether the documented Go client works with Orka's current Go version.
10. If the documented Go client does not work, test a generated client, dynamic Kubernetes client, REST adapter, or small worker-side adapter process.

Exit criteria that the implementation agent must validate before Phase 2:

- A local script, scratch package, or spike branch can create a `SandboxTemplate`, create or claim a sandbox, execute `echo ok`, read/write at least one file, and delete or retain the workspace intentionally.
- The target CRDs, API groups, resource names, and fields are recorded for the pinned `agent-sandbox` version.
- The implementation path is chosen and proven with a minimal compile/run check: documented Go client, generated client, dynamic Kubernetes client, REST adapter, or sidecar/adapter.
- Command execution behavior is documented for streaming, cancellation, timeout, retry, stdout/stderr capture, exit code capture, and output limits.
- Missing-environment checks are explicitly documented. For example, if gVisor/Kata is unavailable locally, the agent records the exact reason and the cluster/runtime required to validate it later.
- The spike includes cleanup evidence, for example `kubectl get sandbox,sandboxclaim` showing expected retained resources only.

### Phase 2: Architecture And API Decision

Goal: Choose the first production integration target and record the decisions that later phases must follow.

Recommended first target:

> Session-scoped coding agent workspaces using the worker-as-client model.

Decision points:

- Should sandbox identity be keyed by `task`, `sessionRef`, `agentRef`, tenant, workspace, or an explicit user-provided reuse key?
- Should Orka own `SandboxClaim` objects directly, or only call the sandbox client from worker Jobs?
- Should the Orka controller watch sandbox resources, or should the worker own the full claim lifecycle for alpha?
- Should results be collected only through Orka's existing result endpoint, or should Orka also store sandbox claim status and artifacts?
- What information must be visible to operators for cleanup, quota, events, metrics, and audits?
- Which workloads are intentionally out of scope for the first alpha?

Recommendation:

- Start with worker-owned claim lifecycle for the prototype.
- Use `sessionRef` as the first reusable workspace boundary.
- Default reuse to `none`; require explicit opt-in for `session` reuse.
- Keep `code_exec` on the current ephemeral Kubernetes Job path for the first integration.
- Store sandbox identity on Task status only after the lifecycle model is proven.
- Avoid a controller-level dependency on optional CRDs until `agent-sandbox` support is enabled.
- Revisit controller ownership once workspaces are durable across many Tasks and need first-class cleanup, quota, and admin UX.

Exit criteria that the implementation agent must validate before Phase 3:

- An ADR, design note, or updated section in this document records the selected lifecycle owner, client strategy, reuse-key strategy, status fields, cleanup owner, and result/artifact path.
- The design explicitly states that Orka chat, Tasks, controller reconciliation, result storage, git/PR handoff, and the existing Kubernetes Job path are not replaced.
- The design explicitly states that sandbox-backed `code_exec` is out of scope unless a separate feature gate and security review are added later.
- The proposed API shape can express all first-alpha scenarios: disabled/default, fresh workspace per task, session reuse, template reference, optional warm pool policy, TTL/shutdown behavior, and cleanup policy.
- A reviewer can map every new concept to either Orka-owned API/status or `agent-sandbox`-owned resources without ambiguity.

### Phase 3: Feature-Gated API And Configuration Scaffolding

Goal: Add an opt-in API/config surface that does not weaken the current `ExecutionSpec` model or expose raw sandbox internals too early.

Prefer user-facing language around **workspace**, not **sandbox**. Operators may understand `SandboxTemplate` and `SandboxClaim`; agent users usually care about a retained session workspace.

Illustrative future API shape:

```go
type WorkspaceSpec struct {
    Enabled bool `json:"enabled,omitempty"`
    TemplateRef *corev1.LocalObjectReference `json:"templateRef,omitempty"`
    ReusePolicy string `json:"reusePolicy,omitempty"` // none/session/agent/workspace
    TTL *metav1.Duration `json:"ttl,omitempty"`
}
```

A Task or Agent could eventually reference it as:

```yaml
spec:
  workspace:
    enabled: true
    reusePolicy: session
    templateRef:
      name: coding-agent
```

If Orka must start with an execution-level alpha knob, keep it clearly provisional:

```yaml
spec:
  execution:
    sandbox:
      enabled: true
      templateRef:
        name: coding-agent
      reusePolicy: session
```

Actions:

1. Add a feature gate or config flag, for example `ORKA_AGENT_SANDBOX_ENABLED`.
2. Add config for router URL, default template, default warm pool policy, namespace strategy, timeout defaults, and cleanup defaults.
3. Add API fields only behind alpha naming or a clearly provisional workspace API.
4. Keep `RuntimeClassName` at the existing level for normal Job execution.
5. For sandbox/workspace execution, prefer putting runtime, storage, resource limits, and pod placement into the referenced sandbox template so platform teams control the environment.
6. Avoid exposing raw `SandboxClaim` details in the user API unless users truly need them.
7. Add status fields only after lifecycle ownership is clear, for example `status.workspace.claimName`, `status.workspace.sandboxName`, `status.workspace.reuseKey`, and `status.workspace.phase`.

Exit criteria that the implementation agent must validate before Phase 4:

- With the feature gate disabled, generated CRDs, defaulted API objects, controller startup, and existing Task reconciliation behave as before.
- `make manifests generate` passes if API types or kubebuilder markers changed.
- `make test` passes.
- Unit tests cover defaulting and validation for workspace fields, including disabled/default behavior, invalid reuse policies, missing template references when enabled, and unsupported combinations.
- A regression test verifies that `JobBuilder` output for non-sandbox Tasks is unchanged, including existing `runtimeClassName` behavior.
- The controller and workers can run in a cluster without `agent-sandbox` CRDs when the feature gate is disabled.
- New status fields, events, and logs do not contain secrets, raw credentials, or full prompts/transcripts.

### Phase 4: Workspace Executor Abstraction

Goal: Isolate Orka from the `agent-sandbox` client implementation and make most behavior testable with fakes before touching real clusters.

Actions:

1. Add a `WorkspaceExecutor` or `SandboxExecutor` interface in Orka so worker code does not depend directly on a specific client library.
2. Model the minimal operations Orka needs first: `Claim`, `WaitReady`, `Exec`, `Upload`, `Download`, `Release`, `Delete`, and `Describe`.
3. Add context-aware cancellation and timeout behavior to every operation.
4. Add structured result types for stdout, stderr, exit code, start/end time, artifacts, and retryable/non-retryable errors.
5. Add a fake implementation for unit tests.
6. Keep `agent-sandbox` imports contained to one adapter package or one integration boundary.
7. Keep `JobBuilder` fallback unchanged when workspace execution is disabled.

Exit criteria that the implementation agent must validate before Phase 5:

- Unit tests with the fake executor cover claim creation, reuse, ready timeout, command success, command failure, command cancellation, artifact upload/download, release, delete, and retained workspace behavior.
- Unit tests prove worker code submits results through Orka's existing result path after sandbox command execution.
- Unit tests prove missing or failed workspace setup surfaces a clear Task condition/error rather than a panic or silent hang.
- `go test ./...` or `make test` passes with no real `agent-sandbox` installation.
- A package-boundary check, code review note, or import grep confirms `agent-sandbox` client types do not leak through general Orka API/controller packages.
- The disabled/default path still executes normal worker Jobs without constructing a workspace executor.

### Phase 5: Worker-As-Client Prototype

Goal: Implement the minimal real `agent-sandbox` path while preserving Orka's existing controller and result-submission model.

Preferred flow when workspace execution is disabled:

1. Build the existing worker Job.
2. Apply RuntimeClass and placement fields as today.
3. Collect result through the existing result path.

Preferred flow when workspace execution is enabled:

1. Resolve effective workspace configuration from Agent and Task.
2. Validate template and reuse policy.
3. Derive a reuse key if reuse is requested.
4. Create or find a `SandboxClaim`.
5. Wait for the sandbox to become ready.
6. Upload prompt/context files if needed.
7. Execute the worker command or runtime command inside the workspace.
8. Collect stdout, stderr, exit code, result payloads, and artifacts.
9. Submit the result through Orka's existing result endpoint.
10. Release, delete, retain, or expire the workspace based on policy.
11. Exit the worker Job.

Actions:

1. Implement the selected client/adapter from Phase 1 behind the Phase 4 executor interface.
2. Add worker configuration for router URL, namespace, template, warm pool policy, claim timeout, command timeout, and cleanup policy.
3. Add clear Task conditions/events for missing CRDs, missing templates, missing router endpoints, claim timeout, command timeout, and cleanup failure.
4. Add a minimal development `SandboxTemplate` and sample Task manifest for manual validation.
5. Add an e2e or scripted Kind validation path that can be promoted to CI once stable.

Exit criteria that the implementation agent must validate before Phase 6:

- In a cluster with the pinned `agent-sandbox` version installed, an Orka Task with workspace enabled creates or claims a sandbox, runs `echo ok`, submits a successful Orka result, and exits the worker Job.
- The same cluster can run a non-sandbox Orka Task successfully with the feature disabled or omitted.
- A missing-template test produces a clear failed Task condition/event and no leaked worker Jobs or sandbox resources.
- A command-timeout test terminates the sandbox command according to Orka timeout policy and reports a failed result.
- A cleanup test verifies the expected post-task state for both delete and retain policies using `kubectl get sandbox,sandboxclaim` or equivalent client output.
- The e2e/script records the names of created `SandboxTemplate`, `SandboxClaim`, `Sandbox`, worker `Job`, and relevant Task status/events.

### Phase 6: Workspace And Session Reuse

Goal: Make reuse predictable, safe, and observable.

Recommended reuse policies:

- `none`: every task gets a fresh workspace.
- `session`: tasks with the same Orka `sessionRef` reuse a workspace.
- `agent`: tasks for the same `agentRef` reuse a workspace within a namespace or tenant.
- `workspace`: tasks reuse an explicitly named workspace key.

Guidelines:

- Default to `none` for safety.
- Prefer explicit `session` reuse for the first reusable agent workspace feature.
- Avoid cross-tenant reuse.
- Include namespace, tenant, agent, runtime type, model family, template name, and workspace key in the reuse identity.
- Make cleanup deterministic with TTLs, shutdown policies, and finalizers where Orka owns the lifecycle.
- Record enough identity in status/events for operators to find and clean up retained or orphaned workspaces.

Exit criteria that the implementation agent must validate before Phase 7:

- Two workspace-enabled Tasks with the same namespace, tenant, `sessionRef`, agent identity, template, and reuse policy can observe a sentinel file created by the first Task from the second Task.
- Two Tasks with different sessions do not reuse the same claim and cannot observe each other's sentinel files.
- Two Tasks with the same session but different template, tenant, namespace, or workspace profile do not reuse the same claim.
- `reusePolicy: none` creates a fresh workspace for each Task and leaves no state visible to the next Task.
- Retain, delete, and TTL policies produce the expected Kubernetes resources after completion.
- Worker crash/retry behavior is deterministic: the retry either reattaches to the intended workspace or fails with a clear condition and cleanup behavior.
- Task status/events expose the reuse key or safe hash, claim name, template, reuse policy, and cleanup decision without exposing secrets.

### Phase 7: Security, RBAC, Secrets, And Networking

Goal: Preserve Orka's security posture while adding long-lived environments.

Actions:

1. Prefer worker-as-client for the first version so sandboxes do not need broad Kubernetes API access.
2. Keep sandbox templates secure by default. The `agent-sandbox` API defaults unspecified service account token automounting to false in `SandboxTemplate` pod specs.
3. Use namespace isolation, quotas, and per-tenant templates where possible.
4. Use default-deny network policy and only allow required traffic to the sandbox router, package registries, git hosts, model providers, and Orka result endpoints.
5. Decide how secrets enter sandboxes. Avoid injecting long-lived model credentials into reusable sandboxes unless the threat model is accepted.
6. Prefer short-lived and narrow-scoped credentials.
7. Prefer command-duration secret injection if `agent-sandbox` supports it, rather than durable credentials in retained filesystems.
8. Redact command output and logs the same way Orka redacts worker output today.
9. Enforce max runtime, idle TTL, storage limits, CPU/memory limits, and max workspace count per tenant.
10. Define cleanup behavior for failed workers, failed claims, orphaned claims, retained claims, and controller restarts.

Secrets are the hardest part of reusable workspaces. If a workspace persists, secrets can leak through:

- files
- environment dumps
- shell history
- git credentials
- npm, pip, cargo, or other package manager caches
- model provider config
- browser profiles
- logs and artifacts

Safer defaults:

- default reuse policy is `none`
- credentials are short-lived and scoped to the current tenant/session/workspace
- model provider secrets are not stored durably inside retained workspaces by default
- retained workspaces require explicit policy and clear user/operator visibility

Exit criteria that the implementation agent must validate before Phase 8:

- RBAC tests or `kubectl auth can-i` checks prove the Orka worker/controller can perform only the sandbox operations required by the selected architecture.
- A negative RBAC test proves an unprivileged sandbox workload cannot create arbitrary Pods, read unrelated Secrets, or modify unrelated `SandboxClaim` resources.
- Pod spec inspection verifies sandbox workloads do not mount Kubernetes service account tokens by default unless a template explicitly opts in.
- Admission or validation tests reject privileged containers, host networking, host PID/IPC, hostPath volumes, broad capabilities, missing resource limits, and disallowed runtime classes for approved templates.
- NetworkPolicy validation in a cluster with enforcing CNI proves default-deny behavior, blocks metadata/private/cluster service access, and allows only explicitly configured egress paths required by the test.
- Secret-handling tests prove configured secrets are redacted from Orka logs/results and are not intentionally written to retained workspace files by Orka-managed code.
- Quota/resource-limit tests prove a tenant cannot exceed the configured maximum workspace count, CPU/memory limits, or storage limits.
- Cleanup tests cover failed workers, failed claims, retained claims, expired claims, controller restart, and orphan cleanup.

### Phase 8: Warm Pools, Templates, And Operator Install

Goal: Make sandbox-backed workspaces practical to operate and fast enough for interactive agent UX.

Actions:

1. Provide operator-owned `SandboxTemplate` examples for coder, reviewer, validator, browser/computer-use, and notebook-like workloads as needed.
2. Add at least one `SandboxWarmPool` example for the first coding agent template.
3. Add Helm/Kustomize or documented install steps for the `agent-sandbox` controller, router, CRDs, templates, warm pools, RBAC, quotas, and network policies.
4. Add template versioning guidance so warm pools do not hide image/template drift.
5. Define what state may exist in a warm pool before claim assignment. Warm pools must not contain tenant code, tokens, private repositories, uncommitted patches, or user-specific browser profiles.
6. Add metrics or events for warm pool hit/miss, claim latency, ready latency, execution latency, cleanup failures, and orphan claims.

Exit criteria that the implementation agent must validate before Phase 9:

- A clean cluster install from the documented steps creates the required CRDs, controller, router, RBAC, one approved template, and one warm pool without manual patching.
- `SandboxWarmPool` reaches the expected ready replica count for the approved template.
- A workspace-enabled Orka Task can claim from the warm pool and emits an event, metric, or status field that identifies whether the warm pool was used.
- A warm-pool workspace is reset or proven clean before tenant assignment: no private repo checkout, user token, previous sentinel file, shell history, browser profile, or task artifact is present.
- Template update behavior is validated: updating the image/template either safely replenishes the pool or blocks incompatible reuse with a clear condition.
- Operator docs include install, upgrade, rollback, quota, cleanup, and troubleshooting steps.

### Phase 9: Tests, Observability, And Documentation

Goal: Make the integration maintainable and diagnosable before alpha rollout.

Tests:

- Unit test API defaulting and validation for workspace fields.
- Unit test reuse-key derivation.
- Unit test fallback to existing `JobBuilder` behavior when workspace execution is disabled.
- Unit test claim lifecycle decisions: create, reuse, delete, retain, timeout.
- Add fake `WorkspaceExecutor` or `SandboxExecutor` tests for agent runtime workers.
- Add separate tests for any future sandbox-backed `code_exec` backend before enabling that backend.
- Add envtest or integration tests only if the CRDs are vendored or installed in CI.
- Add a Kind e2e that installs `agent-sandbox`, creates a template and warm pool, runs an Orka task, verifies output, verifies reuse behavior, and verifies cleanup.

Docs:

- Add a user guide for choosing between RuntimeClass and `agent-sandbox`.
- Add examples for `runtimeClassName: gvisor` on normal Tasks and inside sandbox templates.
- Add an operator guide for installing `agent-sandbox`, templates, warm pools, router, network policy, quotas, and RBAC.
- Add a workspace guide for `none` versus `session` reuse.
- Add troubleshooting for missing CRDs, router connection failures, warm pool exhaustion, claim timeouts, retained workspaces, and orphan cleanup.

Exit criteria that the implementation agent must validate before Phase 10:

- `make manifests generate` passes when API or generated files changed.
- `make test` passes.
- The agent-sandbox e2e path passes in a documented cluster environment, or the PR clearly marks it manual with exact reproduction steps and captured successful output.
- Metrics and events are emitted for claim creation, ready latency, reuse decision, command execution, cleanup decision, cleanup failure, and warm pool hit/miss where applicable.
- Task status conditions are documented and covered by tests for success, missing CRDs, missing template, claim timeout, command timeout, execution failure, cleanup failure, and retained workspace.
- User and operator docs include at least one copy-pastable normal Job example and one copy-pastable sandbox-backed workspace example.
- Troubleshooting docs include exact commands to list Orka Tasks, worker Jobs, `SandboxClaim`, `Sandbox`, warm pools, pods, events, and relevant logs.

### Phase 10: Alpha Rollout

Goal: Ship carefully behind an explicit alpha label.

Actions:

1. Keep the feature disabled by default.
2. Mark APIs alpha and subject to change.
3. Require explicit workspace/sandbox enablement per task, agent, namespace, or installation config.
4. Emit clear Task conditions when sandbox CRDs, templates, warm pools, or router endpoints are missing.
5. Add audit events for workspace creation, reuse, deletion, retention, and failed cleanup.
6. Add admin/operator guidance for listing, retaining, deleting, and quota-limiting workspaces.
7. Define a rollback procedure that disables sandbox-backed workspaces without breaking normal Orka Tasks.

Exit criteria for alpha completion:

- Existing non-sandbox tasks behave exactly as before with the feature disabled.
- A canary run completes at least these scenarios: fresh workspace task, session-reused workspace task, missing template failure, command timeout failure, retained workspace cleanup, and deleted workspace cleanup.
- Operators can see, clean up, and limit long-lived workspaces using documented commands.
- Metrics and audit events give enough information to answer: who created the workspace, which task/session used it, which template was used, whether it was reused, and what cleanup decision was made.
- Rollback is validated by disabling the feature and proving normal Orka worker Jobs and Kubernetes `code_exec` Jobs still pass their tests without `agent-sandbox` CRDs.
- Sandbox-backed `code_exec` remains out of scope unless explicitly enabled by a separate alpha flag, separate tests, and a separate security review.

## Potential Implementation Backlog

1. Spike `agent-sandbox` install in Kind and record pinned version and CRDs.
2. Build one `SandboxTemplate` image that contains a shell, git, a language runtime, and a tiny command server if needed.
3. Verify gVisor and Kata by setting `runtimeClassName` inside the sandbox template pod spec.
4. Prototype a `WorkspaceExecutor` or `SandboxExecutor` with create, open, run, read, write, close, and disconnect methods.
5. Add an agent-runtime prototype that executes Codex, Claude, or Copilot CLI inside a sandbox image.
6. Add reuse-key policy for `sessionRef`.
7. Add status and events for workspace identity and cleanup.
8. Add RBAC, Helm/Kustomize install docs, and e2e tests.
9. Decide whether sandbox-backed `code_exec` is worth the additional threat-model complexity.
10. Decide whether sandbox-native workers are worth the additional lifecycle and result-upload complexity.

## Open Questions And Risks

- `agent-sandbox` API is still `v1alpha1`; CRD names, fields, and client APIs must be pinned and rechecked before implementation.
- The Go client currently documents Go 1.26+ as a prerequisite, while Orka currently uses Go 1.25.3.
- Command execution semantics need confirmation for long-running commands, streaming logs, cancellation, retries, and maximum output size.
- Result and artifact collection may need more than stdout/stderr if agents create files, patches, screenshots, or build outputs.
- Reused workspaces create tenant isolation risks if cleanup, secrets, filesystem ownership, or reuse keys are wrong.
- Reusable sandboxes plus secrets are risky because credentials and generated state can survive through files, caches, logs, histories, or browser profiles.
- Warm pools can hide image/template drift unless update strategy and versioning are explicit.
- Orphan cleanup must work across worker crashes, controller restarts, network partitions, and Kubernetes API failures.
- Network policy defaults may block sidecars, telemetry, package managers, git hosts, or provider APIs unless templates are explicit.
- Code execution may benefit from `agent-sandbox` later, but the existing Kubernetes Job executor remains simpler and safer for stateless snippets.

## Bottom Line

Keep Orka's Kubernetes Job path as the default execution model. RuntimeClass remains the mechanism for low-level isolation on one-shot worker Pods and Kubernetes `code_exec` Jobs.

Add `agent-sandbox` only for workloads where the environment has value beyond a single Task: durable workspaces, warm starts, interactive sessions, retained files, and reconnectable agent runtimes.

The first integration should be a feature-gated, worker-as-client prototype for session-scoped coding agent workspaces. Treat sandbox-backed `code_exec` as a later opt-in backend because it changes the current stateless security model.
