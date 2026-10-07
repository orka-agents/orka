# From equipment alert to approved work order

A pressure alert for pump-1 arrives from the plant's monitoring system
through Orka's gateway. Orka's coordinator runs a data-analysis job on AKS
over the pump's sensor history, then hands the findings to Fibey, a
specialist agent hosted in Microsoft Foundry. Fibey proposes an inspection
work order; Mark, who leads maintenance, approves that exact order before it
runs against the simulated maintenance system. The answer returns through the
gateway, and the run ends on its record: Tasks, the approval, elapsed time,
tokens, and estimated cost.

The alert, sensor history, and work-order service are synthetic; the service
is demo 11's counted simulator and changes no equipment. The analysis,
approvals, Tasks, answers, counts, and token meters come from execution.

## What runs where

| Step | Runs as | Where |
| --- | --- | --- |
| Alert in, answer out | A2A gateway adapter ([orka-gateway-a2a](https://github.com/orka-agents/orka-gateway-a2a)) bound by an Orka Gateway | AKS |
| Coordinator | Orka AI worker, Agent `demo-maintenance-coordinator`, model through Vekil | AKS |
| Sensor analysis | Container Task from `analysis/`, started by the coordinator | AKS |
| Fibey | Foundry Hosted Agent behind Orka's Foundry bridge (demo 13's `fibey-on-foundry-runtime`) | Microsoft Foundry |
| Work order | `create-work-order` Tool, approval required | AKS (simulator) |

The coordinator runs on `FIBEY_FLOW_MODEL` (`gpt-6-luna` at its default
medium reasoning effort) through Vekil; Fibey keeps demo 13's
`FIBEY_HOSTED_MODEL` (`gpt-4.1-mini`). AgentKit's Foundry loop calls tools over
Chat Completions, which `gpt-6-luna` refuses unless reasoning is off
([orka-agents/agentkit#32](https://github.com/orka-agents/agentkit/issues/32)).
The coordinator may delegate only to Fibey, and Fibey's runtime policy is demo
13's: inventory reads run, work orders wait for a person.

## Usage and cost

Orka records the coordinator's model calls, and the walkthrough checks them
against Azure Monitor's per-minute token meter for the coordinator's
deployment. External runtimes do not report token counts to Orka today, so
Fibey's tokens come from its own deployment's Azure meter. Keep other callers
off both deployments while recording. The estimate applies the Azure Retail
Prices API's Global Standard rates for each model; it is not an invoice.

## Prepare

Prepare demo 13 first (`demo/setup/fibey-hosted.sh vekil|tools|runtimes|ready`;
see its README). Demo 14 reuses its Foundry Fibey, Tools, and work-order
service. You also need a clean checkout of
[orka-gateway-a2a](https://github.com/orka-agents/orka-gateway-a2a) next to
this repository (or `DEMO_A2A_REPO`), Docker with buildx, Go, and `az` signed
in with read access to the Foundry account's metrics. Then:

```sh
demo/setup/fibey-flow.sh images    # analysis job and adapter images, A2A client
demo/setup/fibey-flow.sh install   # gateway, coordinator, Mark; patches the controller CA
demo/setup/fibey-flow.sh ready
```

`install` adds the gateway's private CA to the Orka controller and waits for
the rollout, as demo 08's setup does. That restart advances the controller
epoch, so `install` then re-renders demo 13's runtimes. Orka treats a gateway
Task's children as gateway-owned, so `install` also grants `get` on the
`fibey-alerts` Gateway to `orka-ai-worker` (the coordinator reads its
children's results) and `update` to Mark (deciding an approval on
gateway-started work). On a small cluster, scale demo 13's `fibey-on-aks` to
zero while recording; demo 14 does not use it.

## What the walkthrough checks

The gateway created the coordinator Task for this alert; the coordinator has
exactly two children, the pinned analysis image and Fibey's Task; Fibey
received the analysis verbatim. Before approval the service counts one
inventory lookup and zero work orders, and the review names `pump-1`, this
alert's run ID, and exactly `Inspect the pressure transmitter.` After Mark
approves, the service counts one work order, Fibey's Task and call are
unchanged, the coordinator's answer contains the receipt, and the gateway
returned that same answer. Orka's token record for the coordinator must match
its deployment's Azure meter exactly. Full responses, events, meters, and prices stay in
`demo/setup/state/14-fibey-alert-flow/runs/<run-id>/`.

## Checks without a model

```sh
bash -n demo/setup/fibey-flow.sh demo/14-fibey-alert-flow/demo.sh
python3 -m py_compile demo/14-fibey-alert-flow/check.py demo/setup/fibey-flow/prepare.py
python3 demo/14-fibey-alert-flow/analysis/analyze.py
```
