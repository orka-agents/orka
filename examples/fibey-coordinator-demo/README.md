# Fibey Coordinator Demo

Demo-ready Kubernetes/Orka manifests for the Fibey incident-coordinator scenario.
This example assumes PR #207 (`fibey-impl`) or newer: the cluster must have the
autonomous approval-gate CRDs/controller/worker code deployed.

## What the demo shows

- A scheduled scout agent checks network telemetry.
- When a critical/degraded site is found, the scout delegates to an autonomous incident agent.
- The incident agent investigates, writes an investigation dossier, updates its autonomous plan,
  and can call mock work-order/escalation tools.
- With PR #207 code/CRDs deployed, `dispatch-work-order` and
  `escalate-incident` are gated by human approval and receive an injected idempotency key.
- The mock data and tool contract mirror the Build26 Fibey Coordinator sample: rich
  Quincy North telemetry, a portfolio of active incidents, work-order dispatch,
  investigation saving, and escalation.

The action tools point at the bundled mock service by default. Do not point them at real
systems until downstream idempotency is verified and the approval policy is
intentionally configured for that integration.

## Workflow

The demo is a Kubernetes-native version of the Build26 Fibey coordinator story:
Fibey investigates a data-center incident, records context, pauses before a
consequential action, resumes after human approval, and executes the approved
mock action with an idempotency key.

### Runtime diagram

```text
                       optional live LLM path
                 +-------------------------------+
                 | Vekil / OpenAI-compatible LLM |
                 +-------------------------------+
                                ^
                                |
                                v
+----------------+       +----------------+       +----------------------+
| fibey-monitor  | ----> | fibey-scout    | ----> | delegate_task        |
| scheduled Task |       | Agent          |       | creates incident Task |
+----------------+       +----------------+       +----------------------+
                                                            |
                                                            v
+--------------------------------------------------------------------------+
| fibey-incident Agent / autonomous Task                                   |
|                                                                          |
|  1. read evidence      --> check-network-telemetry, get-active-incidents |
|  2. save dossier       --> save-investigation                            |
|  3. propose action     --> dispatch-work-order or escalate-incident      |
|  4. update plan        --> update_plan                                   |
+--------------------------------------------------------------------------+
          |                         |                         ^
          | read/write mock tools   | consequential tool call | resume
          v                         v                         |
+--------------------+       +----------------------+          |
| fibey-mocks Service|       | Orka approval gate   |----------+
| /telemetry         |       | emits approval event |
| /incidents         |       | parks the Task       |
| /investigations    |       | no Pod while pending |
| /work-order        |       +----------------------+
| /escalate          |                  |
+--------------------+                  | approve/decline
                                        v
                              +--------------------+
                              | Orka approvals API |
                              +--------------------+
                                        |
                                        v
                              Resolved approval injected
                              into next worker Job; worker
                              sends Idempotency-Key=<approvalID>
```

1. **Monitor / trigger** — `fibey-scout` can run on the `fibey-monitor` schedule,
   call `check-network-telemetry`, and delegate anomalies to `fibey-incident`.
2. **Investigate** — `fibey-incident` calls `check-network-telemetry` and
   `get-active-incidents` for Quincy North / `INC-20260524-001`.
3. **Persist context** — the agent saves a dossier with `save-investigation`.
4. **Propose action** — the agent prepares a mock field work order or escalation.
5. **Approval gate** — when `approvalRequiredTools` is enabled, the worker emits
   `ApprovalRequested` instead of executing `dispatch-work-order` or
   `escalate-incident` immediately.
6. **Park** — the controller keeps the autonomous Task `Running` and does not
   create new worker Pods while approval is pending.
7. **Decide** — a human approves or declines through the Orka approvals API.
8. **Resume** — the controller starts the next autonomous iteration with resolved
   approval context.
9. **Execute exactly once** — the worker allows the matching approved tool call,
   sends `Idempotency-Key: <approval-id>`, and the mock dedupes retries.
10. **Complete** — the agent calls `update_plan` with `goal_complete:true`.

The incident-agent prompt tells live models to use exact target tool names and
arguments for approvals. If the model changes approved arguments after resume,
Orka treats that as a different target and asks for a new approval.

## Files

| File | Purpose |
| --- | --- |
| `secret-llm.yaml` | Placeholder LLM API-key Secret. Replace the placeholder locally; do not commit real keys. |
| `provider.yaml` | `Provider` named `fibey-llm`. Adjust provider type, model, and optional proxy URL for your cluster. |
| `mocks.yaml` | In-memory mock HTTP service for telemetry, incidents, work orders, saved investigations, and escalation. |
| `tools.yaml` | Five HTTP `Tool` CRDs backed by the mock service. |
| `agents.yaml` | Scout and approval-gated incident agents. |
| `tasks.yaml` | Suspended scheduled monitor task plus one ad-hoc incident smoke task. |
| `kustomization.yaml` | Dependencies and approval-gated agents; intentionally excludes trigger tasks. |

Namespace is fixed to `orka-system` in these manifests. Change it consistently if your
cluster uses a different namespace. Kubernetes object names cannot contain underscores;
custom Tool CR names therefore use hyphens for the Build26 function tools
(`check_network_telemetry` → `check-network-telemetry`, `save_investigation` →
`save-investigation`, and so on). Built-in Orka tools such as `delegate_task`,
`request_approval`, and `update_plan` keep their underscore names.

## Prerequisites

1. Install Orka CRDs/controller/worker images from PR #207 (`fibey-impl`) or newer.
2. Replace `REPLACE_ME` in `secret-llm.yaml` locally with a real provider key, or point
   `provider.yaml` at a cluster/provider secret you already use.

For approval-gate validation, verify the CRD includes the new field:

```bash
kubectl explain agent.spec.coordination.approvalRequiredTools
```

## Apply the demo

```bash
kubectl apply -f examples/fibey-coordinator-demo/secret-llm.yaml \
  -f examples/fibey-coordinator-demo/provider.yaml
kubectl apply -f examples/fibey-coordinator-demo/mocks.yaml
kubectl -n orka-system rollout status deploy/fibey-mocks
kubectl apply -f examples/fibey-coordinator-demo/tools.yaml
kubectl apply -f examples/fibey-coordinator-demo/agents.yaml
kubectl -n orka-system wait --for=condition=Ready provider/fibey-llm --timeout=120s
kubectl apply -f examples/fibey-coordinator-demo/tasks.yaml
```

Or apply dependencies and agents with kustomize, then start the trigger tasks
after the mock backend is ready:

```bash
kubectl apply -k examples/fibey-coordinator-demo
kubectl -n orka-system rollout status deploy/fibey-mocks
kubectl -n orka-system wait --for=condition=Ready provider/fibey-llm --timeout=120s
kubectl apply -f examples/fibey-coordinator-demo/tasks.yaml
```

Verify the one-shot incident smoke first. The scheduled `fibey-monitor` task is
created with `spec.suspend: true` because the mock alert is static; leaving it
active would create duplicate child incident workflows every 10 minutes. Unsuspend
it only for a controlled proactive-scout demo, then suspend it again.

```bash
kubectl -n orka-system get provider fibey-llm
kubectl -n orka-system get svc fibey-mocks
kubectl -n orka-system get tool
kubectl -n orka-system get task fibey-incident-smoke -w

JOB=$(kubectl -n orka-system get task fibey-incident-smoke -o jsonpath='{.status.jobName}')
kubectl -n orka-system logs job/$JOB --all-containers
```


To briefly exercise the scheduled scout path:

```bash
kubectl -n orka-system patch task fibey-monitor --type merge -p '{"spec":{"suspend":false}}'
# Wait for one scheduled tick or create a separate one-shot scout Task for immediate validation.
kubectl -n orka-system patch task fibey-monitor --type merge -p '{"spec":{"suspend":true}}'
```

Tools backed by `*.svc.cluster.local` may show `Available=false` because SSRF validation
marks cluster-internal URLs unavailable; runtime dispatch is not currently gated on that
status. Expose the mock at a public/non-RFC1918 URL and update `tools.yaml` if you need
clean `Available=true` statuses.

## Approval-gated smoke task

Create a fresh smoke task name so previous plan/result state is not reused:

```bash
kubectl -n orka-system apply -f - <<'YAML'
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: fibey-incident-approval-smoke
  namespace: orka-system
spec:
  type: ai
  agentRef:
    name: fibey-incident
  prompt: |
    Incident at Quincy North: alert ALT-20260524-001 [critical]. Investigate and, if warranted,
    request approval before dispatching a transceiver swap work order.
  # Do not set spec.timeout; approval parking should not consume a task timeout.
YAML

kubectl -n orka-system get task fibey-incident-approval-smoke -w
```

Expected behavior:

1. The worker emits `ApprovalRequested` for a gated target tool.
2. The controller parks the autonomous Task with no new Job/LLM calls.
3. The approvals API returns a pending approval for the task.
4. A human approves or declines through the approval decision API.
5. The task resumes and receives resolved approval context.
6. The action call includes an injected `Idempotency-Key` header; the mock dedupes repeated keys while its Pod memory survives.

## Live LLM validation with Vekil

The default manifests are provider-neutral. To validate with the existing in-cluster
Vekil proxy, keep all Fibey tools pointed at `fibey-mocks` and only route LLM calls
through Vekil:

```bash
kubectl -n orka-system patch provider fibey-llm --type merge -p '{"spec":{"type":"openai","baseURL":"http://vekil.vekil-system.svc.cluster.local:1337/v1","defaultModel":"gpt-4.1"}}'
kubectl -n orka-system patch agent fibey-incident --type merge -p '{"spec":{"model":{"name":"gpt-4.1","temperature":0.2}}}'
kubectl -n orka-system patch agent fibey-scout --type merge -p '{"spec":{"model":{"name":"gpt-4.1","temperature":0.3}}}'
```

Then create an approval-gated live smoke task:

```bash
kubectl -n orka-system apply -f - <<'YAML'
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: fibey-live-vekil-approval
  namespace: orka-system
spec:
  type: ai
  agentRef:
    name: fibey-incident
  prompt: |
    Live Vekil LLM approval smoke using only in-cluster Fibey mock tools.
    Do not contact Teams. Do not use external action endpoints.

    Incident INC-20260524-001 at Quincy North: alert ALT-20260524-001 [critical], Rack B-14 Port 3/1/12,
    optical_power_low, Rx -18.2 dBm below -14 dBm threshold, CRC errors observed.

    Required steps:
    1. Use check-network-telemetry for Quincy North and get-active-incidents with incident_id INC-20260524-001.
    2. Save a concise dossier named live-vekil-approval.md using save-investigation.
    3. Then call dispatch-work-order exactly once with these exact arguments:
       title: Swap QSFP-DD transceiver Rack B-14 Port 3/1/12
       priority: P1
       site: Quincy North
       description: Mock work order for live Vekil approval smoke: replace likely failed QSFP-DD optic for INC-20260524-001 at Rack B-14 Port 3/1/12. In-cluster mock only; no external actions.
       assignee: Field Tech On-Site
    4. Orka may park the task for human approval before the dispatch executes. After approval, repeat the same dispatch-work-order call with exactly the same arguments and no changes.
    5. After the dispatch-work-order tool returns success, call update_plan with goal_complete:true and stop.
YAML
```

Approve through the Orka approvals API using an authenticated local client or a
cluster-local port-forward. Do not paste or print bearer tokens in shared logs.
After approval, expect the Task to resume, execute `dispatch-work-order`, and
finish `Succeeded`.

A successful run should show event order similar to:

```text
ToolCallStarted check-network-telemetry
ToolCallCompleted check-network-telemetry
ToolCallStarted get-active-incidents
ToolCallCompleted get-active-incidents
ToolCallStarted save-investigation
ToolCallCompleted save-investigation
ApprovalRequested dispatch-work-order
ApprovalApproved
ToolCallStarted dispatch-work-order
ToolCallCompleted dispatch-work-order
ToolCallStarted update_plan
ToolCallCompleted update_plan
TaskSucceeded
```

To verify downstream idempotency, call the mock work-order endpoint again with the
approved `approvalID` as the `Idempotency-Key` header; it should return
`duplicate:true` for the already-dispatched work order.

## Mock service contract

The HTTP tools POST model arguments to these mock paths; response bodies are returned
verbatim to the LLM. `idempotencyKey` is intentionally omitted from the model-visible
Tool schemas; the approval worker sends it as the `Idempotency-Key` header after human approval. `get-active-incidents` excludes `resolved` incidents by default
unless a status or exact incident ID is provided. Investigation content is capped at
12,000 characters, saved files are capped at 64 KiB including generated headers,
each mock Pod keeps at most 50 files, and the volume is a 10 MiB `emptyDir`.

```text
POST /telemetry   {site, metric}                       -> {status, telemetry, checkedAt}
POST /incidents   {site, severity, status, incident_id?} -> {status, count, incidents[], checkedAt}
POST /investigations {filename, content, incident_id?}     -> {status, path, size_bytes}
POST /work-order  {title, priority, site, description, assignee} + worker-injected Idempotency-Key header
                 -> {status, work_order:{id,status,received,createdAt}, duplicate:false|true}
POST /escalate    {incident_id, reason, escalate_to} + worker-injected Idempotency-Key header
                 -> {status, escalation:{id,status,received,createdAt}, duplicate:false|true}
```

## Persistence and cleanup

Autonomous plan state and approval event state are stored by the controller. The default
local deploy uses an `emptyDir` for `/data`, so controller restarts lose plan/approval
state. Enable persistent store storage before using this beyond a single-session demo.

Cleanup:

```bash
kubectl -n orka-system delete task fibey-monitor fibey-incident-smoke fibey-incident-approval-smoke fibey-live-vekil-approval --ignore-not-found
kubectl -n orka-system delete agent fibey-scout fibey-incident --ignore-not-found
kubectl -n orka-system delete tool check-network-telemetry get-active-incidents save-investigation dispatch-work-order escalate-incident --ignore-not-found
kubectl -n orka-system delete provider fibey-llm --ignore-not-found
kubectl -n orka-system delete deploy,svc,cm fibey-mocks --ignore-not-found
# Delete fibey-llm-secret only if it is demo-only and not reused.
```
