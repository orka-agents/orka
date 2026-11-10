---
slug: /report-remediation
description: "Reproduce a supplied report, propose one fix, and independently validate the candidate."
---

# Report-driven remediation

Report remediation uses an existing Orka installation and an operator-approved
native AI agent and execution environment. ACP agent support is not part of this
initial workflow. It does not provision cloud resources, publish
a branch, or open a pull request for the generated fix.

The workflow has three stages:

1. An agent prepares a reproduction. Trusted execution must demonstrate the
   reported problem on the recorded original source before fix generation.
2. An agent proposes one candidate patch.
3. A separate agent task assesses the candidate using original/candidate
   execution results and normal-behavior checks.

A successful agent task alone does not mean that the issue was reproduced or
fixed. Missing tools, unsupported environments, invalid checks, and incomplete
execution produce an inconclusive result. If the candidate fails validation,
the run stops and returns the candidate as unverified with feedback; it does
not automatically generate another candidate.

The MVP accepts only report validation and generation of one candidate.
Caller-supplied patches and remote ACP proposal clients are outside this scope.
Source-level findings do not establish unexecuted cluster, authorization, or
network behavior; missing required runtime evidence remains inconclusive.

## Evidence scopes

The default evidence scope is **`source-harness`**: independently observed checks
against the selected source inside the validation harness. A `Verified` outcome
means the frozen reproduction and normal-behavior checks passed for the candidate
in that scope. Running the harness in Kubernetes does not qualify the application's
cluster, authorization, deployment, or network behavior.

**`application-runtime`** is an explicit strict requirement for evidence from the
running application environment, not a source-level substitute. It requires the
separately qualified, opt-in application backend described below. The default
source-harness backend rejects requests requiring it with HTTP **422** before
run admission, model Tasks, or executor workloads. Neither `generate` nor
`validate` silently falls back to a different evidence scope.

The backend selects the actual scope; report text, a model's coverage `scope`,
and agent verdicts cannot grant it. Scope is frozen with the run's authority and
bound into execution metadata and its digests. The status field `evidenceScope`
identifies that boundary even while a run is pending; it is not, on its own,
proof that any check ran. Normal Task result summaries and patch-download labels
also state the evidence level.

Older retained runs without scope metadata remain unscoped, preserve their exact
identities and digests, and are never promoted to application-runtime evidence.
Their status omits `evidenceScope`; newly completed legacy results and download
labels say the scope is unrecorded. Already published Task results are not
rewritten.

## Operator configuration

The feature is disabled by default. Enable it with `--remediation-enabled` and
select the configured native Agent with `--remediation-agent`. The Agent must
not grant tools, coordination, fallback models, or ambient memory to proposal
Tasks. Remediation uses separate Tasks for each role, not a shared conversation.
`Agent.spec.soul` must be unset: persona injection is outside the qualified
remediation role prompt and is rejected.

Use the current native AI worker image with memory opt-out support, the
general-worker image for source preparation, and the validation helper image
for independent observation. These, and the selected tool image, must be
referenced by digest. A pinned digest identifies an image; it does not prove
that an older image implements the required worker behavior.
The tool image must contain the dependencies
needed by the checks; checks do not install dependencies from the network.

Build the helper with `make docker-build-validation-helper`. Configure its
published digest with `--validation-helper-image`, and configure the tool image
and observed image identity with `--validation-tool-image` and
`--validation-tool-image-id`. Also supply the observed CRI image IDs for the
general worker and helper with `--validation-preparation-image-id` and
`--validation-helper-image-id`. A container runtime may report an image config
digest rather than the registry manifest digest; these are different identities
and must not be substituted for one another. The standard controller image needs no Git or
`prlimit` installation. Source preparation happens in the worker.
The general-worker image includes a native limit/exec helper for Git. It applies
the existing hard limits after replacing the Go process, so Go's argument and
environment allocation cannot fail under Git's 512 MiB address-space limit.
There is no fallback when the helper is missing or cannot install the limits.

The initial execution profile supports independently observed HTTP checks
against source in the validation container, not arbitrary cluster or external
service scenarios. Configure `--validation-network-canary` to a reachable
literal IP and TCP port outside the check container. An enforcing NetworkPolicy
implementation must block that connection from validation workloads.

Qualified workers must expose cgroup-v2 memory, CPU, PID, and swap limits to the
container. Source preparation requires at most 512 MiB memory, one CPU, 512
processes, and zero swap, plus the worker's native per-process limits. An
unobservable inherited limit is not assumed to be enforced. Use
`--validation-node-selector` to select qualified nodes; its default is
`remediation.orka.ai/qualified=true`. Apply that node label only after validating
the required limits and network isolation. A runtime that cannot
prove these boundaries returns an unsupported-environment result before
executing subject code.

### Application-runtime backend

Select `--remediation-backend=application-runtime` together with
`--remediation-enabled` and the native `--remediation-agent`. The default remains
`--remediation-backend=source-harness`; its HTTP checks and legacy retained
evidence are unchanged. Application-specific settings without explicit backend
selection are rejected. A runtime-only instance rejects omitted or
`source-harness` requirements rather than relabelling them.

The application backend requires a **different, dedicated target cluster**.
The controller retains authority and receipts in Orka; target resources have
their own exact cluster, namespace, Pod, Job, and container identities. Do not
give the application, model, or target observer Orka's kubeconfig.

The Orka controller identity must be able to `get` the control cluster's
`kube-system` Namespace to verify its UID. A namespace-scoped installation needs
an explicit cluster-scoped grant restricted to that resource name; it does not
need permission to read other namespaces. Target credentials must separately
authorize the backend's resource lifecycle and the exact permissions it grants.

Retire unused probe namespaces before enabling dispatch. Beyond `kube-system`
and `local-path-storage`, only the empty standard namespaces and exact run-owned
namespaces are accepted. Default public API and service-account issuer discovery
grants, including their trailing-slash paths, are allowed; unrelated additive
workload privileges remain an isolation failure.

Configure these operator-owned inputs:

| Flags | Required purpose |
| --- | --- |
| `--remediation-application-kubeconfig`, `--remediation-application-context` | Separate kubeconfig and explicit context; no global-context fallback, exec/auth plugins, impersonation, insecure TLS, or proxy URL |
| `--remediation-application-control-cluster-uid`, `--remediation-application-target-cluster-uid` | Distinct expected `kube-system` Namespace UIDs, revalidated against live APIs |
| `--remediation-application-worker-image`, `--remediation-application-worker-image-id` | Digest-pinned trusted worker and independently resolved CRI image ID |
| `--remediation-application-toolchain-image`, `--remediation-application-toolchain-image-id`, `--remediation-application-go-version` | Pinned offline compiler image/identity and exact compiler version |
| `--remediation-application-runtime-image`, `--remediation-application-runtime-image-id` | Optional approved runtime image pair; otherwise the trusted worker image is used |
| `--remediation-application-platform`, `--remediation-application-storage-class` | Target architecture and isolated artifact storage |
| `--remediation-application-node-selector`, `--remediation-application-network-canary` | Dedicated qualified node selector and reachable literal canary `IP:port` |
| `--remediation-application-policy-file` | Optional strict JSON operator profile (at most 32 KiB) admitting resource GVRs/CRD identities, subject privileges, actor identities, synthetic Secret recipes, and pinned fixture catalogue entries |
| `--remediation-application-runtime-class` | Optional target RuntimeClass, bound to its exact observed UID; selecting a class does not replace measured isolation |
| `--remediation-application-api-server` | Optional worker-reachable target HTTPS literal endpoint, distinct from the controller-side kubeconfig endpoint when necessary |
| `--remediation-application-source-endpoint` | Repeatable `github.com=IP` or `api.github.com=IP`; `github.com` is required, only literal IPs on HTTPS port 443 are admitted |
| `--remediation-application-runtime-qualification-file` | Optional strict operator-qualified runtime metadata (at most 16 KiB); omission preserves the native-v2 path, never infers an exemption from RuntimeClass names |

Build the trusted worker with `make docker-build-application-worker`. The target
API endpoint must use verified TLS with an explicit CA. Worker/API policy uses a
directly reachable HTTPS literal IP; configure the worker endpoint separately
when the controller's kubeconfig endpoint is not reachable inside target Pods.
Image references and observed image IDs are different
authorities and must not be substituted for each other. The expected image ID
must match the target Pod's `status.containerStatuses[].imageID`, which may be a
runtime manifest/index reference rather than the image-config ID returned by an
image inspection command. Verify the runtime reference resolves to the admitted
platform manifest and image configuration.

The qualified target must ignore implicit image-defined volumes, for example
with containerd's `ignore_image_defined_volumes = true`. Otherwise a Dockerfile
`VOLUME` can add an unrequested writable node-backed path outside the Pod's
explicit bounded volumes. Include this setting and actual mount verification in
the operator qualification evidence.

Source preparation receives only the configured GitHub host aliases and
destination-IP/443 egress rules. There is no implicit public-host DNS fallback
or broad Internet rule. Source TLS qualification must validate the configured
hostname/SNI and certificate with a live HTTP response, not merely an open TCP
relay. These source endpoints do not replace the target API endpoint, and the
subject, actors, and fixtures do not inherit source-preparation egress.

Static configuration is insufficient: startup readiness and each execution
must establish the backend's measured isolation boundary, positive connectivity
control, denied subject egress, and restricted target API identity/permissions.
Qualification failure leaves runtime admission unavailable. A node label or
NetworkPolicy object alone is not isolation evidence.

Qualification starts under elected controller ownership after the storage
dataset accepts that controller's epoch. Until it completes, new submissions
return unavailable rather than bypassing storage fencing or claiming runtime
evidence. Fresh source and artifact PVCs may retain a real `lost+found` directory;
stale application files, foreign entries, and symlinks remain rejected.

An explicitly qualified `gvisor-bounded-v1` profile is not a blanket sandbox
waiver. It must bind the operator qualification digest and exact target
cluster/node/runtime identities, retain Pod/container/image/ServiceAccount
fences and the required UID/PID/capability/NNP/read-only/seccomp checks, and use
actual non-writable guest cgroup-v1 CPU/memory readback plus the inherited,
non-raiseable hard/soft guest process limit. Virtual unlimited `pids.max` is not
PID-limit evidence. Native ancestor/node no-swap qualification must be labelled
as that source, never invented as a guest measurement. Drift or node loss fails
closed.

Qualified-profile writable work/runner/scratch storage is memory-backed within
the existing finite Pod memory limit. Qualification `nodeMemoryBytes` is the
native node **memory** cap, never a node disk quota. `aggregatePVCBytes` and
`aggregateLogBytes` remain separate aggregate claims with
`aggregate-only-no-per-pvc-guarantee` and
`ephemeral-node-loss-fail-closed` attribution; neither implies an individual
volume's hard disk quota. The installed runtime executable digest, base-spec
digest (`runtimeBaseSpecDigest`) and options digest (`runtimeOptionsDigest`) are
bound separately, without an ambiguous combined configuration hash.
`runtimeHandler` must exactly match the qualified handler; it is not inferred
from a runtime family name. Memory-backed EmptyDirs alone do not qualify every
writable mount: kubelet/runtime-injected termination files and other writable
paths require their own demonstrated backing/bounds. An unbounded writable
termination file remains a readiness blocker. The generic closure is a measured,
inherited, non-raiseable hard **and** soft `RLIMIT_FSIZE`, established by the
trusted worker before consuming untrusted input or executing code. Fixed
prepare/build/installer roles allow at most 512 MiB; subject, fixture (including
replica), observer/actor, boundary and marker roles allow at most 16 MiB.
These are trusted role limits, not model parameters or a caller waiver.
Child launchers must preserve tighter inherited hard limits rather than attempt
to raise them. The termination file remains host-backed and writable but
file-size-bounded; it is not described as tmpfs or read-only. Work/PVC/log
aggregate limits remain separately required. Missing
qualification cannot be replaced by a runtime-name check, fabricated files,
silent native-runtime fallback, or a new privileged inspection service.

After accepting the measured qualification—not before—the operator's Node
configuration must bind these non-secret drift fences:

| Node annotation | Qualification field |
| --- | --- |
| `applicationexec.orka.ai/runtime-qualification` | `qualificationDigest` |
| `applicationexec.orka.ai/runtime-binary` | `runtimeBinaryDigest` |
| `applicationexec.orka.ai/runtime-base-spec` | `runtimeBaseSpecDigest` |
| `applicationexec.orka.ai/runtime-options` | `runtimeOptionsDigest` |

The exact RuntimeClass also requires
`applicationexec.orka.ai/runtime-qualification=qualificationDigest`; its UID and
handler must match the qualification. These annotations detect configuration
drift; they do not measure isolation or authorize admission by themselves.
Backend-generated `/input/runtime.json` carries the qualification and exact
rendered **role** limits, including lower observer/fixture budgets—not a forced
2 GiB/2 CPU allocation for every role. No operator/model cap overrides are
accepted through the proposal.

The first application profile builds one Go main package from the **complete
immutable upstream source tree**, offline and with CGO disabled. It does not
require an application image registry or a Docker build daemon. The model still
receives a bounded source packet; the build context is never silently reduced to
that packet. Unsupported or oversized full source fails closed.
Application preparation admits at most 256 MiB of source content, 320 MiB per
canonical archive, and 32,768 entries. Archives stay in isolated target PVCs;
the existing private record store receives bounded immutable provenance
metadata, patch/diff, and digests, not expanded archives. Legacy source-harness
limits are unchanged.

The optional operator profile is runtime data, not a model-granted policy or
per-project adapter. Custom resources require preinstalled, approved CRDs;
the complete CRD spec digest, actual scope, and live CRD UID are frozen and
revalidated. The controller does not install model-proposed CRDs. Subject
permissions are explicit subsets of the operator profile. Actor policies and
fixture image catalogue entries are likewise operator-owned. Keep actual
profiles outside public source and never put credentials or Secret values in
them. They are read once at startup, frozen with backend authority, and cannot
be reloaded into an active run.

A resource rule must explicitly declare a non-null `namespaced` boolean.
`namespaced:false` permits only subject `get/list/watch` on the exact
operator-pinned **custom** GVR, with `status:false`. These grants use separate
subject-only ClusterRole/ClusterRoleBinding identities with exact-UID cleanup
and additive-grant checks. A bounded metadata UID/resourceVersion read set
protects original/candidate parity without transporting cluster object contents.
This does not admit cluster built-ins such as Secrets, Nodes or RBAC; wildcard
authority, cluster fixtures, actor actions, observation predicates, creation,
mutation, deletion, status/finalizer writes, and ownership operations remain
unsupported. A controller that actually requires those writes cannot be
qualified by silently replacing or simulating them.

`subjectEnvironment` is an operator-owned map with at most eight variable names
(at most 64 characters each). The **only** supported value is the exact literal
template `{{namespace}}`, substituted with each side's trusted run namespace.
Reserved identity, worker, Kubernetes, runtime/loader, credential, proxy and CA
controls cannot be overridden. There are no arbitrary literals, model environment
maps, `EnvFrom`, `ValueFrom`, or wider PodSpec permissions. The same frozen policy
is used on both sides; a namespace-watching application does not need a
project-specific code adapter.

Operator `subjectTLS` may declare at most two bounded bundle IDs and optional
non-wildcard lab DNS names. The backend generates fresh synthetic CA/server
material, never production credentials; the CA private key is not retained.
Only `ca.crt`, `tls.crt`, and `tls.key` are mounted read-only under the fixed
`/subject-config/<ID>` directory. Exact immutable Secret UID and bundle digest,
read-only mount, and certificate/key validity must bind before execution and
cleanup. Arguments may select that directory; models cannot provide bundles,
values, Secret references, host volumes, or mounts. Both sides use the same
frozen declaration. The standard projected serviceaccount namespace file or
approved namespace-template environment supplies application namespace context.

An operator service-fixture catalogue may select up to four `secretFiles`
entries `{id, secretName, key}` from this plan's **declared synthetic** Secret
keys. Only those generated values may be copied into immutable protected fixture
storage and exposed read-only at `/fixture-secrets/<ID>`, with exact UID/digest
checks. No arbitrary Secret namespace/reference, value, environment expansion,
or general volume surface is available. Fixed catalogue argv may read the files;
replica fixtures cannot request configuration or Secret-file inputs.

The native preparation role proposes a frozen `application` plan alongside
declarative reproduction and normal checks. Package selection, arguments,
manifests, narrow permissions, and marker fixtures are run data, not per-project
adapters. Operator-owned compiler/runtime identities cannot be changed by the
proposal. Supported observations are named target API `/status` values and
protected TCP marker observations. Fixture-declared `/spec` values are not proof.
Every API reproduction requires a normal control for the same API resource and
status pointer. Non-replica normal API checks declare `normalValue`, the exact
public healthy value matching their canonical healthy expectation. Null,
whitespace, empty and vacuous nested values cannot establish normal behavior;
genuine non-initial `false` and `0` values are allowed.

The target must be a real owned plan/check fixture or a matching successful actor
create/update, with admitted subject update/patch authority. The baseline comes
from the authoritative fixture Create response or latest matching successful
actor response—not a later GET. Healthy evidence must retain the same UID and
show both a changed resourceVersion and changed reduced status value. Missing
targets cannot stand in for a positive normal control. Replica descendant and
TCP-positive requirements remain separate.

TCP checks support both protected marker reads and a run-owned
receiver observing **subject-originated** traffic. Receive windows begin before
test actions, bind the exact live sender identity, and require a matched positive
normal control at the same receiver/port/marker. Failed setup, truncated frames,
and incomplete windows are not absence proof. Plaintext EOF or bounded explicit
frame termination is supported, not arbitrary TLS protocol plugins.
Zero connections can establish a negative receive observation only after a
complete, data-plane-qualified window with actor stimulus and the matched
positive normal control.

Tests may declare synthetic Secret names/keys, but only the trusted backend
generates their fresh protected values. The model receives references and safe
metadata only. Subject Secret informer access is limited to the isolated
candidate namespace; protected marker copies and observer/actor credentials are
in a separate owned namespace inaccessible to the subject.

Bounded `create`/`update` actions use explicit actor identities from the operator
profile. They use actual actor tokens, not observer impersonation or privileged
fallback, and record actual HTTP authorization outcomes and exact resource
identities. Create names and existing update UID/resourceVersion are fenced
before dispatch. Unknown mutation outcomes cannot be replayed as new work or
treated as successful evidence. Actor receipts must accompany independent
status/receiver evidence and real normal reconciliation.
A 403 is authorization evidence only when the actual actor's exact
SelfSubjectAccessReview independently denies the action; admission or quota
rejections cannot substitute for authorization proof.

API expectations are proposed as
exact values and frozen into canonical value digests by trusted code; models
need not calculate hashes. Runtime evidence does not expose arbitrary API values
or marker contents. Executable check scripts, Dockerfiles, arbitrary fixture
images, caller-supplied credentials or Secret values, model-created CRDs,
cluster-scoped mutation or unadmitted cluster access, and publication are unsupported.
Declared gaps in required runtime behavior stop validation rather than produce
partial evidence presented as a verified report.

The original must reproduce the report **and** establish normal behavior before
one candidate can be generated. The same frozen application and checks run on
the original and candidate. An independent native review receives the recorded
observations, not the generator's conversation. One check revision may repair or
add probes, but must preserve coverage, check identities/kinds, setup resources,
actor actions, the complete application plan, and operator policy. Additional
probes cannot add resource mutations or authority. It reruns both sides without
changing the candidate.

Replica/scaling uses only the bounded operator catalogue `replicaTarget`
`{name, initialReplicas, maxReplicas}`. At most one target is admitted, with
`maxReplicas` between one and four and an operator-fixed initial count between
zero and that maximum. A plan selects `kind: replica`, the catalogue profile and
its exact target name; it cannot change the image, argv, template, security,
runtime class or counts. Non-network replica targets may omit ports.

Subject permissions are Deployment reads and exact-name `deployments/scale`
get/patch/update, never full Deployment or Pod/template mutation. HPA
get/list/watch/create/update requires built-in target admission with
`FailurePolicy: Fail` and `Deny` bindings enforcing the same owned namespace,
exact fixture Deployment reference and finite replica limits. The backend must
prove allowed and forbidden dry-run operations before granting those privileges;
policy objects or proposal validation alone are insufficient. Namespace
Deployment/ReplicaSet/Pod/HPA and aggregate resource quotas remain additional
bounds. Exact Deployment→ReplicaSet→Pod UID/template lifecycle and cleanup must
hold; admission must not be removed while an authorized workload can still act.

Normal zero-to-one activation requires actual actor stimulus, subject
reconciliation and real changed replica/ready-replica observations, not an
actor/model-created HPA or fabricated status. The positive, non-initial ready
transition is required only for a healthy normal result. Failed normal results
still require exact Deployment/descendant identities and measured count/output
agreement, so an unchanged or partially ready count can be compared against the
frozen failure expectation rather than discarded as invalid evidence.
Full one-to-many autoscaling and
metrics-adapter behavior are outside that minimal control claim and require
their real operator-provided prerequisites. No general `apps`/`batch`,
cross-namespace target, arbitrary model PodSpec, Jobs or template mutation is
admitted. Missing admission, metrics, lifecycle or runtime qualification leaves
the requested behavior unsupported rather than permitting a surrogate.

### Native run and model-call limits

Application runs have one durable active-run slot, one candidate, and a
90-minute deadline beginning at submission. Queued time counts toward the
deadline. A restart, expired deadline, or missing acknowledgement does not release
the active slot; the exact prior run must settle before another starts.
The same absolute deadline is carried into mechanical backend recovery and the
native worker's request context; restarting either component cannot reset it.
Cancellation revokes the exact parent before backend recovery can continue.
Cleanup may outlive the deadline but cannot authorize another experiment.

If the coordinator loses a receipt acknowledgement, it recovers only an exact
UID and input digest already recorded by the backend. A reservation proven never
dispatched may settle without execution. A matching workload name, a deadline,
or an unknown create outcome never authorizes replacement work or slot release.

Each native role has one non-replayable **outbound model HTTP-attempt permit**.
The worker consumes it through a Pod/Job/Task-bound authenticated internal API
before sending. Consumption is persisted with the run, so a replacement Pod,
worker restart, or repeated request cannot reset it. At most ten permits may be
allocated per run; today's workflow uses three roles for validation, five for
generation, or six with a check revision. A permit may be consumed without a
successful model call; private Task results report permits consumed, not a
fabricated count of successful inferences.
This implements a maximum-ten **logical Orka model-invocation** budget with a
conservative one-native-request-per-invocation policy, not merely a stage counter.

Budgeted workers disable SDK/outer retries, redirect following, connection
replay, context-overflow retries, blank-final retries, tool loops, memory,
sessions, skills, fallback models, and ambient HTTP proxies. Compatibility API
fallback cannot send a second request: the approved endpoint must support the
provider's initial API surface.

**Gateway-internal upstream attempts are outside this logical budget.**
A gateway may retry or fan out one authorized request internally; operators must
explicitly authorize the concrete model route with that distinction understood.
If a deployment instead requires a hard physical-upstream-attempt cap, it needs
a qualified no-retry route or durable gateway-side accounting bound to the run
and role, charging each upstream retry before sending it. Native Task counts or
the permit ledger alone must not be presented as that stronger guarantee.

Keep the feature's namespace private to authorized report operators. Before
disabling the feature or changing its configured Agent/provider, worker/helper/tool
images, or validation backend settings, stop new submissions and settle active
runs, or cancel them and wait for cleanup under the original configuration.
Accepted runs freeze that authority; restarting with different authority fails
closed even when the SQLite database layout is compatible. Do not abandon active
work by replacing its images or disabling admission before cleanup completes.

Grant callers only the operations they need on the namespace-scoped virtual
resources. For example, this Role permits submission, status, cancellation,
and candidate download; bind it to the intended caller with a RoleBinding.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: report-remediation-caller
  namespace: orka-system
rules:
  - apiGroups: ["core.orka.ai"]
    resources: ["remediations"]
    verbs: ["create", "get"]
  - apiGroups: ["core.orka.ai"]
    resources: ["remediations/cancel"]
    verbs: ["update"]
  - apiGroups: ["core.orka.ai"]
    resources: ["remediations/patch"]
    verbs: ["get"]
  - apiGroups: ["core.orka.ai"]
    resources: ["tasks"]
    verbs: ["get"]
```

The `tasks/get` permission is used for normal agent Task results. It does not
grant permission to create arbitrary Tasks or publish source changes.

## Submit a report

Keep input files outside source checkouts and use a namespace whose access
policy is appropriate for the report. Normalization minimizes input; it is not
permission to disclose confidential content to the configured model provider.

```json
{
  "title": "Reject an invalid quantity",
  "problem": "The quantity endpoint accepts a negative quantity; it should reject it while accepting positive quantities.",
  "repository": "https://github.com/example/project",
  "ref": "v1.2.3",
  "restricted": false
}
```

Supply a specific affected revision. Orka records the exact resolved commit;
it does not substitute a newer version when the supplied version is unavailable.

```bash
orka --server "$ORKA_URL" --namespace "$ORKA_NAMESPACE" \
  remediate start --input /private/report.json --wait
```

For an explicitly qualified application-runtime instance:

```bash
orka --server "$ORKA_URL" --namespace "$ORKA_NAMESPACE" \
  remediate start --input /private/report.json \
  --required-evidence-scope application-runtime --wait --timeout 90m
```

The CLI reports the request identity before submitting. Retain it if the
connection is lost. Retry only the identical request with the same request ID;
a new ID is a new run, not a recovery mechanism.

With `--wait`, progress goes to stderr and the final parent Task result uses
Orka's ordinary agent-result output on stdout, including feedback for failed
verification. Status includes the parent Task name and UID, so the usual Task
commands remain available with their ordinary permissions. There is no separate
remediation feedback format or endpoint.

Use `--mode validate` to assess the report without generating a fix.

The optional `--required-evidence-scope` accepts `source-harness` or
`application-runtime`. Omitting it preserves the existing source-harness default.
For example, the following deliberately fails admission with the default source-harness backend
rather than running a weaker substitute:

```bash
orka remediate start --input /private/report.json --mode validate \
  --required-evidence-scope application-runtime
```

API callers set the optional top-level `requiredEvidenceScope` field on
`POST /api/v1/remediations`, alongside `requestID`, `mode`, `report`,
`repository`, and `ref`. It is not a field inside the supplied report:

```json
{
  "requestID": "synthetic-runtime-check",
  "mode": "validate",
  "requiredEvidenceScope": "application-runtime",
  "repository": "https://github.com/example/project",
  "ref": "v1.2.3",
  "report": {
    "title": "Synthetic application behavior",
    "problem": "Validate the reported behavior in the running application."
  }
}
```

Unknown, empty, null, or malformed explicit scopes are invalid requests (HTTP
400). Reusing an accepted request ID with a different requirement, including
switching between omitted and explicit `source-harness`, is a conflict (HTTP
409); retries must preserve the identical request.

```bash
orka remediate status RUN_ID
orka remediate cancel RUN_ID
orka remediate download RUN_ID --output /private/new-candidate.patch
```

Use the same server, namespace, and authenticated identity for subsequent
operations. Downloads must target a new private file; existing files are not
overwritten. A downloadable patch remains a candidate unless the run's outcome
explicitly says it was verified for the executed checks.

Credential files require owner-only permissions on Unix. On Windows, ownership
and file ACLs must restrict access to the current user, SYSTEM, and Administrators;
Unix mode bits do not establish Windows privacy. Patch downloads must inherit a
similarly private ACL from their output directory. Unsafe ACLs are rejected before
credentials are read or patch bytes are written.

## What is recorded

Orka automatically binds each run to the original revision, checks, candidate
patch, evidence scope, and accepted task/execution identities. These records support comparison
and recovery; there is no user-facing plan-hash or freezing step.

The candidate cannot change its verification criteria. Changing the checks
requires comparing both original and candidate under the changed checks.
Successful verification is limited to those checks and their configured
environment; it is not a claim that the project is free of defects.

Agent assessments receive readable HTTP output or application-observation
metadata only when its exit code, byte length, and digest exactly match a frozen
expectation. Application API values remain represented by value digests and byte
counts. Unmatched or incomplete observations remain unavailable, not inferred
from result labels.

Reports, patches, and execution results are runtime data governed by namespace
authorization and retention. They are not examples to commit to a public
repository. Keep actual incidents, deployment identities, credentials, and run
results out of source control.

Preparation failures emit a bounded stage/category diagnostic in worker stderr
and the Pod termination message, without raw errors or input contents. Collect
these before workload cleanup; a numeric Git exit status alone does not identify
the underlying network or filesystem failure.

## Development checks

The regular Go test suite covers coordination, authorization, input integrity,
and recovery. `make test-remediation-worker` additionally runs the actual native
Git launcher, supervisor, and sandbox guard in offline, resource-bounded
containers with their production capability sets. The launcher test checks hard
limits across repeated execs; the guard test uses a synthetic HTTP server. Pull the pinned
`VALIDATION_TEST_TOOL_IMAGE` first. The `Tests` workflow runs this check in its
`Remediation worker isolation` job, so it cannot silently remain an opt-in skip.
