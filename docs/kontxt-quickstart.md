# Kontxt quickstart: installation and validation

This guide is for operators who are new to `kontxt` TxTokens and want to make Orka accept, authorize, and audit requests that carry transaction identity. It installs/configures the Orka side of the integration; it assumes a kontxt TTS or issuer is already available. The setup stays small: configure Orka to trust that issuer, mint or obtain one TxToken, create one Task, and verify the metadata that Orka stamps on Kubernetes resources.

For deeper reference material after the first smoke test, see [Kontxt TxToken integration](kontxt.md).

## Concepts in one page

| Term | What it means in Orka |
|---|---|
| **Subject token** | A token that proves who the caller is before a TxToken exists. Common examples are a GitHub Actions OIDC token, workload identity token, or another OIDC JWT trusted by kontxt TTS. |
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
2. Orka accepts a valid kontxt TxToken from `Txn-Token`.
3. Orka stamps verified identity in `spec.requestedBy`.
4. Orka stamps safe transaction metadata in `spec.transaction`, labels, and annotations.
5. Jobs and Pods inherit the transaction ID.
6. Tampered TxTokens are rejected.
7. In `enforce` mode, a signed `tctx` mismatch is rejected.

## Prerequisites

You need:

- A Kubernetes cluster with Orka installed. Start with [Getting Started](getting-started.md) if Orka is not installed yet.
- A kontxt issuer/TTS that Orka can reach.
- The TxToken issuer URL, audience, and JWKS URL.
- Either:
  - an existing smoke-test TxToken, or
  - a subject token accepted by kontxt TTS so you can exchange it for a TxToken.
- `kubectl`, `curl`, and `jq` on your workstation.

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

## 3. Obtain a smoke-test TxToken

If your platform team gave you a TxToken, save it to a private file without printing it:

```bash
export TXN_TOKEN_FILE="$(mktemp)"
chmod 600 "$TXN_TOKEN_FILE"
# Paste the token, then press Ctrl-D. Do not paste it into chat, issues, or logs.
cat > "$TXN_TOKEN_FILE"
```

If you need to exchange a subject token through kontxt TTS, save the subject token in a private file and request a TxToken with a narrow scope and `tctx` that matches the Task you will create:

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

The example `REQUEST_DETAILS` becomes signed `tctx`. The `namespace` and `taskType` values intentionally match the Task payload below so the same token will pass once you switch to `enforce` mode. Change `subject_token_type` if your kontxt TTS expects a different subject token type.

## 4. Port-forward the Orka API

```bash
kubectl -n "$ORKA_NS" port-forward "svc/${ORKA_API_SVC}" 18080:8080
```

In another terminal:

```bash
export ORKA_API=http://127.0.0.1:18080
```

First confirm unauthenticated requests are rejected:

```bash
status="$(curl -sS -o /tmp/orka-unauth.json -w '%{http_code}' \
  "${ORKA_API}/api/v1/tasks?namespace=default")"
test "$status" = 401
```

## 5. Create a Task with the TxToken

Create a tiny container Task:

```bash
export TASK_NAME="kontxt-smoke-$(date +%s)"
cat >/tmp/orka-kontxt-task.json <<EOF_JSON
{
  "name": "${TASK_NAME}",
  "namespace": "default",
  "type": "container",
  "image": "busybox:1.36",
  "command": ["/bin/sh", "-c"],
  "args": ["echo kontxt-smoke"]
}
EOF_JSON
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
  --data @/tmp/orka-kontxt-task.json)"
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

## 6. Verify Kubernetes audit correlation

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

## 7. Validate rejection paths

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
  --data @/tmp/orka-kontxt-task.json)"
test "$status" = 401
```

Now switch to `enforce` mode and validate that signed `tctx` constraints are enforced. This assumes the smoke-test token has `tctx.namespace=default`.

```bash
kubectl -n "$ORKA_NS" set env deployment/"$ORKA_DEPLOY" \
  ORKA_CONTEXT_TOKEN_AUTHZ_MODE=enforce
kubectl -n "$ORKA_NS" rollout status deployment/"$ORKA_DEPLOY" --timeout=5m

jq '.name = "kontxt-smoke-denied" | .namespace = "not-default"' \
  /tmp/orka-kontxt-task.json >/tmp/orka-kontxt-denied-task.json

status="$(curl -sS -o /tmp/orka-kontxt-denied-response.json -w '%{http_code}' \
  -X POST "${ORKA_API}/api/v1/tasks" \
  -H "@${TXN_HEADER_FILE}" \
  -H 'Content-Type: application/json' \
  --data @/tmp/orka-kontxt-denied-task.json)"
test "$status" = 403
```

If the denied request returns `201`, the token probably did not include a matching `tctx.namespace`, or Orka is still in `audit`/`off` mode.

## 8. Optional: enable TTS-backed delegation and outbound calls

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
| `401 Unauthorized` | Missing/invalid `Txn-Token`, wrong issuer/audience/JWKS, expired token, unknown `kid`, or missing `typ: txntoken+jwt`. | Confirm Orka env vars, JWKS reachability, and token claims with your kontxt tooling. |
| `403 Forbidden` | `enforce` mode denied missing scope or `tctx` mismatch. | Check controller logs for safe context-token authorization failure fields. |
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
