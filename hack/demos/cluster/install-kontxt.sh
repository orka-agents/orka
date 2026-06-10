#!/usr/bin/env bash
# Install the kontxt (Transaction Token Service) stack into the demo cluster.
#
# Ports the proven bootstrap from scripts/live-github-oidc-e2e.sh:
#   1. Generate an ephemeral RSA key + JWKS into DEMO_WORKDIR.
#   2. Apply a ConfigMap + busybox httpd Deployment + Service serving JWKS.
#   3. Build the scripts/live-kontxt-e2e helper image and load it into kind.
#   4. Apply the TTS Deployment + Service.
#   5. Patch the orka-controller-manager Deployment with the kontxt env vars.
#
# Requires: kind, docker, kubectl, go (for the JWKS generator + image build).

set -Eeuo pipefail

cluster_name="${ORKA_DEMO_CLUSTER:-orka-demo}"
orka_namespace="${ORKA_NAMESPACE:-orka-system}"
controller_deployment="${ORKA_CONTROLLER_DEPLOYMENT:-orka-controller-manager}"

kontxt_jwks_name="${ORKA_KONTXT_JWKS_NAME:-kontxt-jwks}"
kontxt_jwks_port="${ORKA_KONTXT_JWKS_PORT:-8080}"
kontxt_tts_name="${ORKA_KONTXT_TTS_NAME:-kontxt-tts}"
kontxt_tts_port="${ORKA_KONTXT_TTS_PORT:-8080}"
kontxt_issuer="${ORKA_KONTXT_ISSUER:-https://kontxt-demo.test}"
kontxt_audience="${ORKA_KONTXT_AUDIENCE:-orka-demo-kontxt}"
kontxt_subject_issuer="${ORKA_KONTXT_SUBJECT_ISSUER:-https://kubernetes.default.svc.cluster.local}"
kontxt_subject_audience="${ORKA_KONTXT_SUBJECT_AUDIENCE:-kontxt-tts}"
live_kontxt_image="${ORKA_LIVE_KONTXT_IMAGE:-orka-live-kontxt-e2e:demo}"

workdir="${DEMO_WORKDIR:-/tmp/orka-demo}"
mkdir -p "${workdir}"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../.." && pwd)"

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

for cmd in docker kubectl go; do
  command -v "${cmd}" >/dev/null 2>&1 || die "missing required command: ${cmd}"
done

# Select context: prefer kind-<cluster_name> if that kind cluster exists,
# otherwise use the current context (lets the kontxt demo share an existing
# cluster, e.g. the Substrate demo cluster for a single-cluster bootstrap).
if command -v kind >/dev/null 2>&1 && kind get clusters 2>/dev/null | grep -qx "${cluster_name}"; then
  log "Selecting kubectl context kind-${cluster_name}"
  kubectl config use-context "kind-${cluster_name}" >/dev/null
else
  log "kind cluster ${cluster_name} not found; using current context $(kubectl config current-context)"
fi

# Ensure the resources the demos expect: the demo namespace (10-60 default to
# demo-magic) and the Orka API client ServiceAccount used to mint API tokens.
# Both idempotent; other installers may also create them.
demo_namespace="${DEMO_NAMESPACE:-demo-magic}"
orka_client_sa="${ORKA_TOKEN_SERVICE_ACCOUNT:-orka-client}"
orka_client_ns="${ORKA_TOKEN_NAMESPACE:-default}"
kubectl create namespace "${demo_namespace}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl create serviceaccount "${orka_client_sa}" -n "${orka_client_ns}" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# The TTS verifies caller ServiceAccount tokens by fetching the cluster's OIDC
# discovery + JWKS ANONYMOUSLY from inside the pod. Vanilla kind binds
# system:service-account-issuer-discovery to system:unauthenticated by default,
# but some kind variants (e.g. Substrate's create-kind-cluster.sh) do NOT — then
# discovery returns 403 and every token exchange fails with "no access_token".
# Ensure the binding exists (idempotent).
kubectl create clusterrolebinding kontxt-anon-issuer-discovery \
  --clusterrole=system:service-account-issuer-discovery \
  --group=system:unauthenticated \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# 1. Ephemeral RSA key + JWKS. We inline a small generator (mirrors
#    scripts/live-github-oidc-e2e.sh:310–446) so the JWKS schema matches
#    what the TTS verifier in the live-kontxt-e2e helper image expects.
log "Generating ephemeral RSA key + JWKS in ${workdir}"
fixture_generator="${workdir}/generate-kontxt-fixture.go"
cat >"${fixture_generator}" <<'GO'
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"time"

	kontxttoken "github.com/aramase/kontxt/pkg/token"
)

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		panic(fmt.Sprintf("%s is required", name))
	}
	return v
}

func main() {
	jwksPath := mustEnv("KONTXT_JWKS_FILE")
	keyPath := mustEnv("KONTXT_KEY_FILE")
	kidPath := mustEnv("KONTXT_KID_FILE")

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	kid := fmt.Sprintf("kontxt-demo-%d", time.Now().UnixNano())

	jwk := map[string]any{
		"kty": "RSA",
		"use": "sig",
		"kid": kid,
		"alg": kontxttoken.SigningAlgorithm,
		"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
	}
	jwksBytes, err := json.MarshalIndent(map[string]any{"keys": []any{jwk}}, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(jwksPath, jwksBytes, 0o600); err != nil {
		panic(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		panic(err)
	}
	if err := os.WriteFile(kidPath, []byte(kid), 0o600); err != nil {
		panic(err)
	}
}
GO

(
  cd "${repo_root}"
  KONTXT_JWKS_FILE="${workdir}/kontxt-jwks.json" \
    KONTXT_KEY_FILE="${workdir}/kontxt-key.pem" \
    KONTXT_KID_FILE="${workdir}/kontxt-kid.txt" \
    go run "${fixture_generator}"
)

# 2. JWKS ConfigMap + httpd Deployment + Service.
log "Deploying in-cluster JWKS endpoint ${kontxt_jwks_name}"
kubectl create configmap "${kontxt_jwks_name}" \
  -n default \
  --from-file=jwks.json="${workdir}/kontxt-jwks.json" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${kontxt_jwks_name}
  namespace: default
  labels:
    orka.ai/demo: kontxt-infra
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${kontxt_jwks_name}
  template:
    metadata:
      labels:
        app: ${kontxt_jwks_name}
        orka.ai/demo: kontxt-infra
    spec:
      containers:
        - name: jwks
          image: busybox:1.36
          imagePullPolicy: IfNotPresent
          command: ["/bin/sh", "-c"]
          args: ["httpd -f -p ${kontxt_jwks_port} -h /www"]
          ports:
            - containerPort: ${kontxt_jwks_port}
          volumeMounts:
            - name: jwks
              mountPath: /www
              readOnly: true
      volumes:
        - name: jwks
          configMap:
            name: ${kontxt_jwks_name}
            items:
              - key: jwks.json
                path: .well-known/jwks.json
---
apiVersion: v1
kind: Service
metadata:
  name: ${kontxt_jwks_name}
  namespace: default
  labels:
    orka.ai/demo: kontxt-infra
spec:
  selector:
    app: ${kontxt_jwks_name}
  ports:
    - port: ${kontxt_jwks_port}
      targetPort: ${kontxt_jwks_port}
YAML

# 3. Build the TTS helper image, then get it onto the cluster. If the named
#    kind cluster exists, `kind load` it; otherwise push to the cluster's local
#    registry (localhost:<port>) so normal pods can pull it.
log "Building live kontxt helper image ${live_kontxt_image}"
if command -v kind >/dev/null 2>&1 && kind get clusters 2>/dev/null | grep -qx "${cluster_name}"; then
  docker build -t "${live_kontxt_image}" -f "${repo_root}/scripts/live-kontxt-e2e/Dockerfile" "${repo_root}"
  kind load docker-image "${live_kontxt_image}" --name "${cluster_name}"
else
  registry_port="${KIND_REGISTRY_PORT:-5001}"
  live_kontxt_image="localhost:${registry_port}/orka-live-kontxt-e2e:demo"
  log "kind cluster ${cluster_name} not found; pushing ${live_kontxt_image} to the local registry"
  docker build -t "${live_kontxt_image}" -f "${repo_root}/scripts/live-kontxt-e2e/Dockerfile" "${repo_root}"
  docker push "${live_kontxt_image}"
fi

# 3b. The Demo 50 caller image (docker.io/sozercan/orka-kontxt-caller:demo) is
#     published amd64-only, so it ImagePullBackOffs on an arm64 cluster. Build it
#     for the node arch and load/push it so the caller Job runs everywhere.
caller_image="${DEMO_KONTXT_CALLER_IMAGE:-docker.io/sozercan/orka-kontxt-caller:demo}"
node_arch="$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}' 2>/dev/null || echo amd64)"
log "Building kontxt caller image ${caller_image} (arch ${node_arch})"
docker build --platform "linux/${node_arch}" -t "${caller_image}" \
  "${repo_root}/hack/demos/images/kontxt-caller"
if command -v kind >/dev/null 2>&1 && kind get clusters 2>/dev/null | grep -qx "${cluster_name}"; then
  kind load docker-image "${caller_image}" --name "${cluster_name}"
else
  docker push "${caller_image}" || log "warning: could not push ${caller_image}; ensure the cluster can pull it"
fi

# 4. TTS Deployment + Service.
log "Deploying kontxt TTS ${kontxt_tts_name}"
# Remove the legacy anonymous discovery binding if it was created by an
# earlier demo install. The TTS pod can use its mounted service-account token
# for kube API discovery instead of widening unauthenticated cluster access.
kubectl delete clusterrolebinding kontxt-tts-anon-discovery \
  --ignore-not-found >/dev/null 2>&1 || true

kubectl apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${kontxt_tts_name}
  namespace: default
  labels:
    orka.ai/demo: kontxt-infra
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${kontxt_tts_name}
  template:
    metadata:
      labels:
        app: ${kontxt_tts_name}
        orka.ai/demo: kontxt-infra
    spec:
      containers:
        - name: tts
          image: ${live_kontxt_image}
          imagePullPolicy: IfNotPresent
          args:
            - tts-server
            - --addr=:${kontxt_tts_port}
            - --issuer=${kontxt_issuer}
            - --trust-domain=${kontxt_audience}
            - --subject-issuer=${kontxt_subject_issuer}
            - --subject-audience=${kontxt_subject_audience}
            - --replacement-jwks-url=http://${kontxt_tts_name}.default.svc.cluster.local:${kontxt_tts_port}/.well-known/jwks.json
            - --token-lifetime=15m
          env:
            # Make Go's TLS dialer trust the kube API server's CA so the
            # subject-issuer OIDC discovery fetch succeeds.
            - name: SSL_CERT_FILE
              value: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
          ports:
            - containerPort: ${kontxt_tts_port}
---
apiVersion: v1
kind: Service
metadata:
  name: ${kontxt_tts_name}
  namespace: default
  labels:
    orka.ai/demo: kontxt-infra
spec:
  selector:
    app: ${kontxt_tts_name}
  ports:
    - port: ${kontxt_tts_port}
      targetPort: ${kontxt_tts_port}
YAML

kubectl rollout status deployment/"${kontxt_jwks_name}" -n default --timeout=3m
kubectl rollout status deployment/"${kontxt_tts_name}"  -n default --timeout=3m

# 5. Wire the controller's kontxt envs. Mirrors live-github-oidc-e2e.sh:734.
log "Patching ${controller_deployment} env vars for kontxt"
kubectl -n "${orka_namespace}" set env deployment/"${controller_deployment}" \
  ORKA_CONTEXT_TOKEN_PROFILE=kontxt \
  ORKA_CONTEXT_TOKEN_ISSUER="${kontxt_issuer}" \
  ORKA_CONTEXT_TOKEN_AUDIENCE="${kontxt_audience}" \
  ORKA_CONTEXT_TOKEN_JWKS_URL="http://${kontxt_tts_name}.default.svc.cluster.local:${kontxt_tts_port}/.well-known/jwks.json" \
  ORKA_CONTEXT_TOKEN_TTS_URL="http://${kontxt_tts_name}.default.svc.cluster.local:${kontxt_tts_port}" \
  ORKA_CONTEXT_TOKEN_TTS_TOKEN_SOURCE=incoming \
  ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_TYPE=urn:ietf:params:oauth:token-type:txn_token \
  ORKA_CONTEXT_TOKEN_AUTHZ_MODE=enforce \
  ORKA_CONTEXT_TOKEN_TASK_CREATE_SCOPES=write

kubectl -n "${orka_namespace}" rollout status deployment/"${controller_deployment}" --timeout=300s

log "kontxt stack installed. Demo 50 can run."
