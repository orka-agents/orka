---
description: "Run a long-lived, single-user agent host, such as an Agent Host Protocol daemon, on Orka-owned Pods."
---

# Agent hosts

An `AgentHost` runs one long-lived agent host for one user: a daemon that serves the
[Agent Host Protocol](https://microsoft.github.io/agent-host-protocol/specification/overview.html)
(AHP) to clients such as the GitHub Copilot desktop app. Sessions, tools, approvals and
worktrees stay inside the host; the client is a UI. Orka admits the host, runs it from an
operator's `AgentHostTemplate`, restarts it, suspends and wakes it, and cleans it up. The
design is [ADR 0035](https://github.com/orka-agents/orka/blob/main/docs/adr/0035-agent-hosts.md).

Each host is one Pod, a PersistentVolumeClaim for its state, a NetworkPolicy and Secrets in
a dedicated host namespace. Orka does not see the host's turns; it governs who may run a
host, what it runs, what it may reach and the policy pinned in its template.

:::warning[One trusted user per host]
- The sample host, `copilotd`, serves exactly one GitHub user per process.
- The host's agent holds the user's GitHub token, and every process in the Pod runs as
  one uid, so code the agent runs can read it. Egress to public TCP 443 is open. Run hosts only
  for code the user trusts; "public repositories only" is not a sandbox boundary.
:::

## Turn it on

With Helm, set:

```yaml
controller:
  image:
    digest: sha256:<controller image digest>
  agentHosts:
    enabled: true
    namespace: orka-agent-hosts
```

The chart creates the host namespace with the restricted Pod Security profile, the
controller's Role in it, the controller flags and the AgentHost admission webhook. The
`orka-host-agent` sidecar comes from the controller image, so pin it by digest, or set
`controller.agentHosts.agentImage` to another digest-pinned image that carries it. Agent
hosts need `controller.mode: harness-v2`, and the host namespace must differ from the
release namespace and `controller.acpRuntime.namespace`.

Before you disable agent hosts, uninstall, or roll back to a release without them, delete
every AgentHost and wait until it is gone. Without agent hosts the controller stops
reconciling them, so their Pods keep running and their deletion never finishes; an upgrade
that disables agent hosts is refused while AgentHosts remain. Helm keeps the host
namespace, with the hosts' volumes and the controller's Role in it, so a controller with
agent hosts on can still finish cleaning up, including hosts left in an earlier namespace
after `controller.agentHosts.namespace` changes. Delete the namespace yourself once no
AgentHost needs it.

With the Kustomize install:

1. Create the host namespace and the controller's Role in it. The Kustomize install binds
   the controller's manager role per namespace:

   ```bash
   kubectl apply -k config/agent-hosts
   ```

2. Start the controller with:

   ```text
   --enable-agent-hosts
   --agent-host-namespace=orka-agent-hosts
   --agent-host-agent-image=<controller image>@sha256:<digest>
   ```

   The agent image is normally the controller image itself; it carries the
   `orka-host-agent` sidecar. `--agent-host-controller-pod-labels` selects the controller
   Pods allowed to read host status through each host's NetworkPolicy; its default,
   `orka.ai/network-role=controller`, matches the shipped controller Pods.

With either install, create an `AgentHostTemplate` (see
[`config/samples/core_v1alpha1_agenthosttemplate.yaml`](https://github.com/orka-agents/orka/blob/main/config/samples/core_v1alpha1_agenthosttemplate.yaml))
and grant `use` on it to the people who may run hosts. Write the Role as a manifest:
`kubectl create role` refuses `use` because the API does not list that verb.

AgentHosts, their templates and their token Secrets live in the namespace Orka watches
(`--watch-namespace`), because the controller reconciles AgentHosts only there. Everyone's
hosts therefore share that namespace, and whoever may update an AgentHost controls it:
they can suspend or delete it, and deleting it deletes its volume. They can also point it
at a token Secret they can read, which restarts it on its volume as them. So give everyone
one shared Role to create hosts, and each person a Role that names only their own host and
token Secret:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: agent-host-creator
rules:
  - apiGroups: ["core.orka.ai"]
    resources: ["agenthosttemplates"]
    resourceNames: ["copilotd"]
    verbs: ["use"]
  - apiGroups: ["core.orka.ai"]
    resources: ["agenthosts"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: agent-host-alice
rules:
  - apiGroups: ["core.orka.ai"]
    resources: ["agenthosts"]
    resourceNames: ["alice"]
    verbs: ["get", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["secrets"]
    resourceNames: ["alice-github-token"]
    verbs: ["get", "update", "patch"]
```

`create` cannot be limited by name, so agree on host names before hosts are created, and
let an operator create each person's token Secret, or grant `create` on Secrets only where
that is acceptable in the watch namespace. The shared Role grants no `list` or `watch`,
which would show everyone's hosts; `kubectl get agenthost alice` and `kubectl apply` need
only `get` on the named host.

Creating an `AgentHost` requires `use` on its template, and naming a token Secret requires
`get` on that Secret, both checked against the caller by an admission policy.

## Run a host

The sample template runs `copilotd` in relay mode: it registers the host as an environment
on the user's GitHub account and accepts clients through GitHub's relay. The cluster needs
no inbound path.

In the namespace Orka watches:

```bash
kubectl create secret generic alice-github-token --from-literal=token=<GitHub token>
kubectl apply -f config/samples/core_v1alpha1_agenthost.yaml
kubectl get agenthost alice
```

When the host is `Ready`, the environment `orka-alice` appears in the client's
**Environments** list. The token's owner is the only GitHub user the host serves. Orka
copies the token into the host namespace and keeps the copy current, so rotating the
Secret reaches the running host without a restart. Pointing the host at another Secret or
key replaces its Pod on the same volume, because the daemon reads its identity from the
token when it starts.

### Standalone access

A template with `access: Standalone` skips the relay. `access` changes only what Orka
publishes, not the daemon's arguments, so the template must also start the daemon in its
standalone mode: for copilotd, `--standalone` instead of `--relay` and the Mission Control
token options. The client port-forwards to the host's loopback listener and presents the
host's connection token, which Orka writes to the Secret named in
`status.connectionSecretRef`:

```bash
ns=$(kubectl get secret alice-connection -o jsonpath='{.data.namespace}' | base64 -d)
pod=$(kubectl get secret alice-connection -o jsonpath='{.data.pod}' | base64 -d)
port=$(kubectl get secret alice-connection -o jsonpath='{.data.port}' | base64 -d)
kubectl -n "$ns" port-forward "pod/$pod" "8788:$port"
```

Add the host as `ws://127.0.0.1:8788/?tkn=<token>`.

The per-user Role above does not cover these commands. Add the connection Secret, here
`alice-connection`, to its Secret rule, and grant `get` on the host's Pod and `create` on
its `pods/portforward` in the host namespace. The Pod keeps its name for the host's life,
so grant them once the host exists, using the `pod` value above:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: agent-host-alice-standalone
  namespace: orka-agent-hosts
rules:
  - apiGroups: [""]
    resources: ["pods"]
    resourceNames: ["<pod>"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["pods/portforward"]
    resourceNames: ["<pod>"]
    verbs: ["create"]
```

Bind it to the same person, in the host namespace.

### Hosts from GitHub Connections

With [connectors](../guides/connectors.md) enabled, people can get a host by linking
GitHub, with no Secret and no Kubernetes access. An operator adds a template to one GitHub
`ConnectorProvider` in the watch namespace, backed by a GitHub App with expiring user
tokens:

```yaml
spec:
  # ...the provider's OAuth settings, as in Connectors
  agentHost:
    templateRef:
      name: copilotd
```

- The provider needs no tools. A dedicated GitHub App with only the permissions the host
  needs is recommended: a host's token carries all of the app's permissions, and a tool
  call's refresh would replace the host's token.
- When a person links the provider, Orka creates an AgentHost named `gh-<login>` in that
  namespace, which appears as the environment `orka-gh-<login>`. The login is lowercased,
  and a managed user's underscore becomes `--`: `alice_acme` gets `gh-alice--acme`.
- Orka writes the link's access token into the host and refreshes it 15 minutes before it
  expires. copilotd picks up the replaced file without a restart. The refresh token never
  leaves Orka. A token that never expires is refused with `GitHubTokenNeverExpires`, since
  code in the host could keep using a copy of it.
- The host belongs to the first GitHub account it was given. A later token for another
  account fails it with `GitHubIdentityChanged` and keeps the volume.
- Disconnecting stops the host, deletes its token copy and keeps the AgentHost and its
  volume. Linking the same account again restarts the host on its volume.
- Settled failures such as a disconnect, a decline, a refused provider, a grant that no
  longer covers the link, `GitHubIdentityChanged`, or `GitHubTokenNeverExpires` stop the
  host. While a controller catches up with an edit, or a read fails, the host keeps
  running on the token it has and gets no new one.
- A suspended host keeps no token copy. Waking it writes a fresh one, after checking the
  account.
- Only the controller may create a host fed by a Connection, and only from the template its
  provider names. A host keeps that template when the provider later names another one. A
  link annotated `orka.ai/agent-host: declined` gets no host, and declining stops a host it
  already has, keeping its volume.
- The provider must be on github.com: Orka reads the token's account from api.github.com.
- The template must use `access: Relay`. A Standalone host's connection Secret sits in the
  namespace every person's host shares, so these hosts fail with `InvalidTemplate` instead.
- Deleting the AgentHost deletes its volume. While the link stays, Orka creates a fresh host
  for it.
- At most one provider per namespace may set `agentHost`. If several do, none provisions.
  The same happens when the name `gh-<login>` is taken by another AgentHost. Each case is
  reported as an event.

Linking such a provider again is required after `agentHost` is added or changed: it is part
of the consent the person gave.

## The host contract

Orka adds to the template's container named `host`, and promises nothing else:

| Variable | Meaning |
| --- | --- |
| `ORKA_HOST_ID` | The AgentHost UID, stable for the host's life. |
| `ORKA_HOST_NAME` | The AgentHost name, for display. |
| `ORKA_CONNECTION_TOKEN_FILE` | A per-host random token in a memory-backed volume mounted only into `host`. Read it once and delete it. |
| `ORKA_GITHUB_TOKEN_FILE` | The user's GitHub token, read-only, when the template's `githubToken` allows it. |
| `ORKA_STATUS_DIR` | A directory shared with the Orka host agent for the idle status file. |
| `ORKA_HOST_AGENT` | A read-only copy of the Orka host agent, for use as the container's init process. |

The host must serve AHP on `127.0.0.1:<listenPort>`, gated by the connection token, and
keep everything that must survive a restart under `/workspace`. The container's entrypoint is
its PID 1, which adopts every orphaned process, such as git's detached maintenance. Agent daemons
do not reap processes they never started, so start the daemon with
`exec "$ORKA_HOST_AGENT" init -- <command>`: it reaps them, forwards signals to the daemon, and
exits with its status. When the template sets
`idleStatusFile`, the host writes JSON with at least `idle` and `idleSince` there, and
rewrites it at least every five minutes. Orka judges freshness by its `updatedAt`, or by the
file's modification time when it has none, and treats the status as stale after ten minutes,
or when that time or `idleSince` is more than ten minutes ahead. copilotd's `--idle-status-file` format fits.

The `orka-host-agent` sidecar answers the host's probes. The Pod is Ready only after an AHP
`initialize` and `ping` succeed against the loopback listener; the check never sends
`authenticate`, so it cannot claim a single-user host. Afterwards it checks that the host
keeps its idle status file fresh, or, for hosts without one, that the listener accepts
connections. It never keeps the host from being idle.

## Restarts, suspension and wake

- A host Pod that terminates, or whose listener stays down, is replaced with a new one
  on the same volume. A turn that was running is lost; sessions resume from the volume.
- `spec.suspended: true` stops the Pod and keeps the volume and Secrets. Setting it back
  to `false` starts the host again.
- A template's spec cannot change. To change a template, delete it and create it again
  under the same name: each host on it gets a new Pod from the new template, on its
  existing volume. A running turn is lost, and storage changes apply only to new hosts.
- When the host agent image changes, such as after an upgrade of the controller, each
  running host gets a new Pod with it on its volume once the host reports idle, and at
  the latest 24 hours later. Until then the host has the condition
  `HostAgentUpdatePending`. A host replaced at the deadline loses a running turn.
- A template's `idleSuspend.after` suspends hosts that report idle for that long. A
  client cannot wake a suspended relay host; set `spec.wakeRequestedAt` to the current
  time to start it:

  ```bash
  kubectl patch agenthost alice --type merge \
    -p "{\"spec\":{\"wakeRequestedAt\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}}"
  ```

Hosts stay on unless a template enables idle suspension.

copilotd counts a session as busy while its runtime refuses to list dynamic-workflow runs,
so a host with such a session never reports idle. Its `--idle-ignore-unobservable-factories`
flag lets those sessions reach idle; set it only where dynamic workflows are not in use,
because a pending or running workflow can be lost to suspension.

## Troubleshooting

| `Ready` reason | Meaning |
| --- | --- |
| `Provisioning` | The Pod is starting. The message names a container waiting on an image or volume. |
| `Restarting` | The host's Pod failed; Orka replaces it after a backoff that grows from 10 seconds to 5 minutes. |
| `TemplateNotFound` | The template does not exist in the AgentHost's namespace. |
| `InvalidTemplate` | The template breaks a rule in the message. |
| `GitHubTokenRequired` / `GitHubTokenUnavailable` | The template needs a token and the referenced Secret or key is missing. |
| `ObjectCollision` | An object with a host's name exists in the host namespace but was not created for it. Orka never adopts or deletes it. |
| `ObjectRejected` | The API server refused one of the host's objects, for example a Pod naming an unknown RuntimeClass, huge pages whose request and limit differ, or a Pod a quota or admission policy denies. The message carries the server's reason; Orka retries on its next resync. |
| `StateVolumeLost` | The host's volume was deleted. Delete and recreate the AgentHost. |
| `HostAgentUnavailable` | The controller has no `--agent-host-agent-image`. |
| `HostNamespaceChanged` | The controller's `--agent-host-namespace` changed after the host was created. Delete and recreate it. |
| `ConnectionUnavailable` | The host's Connection is gone, unlinked or needs a new consent: linking again restarts the host. Or its provider no longer serves agent hosts: an operator restores the provider's `agentHost`, then the person links again. |
| `GitHubIdentityChanged` | The Connection now links a different GitHub account than the one the host's volume belongs to. Link the original account, or delete the AgentHost. |
| `GitHubTokenNeverExpires` | The host's Connection holds a GitHub token that never expires, which code in the host could read and keep using. Turn on user-token expiration in the GitHub App, then link again. |

A host that fails for a missing template or token has its Pod stopped and keeps its
volume. When the token's Secret or Connection is gone, Orka also deletes its copy.

Deleting an AgentHost removes every object Orka created for it, and only those.
