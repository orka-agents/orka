# GitHub implementation and CI handoff

A native AI coordinator delegates one implementation to a Claude ACP runtime.
Orka's publisher commits and verifies the branch, then the coordinator opens a
pull request and waits up to five minutes for CI. It reports the exact CI status
and stops. It does not merge or attempt another delegated write to that branch.

Delegation cannot supply `workspace.expectedRemoteSHA`, which is required to
update an existing publication branch. Use the optional direct repair workflow
below or a [RepositoryMonitor](../repository-monitor-pr-review-repair) for repairs.

## Configure it

The cluster needs the native AI worker, the Claude ACP runtime, the provider
proxy, and Workspace/Publisher configured. In `agents.yaml`, replace
`my-provider` and the model names with ones available in your installation.
The Claude runtime receives provider access through the proxy.

Create three Secrets in the controller's watched namespace:

```bash
kubectl -n orka-system create secret generic repository-read \
  --from-literal=token='<read-token>'
kubectl -n orka-system create secret generic repository-publish \
  --from-literal=token='<write-token>'
kubectl -n orka-system create secret generic repository-forge \
  --from-literal=token='<forge-token>'
```

There are four credential roles. This same-repository example uses
`repository-read` for both `readCredentialRef` and
`publicationReadCredentialRef`. If the publication repository differs, provide
a target-read Secret for `publicationReadCredentialRef`.
`publicationCredentialRef` authorizes the push; `forgeCredentialRef` authorizes
PR creation. None enters the ACP process tree.

`secret.yaml` is an optional template for those Secrets. Replace its placeholders
before applying it. The kustomization applies only Agents, so it cannot overwrite
credentials or start a Task before configuration is ready.

Edit `task.yaml` before creating a Task:

- Set the source and publication repository URLs and source branch.
- Set all four credential references.
- Choose a new, unused `pushBranch` for each run and the intended `prBaseBranch`.
- Replace the sample implementation request with work appropriate to that repository.

## Run and verify

```bash
kubectl -n orka-system apply -k examples/github-cicd
task_name="$(kubectl -n orka-system create -f examples/github-cicd/task.yaml -o jsonpath='{.metadata.name}')"
kubectl -n orka-system get task "$task_name" -w
```

Inspect the coordinator result and its child Task. Require a verified
`status.delivery` receipt with a `headSHA`, then inspect that commit and the PR.
The coordinator reports `passed`, `failed`, `pending`, `no_checks`, or `closed`;
only `passed` means CI is green. A successful model response alone does not prove
that publication or CI succeeded.

## Optional direct CI repair

Copy `github-actions-webhook.yaml` into `.github/workflows/` in the repository.
It handles failed runs of a workflow named `CI` for same-repository PRs. Fork PRs
and branch-only runs are skipped. Configure repository secrets `ORKA_API_URL`
and `ORKA_TOKEN`; the optional `ORKA_NAMESPACE` repository variable defaults to
`orka-system`.

The Task namespace needs `claude-coder`, `repository-read`, and
`repository-publish`. The repair Task checks out the failed commit using `ref`
and supplies that same SHA as `expectedRemoteSHA`. Publication stops if the
branch has moved. A source branch protected by repository rules can also reject
the update. This workflow leaves the existing PR open and never merges it.

This GitHub Actions workflow is separate from the coordinator's single pass.
It starts an Orka Task through the API and does not wait for its result; inspect
the resulting Task and delivery receipt in Orka.
