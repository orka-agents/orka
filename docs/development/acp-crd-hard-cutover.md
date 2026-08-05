# ACP v2 CRD hard cutover

`scripts/upgrade-orka-crds.sh` is the only supported helper for replacing a
cluster that may still contain `orka.harness.v1` AgentRuntime state. It is a
fail-closed gate, not a migration tool. It never migrates or deletes
AgentRuntime, Agent, or GatewayBinding objects.

Do not store backups, verification markers, or copied SQLite data in the
repository. They may contain sensitive operational data.

## 1. Create and verify both backups

Create a consistent backup of the controller SQLite/PVC state. A CSI snapshot,
offline PVC archive, raw SQLite backup, or provider snapshot receipt is
acceptable as the `--sqlite-backup` file, but the underlying backup must be
restore-tested or independently inspected before its marker is written. A
snapshot receipt file must identify the immutable snapshot that was tested.

Export the pre-cutover CR inventory as one Kubernetes JSON List before deleting
or migrating legacy objects:

```bash
set -Eeuo pipefail
umask 077

context=sertac-aks
backup_dir=/absolute/secure/path/orka-acp-cutover
install -d -m 0700 "$backup_dir"
chmod 0700 "$backup_dir"

cluster_uid="$(
  kubectl --context "$context" get namespace kube-system \
    -o jsonpath='{.metadata.uid}'
)"
api_server_identity_sha256="$(
  kubectl --context "$context" config view --minify --flatten -o json \
    | jq -c '
        .clusters
        | if length == 1 then
            .[0].cluster
            | {
                server: (.server // null),
                certificateAuthorityData: (.["certificate-authority-data"] // null),
                insecureSkipTLSVerify: (.["insecure-skip-tls-verify"] // null),
                tlsServerName: (.["tls-server-name"] // null),
                proxyURL: (.["proxy-url"] // null),
                disableCompression: (.["disable-compression"] // null)
              }
          else
            error("expected exactly one target cluster")
          end
      ' \
    | shasum -a 256 \
    | awk '{print tolower($1)}'
)"

cr_tmp="$(mktemp "$backup_dir/.orka-crs.XXXXXX")"
cleanup_cr_tmp() {
  rm -f "$cr_tmp"
}
trap cleanup_cr_tmp EXIT

if ! kubectl --context "$context" get \
  agentruntimes.core.orka.ai,agents.core.orka.ai,gatewaybindings.gateway.orka.ai \
  --all-namespaces -o json >"$cr_tmp"; then
  echo "CR inventory capture failed; refusing to create a backup" >&2
  exit 1
fi
if ! jq -e '.kind == "List" and (.items | type == "array")' \
  "$cr_tmp" >/dev/null; then
  echo "CR inventory validation failed; refusing to create a backup" >&2
  exit 1
fi
chmod 0600 "$cr_tmp"
mv "$cr_tmp" "$backup_dir/orka-crs.json"
trap - EXIT
```

After verifying each backup, create a digest-bound operator attestation. Use
`kind=sqlite-pvc` for the SQLite/PVC artifact and `kind=orka-crs` for the CR
inventory:

```bash
write_verified_marker() {
  kind="$1"
  backup="$2"
  marker="$3"
  digest="$(shasum -a 256 "$backup" | awk '{print tolower($1)}')"
  verified_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  cat >"$marker" <<MARKER
format=orka-backup-verification-v1
kind=$kind
context=$context
cluster_uid=$cluster_uid
api_server_identity_sha256=$api_server_identity_sha256
sha256=$digest
verified=true
verified_at=$verified_at
MARKER
  chmod 0600 "$backup" "$marker"
}

# Run these only after the corresponding restore/inspection check succeeds.
write_verified_marker sqlite-pvc \
  "$backup_dir/controller-manager-store.snapshot-receipt.json" \
  "$backup_dir/controller-manager-store.snapshot-receipt.json.verified"
write_verified_marker orka-crs \
  "$backup_dir/orka-crs.json" \
  "$backup_dir/orka-crs.json.verified"
```

The upgrade script rejects relative paths, symbolic links, empty files,
context or cluster-identity mismatches, malformed markers, and digest
mismatches. The `cluster_uid` binds the backup to the target cluster rather than
to a mutable kubeconfig alias. `api_server_identity_sha256` also binds the
canonical API server and TLS settings. The script recomputes both cluster
identities and both backup digests again immediately before the mutating CRD
apply.

## 2. Pause every affected Gateway workload

The CR backup is used to preserve the historical chain:

```text
orka.harness.v1 AgentRuntime <- Agent.runtime.runtimeRef <- GatewayBinding
```

For every Gateway reached by that chain, scale the actual adapter Deployment or
StatefulSet to zero and wait until its controller reports the current generation
and zero replicas. Keep the workload at zero throughout the cutover.

```bash
kubectl --context "$context" -n orka-gateway-telegram \
  scale deployment/orka-gateway-telegram --replicas=0
kubectl --context "$context" -n orka-gateway-telegram \
  rollout status deployment/orka-gateway-telegram
```

Pass an explicit mapping for every affected Gateway. The mapping binds the
namespaced Gateway object to the workload that was paused:

```text
--gateway-workload GATEWAY_NAMESPACE/GATEWAY_NAME=deployment/WORKLOAD_NAMESPACE/WORKLOAD_NAME
```

The workload must be in the Gateway namespace. Each mapped Deployment or
StatefulSet must exist with `spec.replicas=0`, zero reported replicas, and an
`observedGeneration` at least as new as `metadata.generation`. Missing, stale,
or partially terminated workloads block the upgrade.

## 3. Remove or migrate legacy references

Run the script once after the Gateways are paused. It reports every remaining
blocker and exits without applying CRDs:

```bash
scripts/upgrade-orka-crds.sh \
  --context "$context" \
  --sqlite-backup "$backup_dir/controller-manager-store.snapshot-receipt.json" \
  --sqlite-backup-marker "$backup_dir/controller-manager-store.snapshot-receipt.json.verified" \
  --cr-backup "$backup_dir/orka-crs.json" \
  --cr-backup-marker "$backup_dir/orka-crs.json.verified" \
  --gateway-workload \
    orka-gateway-telegram/telegram=deployment/orka-gateway-telegram/orka-gateway-telegram
```

Migrate or delete the reported live objects in dependency order:

1. GatewayBindings that reference affected Agents.
2. Agents that reference v1 AgentRuntimes.
3. `orka.harness.v1` AgentRuntimes.

Every live Agent with `spec.runtime.type: opencode` is also a pre-cutover
blocker. The pre-cutover Agent CRD cannot retain `spec.model.contextWindow` and
does not admit the new built-in OpenCode shape, so do not try to patch such an
Agent in place. Export it after taking the verified CR backup, quiesce any
callers, and delete it before rerunning the gate. After the new CRD is applied,
recreate it with a provider-qualified `spec.model.name`, reviewed positive
`contextWindow` and `maxTokens`, and no `providerRef`, provider Secret, or
Agent-level system prompt. Alternatively, migrate the live Agent to
`claude`, `codex`, `copilot`, or an admin-registered `runtimeRef` before the
cutover.

Do not resume Gateway workloads during this process.

Legacy harness-wrapper Deployments, Services, and Secrets are also blockers.
Without an explicit flag, the script prints exact cleanup commands and does not
delete anything. To let the script delete only the reported wrapper
Deployment/Service/Secret resources, rerun with:

```text
--delete-legacy-wrapper
```

That flag does not delete ServiceAccounts, AgentRuntimes, Agents,
GatewayBindings, or any other resource.

## 4. Apply only after the gate is clean

Rerun the same command with the same verified backup files and Gateway mappings.
The script:

1. validates the backup markers and CR inventory;
2. discovers live v1 resources and the historical reference chain;
3. verifies every affected Gateway workload is fully at zero;
4. rejects any remaining wrapper Deployment, Service, or Secret;
5. performs a server-side dry run of the new CRDs;
6. repeats all backup and live-state checks;
7. revalidates the backup digests and immutable cluster identity immediately
   before mutation; and
8. only then performs the server-side CRD apply.

Any failed read, discovery call, marker check, workload check, deletion, or
dry-run aborts the process. The focused shell fixture suite is:

```bash
/bin/bash scripts/tests/upgrade-orka-crds-test.sh
```
