# Native Substrate troubleshooting

- Docker preflight timeout: restore the engine before retrying. Inspect other
  users of Docker before restarting it. The installer fails before touching a
  cluster when preflight fails.
- Source mismatch: compare the origin, commit, and protocol digest with
  `hack/agent-substrate/upstream.env`. Upgrade them together with conformance;
  do not reapply the removed provider patches.
- Control authentication: use a verified server CA and exactly one of rotating
  mTLS or a bearer file. The official local install projects PodCertificate and
  ClusterTrustBundle volumes. Never copy private credentials into templates.
- Missing template: use `kubectl ate get actor-template --atespace <space>`.
  `templateRef.namespace` means native Atespace, not an ActorTemplate CRD namespace.
- Pending placement: verify the infrastructure template selects exactly one
  dedicated WorkerPool and that a worker is Active with capacity for one Actor.
  Physical worker capacity is separate from Actor count.
- Stream stops early (`runtime prompt stream disconnected`, Task
  `OutcomeUnknown`, or `ExecutionEventPersistenceFailed` if Core can't record
  the stream-failure event): with the Envoy dataplane, raise the native router's
  `--route-timeout` above the longest turn. It caps total response time, so
  streamed updates don't extend it. agentgateway ignores the flag, so set the
  route timeout in its static configuration. An Envoy router rollout also resets
  streams after a fixed drain window of about 15s, which `--drain-timeout`
  doesn't change.
- Suspended or failed workspace: inspect sanitized conditions first. Private
  controller ConfigMaps retain Actor and Tag provenance. A snapshot alone is
  not workload termination proof. Do not forge consent annotations or unstick
  finalizers while a workload may remain.
- Explicit recovery: export with `recoverLastCheckpoint: true`, then create a
  new workspace from the Ready checkpoint's exact UID/digest. The failed source
  Task remains uncertain and is never automatically replayed.
- Finalizer blocked: restore access to the same provider and worker namespace.
  Existing data cleanup remains active when new admission is disabled. Removing
  public checkpoint references cannot remove data already acquired by a target.
