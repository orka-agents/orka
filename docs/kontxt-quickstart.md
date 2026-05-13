# Kontxt quickstart: GitHub OIDC to Orka TxTokens

This guide is for operators who are new to `kontxt` TxTokens and want a concrete end-to-end validation path for Orka. The recommended smoke test uses the same identity pattern Orka validates in live automation:

```text
GitHub Actions OIDC token
  → kontxt TTS token exchange
  → kontxt TxToken
  → Orka API call with Txn-Token
  → Task / Job / Pod transaction metadata
```

This guide installs/configures the **Orka side** of the integration. It assumes a kontxt TTS/issuer already exists and is configured to trust GitHub Actions OIDC as a subject-token issuer. If you are not using GitHub Actions, use the [existing TxToken or other subject token](#path-b-existing-txtoken-or-other-subject-token) path after reading the concepts.

For deeper reference material after the first smoke test, see [Kontxt TxToken integration](kontxt.md).

## The problem this solves

CI systems, developer portals, and automation platforms often call Orka with a bearer token that proves **who** is calling, but not enough about **why this specific request is allowed**. Without transaction-scoped context, platforms tend to fall back to one of three weak patterns:

- long-lived API keys or broad ServiceAccount tokens shared across many jobs;
- ad-hoc headers such as `X-Repo`, `X-Actor`, or `X-Workflow` that downstream services must trust blindly;
- audit trails that require stitching together GitHub logs, Orka Tasks, Kubernetes Jobs/Pods, and downstream service logs by time and guesswork.

That is risky for autonomous or delegated work. A coordinator agent may create child Tasks, workers may call tools, and downstream services need to know whether each hop is still inside the original request's allowed scope. Identity alone is not enough; each hop also needs signed request context and a stable transaction ID.

## How kontxt + Orka solves it

`kontxt` turns an existing identity token, such as a GitHub Actions OIDC JWT, into a short-lived TxToken that carries signed transaction context. Orka consumes that TxToken at the API boundary and makes it Kubernetes-native:

1. **Request-level authorization** — Orka verifies the TxToken signature, issuer, audience, time claims, and required kontxt claims, then checks operation scopes such as `orka:tasks:create`. In `enforce` mode, selected signed `tctx` fields constrain the actual request, for example namespace, task type, agent, repository, provider, model, and allowed tools.
2. **Non-expanding delegation** — When agents delegate work, Orka can exchange a parent TxToken through kontxt TTS for a child TxToken. Child scopes must be a subset of the parent transaction scopes, so delegated work cannot silently gain privilege.
3. **End-to-end audit correlation** — Orka stamps safe transaction metadata onto Tasks, Jobs, Pods, worker environment, and CLI/audit views. Operators can follow the same `txn` across the workflow without storing raw TxTokens or full sensitive context.

In the recommended example below, GitHub proves the workflow identity, kontxt signs the transaction context, and Orka enforces and records that context as the work moves through Kubernetes.

## Concepts in one page

| Term | What it means in Orka |
|---|---|
| **Subject token** | A token that proves who the caller is before a TxToken exists. In the recommended path this is a GitHub Actions OIDC JWT. |
| **GitHub Actions OIDC** | A short-lived JWT minted by GitHub when a workflow has `id-token: write`. kontxt TTS verifies this token before issuing a TxToken. |
| **kontxt TTS** | The Token Translation Service. It verifies a subject token and issues a signed TxToken with a transaction ID, scope, and signed context. |
| **TxToken** | A short-lived JWT with `typ: txntoken+jwt`. Orka reads it from the `Txn-Token` header by default. |
| **JWKS** | Public signing keys for the TxToken issuer. Orka uses the JWKS URL to verify TxToken signatures. |
| **`txn`** | The transaction ID. Orka copies it into Task, Job, Pod, and CLI/audit metadata so one request can be followed end to end. |
| **`scope`** | Space-separated permissions in the TxToken. Orka can require scopes such as `orka:tasks:create`, `orka:providers:use`, or `orka:tools:use`. |
| **`tctx`** | Signed transaction context. Orka uses selected fields such as namespace, task type, agent, repo, branch, provider, model, and allowed tools as request constraints. |
| **`rctx`** | Signed requester context. Orka treats this as sensitive and stores only safe digests/selected fields. |
| **Child token** | A TxToken exchanged from a parent TxToken for delegated work. Orka requires child scopes to be a subset of the parent transaction scopes. |

The important security rule: **Orka never stores raw TxTokens in Task specs/status, logs, labels, annotations, metrics, artifacts, or durable memory.** Delegated raw child tokens are stored only in owner-referenced Kubernetes Secrets so workers can mount them.

## What you will validate

By the end of this guide you should have validated:

1. Orka rejects unauthenticated API calls.
2. GitHub Actions can mint an OIDC subject token without a long-lived secret.
3. kontxt TTS exchanges that subject token for a TxToken.
4. Orka accepts the valid TxToken from `Txn-Token`.
5. Orka stamps verified identity in `spec.requestedBy`.
6. Orka stamps safe transaction metadata in `spec.transaction`, labels, and annotations.
7. Jobs and Pods inherit the transaction ID.
8. Tampered TxTokens are rejected.
9. In `enforce` mode, a signed `tctx` mismatch is rejected.

## Prerequisites

You need:

- A Kubernetes cluster with Orka installed. Start with [Getting Started](getting-started.md) if Orka is not installed yet.
- A kontxt issuer/TTS that Orka and the smoke-test runner can reach.
- A kontxt TTS configuration that trusts GitHub Actions OIDC:
  - subject issuer: `https://token.actions.githubusercontent.com`
  - subject audience: the GitHub OIDC audience you request in the workflow, for example `orka-kontxt-smoke`
- The TxToken issuer URL, audience, and JWKS URL used by Orka.
- An Orka API endpoint reachable from the smoke-test runner. For GitHub-hosted runners this usually means a public/private network path to Orka; for private clusters use a self-hosted runner or run `kubectl port-forward` from a job that has kubeconfig access.
- `kubectl`, `curl`, and `jq`.

Do not run the commands with shell tracing (`set -x`) enabled. Keep token files private and delete them after the smoke test.

## 1. Capture the deployment values

Set these values for your environment. The controller deployment and service names depend on how Orka was installed, so list them first if you are unsure.

```bash
export ORKA_NS=orka-system
kubectl -n "$ORKA_NS" get deploy,svc

# Kustomize installs commonly use orka-controller-manager / orka-api.
# Helm installs commonly use <release>-controller / <release>.
export ORKA_DEPLOY=orka-controller-manager
export ORKA_API_SVC=orka-api

export KONTXT_ISSUER=https://kontxt-tts.example.test
export KONTXT_AUDIENCE=orka-api
export KONTXT_JWKS_URL=https://kontxt-tts.example.test/.well-known/jwks.json
export KONTXT_TTS_URL=https://kontxt-tts.example.test
```

The controller Pod must be able to reach `KONTXT_JWKS_URL`. If the URL is cluster-internal, use its in-cluster DNS name rather than a laptop-only address.

Optional reachability check from the cluster:

```bash
kubectl -n "$ORKA_NS" run kontxt-jwks-check \
  --rm -i --restart=Never \
  --image=curlimages/curl:8.11.1 \
  -- "$KONTXT_JWKS_URL"
```

## 2. Configure Orka to trust kontxt TxTokens

Start in `audit` authorization mode. In audit mode Orka validates TxTokens and records would-deny authorization decisions, but it does not reject otherwise valid requests because of missing scopes or context mismatches.

```bash
kubectl -n "$ORKA_NS" set env deployment/"$ORKA_DEPLOY" \
  ORKA_CONTEXT_TOKEN_PROFILE=kontxt \
  ORKA_CONTEXT_TOKEN_ISSUER="$KONTXT_ISSUER" \
  ORKA_CONTEXT_TOKEN_AUDIENCE="$KONTXT_AUDIENCE" \
  ORKA_CONTEXT_TOKEN_JWKS_URL="$KONTXT_JWKS_URL" \
  ORKA_CONTEXT_TOKEN_AUTHZ_MODE=audit

kubectl -n "$ORKA_NS" rollout status deployment/"$ORKA_DEPLOY" --timeout=5m
```

This keeps Kubernetes ServiceAccount and OIDC bearer-token authentication working. Kontxt TxTokens are read from `Txn-Token` by default. Only opt in to `Authorization: Bearer` context-token support if you need it:

```bash
kubectl -n "$ORKA_NS" set env deployment/"$ORKA_DEPLOY" \
  ORKA_CONTEXT_TOKEN_HEADERS=Txn-Token,Authorization:Bearer
```

## 3. Choose a validation path

Use **Path A** for the recommended GitHub Actions OIDC flow. Use **Path B** if you already have a TxToken or if your subject token comes from another identity provider.

### Path A: GitHub Actions OIDC recommended

Create a workflow with `id-token: write`. The example below requests a GitHub OIDC token, exchanges it through kontxt TTS, and stores the resulting TxToken in a temporary file without printing either token.

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
      # These values must match your kontxt/Orka deployment.
      ORKA_API: https://orka.example.test
      GITHUB_OIDC_AUDIENCE: orka-kontxt-smoke
      KONTXT_TTS_URL: https://kontxt-tts.example.test
      KONTXT_REQUESTING_WORKLOAD: spiffe://example.test/ns/default/sa/orka-smoke-client
    steps:
      - name: Install jq
        run: sudo apt-get update && sudo apt-get install -y jq

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
```

Continue with [Create a Task with the TxToken](#4-create-a-task-with-the-txtoken). In GitHub Actions, add the commands in sections 4 and 6 as later `run` steps in the same job so they can read `TXN_TOKEN_FILE`; run the Kubernetes metadata checks in section 5 from a runner or workstation that has kubeconfig access. The example `request_details` becomes signed `tctx`; `namespace=default` and `taskType=container` intentionally match the Task payload below so the same token will pass once you switch to `enforce` mode.

### Path B: existing TxToken or other subject token

If your platform team gave you a TxToken, save it to a private file without printing it:

```bash
export TXN_TOKEN_FILE="$(mktemp)"
chmod 600 "$TXN_TOKEN_FILE"
# Paste the token, then press Ctrl-D. Do not paste it into chat, issues, or logs.
cat > "$TXN_TOKEN_FILE"
```

If you need to exchange a non-GitHub subject token through kontxt TTS, save the subject token in a private file and request a TxToken with a narrow scope and `tctx` that matches the Task you will create:

```bash
export SUBJECT_TOKEN_FILE=/path/to/subject-token.jwt
export TXN_TOKEN_FILE="$(mktemp)"
chmod 600 "$TXN_TOKEN_FILE"

export KONTXT_REQUESTING_WORKLOAD=spiffe://example.test/ns/default/sa/orka-smoke-client
REQUEST_DETAILS="$(jq -cn '{namespace:"default",taskType:"container"}')"
REQUEST_CONTEXT="$(jq -cn '{purpose:"orka-kontxt-smoke"}')"

curl -fsS -X POST "${KONTXT_TTS_URL%/}/token_endpoint" \
  -H "X-Kontxt-Workload: ${KONTXT_REQUESTING_WORKLOAD}" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'grant_type=urn:ietf:params:oauth:grant-type:token-exchange' \
  --data-urlencode "subject_token@${SUBJECT_TOKEN_FILE}" \
  --data-urlencode 'subject_token_type=urn:ietf:params:oauth:token-type:id_token' \
  --data-urlencode 'requested_token_type=urn:ietf:params:oauth:token-type:txn_token' \
  --data-urlencode 'scope=orka:tasks:create' \
  --data-urlencode "request_details=${REQUEST_DETAILS}" \
  --data-urlencode "request_context=${REQUEST_CONTEXT}" \
  | jq -er '.access_token' > "$TXN_TOKEN_FILE"
```

Change `subject_token_type` if your kontxt TTS expects a different subject token type.

## 4. Create a Task with the TxToken

Set `ORKA_API` to a reachable Orka API endpoint. For a local operator smoke test this can be a port-forward:

```bash
kubectl -n "$ORKA_NS" port-forward "svc/${ORKA_API_SVC}" 18080:8080
export ORKA_API=http://127.0.0.1:18080
```

For a GitHub Actions smoke test, `ORKA_API` should already point at the endpoint reachable by the runner; put the rest of this section in a later `run` step in the same job.

First confirm unauthenticated requests are rejected:

```bash
status="$(curl -sS -o /tmp/orka-unauth.json -w '%{http_code}' \
  "${ORKA_API}/api/v1/tasks?namespace=default")"
test "$status" = 401
```

Create a tiny container Task as a Kubernetes-style manifest:

```bash
export TASK_NAME="kontxt-smoke-$(date +%s)"
cat >/tmp/orka-kontxt-task.yaml <<EOF_YAML
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: ${TASK_NAME}
  namespace: default
spec:
  type: container
  image: busybox:1.36
  command:
    - /bin/sh
    - -c
  args:
    - echo kontxt-smoke
EOF_YAML
```

Do **not** `kubectl apply` this smoke-test manifest. Sending it through the Orka REST API is what validates the `Txn-Token` and lets Orka stamp `spec.requestedBy` / `spec.transaction`. Convert the manifest into the REST create payload at the API boundary:

```bash
kubectl create --dry-run=client -f /tmp/orka-kontxt-task.yaml -o json \
  | jq '{name: .metadata.name, namespace: .metadata.namespace} + .spec' \
  >/tmp/orka-kontxt-task-request.json
```

Build a temporary curl header file so the token does not appear in shell history:

```bash
export TXN_HEADER_FILE="$(mktemp)"
chmod 600 "$TXN_HEADER_FILE"
printf 'Txn-Token: %s\n' "$(cat "$TXN_TOKEN_FILE")" > "$TXN_HEADER_FILE"
```

Create the Task:

```bash
status="$(curl -sS -o /tmp/orka-kontxt-task-response.json -w '%{http_code}' \
  -X POST "${ORKA_API}/api/v1/tasks" \
  -H "@${TXN_HEADER_FILE}" \
  -H 'Content-Type: application/json' \
  --data @/tmp/orka-kontxt-task-request.json)"
test "$status" = 201
```

Validate requester and transaction metadata in the response:

```bash
jq -e '
  (.spec.requestedBy.subject // "") != ""
  and .spec.transaction.profile == "kontxt"
  and (.spec.transaction.id // "") != ""
  and (.spec.transaction.scope // "" | contains("orka:tasks:create"))
  and (.spec.transaction.contextDigest // "" | startswith("sha256:"))
  and (.spec.transaction.requesterContextDigest // "" | startswith("sha256:"))
  and .metadata.labels["orka.ai/transaction-id"] == .spec.transaction.id
  and .metadata.annotations["orka.ai/transaction-id"] == .spec.transaction.id
' /tmp/orka-kontxt-task-response.json
```

## 5. Verify Kubernetes audit correlation

The Task should persist the same safe metadata:

```bash
kubectl -n default get task "$TASK_NAME" -o json \
  | jq -e '
      .spec.transaction.profile == "kontxt"
      and (.spec.transaction.id // "") != ""
      and .metadata.labels["orka.ai/transaction-id"] == .spec.transaction.id
      and .metadata.annotations["orka.ai/transaction-id"] == .spec.transaction.id
    '
```

The Job and Pod should inherit the transaction ID:

```bash
TXN_ID="$(jq -r '.spec.transaction.id' /tmp/orka-kontxt-task-response.json)"

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

## 6. Validate rejection paths

A tampered TxToken should be rejected with `401`:

```bash
BAD_TOKEN_FILE="$(mktemp)"
BAD_HEADER_FILE="$(mktemp)"
chmod 600 "$BAD_TOKEN_FILE" "$BAD_HEADER_FILE"
cp "$TXN_TOKEN_FILE" "$BAD_TOKEN_FILE"
printf 'tampered' >> "$BAD_TOKEN_FILE"
printf 'Txn-Token: %s\n' "$(cat "$BAD_TOKEN_FILE")" > "$BAD_HEADER_FILE"

status="$(curl -sS -o /tmp/orka-kontxt-tampered-response.json -w '%{http_code}' \
  -X POST "${ORKA_API}/api/v1/tasks" \
  -H "@${BAD_HEADER_FILE}" \
  -H 'Content-Type: application/json' \
  --data @/tmp/orka-kontxt-task-request.json)"
test "$status" = 401
```

Now switch to `enforce` mode and validate that signed `tctx` constraints are enforced. This assumes the smoke-test token has `tctx.namespace=default`.

```bash
kubectl -n "$ORKA_NS" set env deployment/"$ORKA_DEPLOY" \
  ORKA_CONTEXT_TOKEN_AUTHZ_MODE=enforce
kubectl -n "$ORKA_NS" rollout status deployment/"$ORKA_DEPLOY" --timeout=5m

sed 's/namespace: default/namespace: not-default/' \
  /tmp/orka-kontxt-task.yaml >/tmp/orka-kontxt-denied-task.yaml

kubectl create --dry-run=client -f /tmp/orka-kontxt-denied-task.yaml -o json \
  | jq '{name: "kontxt-smoke-denied", namespace: .metadata.namespace} + .spec' \
  >/tmp/orka-kontxt-denied-task-request.json

status="$(curl -sS -o /tmp/orka-kontxt-denied-response.json -w '%{http_code}' \
  -X POST "${ORKA_API}/api/v1/tasks" \
  -H "@${TXN_HEADER_FILE}" \
  -H 'Content-Type: application/json' \
  --data @/tmp/orka-kontxt-denied-task-request.json)"
test "$status" = 403
```

If the denied request returns `201`, the token probably did not include a matching `tctx.namespace`, or Orka is still in `audit`/`off` mode.

## 7. Optional: enable TTS-backed delegation and outbound calls

The smoke test above validates API ingress and audit correlation. If agents need to delegate work or call downstream HTTP Tools with TxTokens, configure the controller with the TTS URL and worker scopes:

```bash
kubectl -n "$ORKA_NS" set env deployment/"$ORKA_DEPLOY" \
  ORKA_CONTEXT_TOKEN_TTS_URL="$KONTXT_TTS_URL" \
  ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_TYPE=urn:ietf:params:oauth:token-type:txn_token \
  ORKA_CONTEXT_TOKEN_CHILD_SCOPE=orka:agents:run \
  ORKA_CONTEXT_TOKEN_OUTBOUND_SCOPE=orka:tools:use
kubectl -n "$ORKA_NS" rollout status deployment/"$ORKA_DEPLOY" --timeout=5m
```

For delegation, the worker must have a mounted subject token file. Orka automatically mounts one for child Tasks that reference an Orka-owned transaction-token Secret. A successful TTS-backed child delegation should produce:

- a child Task with the same `spec.transaction.id` as the parent;
- `orka.ai/transaction-id` labels/annotations on the child Task, Job, and Pod;
- an `orka.ai/transaction-token-secret` annotation on the child Task;
- a child TxToken whose scope is the configured child scope;
- a failure before child creation if the requested child scope is not present in the parent transaction scopes.

## Troubleshooting

| Symptom | Likely cause | Check |
|---|---|---|
| GitHub OIDC request fails | Workflow is missing `permissions.id-token: write`, or the step is not running inside GitHub Actions. | Confirm the workflow permissions and the `ACTIONS_ID_TOKEN_REQUEST_*` environment variables. |
| TTS exchange returns `401`/`403` | kontxt TTS does not trust the GitHub OIDC issuer/audience, the subject token expired, or the requested scope/context is not allowed. | Confirm TTS subject issuer/audience and the requested `GITHUB_OIDC_AUDIENCE`. |
| Orka returns `401 Unauthorized` | Missing/invalid `Txn-Token`, wrong issuer/audience/JWKS, expired token, unknown `kid`, or missing `typ: txntoken+jwt`. | Confirm Orka env vars, JWKS reachability, and token claims with your kontxt tooling. |
| Orka returns `403 Forbidden` | `enforce` mode denied missing scope or `tctx` mismatch. | Check controller logs for safe context-token authorization failure fields. |
| Task created but no `spec.transaction` | Request authenticated as ServiceAccount/OIDC instead of context token. | Use the raw `Txn-Token` header or explicitly configure context-token bearer support. |
| Job/Pod missing transaction labels | The controller may not have reconciled the Task yet, or the Task was created without transaction metadata. | Wait for the Job/Pod and inspect the Task first. |
| Child Task has no token Secret | The worker did not have `ORKA_CONTEXT_TOKEN_TTS_URL`, `ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_FILE`, and `ORKA_CONTEXT_TOKEN_CHILD_SCOPE`. | Inspect child/parent worker env and the parent Task transaction scopes. |
| Downstream service rejects token | Downstream verifier expects a different audience, issuer, transaction ID, or scope. | Align downstream verifier config with the TTS-issued token. |

## Cleanup

Delete local token/header files when done:

```bash
rm -f "$TXN_TOKEN_FILE" "$TXN_HEADER_FILE" "${BAD_TOKEN_FILE:-}" "${BAD_HEADER_FILE:-}"
```

Delete the smoke Task if you do not want to keep it for audit examples:

```bash
kubectl -n default delete task "$TASK_NAME" --ignore-not-found=true
```
