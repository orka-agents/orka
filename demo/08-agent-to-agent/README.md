# The order desk asks Orka for help

A customer wants 24 replacement filters today. There are 18 in stock, and the next
delivery arrives tomorrow. The order-desk application asks the inventory team's
agent for a recommendation, retries its request, and asks for a customer reply.
It then retrieves that reply after the message adapter has been replaced.

This is a standalone terminal walkthrough, aimed at roughly four minutes with
waiting compressed by the existing 100 by 28 asciinema recorder. `demo.sh` itself
does not record. It uses the shared chapter markers, narration, and typed commands.

## Prepare before presenting

Start with the demo cluster and its real Codex runtime and model connection
prepared by the existing cluster setup. The Orka gateway database must use a PVC.
Use the Orka CLI built from this checkout. Keep this step outside the walkthrough.

```bash
# demo/setup/env.sh supplies a single explicit KUBECONFIG and ORKA_NAMESPACE.
# The setup rejects contexts that are not local kind contexts.
export DEMO_A2A_REPO=/absolute/path/to/orka-gateway-a2a
bash demo/setup/agent-to-agent.sh
```

The A2A checkout must be clean. Setup builds its real `cmd/client` program into
`bin/demo-a2a/a2a-client`, builds and loads the adapter image, and records the source
revision. It requires Go, Docker, kind, kubectl, jq, Python 3, OpenSSL, and curl.
No published adapter image is assumed.

Setup creates only resources named `demo-a2a-*`, plus the `demo-a2a` Gateway, in the
selected Orka namespace. The GatewayClass is cluster scoped and includes the
namespace in its name. It prepares the inventory Agent, an adapter Service and
Deployment, gateway routing, reader permissions, a NetworkPolicy, and credentials.
The Agent uses the existing Codex runtime with no model tools enabled. The API
Service must select the specified controller.

Setup also adds the demo CA ConfigMap mount and its directory to `SSL_CERT_DIR`
on the selected controller container, preserving an existing literal directory
list. This rolls that controller during preparation and temporarily affects its
API availability. The default target is `orka-system/orka-controller-manager`,
container `manager`. Set `ORKA_NAMESPACE`, `ORKA_CONTROLLER_DEPLOYMENT`, and
`ORKA_CONTROLLER_CONTAINER` for a different local installation. The script checks
the watched namespace, persistent database, Service selector, and object identity
before patching. Its patch includes the deployment UID and resource version.

Optional preparation settings are `DEMO_A2A_MODEL`, default `gpt-5.5`,
`DEMO_A2A_RUNTIME_SECRET`, default `copilot-runtime-key`, and `DEMO_A2A_PORT`,
default `8443`. The existing runtime credential must already be valid for the
chosen model. The adapter talks to `ORKA_API_SERVICE`, default `orka-api`, on port
8080 in the same namespace.

The caller, inbound gateway, and outbound gateway credentials are distinct random
tokens. The adapter's fourth credential is its rotating ServiceAccount token.
Private token/key files stay in the ignored installation directory with restricted
permissions. TLS is verified by both the client and the controller. The generated
certificate lasts 30 days. Setup refuses a certificate with less than a day left,
foreign resource collisions, mismatched credential files, or state from another
cluster or namespace. Repeating setup reuses the existing owned objects and keys.

## Rehearse without recording

```bash
bash demo/08-agent-to-agent/demo.sh
```

The walkthrough introduces the customer request, discovers the public Agent Card,
sends the work, reads the recommendation, retries the exact request, and sends a
follow-up with a new request ID in the same conversation. Only after both Tasks
succeed does it restart `deployment/demo-a2a-adapter` and retrieve the reply again.
It reopens its own port-forward after the replacement.

The shell's `a2a-client` function supplies the prepared URL, caller-token file, and
CA file to the actual A2A client binary. The visible request and task-ID arguments
go directly to that program. `wait_task` is the existing demo helper. Other local
helpers collect real records and verify their relationships.

Each run uses new message and conversation IDs. It saves complete JSON responses,
all polled gateway events, Task snapshots, and adapter Pod identities under
`demo/setup/state/08-agent-to-agent/runs/<run-id>/raw/`. It writes a calculated
`evidence.json` alongside them only when all final checks pass. Prior runs remain
available; this script does not delete their Tasks or Sessions.

The checks require the retry to return the original public reference and Task UID,
exactly two Tasks in one Session, stock quantities and tomorrow's delivery in the
actual answers, matching A2A and Orka result text, a different ready adapter Pod,
and no additional Gateway Tasks after replacement and retrieval. Run one
walkthrough against this adapter at a time. Any missing evidence stops the story.

## Scope and verification

The scenario facts in `order.txt` are demonstration data. All answers, public
references, Tasks, Sessions, and restart evidence come from execution. The demo
shows recovery of access to retained completed results. It does not show recovery
of interrupted execution or indefinite result retention.

The adapter implements a text-only subset of A2A 1.0. One caller token gives
access to this adapter's configured caller domain. This is not a demonstration of
individual user authorization or full A2A support. See the adapter's
[protocol](https://github.com/orka-agents/orka-gateway-a2a/blob/main/docs/protocol.md)
and [compatibility notes](https://github.com/orka-agents/orka-gateway-a2a/blob/main/docs/compatibility.md).
This demo uses a runtime-backed Agent to avoid assuming the separate native-AI
gateway prerequisites are installed.

Offline checks cover failed correlations, duplicate work, missing conversation
facts, and the controller CA patch boundary:

```bash
bash -n demo/setup/agent-to-agent.sh demo/08-agent-to-agent/demo.sh
python3 -m unittest discover -s demo/08-agent-to-agent -p 'test_*.py'
```

These checks do not establish a successful live rehearsal. No recording is
included. Use the repository's normal recording wrapper only when ready to record.
