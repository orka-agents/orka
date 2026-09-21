# Check the supplier, keep purchasing closed

The inventory team needs 20 replacement filters. Its assistant can use a supplier
API to check stock or request an order, but the platform has configured a gateway
route only for checking stock. The story ends with three records agreeing about
what happened: Orka's tool attempt, the gateway's request log, and the supplier's
receipt.

The supplier is a real HTTP service with synthetic stock, 32 filters. Its order
endpoint can create orders. Both Tool descriptions are available to the agent,
so an attempted order must reach the gateway and receive its actual refusal.
An agent declining to try, a network failure, or a missing log does not pass.

## Prepare and rehearse

Start with the existing local demo cluster, matching Orka CLI, native AI worker,
and authenticated model Provider. Set its single scoped `KUBECONFIG`, namespace,
and API connection in `demo/setup/env.sh`. The helper also finds `bin/orka` after
`make build-cli`.

Install agentgateway before presenting. This demo uses the policy and logging
fields in agentgateway v1.3.1. The integration repository pins the Helm chart
digests and Kubernetes v1.33.7. Use that supported local cluster version rather
than assuming a newer Kubernetes version accepts those CRDs.

```sh
export AGENTGATEWAY_SOURCE=/absolute/path/to/orka-integration-agentgateway
INSTALL_AGENTGATEWAY=1 demo/setup/governed-tools.sh

# Once agentgateway is installed, ordinary preparation needs no Helm changes.
demo/setup/governed-tools.sh
demo/09-governed-tools/demo.sh
```

These commands do not record. The optional installer flag runs the integration's
`scripts/install-agentgateway.sh`, which installs shared cluster definitions and
the controller in `agentgateway-system`. Use a disposable demo cluster. The
ordinary setup adds only this demo's named resources and gateway class. It
rejects existing names without this demo's ownership label and state belonging
to another namespace incarnation. It requires kubectl, jq, Python 3, and OpenSSL;
the installer additionally needs Helm and curl.

The default model is `DEMO_AI_MODEL=claude-opus-4.7` through
`DEMO_PROVIDER_REF=copilot`. Set those values to another prepared model and
Provider with tool support if needed. The walkthrough creates two Tools and one
assistant with fresh names before its opening chapter.

Setup generates a supplier credential in a temporary private file and installs
it as a Kubernetes Secret. Only the supplier and gateway read it. No credential
is copied into Tool definitions, task prompts, logs, or displayed receipts. A
receipt reports `credentialAccepted` as a boolean.

`example.com` is the Tool's routing hostname. The gateway maps it to the local
supplier Service; the supplier request does not go to the public website.
Only `GET /v1/stock` has a route. The gateway adds the supplier credential there
and removes any incoming authorization and transaction-token headers first.
`POST /v1/orders` has no route and receives HTTP 404.

## What the run checks

The stock Task must call the stock Tool once and succeed. The supplier must
receive an authenticated request for the right item, return 32, and the agent's
answer must mention both that stock and the requested quantity of 20.

The second Task must actually call the order Tool once. Orka must record
`gateway returned HTTP 404`, and the gateway's own log must show the corresponding
POST and 404 for this run. The supplier must show no order request and zero
orders. The supplier instance identity must stay the same throughout, so a
restart cannot hide an order by clearing its receipts.

The final table is calculated from those records. Full event pages, task results,
gateway logs, safe receipts, and configuration views stay under
`demo/setup/state/09-governed-tools/runs/<run-id>/`. Run one walkthrough at a time
against this supplier. It keeps its in-memory receipts until its Pod restarts.
The script aborts if any order has reached it, including an unauthenticated one.
To investigate a failed rehearsal, keep that run's saved responses before
restarting `deployment/demo-supplier` for a new attempt.

This demo controls the two configured HTTP Tools. It does not establish network
restrictions for arbitrary programs an agent might run. Setup and source image
builds remain outside the walkthrough. No cast is included.

## Checks without a model

```sh
bash -n demo/setup/governed-tools.sh demo/09-governed-tools/demo.sh
python3 -m unittest discover -s demo/09-governed-tools -p 'test_*.py'
```

The HTTP tests exercise real authenticated stock and order requests against the
supplier. Evidence tests reject missing gateway logs, wrong-run receipts, generic
network failures, lost events, supplier restarts, and an order that reached the
backend but failed authentication. These tests use saved-response fixtures and
do not replace a full rehearsal with a real model.

Configuration references are the
[Orka integration](https://github.com/orka-agents/orka-integration-agentgateway),
[gateway access-log example](https://github.com/agentgateway/agentgateway/blob/v1.3.1/controller/pkg/agentgateway/plugins/testdata/frontendpolicy/accesslog.yaml),
and [request-log fields](https://github.com/agentgateway/agentgateway/blob/v1.3.1/schema/cel.md).
