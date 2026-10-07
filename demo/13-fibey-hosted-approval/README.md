# One approval policy, two places Fibey runs

Demo 11's pump alert, sent to two copies of Fibey at once. One runs on the
team's AKS cluster, hosted by Orka. The other is a Microsoft Foundry hosted
agent that Orka reaches through its Foundry bridge. Both use the same model,
instructions, and Tools, and Orka applies one policy to both: inventory reads
run, work orders wait for Lee, the shift lead. Each approved work order's
receipt returns to the Task that asked for it.

The incident is synthetic and the work-order service is demo 11's counted
simulator; it records receipts in SQLite and changes no equipment. The
approvals, Tasks, answers, and counts come from execution.

## What runs where

| | Fibey on AKS | Fibey in Foundry |
| --- | --- | --- |
| Orka AgentRuntime | `fibey-on-aks-runtime` | `fibey-on-foundry-runtime` |
| Agent process | AgentKit (Microsoft Agent Framework) in the runtime Pod | AgentKit brokered Responses server in a Foundry Hosted Agent |
| Orka side | supervisor | supervisor and Foundry broker |
| Model access | Orka provider proxy → Vekil → Foundry, workload identity | Hosted agent's own Foundry identity |
| Tool calls | brokered by Orka | brokered by Orka |

Both runtimes pin the AgentKit and Foundry revisions qualified by Orka's
human-approval-v2 E2E, the same Orka supervisor as the deployed controller,
and the approval policy:

```yaml
allowedTools: [create-work-order, read-inventory]
disallowedTools: []
allowBash: false
approvalRequiredTools: [create-work-order]
```

## Prepare

Install Orka from `main` with `make deploy` in `orka-system`, then fill in the
demo 13 block of `demo/setup/env.sh` (see `env.sh.example`). You need:

- A Foundry project with hosted agents enabled, an ACR its managed identity
  can pull from, and a model deployment (`FIBEY_HOSTED_MODEL`).
- A user-assigned managed identity with Foundry User on that project and
  federated credentials for `vekil-system/vekil` and
  `orka-system/fibey-hosted-foundry` on the cluster's OIDC issuer.
- A Docker host for builds with Go, Python 3.11+, and push access to
  `FIBEY_HOSTED_IMAGE_REPOSITORY` and the ACR.

Then, in order:

```sh
demo/setup/fibey-hosted.sh vekil
# On the Docker host, with the same FIBEY_HOSTED_* values and FIBEY_HOSTED_TAG:
demo/setup/fibey-hosted/build.sh adapters WORK ORKA_CHECKOUT
# Create the hosted agent version from WORK/images.json's agentkit-hosted image
# with REST (az cognitiveservices agent create cannot set the session timeout):
#   protocol responses 2.0.0, session_configuration.idle_timeout_seconds 1800,
#   AGENTKIT_FOUNDRY_BROKERED_MODEL_LOOP=1,
#   AGENTKIT_FOUNDRY_RESPONSE_STATE_TTL_SECONDS=1800,
#   AGENTKIT_FOUNDRY_BROKERED_CONTINUATION_PROOF=<the private proof>.
# Grant the new version's instance identity Foundry User on the project and account.
FIBEY_HOSTED_VERSION=<version> demo/setup/fibey-hosted/build.sh foundry WORK ORKA_CHECKOUT
demo/setup/fibey-hosted.sh tools
demo/setup/fibey-hosted.sh runtimes
demo/setup/fibey-hosted.sh ready
```

The hosted image keeps AgentKit on loopback behind `foundry_ingress.py`,
because AgentKit refuses a public bind without its own bearer and Foundry's
Entra-authenticated ingress does not send one. Brokered continuations still
require the continuation proof shared with Orka's Foundry broker.

## What the walkthrough checks

The same checks as demo 11, once per Fibey: before approval the service has
counted one inventory lookup and zero work orders; each review names `pump-1`,
its own run, and exactly `Inspect the pressure transmitter.`; after approval
the service counts one work order, the answer contains that receipt, and the
original Task and call are unchanged. The closing table shows both columns.
Full responses and event pages stay in
`demo/setup/state/13-fibey-hosted-approval/runs/<run-id>/`.

## Checks without a model

```sh
bash -n demo/setup/fibey-hosted.sh demo/13-fibey-hosted-approval/demo.sh
python3 -m py_compile demo/13-fibey-hosted-approval/check.py demo/setup/fibey-hosted/*.py
```
