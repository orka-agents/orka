#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: deploy_vekil_reverse_proxy.sh [options]

Deploy Vekil as a Kubernetes reverse proxy.

Options:
  --context NAME                    kubectl context to use
  --namespace NAME                  target namespace (default: vekil-system)
  --name NAME                       deployment/service name (default: vekil)
  --image IMAGE                     container image (default: ghcr.io/sozercan/vekil:latest)
  --image-pull-policy POLICY        Always, IfNotPresent, or Never (default: Always)
  --replicas N                      deployment replicas (default: 1)
  --port N                          listen and service port (default: 1337)
  --service-type TYPE               ClusterIP, NodePort, or LoadBalancer (default: ClusterIP)
  --no-network-policy               do not render the default restrictive NetworkPolicy
                                    and delete the managed policy if it exists
  --providers-config PATH           JSON/YAML provider config to mount as a ConfigMap
  --providers-configmap NAME        ConfigMap name for providers config (default: <name>-providers)
  --env-secret ENV=SECRET:KEY       add env var from an existing Secret (repeatable)
  --create-copilot-token-secret SECRET[:KEY]
                                    create/update Secret from COPILOT_GITHUB_TOKEN,
                                    inject it into the pod, and restart if changed
                                    (default key: token)
  --codex-auth-secret SECRET[:KEY]  mount an existing Secret key as CODEX_HOME/auth.json
                                    (default key: auth.json)
  --token-pvc CLAIM                 use an existing PVC for TOKEN_DIR instead of emptyDir
  --log-level LEVEL                 debug, info, or error (default: info)
  --timeout DURATION                rollout timeout (default: 180s)
  --no-create-namespace             do not create/apply the namespace
  --skip-wait                       do not wait for rollout status
  --print                           print workload manifest instead of applying it
  -h, --help                        show this help

Examples:
  deploy_vekil_reverse_proxy.sh --context kind-orka
  deploy_vekil_reverse_proxy.sh --providers-config ./providers.yaml \
    --env-secret AZURE_OPENAI_API_KEY=azure-openai:key
  deploy_vekil_reverse_proxy.sh \
    --env-secret COPILOT_GITHUB_TOKEN=copilot-github-token:token
  COPILOT_GITHUB_TOKEN=ghu_xxx deploy_vekil_reverse_proxy.sh \
    --create-copilot-token-secret copilot-github-token:token
USAGE
}

error() {
  echo "Error: $*" >&2
  exit 1
}

require_cmd() {
  local cmd="$1"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    error "Missing required command: $cmd"
  fi
}

is_dns_label() {
  [[ "$1" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] && ((${#1} <= 63))
}

is_dns_subdomain() {
  [[ "$1" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$ ]] && ((${#1} <= 253))
}

is_secret_key() {
  [[ "$1" =~ ^[-._A-Za-z0-9]+$ ]]
}

is_env_name() {
  [[ "$1" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]
}

is_nonnegative_int() {
  [[ "$1" =~ ^[0-9]+$ ]]
}

is_positive_int() {
  is_nonnegative_int "$1" && ((10#$1 > 0))
}

context_name=""
namespace="vekil-system"
name="vekil"
image="ghcr.io/sozercan/vekil:latest"
image_pull_policy="Always"
replicas="1"
port="1337"
service_type="ClusterIP"
providers_config=""
providers_configmap=""
providers_config_key=""
providers_mount_path=""
log_level="info"
timeout="180s"
create_namespace="true"
wait_rollout="true"
print_only="false"
network_policy="true"
token_pvc=""
copilot_token_secret=""
copilot_token_key="token"
codex_auth_secret=""
codex_auth_key="auth.json"
copilot_token_secret_changed="false"
providers_configmap_changed="false"

declare -a env_secret_envs=()
declare -a env_secret_names=()
declare -a env_secret_keys=()
declare -a tmp_files=()

cleanup_tmp_files() {
  if ((${#tmp_files[@]})); then
    rm -f "${tmp_files[@]}"
  fi
}
trap cleanup_tmp_files EXIT

add_env_secret_ref() {
  local env_name="$1"
  local secret_name="$2"
  local secret_key="$3"
  local i

  is_env_name "$env_name" || error "Invalid env var name in --env-secret: $env_name"
  is_dns_subdomain "$secret_name" || error "Invalid Secret name in --env-secret: $secret_name"
  is_secret_key "$secret_key" || error "Invalid Secret key in --env-secret: $secret_key"

  for i in "${!env_secret_envs[@]}"; do
    if [[ "${env_secret_envs[$i]}" == "$env_name" ]]; then
      error "Duplicate Secret-backed env var: $env_name"
    fi
  done

  env_secret_envs+=("$env_name")
  env_secret_names+=("$secret_name")
  env_secret_keys+=("$secret_key")
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --context)
      context_name="${2:?missing value for --context}"
      shift 2
      ;;
    --namespace)
      namespace="${2:?missing value for --namespace}"
      shift 2
      ;;
    --name)
      name="${2:?missing value for --name}"
      shift 2
      ;;
    --image)
      image="${2:?missing value for --image}"
      shift 2
      ;;
    --image-pull-policy)
      image_pull_policy="${2:?missing value for --image-pull-policy}"
      shift 2
      ;;
    --replicas)
      replicas="${2:?missing value for --replicas}"
      shift 2
      ;;
    --port)
      port="${2:?missing value for --port}"
      shift 2
      ;;
    --service-type)
      service_type="${2:?missing value for --service-type}"
      shift 2
      ;;
    --no-network-policy)
      network_policy="false"
      shift
      ;;
    --providers-config)
      providers_config="${2:?missing value for --providers-config}"
      shift 2
      ;;
    --providers-configmap)
      providers_configmap="${2:?missing value for --providers-configmap}"
      shift 2
      ;;
    --env-secret)
      ref="${2:?missing value for --env-secret}"
      env_name="${ref%%=*}"
      rest="${ref#*=}"
      [[ "$env_name" != "$ref" && -n "$env_name" && -n "$rest" ]] || error "--env-secret must be ENV=SECRET:KEY"
      secret_name="${rest%%:*}"
      secret_key="${rest#*:}"
      [[ "$secret_key" != "$rest" && -n "$secret_name" && -n "$secret_key" ]] || error "--env-secret must be ENV=SECRET:KEY"
      add_env_secret_ref "$env_name" "$secret_name" "$secret_key"
      shift 2
      ;;
    --create-copilot-token-secret)
      ref="${2:?missing value for --create-copilot-token-secret}"
      if [[ "$ref" == *:* ]]; then
        copilot_token_secret="${ref%%:*}"
        copilot_token_key="${ref#*:}"
      else
        copilot_token_secret="$ref"
        copilot_token_key="token"
      fi
      [[ -n "$copilot_token_secret" && -n "$copilot_token_key" ]] || error "--create-copilot-token-secret must be SECRET[:KEY]"
      shift 2
      ;;
    --codex-auth-secret)
      ref="${2:?missing value for --codex-auth-secret}"
      if [[ "$ref" == *:* ]]; then
        codex_auth_secret="${ref%%:*}"
        codex_auth_key="${ref#*:}"
      else
        codex_auth_secret="$ref"
        codex_auth_key="auth.json"
      fi
      [[ -n "$codex_auth_secret" && -n "$codex_auth_key" ]] || error "--codex-auth-secret must be SECRET[:KEY]"
      shift 2
      ;;
    --token-pvc)
      token_pvc="${2:?missing value for --token-pvc}"
      shift 2
      ;;
    --log-level)
      log_level="${2:?missing value for --log-level}"
      shift 2
      ;;
    --timeout)
      timeout="${2:?missing value for --timeout}"
      shift 2
      ;;
    --no-create-namespace)
      create_namespace="false"
      shift
      ;;
    --skip-wait)
      wait_rollout="false"
      shift
      ;;
    --print)
      print_only="true"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      error "Unknown argument: $1"
      ;;
  esac
done

is_dns_label "$namespace" || error "Invalid namespace: $namespace"
# --name is used for both the Deployment and Service, so keep it DNS-label safe.
is_dns_label "$name" || error "Invalid name: $name"
[[ "$image" != *$'\n'* && "$image" != *$'\t'* && "$image" != *' '* ]] || error "Image must not contain whitespace"
case "$image_pull_policy" in Always|IfNotPresent|Never) ;; *) error "Invalid image pull policy: $image_pull_policy" ;; esac
is_nonnegative_int "$replicas" || error "Invalid replicas: $replicas"
is_positive_int "$port" || error "Invalid port: $port"
((10#$port <= 65535)) || error "Invalid port: $port"
case "$service_type" in ClusterIP|NodePort|LoadBalancer) ;; *) error "Invalid service type: $service_type" ;; esac
case "$log_level" in debug|info|error) ;; *) error "Invalid log level: $log_level" ;; esac
if [[ "$network_policy" == "true" && "$service_type" != "ClusterIP" ]]; then
  echo "Warning: --service-type $service_type exposes Vekil externally, but the default NetworkPolicy only permits labeled in-cluster sources. Use --no-network-policy or extend the policy for the intended external source ranges." >&2
fi
if [[ -n "$providers_configmap" ]]; then
  is_dns_subdomain "$providers_configmap" || error "Invalid ConfigMap name: $providers_configmap"
else
  providers_configmap="${name}-providers"
fi
if [[ -n "$providers_config" ]]; then
  is_dns_subdomain "$providers_configmap" || error "Invalid ConfigMap name: $providers_configmap"
fi
if [[ -n "$token_pvc" ]]; then
  is_dns_subdomain "$token_pvc" || error "Invalid PVC name: $token_pvc"
fi
if [[ -n "$copilot_token_secret" ]]; then
  is_dns_subdomain "$copilot_token_secret" || error "Invalid Copilot token Secret name: $copilot_token_secret"
  is_secret_key "$copilot_token_key" || error "Invalid Copilot token Secret key: $copilot_token_key"
  add_env_secret_ref "COPILOT_GITHUB_TOKEN" "$copilot_token_secret" "$copilot_token_key"
fi
if [[ -n "$codex_auth_secret" ]]; then
  is_dns_subdomain "$codex_auth_secret" || error "Invalid Codex auth Secret name: $codex_auth_secret"
  is_secret_key "$codex_auth_key" || error "Invalid Codex auth Secret key: $codex_auth_key"
fi

if [[ -n "$providers_config" ]]; then
  [[ -f "$providers_config" ]] || error "Providers config not found: $providers_config"
  case "$providers_config" in
    *.json) providers_config_key="providers.json" ;;
    *.yaml|*.yml) providers_config_key="providers.yaml" ;;
    *) error "Providers config must end in .json, .yaml, or .yml" ;;
  esac
  providers_mount_path="/etc/vekil/${providers_config_key}"
  if grep -Eq '(^|[,{[:space:]])"?api_key"?[[:space:]]*:' "$providers_config"; then
    error "Providers config appears to contain inline api_key. Use api_key_env plus --env-secret so secrets stay in Kubernetes Secrets, not ConfigMaps."
  fi
fi

declare -a kubectl_args=()
if [[ -n "$context_name" ]]; then
  kubectl_args+=(--context "$context_name")
fi

k() {
  if ((${#kubectl_args[@]})); then
    kubectl "${kubectl_args[@]}" "$@"
  else
    kubectl "$@"
  fi
}

create_copilot_token_secret() {
  local token_file
  local apply_output

  [[ -n "${COPILOT_GITHUB_TOKEN:-}" ]] || error "COPILOT_GITHUB_TOKEN must be set when using --create-copilot-token-secret"

  token_file="$(mktemp)"
  tmp_files+=("$token_file")
  chmod 600 "$token_file"
  printf '%s' "$COPILOT_GITHUB_TOKEN" > "$token_file"

  [[ -s "$token_file" ]] || error "No token read from COPILOT_GITHUB_TOKEN"

  apply_output="$(k -n "$namespace" create secret generic "$copilot_token_secret" \
    --from-file="${copilot_token_key}=${token_file}" \
    --dry-run=client -o yaml | k apply -f -)"
  echo "$apply_output"
  if [[ "$apply_output" != *" unchanged"* ]]; then
    copilot_token_secret_changed="true"
  fi
}

apply_providers_configmap() {
  local apply_output

  apply_output="$(k -n "$namespace" create configmap "$providers_configmap" \
    --from-file="${providers_config_key}=${providers_config}" \
    --dry-run=client -o yaml | k apply -f -)"
  echo "$apply_output"
  # Vekil reads providers config at startup, so restart pods when the
  # ConfigMap is created or updated underneath an otherwise unchanged template.
  if [[ "$apply_output" == *" created"* || "$apply_output" == *" configured"* ]]; then
    providers_configmap_changed="true"
  fi
}

render_env_secrets() {
  local i
  for i in "${!env_secret_envs[@]}"; do
    cat <<EOF_ENV
            - name: ${env_secret_envs[$i]}
              valueFrom:
                secretKeyRef:
                  name: ${env_secret_names[$i]}
                  key: ${env_secret_keys[$i]}
EOF_ENV
  done
}

render_workload() {
  if [[ "$create_namespace" == "true" && "$namespace" != "default" ]]; then
    cat <<EOF_NS
apiVersion: v1
kind: Namespace
metadata:
  name: $namespace
---
EOF_NS
  fi

  cat <<EOF_DEPLOY
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $name
  namespace: $namespace
  labels:
    app.kubernetes.io/name: vekil
    app.kubernetes.io/instance: $name
    app.kubernetes.io/managed-by: vekil-reverse-proxy-deploy
spec:
  replicas: $replicas
  selector:
    matchLabels:
      app.kubernetes.io/name: vekil
      app.kubernetes.io/instance: $name
  template:
    metadata:
      labels:
        app.kubernetes.io/name: vekil
        app.kubernetes.io/instance: $name
    spec:
      containers:
        - name: vekil
          image: $image
          imagePullPolicy: $image_pull_policy
          ports:
            - name: http
              containerPort: $port
          env:
            - name: PORT
              value: "$port"
            - name: HOST
              value: "0.0.0.0"
            - name: TOKEN_DIR
              value: /home/nonroot/.config/vekil
            - name: LOG_LEVEL
              value: $log_level
EOF_DEPLOY

  if [[ -n "$providers_config" ]]; then
    cat <<EOF_PROVIDERS_ENV
            - name: PROVIDERS_CONFIG
              value: $providers_mount_path
EOF_PROVIDERS_ENV
  fi

  if [[ -n "$codex_auth_secret" ]]; then
    cat <<'EOF_CODEX_ENV'
            - name: CODEX_HOME
              value: /home/nonroot/.codex
EOF_CODEX_ENV
  fi

  render_env_secrets

  cat <<'EOF_PROBES'
          readinessProbe:
            httpGet:
              path: /readyz
              port: http
            initialDelaySeconds: 5
            periodSeconds: 10
            timeoutSeconds: 5
            failureThreshold: 12
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
            initialDelaySeconds: 10
            periodSeconds: 20
            timeoutSeconds: 5
            failureThreshold: 3
          volumeMounts:
            - name: token-cache
              mountPath: /home/nonroot/.config/vekil
EOF_PROBES

  if [[ -n "$providers_config" ]]; then
    cat <<'EOF_PROVIDER_MOUNT'
            - name: providers-config
              mountPath: /etc/vekil
              readOnly: true
EOF_PROVIDER_MOUNT
  fi

  if [[ -n "$codex_auth_secret" ]]; then
    cat <<EOF_CODEX_MOUNT
            - name: codex-auth
              mountPath: /home/nonroot/.codex
              readOnly: true
EOF_CODEX_MOUNT
  fi

  cat <<'EOF_VOLUMES'
      volumes:
        - name: token-cache
EOF_VOLUMES

  if [[ -n "$token_pvc" ]]; then
    cat <<EOF_TOKEN_PVC
          persistentVolumeClaim:
            claimName: $token_pvc
EOF_TOKEN_PVC
  else
    cat <<'EOF_TOKEN_EMPTYDIR'
          emptyDir: {}
EOF_TOKEN_EMPTYDIR
  fi

  if [[ -n "$providers_config" ]]; then
    cat <<EOF_PROVIDER_VOLUME
        - name: providers-config
          configMap:
            name: $providers_configmap
EOF_PROVIDER_VOLUME
  fi

  if [[ -n "$codex_auth_secret" ]]; then
    cat <<EOF_CODEX_VOLUME
        - name: codex-auth
          secret:
            secretName: $codex_auth_secret
            items:
              - key: $codex_auth_key
                path: auth.json
EOF_CODEX_VOLUME
  fi

  cat <<EOF_SERVICE
---
apiVersion: v1
kind: Service
metadata:
  name: $name
  namespace: $namespace
  labels:
    app.kubernetes.io/name: vekil
    app.kubernetes.io/instance: $name
    app.kubernetes.io/managed-by: vekil-reverse-proxy-deploy
spec:
  type: $service_type
  selector:
    app.kubernetes.io/name: vekil
    app.kubernetes.io/instance: $name
  ports:
    - name: http
      port: $port
      targetPort: http
EOF_SERVICE

  if [[ "$network_policy" == "true" ]]; then
    cat <<EOF_NETWORK_POLICY
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: ${name}-ingress
  namespace: $namespace
  labels:
    app.kubernetes.io/name: vekil
    app.kubernetes.io/instance: $name
    app.kubernetes.io/managed-by: vekil-reverse-proxy-deploy
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: vekil
      app.kubernetes.io/instance: $name
  policyTypes:
    - Ingress
  ingress:
    - from:
        - podSelector:
            matchLabels:
              vekil.sozercan.io/access: "true"
        - namespaceSelector:
            matchLabels:
              vekil.sozercan.io/access: "true"
      ports:
        - protocol: TCP
          port: $port
EOF_NETWORK_POLICY
  fi
}

if [[ "$print_only" == "true" ]]; then
  render_workload
  if [[ -n "$providers_config" ]]; then
    echo "# NOTE: Providers ConfigMap '$providers_configmap' is created from '$providers_config' during apply and is not printed to avoid echoing config contents." >&2
  fi
  if [[ -n "$copilot_token_secret" ]]; then
    echo "# NOTE: Copilot token Secret '$copilot_token_secret' is created from COPILOT_GITHUB_TOKEN during apply and is not printed." >&2
  fi
  exit 0
fi

require_cmd kubectl

if [[ "$create_namespace" == "true" && "$namespace" != "default" ]]; then
  k create namespace "$namespace" --dry-run=client -o yaml | k apply -f -
fi

if [[ -n "$copilot_token_secret" ]]; then
  create_copilot_token_secret
fi

if [[ -n "$providers_config" ]]; then
  apply_providers_configmap
fi

render_workload | k apply -f -

if [[ "$network_policy" == "false" ]]; then
  k -n "$namespace" delete networkpolicy "${name}-ingress" --ignore-not-found
fi

if [[ "$copilot_token_secret_changed" == "true" || "$providers_configmap_changed" == "true" ]]; then
  k -n "$namespace" rollout restart "deployment/$name"
fi

if [[ "$wait_rollout" == "true" ]]; then
  k -n "$namespace" rollout status "deployment/$name" --timeout="$timeout"
fi

k -n "$namespace" get deploy,svc,pods -l "app.kubernetes.io/instance=$name"
cat <<EOF_DONE

Vekil service URL inside the cluster:
  http://$name.$namespace.svc.cluster.local:$port

Access control:
EOF_DONE
if [[ "$network_policy" == "true" ]]; then
  cat <<EOF_DONE
  The default NetworkPolicy allows TCP/$port only from labeled in-cluster sources:
    - pods in namespace '$namespace' labeled 'vekil.sozercan.io/access=true'
    - pods in namespaces labeled 'vekil.sozercan.io/access=true'
  NodePort/LoadBalancer traffic and kubelet probes may need additional ingress exceptions.
  NetworkPolicy enforcement depends on the cluster CNI.
EOF_DONE
else
  cat <<EOF_DONE
  No NetworkPolicy is rendered. The managed policy '${name}-ingress' was deleted if it existed.
  Ensure another authentication or network boundary protects this proxy before sharing it.
EOF_DONE
fi
cat <<EOF_DONE

Local verification:
  kubectl${context_name:+ --context $context_name} -n $namespace port-forward svc/$name $port:$port
  curl http://127.0.0.1:$port/healthz
  curl http://127.0.0.1:$port/readyz
  curl http://127.0.0.1:$port/v1/models
EOF_DONE
