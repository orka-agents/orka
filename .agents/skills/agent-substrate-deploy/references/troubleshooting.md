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
- Stream stops early: configure the native router's `--route-timeout` for the
  longest Task, and its shutdown grace if long turns must survive router drain.
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
