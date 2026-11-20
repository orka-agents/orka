# Container Task Secret use policy

An opt-in ValidatingAdmissionPolicy that lets an operator give specific
container Tasks specific credentials, without the cluster-wide
`ORKA_AGENT_DIRECT_SECRET_MOUNTS` switch.

A `type: container` Task that names a Secret, through `spec.secretRef`
or `spec.env[].valueFrom.secretKeyRef`, is admitted only if the identity that
writes it holds the `use` verb on that Secret in the Task's namespace. A
`secretRef` in another namespace is refused. Any later change to the Task's
spec, such as a new image or command, is checked again, even when the Secrets
stay the same; only metadata-only updates, such as the controller's own label
and finalizer writes, are not. The labels and annotations that record a
Task's parent, scheduled run, chat session, coordinator, or API origin count
as part of the check: changing them needs `use` too.

Apply this directory together with
`--container-task-secret-use-authorization-enabled` on the controller. The
Helm value `controller.containerTaskSecretUse.enabled` does both. Before it
delivers a Secret, the controller confirms that the policy and a `Deny`
binding named `container-task-secret-use.orka.ai` (or
`--container-task-secret-use-policy-name`) exist; without them a container
Task that names a Secret fails before its Pod is created. The flag cannot be
combined with `ORKA_AGENT_DIRECT_SECRET_MOUNTS`.

Replace `ORKA_NAMESPACE` and `CONTROLLER_SA` in the policy with the
controller's namespace and ServiceAccount. The controller copies a scheduled
Task's spec into each run; the policy admits those runs from the controller,
and the controller delivers to a run only when it names exactly the Secrets of
its scheduled Task and that Task was not created by another Task.

## Who can receive Secrets

Orka never grants `use` on Secrets to the controller or to any worker
ServiceAccount. So in this mode:

- Tasks an agent writes with a worker identity are refused at admission.
- Tasks created through the Orka API, chat, or tools are written by the
  controller and are refused too. The controller also refuses to deliver to a
  Task that carries chat, coordinator, API, or parent-Task provenance.
- Only Tasks written directly to Kubernetes by a principal you grant `use`,
  for example the identity that applies your job manifests, receive Secrets.

Grant `use` with a Role rule that lists the Secrets by name:

```yaml
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["use"]
  resourceNames: ["repository-bot-key"]
```

An identity with `*` verbs on Secrets also holds `use`. Keep the controller's
own Secrets out of any grant.

Admission checks only writes made after the policy is in force. The controller
therefore refuses a container Task, or for a scheduled run its scheduled Task,
created before the policy and its binding were created or their specs last
changed, plus a 30-second window while API servers load the change. Recreate such Tasks after you
enable the mode.

`spec.secretRef` is mounted as files under `/secrets/task`, one file per key.
An `env` entry's `secretKeyRef` becomes that environment variable.
`configMapKeyRef` and `fileKeyRef` stay refused for container Tasks.

This binding matches every namespace. With more than one installation on a
cluster, add a `matchResources.namespaceSelector` on
`kubernetes.io/metadata.name` for this installation's watched namespace, as
the Helm chart does. The controller delivers only while it can prove the
policy checks every Task create and update in the Task's namespace: it must
fail closed and check `use` on Secrets, and neither it nor the binding may use
an object selector, an exclusion, parameters, or a namespace selector other
than that name label.
