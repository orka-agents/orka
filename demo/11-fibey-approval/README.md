# Fibey investigates; a person approves the work

A pressure reading drops after maintenance at the fictional Quincy North site.
Fibey, the team's operations assistant, checks inventory and proposes an
inspection of the pressure transmitter. Orka keeps that work order waiting
until the presenter, acting as the shift lead, approves it. The receipt then
returns to Fibey in the original Task.

This differs from demo 09. There, the gateway keeps purchasing closed. Here,
the platform permits an action after a person's decision. It also adds tools
and review to the earlier `examples/fibey-custom-agent-demo`, which only
analyzes an incident in text.

The incident is synthetic. The tool service is a real HTTP server that records
each invocation in SQLite. Its work orders are simulated receipts; it does
not contact a maintenance system or change equipment. The approval, Task,
agent response, and counts shown by the walkthrough come from execution.

## Prepare Fibey

Use a dedicated local demo namespace watched by a controller with
[#589](https://github.com/orka-agents/orka/pull/589), its updated CRDs, and
persistent execution-event storage. Set `KUBECONFIG`, `ORKA_NAMESPACE`, and the
API connection in `demo/setup/env.sh`. Build the matching CLI and prepare the
usual `orka-client` ServiceAccount with Task read/create permissions.

Prepare an external AgentKit runtime using the approval example in that
checkout. The example's README covers the builds, registration, profile
generation, and acceptance checks. Use this demo's
[`agentkitfile.yaml.example`](agentkitfile.yaml.example) for Fibey's instructions
in place of the example's generic assistant. Build the Microsoft Agent Framework
adapter and AgentKit frontend with approval support and pin their image digests.
Model access and credentials belong in this preparation, outside the walkthrough.

Generate the profile from the exact configuration baked into the new image.
Its policy must be:

```yaml
allowedTools: [create-work-order, read-inventory]
disallowedTools: []
allowBash: false
approvalRequiredTools: [create-work-order]
```

The original read-only Fibey profile cannot receive these permissions as Task
overrides. Use a new profile and registration with `workspaceIntent: read`.
The namespace must have `orka.ai/controller-mode: harness-v2`, and the
AgentRuntime must pass conformance for its current configuration.

Approval support requires qualification of the exact adapter, model, and saved
configuration. Follow the example's counted acceptance checks and explicit
`ORKA_ACP_BROKERED_TOOL_APPROVAL_PROFILE_DIGEST` opt-in. Review can wait at most
600 seconds, the enclosing tool call allows 900 seconds, and this Task allows
20 minutes. The walkthrough demonstrates one approval; it does not replace
the runtime's decline, expiry, cancellation, and recovery checks.

An already qualified Foundry deployment with Fibey's instructions and the same
policy can also be selected. Follow #589's hosted preparation and the companion
Foundry runtime instructions, including retained response state and continuation
support. A local hosted test does not establish Azure gateway compatibility.

## Prepare the tools and rehearse

Once the runtime is ready, set these in `demo/setup/env.sh`:

```sh
export DEMO_APPROVAL_SOURCE="$HOME/projects/orka.human-approval-v2"
export DEMO_FIBEY_RUNTIME=fibey-approval-agentkit-runtime
```

`DEMO_APPROVAL_SOURCE` defaults to this repository. Until it contains #589, use
the sibling checkout above. Setup reads the simulator and Tool manifests from
that checkout's committed HEAD, saves the revision, and leaves its working
files untouched. Runtime builds must include the same approval contract.

```sh
demo/setup/fibey-approval.sh
demo/11-fibey-approval/demo.sh
```

Neither command records. Setup needs Git, kubectl, jq, and Python 3. The
walkthrough also needs curl and the matching Orka CLI. Run it through the
worktree's `kindctl exec` when using a kindctl-managed cluster.

Setup installs the counted service with persistent storage, the two named
Tools, `Agent/demo-fibey`, and a Role granting `orka-client` permission to decide
Task approvals. The decision API also requires permission to patch the Task;
the Role includes it. Setup refuses existing resource names without demo 11's ownership
label. In particular, it must not replace another installation's
`read-inventory` or `create-work-order` Tools. It does not provision the runtime
or change an existing runtime's profile.

The walkthrough checks the saved installation before submitting work. Run
setup again after intentionally changing that configuration. Every walkthrough
uses a fresh Task name and retains earlier Tasks and receipts. Keep the SQLite
volume when investigating a failed run; deleting it would erase the evidence.

## What the walkthrough checks

The six chapters introduce the incident, explain the two tool permissions,
submit the Task, inspect the waiting action and zero-order count, approve the
inspection, and read Fibey's reply. New terms are explained when
they first appear. Preparation and image builds stay outside these chapters.

Before approval, the service must have counted one inventory lookup and zero
work orders. The pending review must name `pump-1`, the current run, and exactly
`Inspect the pressure transmitter.` The review must still be pending on the
same Task attempt and waiting call immediately before the decision.

After approval, Orka must record the presenter's decision and successful tool
execution. The service must count exactly one work order, and Fibey's answer
must confirm its creation and contain its actual receipt ID. The expected
presenter comes from `ORKA_CLIENT_SA`, saved before the request. The original Task and call must remain
the same. Missing event pages, changed configuration or receipt storage, early
execution, duplicate execution, and a generic answer without the receipt stop
the script before its closing narration.

The final table is calculated from these records. Complete responses and all
event pages stay in `demo/setup/state/11-fibey-approval/runs/<run-id>/`.
The walkthrough is intended for the existing 100x28 recorder with compressed
waiting. Pacing still needs a full rehearsal against the prepared model.
No recording is included.

## Checks without a model

```sh
bash -n demo/setup/fibey-approval.sh demo/11-fibey-approval/demo.sh
python3 -m unittest discover -s demo/11-fibey-approval -p 'test_*.py'
```

The evidence tests use API-shaped fixtures and counterexamples. The reused
service has its own real HTTP and persistence tests in
`examples/human-approval-v2/test_simulated_tools.py` in the source checkout.
These checks do not stand in for a real-model rehearsal or hosted qualification.
