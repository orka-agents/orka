---
slug: /kontxt-quickstart
---

# Kontxt quickstart: use Kubernetes identity to call Orka without long-lived tokens

This guide shows one successful Orka + kontxt run using a Kubernetes projected ServiceAccount token as the caller identity. The caller runs inside the cluster, exchanges its bound ServiceAccount token for a short-lived kontxt TxToken, and calls Orka with `Txn-Token`.

We are going to run through a basic example: clone a repository, verify its root `LICENSE` is MIT, and prove the worker received safe transaction metadata.

It is only an example workload; replace it with your own build, policy, compliance, or agent task. The kontxt/Orka transaction-token flow is the important part.

## If you only remember one thing

```text
Kubernetes proves who the caller Pod is.
kontxt signs why this request is allowed.
Orka enforces and audits the signed transaction.
```

In this quickstart:

```text
Initial caller Job Pod
  → projected Kubernetes ServiceAccount token
  → kontxt TTS
  → kontxt TxToken
  → Orka API with Txn-Token
  → Orka-created Task / Job / worker Pod with transaction metadata
```

This guide deliberately covers only the runnable Kubernetes ServiceAccount smoke-test path. For the broader model, alternate identity providers, authz modes, delegation, outbound propagation, and full `scope`/`tctx` behavior, see [Kontxt TxToken integration](../concepts/kontxt.md).

## What you will validate

You are done when:

- a low-privilege caller Job uses a projected ServiceAccount token rather than a long-lived secret;
- the caller Job exchanges that subject token with kontxt TTS for a TxToken;
- Orka creates a Task when called with the valid TxToken and returns `201`;
- the response includes `spec.transaction.profile=kontxt`;
- the response includes a transaction ID and safe transaction digests;
- the Orka-created Task/Job/Pod carry `orka.ai/transaction-id`;
- the worker Pod finishes successfully and its result proves the MIT license check ran with transaction metadata.

## Prerequisites

You need:

- a Kubernetes cluster with Orka installed;
- kubeconfig access for the setup and validation commands;
- permission to install [`kontxt`](https://github.com/aramase/kontxt) TTS with Helm, or an existing kontxt TTS/issuer you can use;
- a Kubernetes ServiceAccount token issuer/JWKS configuration that kontxt TTS can trust;
- an Orka API Service reachable from the in-cluster caller Job;
- worker Pod network access to the repository being checked, or an internal mirror you can set as `REPO_URL`;
- `helm`, `kubectl`, `curl`, and `jq` on your workstation.

> **kontxt TTS deployment note**
>
> kontxt TTS is separate from Orka. It is provided by [`github.com/aramase/kontxt`](https://github.com/aramase/kontxt); see the upstream [`cmd/tts`](https://github.com/aramase/kontxt/tree/main/cmd/tts) service and Kubernetes/Helm quick start in that repo. It can run in Kubernetes as a Deployment/Service/Pod, or it can run outside the cluster as a centralized service. Orka only needs the TTS/JWKS URLs for the kontxt service you operate.

Fill in these values before starting. The defaults assume kontxt and Orka are exposed as in-cluster Services.

```bash
export ORKA_NS=orka-system
export ORKA_DEPLOY=orka-controller-manager
export ORKA_API_SVC=orka-api
export ORKA_API=http://orka-api.orka-system.svc:8080

export KONTXT_NS=kontxt-system
export KONTXT_RELEASE=kontxt
export KONTXT_TRUST_DOMAIN=orka-api
export KONTXT_TTS_URL=http://kontxt-tts.kontxt-system.svc
export KONTXT_ISSUER=https://kontxt-tts.kontxt-system.svc
export KONTXT_AUDIENCE=orka-api
export KONTXT_JWKS_URL=http://kontxt-tts.kontxt-system.svc/.well-known/jwks.json

export CALLER_NS=default
export CALLER_SA=orka-kontxt-caller
export KONTXT_SUBJECT_AUDIENCE=kontxt-tts
export KONTXT_REQUESTING_WORKLOAD="spiffe://example.test/ns/${CALLER_NS}/sa/${CALLER_SA}"
```

Token safety rules:

- do not print Kubernetes projected ServiceAccount tokens or TxTokens;
- do not use `set -x` in these scripts;
- store tokens only in temporary files created with `umask 077` or mode `0600`;
- log transaction IDs and digests, not tokens.

Orka never stores raw TxTokens in Task specs/status, logs, labels, annotations, metrics, artifacts, or durable memory.

Before running the smoke test, make sure the installed Orka `Task` CRD is current. Older CRDs can silently prune `spec.transaction`, which makes the REST call appear to succeed while the persisted Task is missing transaction metadata.

From the root of this repository, apply the current Task CRD:

```bash
kubectl apply -f config/crd/bases/core.orka.ai_tasks.yaml
```

Expected result: `tasks.core.orka.ai` is configured or unchanged.

## Step 1: Install kontxt TTS with Kubernetes ServiceAccount-token trust

Run this step from a workstation with kubeconfig access. If your platform team already operates kontxt TTS, skip the install and use their TTS/JWKS URLs, but confirm that TTS trusts your Kubernetes ServiceAccount token issuer and `KONTXT_SUBJECT_AUDIENCE`.

Discover the Kubernetes ServiceAccount token issuer advertised by your API server:

```bash
kubectl get --raw /.well-known/openid-configuration | jq '{issuer, jwks_uri}'

export KUBERNETES_SERVICEACCOUNT_ISSUER="$(
  kubectl get --raw /.well-known/openid-configuration | jq -r .issuer
)"

test -n "$KUBERNETES_SERVICEACCOUNT_ISSUER"
```

Expected result: `KUBERNETES_SERVICEACCOUNT_ISSUER` is the `iss` value that appears in projected ServiceAccount tokens for this cluster.

By default, let kontxt TTS discover the issuer metadata from the issuer URL:

```bash
export KUBERNETES_SERVICEACCOUNT_DISCOVERY_URL="${KUBERNETES_SERVICEACCOUNT_ISSUER%/}/.well-known/openid-configuration"
```

If the issuer URL or its JWKS endpoint is not reachable from the kontxt TTS Pod, fix issuer/JWKS reachability or use the smoke-test mirror below. See [Kubernetes ServiceAccount token trust](../concepts/kontxt.md#kubernetes-serviceaccount-token-trust) for the trust model and production options.

> **Static discovery mirror is for smoke tests and dev clusters**
>
> The following mirror snapshots the current Kubernetes JWKS into a ConfigMap and serves it over an in-cluster Service. Use it only when you understand the JWKS rotation tradeoff described in the reference doc.

```bash
export KUBERNETES_SERVICEACCOUNT_DISCOVERY_URL="http://kubernetes-oidc-discovery.${KONTXT_NS}.svc/.well-known/openid-configuration"
export KUBERNETES_SERVICEACCOUNT_JWKS_URL="http://kubernetes-oidc-discovery.${KONTXT_NS}.svc/openid/v1/jwks"

kubectl create namespace "$KONTXT_NS" --dry-run=client -o yaml | kubectl apply -f -

kubectl get --raw /openid/v1/jwks > /tmp/kubernetes-sa-jwks.json
jq -n \
  --arg issuer "$KUBERNETES_SERVICEACCOUNT_ISSUER" \
  --arg jwks_uri "$KUBERNETES_SERVICEACCOUNT_JWKS_URL" \
  '{
    issuer: $issuer,
    jwks_uri: $jwks_uri,
    response_types_supported: ["id_token"],
    subject_types_supported: ["public"],
    id_token_signing_alg_values_supported: ["RS256"]
  }' > /tmp/kubernetes-sa-openid-configuration.json

kubectl -n "$KONTXT_NS" create configmap kubernetes-oidc-discovery \
  --from-file=openid-configuration=/tmp/kubernetes-sa-openid-configuration.json \
  --from-file=jwks=/tmp/kubernetes-sa-jwks.json \
  --dry-run=client -o yaml \
  | kubectl apply -f -

cat <<EOF_YAML | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kubernetes-oidc-discovery
  namespace: ${KONTXT_NS}
  labels:
    app.kubernetes.io/name: kubernetes-oidc-discovery
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: kubernetes-oidc-discovery
  template:
    metadata:
      labels:
        app.kubernetes.io/name: kubernetes-oidc-discovery
    spec:
      containers:
        - name: httpd
          image: busybox:1.36
          imagePullPolicy: IfNotPresent
          command: ["sh", "-c", "httpd -f -p 8080 -h /www"]
          ports:
            - name: http
              containerPort: 8080
          readinessProbe:
            httpGet:
              path: /.well-known/openid-configuration
              port: http
            initialDelaySeconds: 1
            periodSeconds: 5
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            runAsNonRoot: true
            runAsUser: 65534
            runAsGroup: 65534
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: oidc-documents
              mountPath: /www
              readOnly: true
      volumes:
        - name: oidc-documents
          configMap:
            name: kubernetes-oidc-discovery
            items:
              - key: openid-configuration
                path: .well-known/openid-configuration
              - key: jwks
                path: openid/v1/jwks
---
apiVersion: v1
kind: Service
metadata:
  name: kubernetes-oidc-discovery
  namespace: ${KONTXT_NS}
  labels:
    app.kubernetes.io/name: kubernetes-oidc-discovery
spec:
  selector:
    app.kubernetes.io/name: kubernetes-oidc-discovery
  ports:
    - name: http
      port: 80
      targetPort: http
EOF_YAML

kubectl -n "$KONTXT_NS" rollout status deploy/kubernetes-oidc-discovery --timeout=2m
```

Create a Helm values file that configures kontxt TTS to issue Orka-scoped TxTokens and trust projected Kubernetes ServiceAccount tokens with the audience used later in this quickstart:

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
          url: "${KUBERNETES_SERVICEACCOUNT_ISSUER}"
          discoveryURL: "${KUBERNETES_SERVICEACCOUNT_DISCOVERY_URL}"
          audiences:
            - "${KONTXT_SUBJECT_AUDIENCE}"
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

If you do not use the mirror and instead need TTS to trust the Kubernetes API server CA for HTTPS issuer discovery, patching the CA environment before the restart is an alternative:

```bash
kubectl -n "$KONTXT_NS" set env deploy/kontxt-tts \
  SSL_CERT_FILE=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt
```

Restart TTS so it reads the rendered configuration, then wait for each kontxt Deployment explicitly:

```bash
kubectl -n "$KONTXT_NS" rollout restart deploy/kontxt-tts

kubectl -n "$KONTXT_NS" get pods,svc
kubectl -n "$KONTXT_NS" rollout status deploy/kontxt-tts --timeout=5m
kubectl -n "$KONTXT_NS" rollout status deploy/kontxt-extauth --timeout=5m
kubectl -n "$KONTXT_NS" rollout status deploy/kontxt-extauth-generate --timeout=5m
kubectl -n "$KONTXT_NS" rollout status deploy/kontxt-controller --timeout=5m
```

Expected result: the kontxt TTS Deployment rolls out and exposes a Service that matches `KONTXT_TTS_URL`.

## Step 2: Configure Orka to trust kontxt TxTokens

Run this step from a workstation with kubeconfig access.

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

## Step 3: Run the in-cluster caller Job end to end

Create a low-privilege ServiceAccount for the initial caller. This ServiceAccount does not need Kubernetes API RBAC for this example because the Pod calls Orka and kontxt over HTTP, not the Kubernetes API.

```bash
kubectl -n "$CALLER_NS" create serviceaccount "$CALLER_SA" --dry-run=client -o yaml \
  | kubectl apply -f -
```

If you rerun the example, remove the previous caller Job and MIT license check Task first:

```bash
kubectl -n "$CALLER_NS" delete job orka-kontxt-caller --ignore-not-found=true
kubectl -n default delete task kontxt-mit-license-check --ignore-not-found=true
```

Create the one-shot caller Job. The Job is intentionally limited to the happy path so each step is visible:

1. exchange the projected Kubernetes ServiceAccount token for a kontxt TxToken;
2. call Orka with `Txn-Token` to create `kontxt-mit-license-check`;
3. wait for the Orka worker and print the result.

Step 4 performs the detailed metadata validation after the Task exists. Adjust `KONTXT_TTS_URL`, `ORKA_API`, `KONTXT_REQUESTING_WORKLOAD`, and `REPO_URL` in the manifest if your in-cluster service names, kontxt policy, or repository source use different values.

```bash
cat >/tmp/orka-kontxt-caller-job.yaml <<'EOF_YAML'
apiVersion: batch/v1
kind: Job
metadata:
  name: orka-kontxt-caller
  namespace: default
spec:
  backoffLimit: 0
  template:
    metadata:
      labels:
        app.kubernetes.io/name: orka-kontxt-caller
    spec:
      restartPolicy: Never
      serviceAccountName: orka-kontxt-caller
      automountServiceAccountToken: false
      containers:
        - name: caller
          image: alpine:3.20
          imagePullPolicy: IfNotPresent
          env:
            - name: KONTXT_TTS_URL
              value: http://kontxt-tts.kontxt-system.svc
            - name: ORKA_API
              value: http://orka-api.orka-system.svc:8080
            - name: KONTXT_REQUESTING_WORKLOAD
              value: spiffe://example.test/ns/default/sa/orka-kontxt-caller
            - name: TASK_NAMESPACE
              value: default
            - name: TASK_NAME
              value: kontxt-mit-license-check
            - name: REPO_URL
              value: https://github.com/orka-agents/orka.git
          command:
            - /bin/sh
            - -c
          args:
            - |
              set -eu
              umask 077

              apk add --no-cache curl jq >/dev/null

              subject_token_file=/var/run/secrets/kontxt/token
              workdir="$(mktemp -d)"
              trap 'rm -rf "${workdir}"' EXIT

              tx_token_file="${workdir}/tx-token"
              tx_header_file="${workdir}/tx-header"
              worker_script="${workdir}/worker.sh"
              task_request="${workdir}/task-request.json"
              response_file="${workdir}/task-response.json"
              task_status_file="${workdir}/task-status.json"
              result_file="${workdir}/task-result.json"

              request_details="$(jq -cn --arg ns "${TASK_NAMESPACE}" '{namespace:$ns,taskType:"container"}')"
              request_context="$(jq -cn --arg repo "${REPO_URL}" '{purpose:"mit-license-check",source:"kubernetes-projected-service-account",repository:$repo}')"

              echo "1/3 exchange projected ServiceAccount token for a kontxt TxToken"
              curl -fsS -X POST "${KONTXT_TTS_URL%/}/token_endpoint" \
                -H "X-Kontxt-Workload: ${KONTXT_REQUESTING_WORKLOAD}" \
                -H 'Content-Type: application/x-www-form-urlencoded' \
                --data-urlencode 'grant_type=urn:ietf:params:oauth:grant-type:token-exchange' \
                --data-urlencode "subject_token@${subject_token_file}" \
                --data-urlencode 'subject_token_type=urn:ietf:params:oauth:token-type:jwt' \
                --data-urlencode 'requested_token_type=urn:ietf:params:oauth:token-type:txn_token' \
                --data-urlencode 'scope=orka:tasks:create orka:tasks:get' \
                --data-urlencode "request_details=${request_details}" \
                --data-urlencode "request_context=${request_context}" \
                | jq -er '.access_token' > "${tx_token_file}"
              printf 'Txn-Token: %s\n' "$(cat "${tx_token_file}")" > "${tx_header_file}"

              cat > "${worker_script}" <<'EOF_WORKER'
              set -eu
              : "${ORKA_TRANSACTION_ID:?missing transaction id}"
              test "${ORKA_TRANSACTION_PROFILE:-}" = kontxt

              repo="${REPO_URL:-https://github.com/orka-agents/orka.git}"
              export HOME=/tmp
              workdir="$(mktemp -d)"
              git clone --depth 1 "${repo}" "${workdir}" >/dev/null 2>&1

              license="$(find "${workdir}" -maxdepth 1 -iname 'LICENSE*' -print -quit)"
              test -n "${license}"
              grep -Eq '^MIT License$' "${license}"
              grep -q 'Permission is hereby granted, free of charge' "${license}"

              printf 'MIT license check passed for %s in transaction %s\n' "${repo}" "${ORKA_TRANSACTION_ID}"
              EOF_WORKER

              jq -cn \
                --arg name "${TASK_NAME}" \
                --arg namespace "${TASK_NAMESPACE}" \
                --arg image alpine/git:2.45.2 \
                --arg repo "${REPO_URL}" \
                --rawfile script "${worker_script}" \
                '{
                  name: $name,
                  namespace: $namespace,
                  type: "container",
                  image: $image,
                  command: ["/bin/sh", "-c"],
                  args: [$script],
                  env: [{name: "REPO_URL", value: $repo}]
                }' > "${task_request}"

              echo "2/3 create Orka Task ${TASK_NAME} through the Orka REST API"
              create_status="$(curl -sS -o "${response_file}" -w '%{http_code}' \
                -X POST "${ORKA_API%/}/api/v1/tasks" \
                -H "@${tx_header_file}" \
                -H 'Content-Type: application/json' \
                --data @"${task_request}")"
              echo "create status=${create_status}"
              if [ "${create_status}" != 201 ]; then
                cat "${response_file}"
                exit 1
              fi

              task_name="$(jq -er '.metadata.name' "${response_file}")"
              txn_id="$(jq -er '.spec.transaction.id' "${response_file}")"
              echo "task=${task_name}"
              echo "transactionID=${txn_id}"

              echo "3/3 wait for the Orka worker and print its result"
              phase=""
              for _ in $(seq 1 90); do
                get_status="$(curl -sS -o "${task_status_file}" -w '%{http_code}' \
                  -H "@${tx_header_file}" \
                  "${ORKA_API%/}/api/v1/tasks/${task_name}?namespace=${TASK_NAMESPACE}")"
                if [ "${get_status}" != 200 ]; then
                  cat "${task_status_file}"
                  exit 1
                fi

                phase="$(jq -r '.status.phase // ""' "${task_status_file}")"
                case "${phase}" in
                  Succeeded) break ;;
                  Failed|Cancelled)
                    cat "${task_status_file}"
                    exit 1
                    ;;
                esac
                sleep 2
              done

              echo "task phase=${phase}"
              test "${phase}" = Succeeded

              curl -fsS \
                -H "@${tx_header_file}" \
                "${ORKA_API%/}/api/v1/tasks/${task_name}/result?namespace=${TASK_NAMESPACE}" \
                > "${result_file}"
              jq -er '.result | contains("MIT license check passed for") and contains("in transaction")' "${result_file}" >/dev/null
              jq -r '.result' "${result_file}"
          volumeMounts:
            - name: kontxt-subject-token
              mountPath: /var/run/secrets/kontxt
              readOnly: true
      volumes:
        - name: kontxt-subject-token
          projected:
            sources:
              - serviceAccountToken:
                  path: token
                  audience: kontxt-tts
                  expirationSeconds: 600
EOF_YAML

kubectl apply -f /tmp/orka-kontxt-caller-job.yaml
```

Wait for the initial caller Job and inspect its logs:

```bash
kubectl -n default wait --for=condition=complete job/orka-kontxt-caller --timeout=5m
kubectl -n default logs job/orka-kontxt-caller
```

Expected safe log lines include:

```text
1/3 exchange projected ServiceAccount token for a kontxt TxToken
2/3 create Orka Task kontxt-mit-license-check through the Orka REST API
create status=201
task=kontxt-mit-license-check
transactionID=txn-...
3/3 wait for the Orka worker and print its result
task phase=Succeeded
MIT license check passed for https://github.com/orka-agents/orka.git in transaction txn-...
```

The caller Job Pod and worker Pod are separate:

```text
caller Job Pod: reads projected ServiceAccount token, exchanges it, calls Orka
worker Pod: created by Orka for Task kontxt-mit-license-check, clones the repository, checks the MIT license, and receives safe transaction metadata
```

## Step 4: Validate Kubernetes transaction metadata

The Task should persist safe transaction metadata. Extract the transaction ID from the Task and validate the Task fields:

```bash
export TASK_NAME=kontxt-mit-license-check
export TXN_ID="$(kubectl -n default get task "$TASK_NAME" -o jsonpath='{.spec.transaction.id}')"
test -n "$TXN_ID"

kubectl -n default get task "$TASK_NAME" -o json \
  | jq -e --arg txn "$TXN_ID" '
      .spec.transaction.profile == "kontxt"
      and .spec.transaction.id == $txn
      and (.spec.transaction.contextDigest // "" | startswith("sha256:"))
      and (.spec.transaction.requesterContextDigest // "" | startswith("sha256:"))
      and .metadata.labels["orka.ai/transaction-id"] == $txn
      and .metadata.annotations["orka.ai/transaction-id"] == $txn
    '
```

Expected result: `jq` exits successfully and prints `true`.

The Orka-created Job and Pod should inherit the transaction ID:

```bash
kubectl -n default get jobs -l "orka.ai/task=${TASK_NAME}" -o json \
  | jq -e --arg txn "$TXN_ID" '
      .items[0].metadata.labels["orka.ai/transaction-id"] == $txn
      and .items[0].metadata.annotations["orka.ai/transaction-id"] == $txn
      and .items[0].spec.template.metadata.labels["orka.ai/transaction-id"] == $txn
      and .items[0].spec.template.metadata.annotations["orka.ai/transaction-id"] == $txn
    '

kubectl -n default get pods -l "orka.ai/task=${TASK_NAME}" -o json \
  | jq -e --arg txn "$TXN_ID" '
      .items[0].metadata.labels["orka.ai/transaction-id"] == $txn
      and .items[0].metadata.annotations["orka.ai/transaction-id"] == $txn
    '
```

Expected result: both `jq` commands exit successfully and print `true`.

If your Orka CLI is already authenticated with a normal ServiceAccount/OIDC token, you can also inspect the transaction metadata:

```bash
orka task list --server "$ORKA_API" --transaction "$TXN_ID"
orka task get --server "$ORKA_API" --show-transaction "$TASK_NAME"
orka audit trace --server "$ORKA_API" "$TXN_ID"
```

## Optional: Validate enforce-mode `tctx` rejection

After the happy path passes, switch Orka from `audit` to `enforce` and run a second in-cluster caller Job that intentionally violates the signed `tctx.namespace=default` constraint.

In the successful caller Job, kontxt signed this request context:

```json
{
  "namespace": "default",
  "taskType": "container"
}
```

When Orka is in `enforce` mode, a request for namespace `not-default` should fail even though the caller has a valid TxToken.

```bash
kubectl -n "$ORKA_NS" set env deployment/"$ORKA_DEPLOY" \
  ORKA_CONTEXT_TOKEN_AUTHZ_MODE=enforce
kubectl -n "$ORKA_NS" rollout status deployment/"$ORKA_DEPLOY" --timeout=5m
```

Create and run the denial caller Job:

```bash
kubectl -n default delete job orka-kontxt-denied-caller --ignore-not-found=true

cat >/tmp/orka-kontxt-denied-caller-job.yaml <<'EOF_YAML'
apiVersion: batch/v1
kind: Job
metadata:
  name: orka-kontxt-denied-caller
  namespace: default
spec:
  backoffLimit: 0
  template:
    metadata:
      labels:
        app.kubernetes.io/name: orka-kontxt-denied-caller
    spec:
      restartPolicy: Never
      serviceAccountName: orka-kontxt-caller
      automountServiceAccountToken: false
      containers:
        - name: caller
          image: alpine:3.20
          imagePullPolicy: IfNotPresent
          env:
            - name: KONTXT_TTS_URL
              value: http://kontxt-tts.kontxt-system.svc
            - name: ORKA_API
              value: http://orka-api.orka-system.svc:8080
            - name: KONTXT_REQUESTING_WORKLOAD
              value: spiffe://example.test/ns/default/sa/orka-kontxt-caller
          command:
            - /bin/sh
            - -c
          args:
            - |
              set -eu
              umask 077
              cleanup() {
                rm -f \
                  "${tx_token_file:-}" \
                  "${tx_header_file:-}" \
                  "${task_request:-}" \
                  "${response_file:-}"
              }
              trap cleanup EXIT

              apk add --no-cache curl jq >/dev/null

              subject_token_file=/var/run/secrets/kontxt/token
              tx_token_file="$(mktemp)"
              tx_header_file="$(mktemp)"
              task_request="$(mktemp).json"
              response_file="$(mktemp).json"

              request_details='{"namespace":"default","taskType":"container"}'
              request_context='{"purpose":"kubernetes-projected-service-account-denied-smoke","source":"kubernetes-projected-service-account"}'

              curl -fsS -X POST "${KONTXT_TTS_URL%/}/token_endpoint" \
                -H "X-Kontxt-Workload: ${KONTXT_REQUESTING_WORKLOAD}" \
                -H 'Content-Type: application/x-www-form-urlencoded' \
                --data-urlencode 'grant_type=urn:ietf:params:oauth:grant-type:token-exchange' \
                --data-urlencode "subject_token@${subject_token_file}" \
                --data-urlencode 'subject_token_type=urn:ietf:params:oauth:token-type:jwt' \
                --data-urlencode 'requested_token_type=urn:ietf:params:oauth:token-type:txn_token' \
                --data-urlencode 'scope=orka:tasks:create' \
                --data-urlencode "request_details=${request_details}" \
                --data-urlencode "request_context=${request_context}" \
                | jq -er '.access_token' > "${tx_token_file}"

              printf 'Txn-Token: %s\n' "$(cat "${tx_token_file}")" > "${tx_header_file}"

              cat > "${task_request}" <<'EOF_JSON'
              {
                "name": "kontxt-k8s-denied",
                "namespace": "not-default",
                "type": "container",
                "image": "busybox:1.36",
                "command": ["/bin/sh", "-c"],
                "args": ["echo should-not-run"]
              }
              EOF_JSON

              status="$(curl -sS -o "${response_file}" -w '%{http_code}' \
                -X POST "${ORKA_API%/}/api/v1/tasks" \
                -H "@${tx_header_file}" \
                -H 'Content-Type: application/json' \
                --data @"${task_request}")"
              echo "denied status=${status}"
              if [ "${status}" != 403 ]; then
                cat "${response_file}"
                exit 1
              fi
          volumeMounts:
            - name: kontxt-subject-token
              mountPath: /var/run/secrets/kontxt
              readOnly: true
      volumes:
        - name: kontxt-subject-token
          projected:
            sources:
              - serviceAccountToken:
                  path: token
                  audience: kontxt-tts
                  expirationSeconds: 600
EOF_YAML

kubectl apply -f /tmp/orka-kontxt-denied-caller-job.yaml
kubectl -n default wait --for=condition=complete job/orka-kontxt-denied-caller --timeout=5m
kubectl -n default logs job/orka-kontxt-denied-caller
```

Expected safe log line:

```text
denied status=403
```

If the denied request returns `201`, the token probably did not include a matching `tctx.namespace`, or Orka is still in `audit`/`off` mode.

If you enabled enforce mode only for this check, switch back to audit mode before continuing with other experiments:

```bash
kubectl -n "$ORKA_NS" set env deployment/"$ORKA_DEPLOY" \
  ORKA_CONTEXT_TOKEN_AUTHZ_MODE=audit
kubectl -n "$ORKA_NS" rollout status deployment/"$ORKA_DEPLOY" --timeout=5m
```

## Other identity providers

This quickstart intentionally stays on the Kubernetes projected ServiceAccount path. For GitHub Actions OIDC, Microsoft Entra Workload ID, and other subject-token issuers, see [Subject-token sources for kontxt TTS](../concepts/kontxt.md#subject-token-sources-for-kontxt-tts). The Orka side is unchanged: it validates the resulting TxToken from the `Txn-Token` header.

## Troubleshooting

|Symptom|Likely cause|Check|
|---|---|---|
|Kubernetes issuer discovery fails|The API server does not expose OIDC discovery for ServiceAccount tokens, or your kubeconfig user cannot read the discovery endpoint.|Run `kubectl get --raw /.well-known/openid-configuration` and confirm the cluster's ServiceAccount issuer/JWKS setup.|
|TTS exchange returns `401`/`403`|kontxt TTS does not trust the Kubernetes ServiceAccount issuer, the projected token audience does not match `KONTXT_SUBJECT_AUDIENCE`, the token expired, or the requested scope/context is not allowed.|Check the Job's projected token `audience`, TTS `subjectTokens` config, and TTS logs.|
|Caller Job cannot reach TTS or Orka|The in-cluster Service DNS name, namespace, or port is different from the manifest.|From a debug Pod, curl `KONTXT_TTS_URL` and `ORKA_API`; update the Job env values.|
|Orka returns `401 Unauthorized`|Missing/invalid `Txn-Token`, wrong issuer/audience/JWKS, expired token, unknown `kid`, or missing `typ: txntoken+jwt`.|Confirm Orka env vars, JWKS reachability, and token claims with your kontxt tooling.|
|Orka returns `403 Forbidden`|`enforce` mode denied missing scope or `tctx` mismatch.|Check controller logs for safe context-token authorization failure fields.|
|Task response has `spec.transaction`, but the stored Task does not|The installed `tasks.core.orka.ai` CRD is stale and pruned the new `spec.transaction` fields.|From the repo root, run `kubectl apply -f config/crd/bases/core.orka.ai_tasks.yaml`, delete the old Task, and rerun the caller Job.|
|Task response has no `spec.transaction`|Request authenticated as a normal ServiceAccount/OIDC bearer request instead of a kontxt TxToken request.|Use the raw `Txn-Token` header against the Orka REST API. Do not `kubectl apply` the Task manifest directly for this smoke test.|
|Job/Pod missing transaction labels|The controller may not have reconciled the Task yet, or the Task was created without transaction metadata.|Wait for the worker Job/Pod and inspect the Task first.|

## Cleanup

Delete the caller Jobs and ServiceAccount when you are done. Delete the Task too if you do not need to keep the audit object.

```bash
kubectl -n default delete job orka-kontxt-caller --ignore-not-found=true
kubectl -n default delete job orka-kontxt-denied-caller --ignore-not-found=true
kubectl -n default delete serviceaccount orka-kontxt-caller --ignore-not-found=true
# Optional: removes the Orka Task audit object and worker resources/result.
kubectl -n default delete task kontxt-mit-license-check --ignore-not-found=true
rm -f /tmp/kontxt-orka-values.yaml /tmp/orka-kontxt-caller-job.yaml /tmp/orka-kontxt-denied-caller-job.yaml \
  /tmp/kubernetes-sa-jwks.json /tmp/kubernetes-sa-openid-configuration.json
```
