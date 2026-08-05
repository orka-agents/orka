---
slug: /substrate
---

# Agent Substrate Workspaces

Agent Substrate is the planned next execution-workspace phase after the initial ACP core runtime. The current built-in `type: agent` path does not run Tasks in Substrate Actors and rejects `Task.spec.execution.workspace` rather than falling back to the removed worker-based path.

Use top-level `Task.spec.workspace` for current ACP repository input and publication policy:

```yaml
spec:
  type: agent
  workspace:
    intent: write
    gitRepo: https://github.com/example/project.git
    readCredentialRef:
      name: project-read
    publicationGitRepo: https://github.com/example/project.git
    publicationCredentialRef:
      name: project-publish
    pushBranch: orka/example-change
```

The separate Workspace/Publisher performs clone, deterministic commit preparation, exact-ref push, independent verification, and optional PR reconciliation. The ACP runtime or future Actor must not receive publication credentials.

## Planned ACP seam

The intended Substrate mapping is one Orka RuntimeSession per warm, suspendable Actor behind the same `orka.harness.v2` operations:

```text
Task attempt
  -> logical RuntimePool / trust domain
  -> RuntimeSession
  -> Substrate Actor hosting one ACP provider session
  -> validation and durable workspace delta
  -> Orka Workspace/Publisher
```

Substrate would supply physical placement, suspension, and resume. Orka would remain authoritative for:

- Task attempt state and `OutcomeUnknown` classification;
- controller epoch, pool/session generations, exact instance fences, and request digests;
- prompt leases, permission decisions, cancellation, and external-effect identities;
- canonical transcript and result finalization;
- workspace baseline, validated delta, BranchClaim, Publication, and delivery receipt;
- credential separation and prompt-scoped broker authority.

## Required properties before enablement

A production integration must prove:

1. an Actor maps to one immutable runtime instance and boot identity;
2. suspension cannot occur during a prompt, validation, publication, finalization, or Session lease release;
3. resume never silently replays an accepted prompt;
4. process-tree cleanup and private session filesystem boundaries remain verifiable;
5. provider and broker capabilities are revoked while idle, suspended, validating, or publishing;
6. source-read, target-read, target-write, and forge credentials never enter the Actor's ACP process tree;
7. a replaced or lost Actor is classified using durable Orka state rather than provider history guesses;
8. cleanup removes all Orka- and Substrate-owned resources for the test trust domain.

## Local evaluation material

The existing kind/Substrate demo assets describe an earlier prototype and are not the supported ACP v2 release path. Do not build a runtime image around the removed shared wrapper or validate success through a per-Task worker Job. Current live release validation targets the Kubernetes RuntimePool implementation; Substrate acceptance comes only after an Actor-backed v2 supervisor is implemented and passes the same conformance, crash, security, and publication gates.
