# ADR 0047: Throwaway AKS clusters for validating Kubernetes projects

Date: 2026-11-08

## Status

Proposed. Extends repository monitor validation (`spec.validation.image`) and,
in a later stage, security-scan validation. It does not change validation for
monitors that do not ask for a cluster.

## Context

Repository monitor validation runs one reviewer-chosen command in a container
Task: the configured image, the exact head checked out read-only, a 45-minute
timeout, and a deny-all NetworkPolicy that must exist before the command is
released. Security-scan validation runs validator agent Tasks on Orka's own
runtimes. Both run in the cluster where the Orka controller runs, and neither
can reach a Kubernetes API.

That is enough for a library or a CLI, but not for a Kubernetes project. A
controller, operator, or Helm chart is only validated when its manifests apply,
its CRDs install, its images run, and its end-to-end tests pass against a real
API server and real nodes. Today a monitor can only lint and unit-test such a
repository, so a review or repair can pass while the change breaks the
project's install.

The cluster has to be throwaway. A shared, long-lived test cluster carries
state from one run into the next (CRDs, webhooks, leftover namespaces, cluster
roles), so a result depends on what ran before it, and untrusted pull-request
code would run beside other repositories' runs. Each validation needs its own
cluster, created for it and deleted after it.

A live run on 2026-11-08 had an Orka agent create an AKS cluster itself
through the Azure CLI forwarding gateway (ADR 0040) with its agent identity.
The write worked, but it showed why the agent should not own the lifecycle:
every write waits for a person's approval; waiting on the approval and the
create spent the agent's whole turn budget before it could fetch credentials;
and cost, size, and deletion were left to the model.

## Decision

The controller, not the agent, creates one AKS cluster per validation run,
hands the validation command a kubeconfig for it, and deletes it when the run
ends. AKS is the first backend. The API names a class, so later backends (for
example vcluster or kind in a microVM sandbox) can be added without changing
monitors.

### Admins define cluster classes; runs claim one

A cluster-scoped `EphemeralClusterClass`, written by an admin, fixes everything
that costs money or grants access:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: EphemeralClusterClass
metadata:
  name: aks-small
spec:
  aks:
    tenantID: <tenant>
    subscriptionID: <subscription>
    resourceGroup: orka-validation-clusters    # the only place clusters are made
    location: westus2
    identity:
      clientID: <user-assigned managed identity>
    nodePool:
      vmSize: Standard_D2s_v5
      count: 1
    kubernetesVersions: ["1.35", "1.36"]       # a run may pick one of these
    tier: Free
  maxLifetime: 2h          # hard ceiling, from creation to deletion
  provisionTimeout: 20m
  maxConcurrent: 3         # per class, across namespaces
  allowedNamespaces: [team-a]
```

A monitor asks for a cluster in its validation settings:

```yaml
spec:
  validation:
    image: ghcr.io/example/operator-validation@sha256:…
    cluster:
      classRef: {name: aks-small}
      kubernetesVersion: "1.36"   # optional; must be in the class's list
```

For each validation Task that asks for one, the controller creates a
namespaced `EphemeralCluster` owned by that Task. Its spec is a frozen copy of
the class's settings at claim time, so a class edit never changes a cluster
mid-run, and its status records the phase (`Pending`, `Provisioning`, `Ready`,
`Deleting`, `Deleted`, `Failed`), the Azure resource ID, the Kubernetes
version, and timestamps. The validation evidence records the class, the frozen
spec's digest, and the version, next to the image and command digests it
records today.

### The controller drives Azure with a scoped managed identity

The class names a user-assigned managed identity that has Contributor on the
class's resource group and nothing else. It is federated to a dedicated
ServiceAccount, `orka-ephemeral-clusters`, in the controller namespace; the
controller requests a short-lived token for that ServiceAccount
(audience `api://AzureADTokenExchange`) and exchanges it with Entra
(`client_credentials` with a `client_assertion`) for an Azure Resource Manager
token. No secret is stored. The agent identity and the forwarding gateway are
not involved: the agent never holds or chooses the credential.

The cluster name is derived from the validation Task's UID, so the
create (`PUT managedClusters`) is idempotent across controller restarts and
retries, and a second claim for the same Task finds the same cluster. Each
cluster is tagged with the Orka installation, the Task UID, the class, and an
`expires-at` time.

### The command is released only once the cluster is ready

The existing validation gate holds the command until the NetworkPolicy is in
place. A cluster-backed run adds two conditions before release:

1. The `EphemeralCluster` is `Ready`: provisioning succeeded and the API server
   answers.
2. The kubeconfig Secret exists. The controller fetches the cluster's
   credential (`listClusterAdminCredential`) into a Secret owned by the
   `EphemeralCluster`, mounted read-only into the validation Pod with
   `KUBECONFIG` pointing at it. The credential is valid only for this cluster,
   which is deleted after the run, and it is never logged or written to Task
   status or results.

Provisioning has its own `provisionTimeout` and does not consume the command's
45-minute timeout. A cluster that fails to provision, or a class at
`maxConcurrent`, makes the validation `unavailable`, as other failures before
the command does, so the head stays eligible for a later review.

### Network: the validation Pod reaches only its cluster

The deny-all NetworkPolicy gains exactly one egress rule: TCP 443 to the
cluster's API server address. The API server's authorized IP ranges are set to
the Orka cluster's egress addresses, so nothing else can reach it.

The project's own workloads, including image builds, run inside the throwaway
cluster, not in the validation Pod. The Pod stays unprivileged under the same
Pod Security settings as today; a build that needs privileges (BuildKit,
kaniko, `docker build` in a Pod) runs as a Pod in the throwaway cluster, which
is disposable and holds no Orka credential. Source reaches it through the API
server (`kubectl cp`, ConfigMaps, or a Job that reads a tarball), and images go
to a registry the validation command deploys into the throwaway cluster, so no
shared registry receives pull-request images.

The throwaway cluster's nodes keep default outbound internet access, so
validation can pull base images and dependencies there even though the
validation Pod itself is offline.

### Deletion is guaranteed three ways

- **On completion:** when the validation Task reaches a terminal phase, its
  `EphemeralCluster` is deleted, and its finalizer deletes the Azure cluster
  (and with it the node resource group) before the object goes away.
- **On expiry:** the controller deletes any cluster past its frozen
  `maxLifetime`, whatever the Task's state.
- **On loss:** a periodic sweep lists clusters in each class's resource group
  tagged with this installation and deletes any that has no live
  `EphemeralCluster` or whose `expires-at` has passed. This covers a deleted
  namespace, a stripped finalizer, or an uninstall that left clusters behind.

### Security scans come second

Scan validators are agent Tasks on shared runtimes, and their runtime Pods must
stay sealed. In a second stage, a `full`-mode validator gets a brokered tool
that runs one container Task against a throwaway cluster, as `run_validation`
does for reviewers, so the agent runtime never receives the kubeconfig or the
network rule.

## Alternatives considered

- **Let the agent create the cluster through the Azure CLI gateway.** Proven
  to work on 2026-11-08, but each write waits for approval, the wait spends the
  agent's turn budget, the agent would hold the cluster credential, and size,
  cost, and deletion depend on the model. Kept for agents that manage Azure,
  not for validation.
- **A long-lived shared test cluster.** Cheap and fast, but state leaks from
  one run into the next, and untrusted pull-request code shares a cluster
  with other runs. Not throwaway.
- **vcluster on a dedicated node pool.** Starts in seconds and costs almost
  nothing, but workloads share the pool's nodes and kernel, which is weaker
  isolation than a separate cluster. A good second backend behind the same
  class API.
- **kind inside the validation Pod.** Needs a privileged container on Orka's
  own nodes, which breaks Pod Security and shares the host with the control
  plane.
- **Cluster API.** A general way to make clusters, but it adds a management
  cluster and providers to install and upgrade, for one AKS backend. Worth
  revisiting if several cloud backends are needed.

## Consequences

- Each cluster-backed validation takes about 5 to 8 minutes longer, and a
  1-node `Standard_D2s_v5` Free-tier cluster costs roughly $0.05 to $0.10 per
  run. A warm pool of precreated clusters, each still used once, could cut the
  wait; it is left for later because it costs money while idle.
- The controller gains Azure credentials. Their reach is one resource group
  per class, they are short-lived, and they come from federation, not a
  stored secret.
- The validation NetworkPolicy is no longer always deny-all: cluster-backed
  runs allow one API server address. Runs without a cluster keep today's
  policy exactly.
- Validation of a private repository sends its source into a cluster with
  internet egress. Classes for private repositories should restrict that
  egress (for example a user-defined outbound route through a firewall);
  the first version documents the risk and leaves egress control to the
  class's network settings.
- AKS is the only backend at first, so the feature needs an Azure
  subscription. Other backends reuse the class and claim API.
- Two new CRDs, `EphemeralClusterClass` and `EphemeralCluster`, and a
  controller loop with a finalizer and a sweep.
