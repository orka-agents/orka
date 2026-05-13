# Kontxt quickstart: use OIDC identity to call Orka without long-lived tokens

This guide shows one successful Orka + kontxt run. It uses GitHub Actions OIDC because it is easy to reproduce and matches Orka's live validation, but the pattern is not GitHub-specific.

## If you only remember one thing

```text
GitHub / Entra / Kubernetes proves who the caller is.
kontxt signs why this request is allowed.
Orka enforces and audits the signed transaction.
```

In this quickstart:

```text
GitHub Actions OIDC token
  → kontxt TTS
  → kontxt TxToken
  → Orka API with Txn-Token
  → Kubernetes Task / Job / Pod with transaction metadata
```

For deeper reference material, including Entra, Kubernetes projected ServiceAccount tokens, delegation, outbound propagation, and full scope/`tctx` behavior, see [Kontxt TxToken integration](kontxt.md).

## The problem this solves

CI systems, developer portals, and automation platforms often call Orka with a bearer token that proves **who** is calling, but not enough about **why this specific request is allowed**. Without transaction-scoped context, platforms tend to fall back to weak patterns:

- long-lived API keys or broad ServiceAccount tokens shared across many jobs;
- ad-hoc headers such as `X-Repo`, `X-Actor`, or `X-Workflow` that downstream services must trust blindly;
- audit trails that require stitching together GitHub logs, Orka Tasks, Kubernetes Jobs/Pods, and downstream service logs by time and guesswork.

That is risky for autonomous or delegated work. Identity alone is not enough; each hop also needs signed request context and a stable transaction ID.

## How it works

| Piece | Role in this guide |
|---|---|
| **Subject token** | Proves who the caller is. In this guide, it is a GitHub Actions OIDC JWT. |
| **kontxt TTS** | A separate service from [`github.com/aramase/kontxt`](https://github.com/aramase/kontxt) that verifies the subject token and exchanges it for a TxToken. In Kubernetes it is usually a Deployment/Service with one or more Pods; Orka does not deploy it for you. |
| **TxToken** | A short-lived transaction token sent to Orka in the `Txn-Token` header. |
| **`scope`** | The permissions in the TxToken, such as `orka:tasks:create`. |
| **`tctx`** | Signed request context, such as `namespace=default` and `taskType=container`. |
| **`txn`** | The transaction ID used to correlate Orka Tasks, Jobs, Pods, and logs. |
| **Orka** | Verifies the TxToken, enforces scope/`tctx`, and stamps safe transaction metadata. |

## What you will validate

You are done when:

- the smoke workflow prints `unauthenticated status=401`;
- the smoke workflow prints `create status=201`;
- the response includes `spec.transaction.profile=kontxt`;
- the response includes a transaction ID and safe transaction digests;
- the smoke workflow prints `tampered status=401`;
- optionally, the Kubernetes Task/Job/Pod carry `orka.ai/transaction-id`.

## Prerequisites

You need:

- a Kubernetes cluster with Orka installed;
- kubeconfig access for Steps 1 and 2;
- permission to install [`kontxt`](https://github.com/aramase/kontxt) TTS with Helm, or an existing kontxt TTS/issuer you can use;
- an Orka API URL reachable from the GitHub Actions runner;
- `helm`, `kubectl`, `curl`, and `jq`.

For private clusters, use a self-hosted GitHub runner or run the Kubernetes validation step from your workstation.

> **kontxt TTS deployment note**
>
> kontxt TTS is separate from Orka. It is provided by [`github.com/aramase/kontxt`](https://github.com/aramase/kontxt); see the upstream [`cmd/tts`](https://github.com/aramase/kontxt/tree/main/cmd/tts) service and Kubernetes/Helm quick start in that repo. It can run in Kubernetes as a Deployment/Service/Pod, or it can run outside the cluster as a centralized service. Orka only needs the TTS/JWKS URLs for the kontxt service you operate.

Fill in these values before starting:

```bash
export ORKA_NS=orka-system
export ORKA_DEPLOY=orka-controller-manager
export ORKA_API_SVC=orka-api

export KONTXT_NS=kontxt-system
export KONTXT_RELEASE=kontxt
export KONTXT_TRUST_DOMAIN=orka-api

export ORKA_API=https://orka.example.test
export KONTXT_TTS_URL=http://kontxt-tts.kontxt-system.svc
export KONTXT_ISSUER=https://kontxt-tts.kontxt-system.svc
export KONTXT_AUDIENCE=orka-api
export KONTXT_JWKS_URL=http://kontxt-tts.kontxt-system.svc/.well-known/jwks.json
export GITHUB_OIDC_AUDIENCE=orka-kontxt-smoke
export KONTXT_REQUESTING_WORKLOAD=spiffe://example.test/ns/default/sa/orka-smoke-client
```

Token safety rules:

- do not print GitHub OIDC tokens or TxTokens;
- do not use `set -x` in these scripts;
- store tokens only in temporary files created with `umask 077` or mode `0600`;
- log transaction IDs and digests, not tokens.

Orka never stores raw TxTokens in Task specs/status, logs, labels, annotations, metrics, artifacts, or durable memory.

## Step 1: Install kontxt TTS with Helm

Run this step from a workstation or runner with kubeconfig access. If your platform team already operates kontxt TTS, skip this step and use their TTS/JWKS URLs.

Create a Helm values file that configures kontxt TTS to issue Orka-scoped TxTokens and trust GitHub Actions OIDC as the subject-token issuer:

```bash
cat >/tmp/kontxt-orka-values.yaml <<EOF_VALUES
tts:
  config:
    trustDomain: "${KONTXT_TRUST_DOMAIN}"
    issuer: "${KONTXT_ISSUER}"
    # 15m keeps the smoke test forgiving. Use a shorter lifetime for production.
    tokenLifetime: "15m"
    subjectTokens:
      - issuer:
          url: "https://token.actions.githubusercontent.com"
          audiences:
            - "${GITHUB_OIDC_AUDIENCE}"
        claimMappings:
          subject:
            claim: "sub"
EOF_VALUES
```

Install the kontxt Helm chart:

```bash
helm upgrade --install "$KONTXT_RELEASE" oci://ghcr.io/aramase/charts/kontxt --version 0.0.1 \
  --create-namespace \
  --namespace "$KONTXT_NS" \
  -f /tmp/kontxt-orka-values.yaml
```

Wait for kontxt Pods to be ready:

```bash
kubectl -n "$KONTXT_NS" get pods,svc
kubectl -n "$KONTXT_NS" rollout status deploy -l app.kubernetes.io/instance="$KONTXT_RELEASE" --timeout=5m
```

Expected result: the kontxt TTS Deployment rolls out and exposes a Service that matches `KONTXT_TTS_URL`.

If you run the GitHub smoke test on a GitHub-hosted runner, expose kontxt TTS and Orka API through endpoints that runner can reach and set the workflow `KONTXT_TTS_URL` / `ORKA_API` to those external URLs. If you use a self-hosted runner in the cluster network, the in-cluster service URLs can be used directly.

## Step 2: Configure Orka to trust kontxt TxTokens

Run this step from a workstation or runner with kubeconfig access.

First confirm the Orka deployment and service names:

```bash
kubectl -n "$ORKA_NS" get deploy,svc
```

The controller Pod must be able to reach `KONTXT_JWKS_URL`. Optional reachability check from inside the cluster:

```bash
kubectl -n "$ORKA_NS" run kontxt-jwks-check \
  --rm -i --restart=Never \
  --image=curlimages/curl:8.11.1 \
  -- "$KONTXT_JWKS_URL"
```

Expected result: the command prints JWKS JSON and exits successfully.

Configure Orka in `audit` mode first. In audit mode Orka validates TxTokens and records would-deny authorization decisions, but it does not reject otherwise valid requests because of missing scopes or context mismatches.

```bash
kubectl -n "$ORKA_NS" set env deployment/"$ORKA_DEPLOY" \
  ORKA_CONTEXT_TOKEN_PROFILE=kontxt \
  ORKA_CONTEXT_TOKEN_ISSUER="$KONTXT_ISSUER" \
  ORKA_CONTEXT_TOKEN_AUDIENCE="$KONTXT_AUDIENCE" \
  ORKA_CONTEXT_TOKEN_JWKS_URL="$KONTXT_JWKS_URL" \
  ORKA_CONTEXT_TOKEN_AUTHZ_MODE=audit

kubectl -n "$ORKA_NS" rollout status deployment/"$ORKA_DEPLOY" --timeout=5m
```

Expected result:

```text
deployment "<orka controller>" successfully rolled out
```

Kontxt TxTokens are read from `Txn-Token` by default. Keep that default for the first smoke test.

## Step 3: Run the GitHub Actions smoke test

This step shows the GitHub Actions version of the generic flow. GitHub is not required for kontxt or Orka; it is used here because it is an easy way to demonstrate a real OIDC subject token without storing long-lived secrets.

Create `.github/workflows/orka-kontxt-smoke.yml` in a test repository that is allowed to call your Orka and kontxt endpoints. Replace the placeholder `env` values or map them to repository/environment variables.

```yaml
name: Orka Kontxt Smoke Test

on:
  workflow_dispatch:

permissions:
  contents: read
  id-token: write

jobs:
  smoke:
    runs-on: ubuntu-latest
    env:
      ORKA_API: https://orka.example.test
      GITHUB_OIDC_AUDIENCE: orka-kontxt-smoke
      KONTXT_TTS_URL: https://kontxt-tts.example.test
      KONTXT_REQUESTING_WORKLOAD: spiffe://example.test/ns/default/sa/orka-smoke-client
    steps:
      - name: Install tools
        shell: bash
        run: |
          set -euo pipefail
          sudo apt-get update
          sudo apt-get install -y jq
          curl -fsSLO "https://dl.k8s.io/release/$(curl -fsSL https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
          chmod +x kubectl
          sudo mv kubectl /usr/local/bin/kubectl

      - name: Exchange GitHub OIDC for TxToken
        shell: bash
        run: |
          set -euo pipefail
          umask 077

          subject_token_file="$(mktemp)"
          tx_token_file="$(mktemp)"

          curl -fsS \
            -H "Authorization: Bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}" \
            "${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=${GITHUB_OIDC_AUDIENCE}" \
            | jq -er '.value' > "${subject_token_file}"

          request_details="$(jq -cn '{namespace:"default",taskType:"container"}')"
          request_context="$(jq -cn '{purpose:"orka-kontxt-smoke",source:"github-actions"}')"

          curl -fsS -X POST "${KONTXT_TTS_URL%/}/token_endpoint" \
            -H "X-Kontxt-Workload: ${KONTXT_REQUESTING_WORKLOAD}" \
            -H 'Content-Type: application/x-www-form-urlencoded' \
            --data-urlencode 'grant_type=urn:ietf:params:oauth:grant-type:token-exchange' \
            --data-urlencode "subject_token@${subject_token_file}" \
            --data-urlencode 'subject_token_type=urn:ietf:params:oauth:token-type:id_token' \
            --data-urlencode 'requested_token_type=urn:ietf:params:oauth:token-type:txn_token' \
            --data-urlencode 'scope=orka:tasks:create' \
            --data-urlencode "request_details=${request_details}" \
            --data-urlencode "request_context=${request_context}" \
            | jq -er '.access_token' > "${tx_token_file}"

          echo "TXN_TOKEN_FILE=${tx_token_file}" >> "${GITHUB_ENV}"

      - name: Create Orka Task with TxToken
        shell: bash
        run: |
          set -euo pipefail
          umask 077

          task_name="kontxt-smoke-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
          task_manifest="$(mktemp).yaml"
          task_request="$(mktemp).json"
          tx_header_file="$(mktemp)"
          response_file="$(mktemp).json"

          cat > "${task_manifest}" <<EOF_TASK
          apiVersion: core.orka.ai/v1alpha1
          kind: Task
          metadata:
            name: ${task_name}
            namespace: default
          spec:
            type: container
            image: busybox:1.36
            command:
              - /bin/sh
              - -c
            args:
              - echo kontxt-smoke
          EOF_TASK

          # Do not kubectl apply this manifest. Sending it through the Orka REST API
          # is what validates Txn-Token and lets Orka stamp transaction metadata.
          kubectl create --dry-run=client --validate=false -f "${task_manifest}" -o json \
            | jq '{name: .metadata.name, namespace: .metadata.namespace} + .spec' \
            > "${task_request}"

          printf 'Txn-Token: %s\n' "$(cat "${TXN_TOKEN_FILE}")" > "${tx_header_file}"

          unauth_status="$(curl -sS -o /tmp/orka-unauth.json -w '%{http_code}' \
            "${ORKA_API%/}/api/v1/tasks?namespace=default")"
          echo "unauthenticated status=${unauth_status}"
          test "${unauth_status}" = 401

          create_status="$(curl -sS -o "${response_file}" -w '%{http_code}' \
            -X POST "${ORKA_API%/}/api/v1/tasks" \
            -H "@${tx_header_file}" \
            -H 'Content-Type: application/json' \
            --data @"${task_request}")"
          echo "create status=${create_status}"
          test "${create_status}" = 201

          jq -e '
            (.spec.requestedBy.subject // "") != ""
            and .spec.transaction.profile == "kontxt"
            and (.spec.transaction.id // "") != ""
            and (.spec.transaction.scope // "" | contains("orka:tasks:create"))
            and (.spec.transaction.contextDigest // "" | startswith("sha256:"))
            and (.spec.transaction.requesterContextDigest // "" | startswith("sha256:"))
            and .metadata.labels["orka.ai/transaction-id"] == .spec.transaction.id
            and .metadata.annotations["orka.ai/transaction-id"] == .spec.transaction.id
          ' "${response_file}"

          jq '{task: .metadata.name, transactionID: .spec.transaction.id, scope: .spec.transaction.scope}' "${response_file}"

          bad_token_file="$(mktemp)"
          bad_header_file="$(mktemp)"
          cp "${TXN_TOKEN_FILE}" "${bad_token_file}"
          printf 'tampered' >> "${bad_token_file}"
          printf 'Txn-Token: %s\n' "$(cat "${bad_token_file}")" > "${bad_header_file}"

          tampered_status="$(curl -sS -o /tmp/orka-tampered.json -w '%{http_code}' \
            -X POST "${ORKA_API%/}/api/v1/tasks" \
            -H "@${bad_header_file}" \
            -H 'Content-Type: application/json' \
            --data @"${task_request}")"
          echo "tampered status=${tampered_status}"
          test "${tampered_status}" = 401
```

Expected status lines:

```text
unauthenticated status=401
create status=201
tampered status=401
```

Expected safe summary shape:

```json
{
  "task": "kontxt-smoke-...",
  "transactionID": "txn-...",
  "scope": "orka:tasks:create"
}
```

## Step 4: Validate Kubernetes transaction metadata

This step is optional for the first green run, but it is the best proof that Orka made the TxToken Kubernetes-native. Run it from a self-hosted runner or workstation with kubeconfig access.

Use the Task name and transaction ID printed by the workflow:

```bash
export TASK_NAME=kontxt-smoke-...
export TXN_ID=txn-...
```

The Task should persist safe transaction metadata:

```bash
kubectl -n default get task "$TASK_NAME" -o json \
  | jq -e --arg txn "$TXN_ID" '
      .spec.transaction.profile == "kontxt"
      and .spec.transaction.id == $txn
      and .metadata.labels["orka.ai/transaction-id"] == $txn
      and .metadata.annotations["orka.ai/transaction-id"] == $txn
    '
```

Expected result: `jq` exits successfully and prints `true`.

The Job and Pod should inherit the transaction ID:

```bash
kubectl -n default get jobs -l "orka.ai/task=${TASK_NAME}" -o json \
  | jq -e --arg txn "$TXN_ID" '
      .items[0].metadata.labels["orka.ai/transaction-id"] == $txn
      and .items[0].spec.template.metadata.labels["orka.ai/transaction-id"] == $txn
    '

kubectl -n default get pods -l "orka.ai/task=${TASK_NAME}" -o json \
  | jq -e --arg txn "$TXN_ID" '
      .items[0].metadata.labels["orka.ai/transaction-id"] == $txn
    '
```

If your Orka CLI is already authenticated with a normal ServiceAccount/OIDC token, you can also inspect the transaction metadata:

```bash
orka task list --server "$ORKA_API" --transaction "$TXN_ID"
orka task get --server "$ORKA_API" --show-transaction "$TASK_NAME"
orka audit trace --server "$ORKA_API" "$TXN_ID"
```

## Optional: Validate enforce-mode `tctx` rejection

After the happy path passes, switch Orka from `audit` to `enforce` and submit a request that violates the signed `tctx.namespace=default` constraint.

In the smoke test, kontxt signs this request context:

```json
{
  "namespace": "default",
  "taskType": "container"
}
```

When Orka is in `enforce` mode, a request for namespace `not-default` should fail.

```bash
kubectl -n "$ORKA_NS" set env deployment/"$ORKA_DEPLOY" \
  ORKA_CONTEXT_TOKEN_AUTHZ_MODE=enforce
kubectl -n "$ORKA_NS" rollout status deployment/"$ORKA_DEPLOY" --timeout=5m
```

Create a Kubernetes-style manifest that intentionally changes the namespace:

```bash
cat >/tmp/orka-kontxt-denied-task.yaml <<EOF_YAML
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: kontxt-smoke-denied
  namespace: not-default
spec:
  type: container
  image: busybox:1.36
  command:
    - /bin/sh
    - -c
  args:
    - echo should-not-run
EOF_YAML

kubectl create --dry-run=client --validate=false -f /tmp/orka-kontxt-denied-task.yaml -o json \
  | jq '{name: .metadata.name, namespace: .metadata.namespace} + .spec' \
  >/tmp/orka-kontxt-denied-task-request.json
```

Build a temporary header file from the same TxToken used for the successful request, then submit the denied request:

```bash
export TXN_HEADER_FILE="${TXN_HEADER_FILE:-$(mktemp)}"
chmod 600 "$TXN_HEADER_FILE"
printf 'Txn-Token: %s\n' "$(cat "$TXN_TOKEN_FILE")" > "$TXN_HEADER_FILE"

status="$(curl -sS -o /tmp/orka-kontxt-denied-response.json -w '%{http_code}' \
  -X POST "${ORKA_API%/}/api/v1/tasks" \
  -H "@${TXN_HEADER_FILE}" \
  -H 'Content-Type: application/json' \
  --data @/tmp/orka-kontxt-denied-task-request.json)"
echo "$status"
# expected: 403
test "$status" = 403
```

If the denied request returns `201`, the token probably did not include a matching `tctx.namespace`, or Orka is still in `audit`/`off` mode.

## Other identity sources

This quickstart uses GitHub Actions OIDC for the runnable smoke test. The same kontxt flow can use other subject-token sources:

| Source | Use when |
|---|---|
| Microsoft Entra Agent ID / Workload ID | Enterprise-managed agents or services need governed identity. |
| Kubernetes projected ServiceAccount tokens | In-cluster workloads need TxTokens without an external OIDC provider. |
| Other OIDC/JWT issuers | Your organization already has a trusted issuer that kontxt TTS can validate. |

For all of these, only the subject-token acquisition and TTS trust policy change. Orka still validates the resulting kontxt TxToken from the `Txn-Token` header.

## Troubleshooting

| Symptom | Likely cause | Check |
|---|---|---|
| GitHub OIDC request fails | Workflow is missing `permissions.id-token: write`, or the step is not running inside GitHub Actions. | Confirm the workflow permissions and the `ACTIONS_ID_TOKEN_REQUEST_*` environment variables. |
| TTS exchange returns `401`/`403` | kontxt TTS does not trust the subject-token issuer/audience, the subject token expired, or the requested scope/context is not allowed. | Confirm TTS subject issuer/audience and the requested `GITHUB_OIDC_AUDIENCE`. |
| Orka returns `401 Unauthorized` | Missing/invalid `Txn-Token`, wrong issuer/audience/JWKS, expired token, unknown `kid`, or missing `typ: txntoken+jwt`. | Confirm Orka env vars, JWKS reachability, and token claims with your kontxt tooling. |
| Orka returns `403 Forbidden` | `enforce` mode denied missing scope or `tctx` mismatch. | Check controller logs for safe context-token authorization failure fields. |
| Task created but no `spec.transaction` | Request authenticated as ServiceAccount/OIDC instead of context token. | Use the raw `Txn-Token` header. |
| Job/Pod missing transaction labels | The controller may not have reconciled the Task yet, or the Task was created without transaction metadata. | Wait for the Job/Pod and inspect the Task first. |

## Cleanup

Delete local token/header files when done:

```bash
rm -f "${TXN_TOKEN_FILE:-}" "${TXN_HEADER_FILE:-}" "${BAD_TOKEN_FILE:-}" "${BAD_HEADER_FILE:-}"
```

Delete the smoke Task if you do not want to keep it for audit examples:

```bash
kubectl -n default delete task "$TASK_NAME" --ignore-not-found=true
```
