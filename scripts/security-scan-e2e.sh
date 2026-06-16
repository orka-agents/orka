#!/usr/bin/env bash

set -Eeuo pipefail

log() {
  printf '==> %s\n' "$*" >&2
}

warn() {
  printf 'warning: %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

sanitize_image_tag() {
  printf '%s' "$1" | LC_ALL=C tr -c 'A-Za-z0-9_.-' '-'
}

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"

kind_cluster="${KIND_CLUSTER:-orka-security-scan-e2e}"
orka_namespace="${ORKA_NAMESPACE:-orka-system}"
test_namespace="${ORKA_SECURITY_SCAN_E2E_NAMESPACE:-default}"
orka_controller_deployment="${ORKA_CONTROLLER_DEPLOYMENT:-orka-controller-manager}"
orka_harness_wrapper_deployment="${ORKA_HARNESS_WRAPPER_DEPLOYMENT:-orka-agent-harness-wrapper}"
orka_api_service="${ORKA_API_SERVICE:-orka-api}"
orka_api_service_port="${ORKA_API_SERVICE_PORT:-8080}"
orka_api_local_port="${ORKA_API_LOCAL_PORT:-18086}"
wait_timeout="${ORKA_SECURITY_SCAN_WAIT_TIMEOUT:-25m}"
target_repo="${ORKA_SECURITY_SCAN_TARGET_REPO:-https://github.com/sozercan/nodejs-goof}"
target_branch="${ORKA_SECURITY_SCAN_TARGET_BRANCH:-main}"
target_ref="${ORKA_SECURITY_SCAN_TARGET_REF:-add14ba59e98240d9e00a235dd7d42cd61ae9912}"
agent_name="${ORKA_SECURITY_SCAN_AGENT:-security-scan-e2e-agent}"
scan_name="${ORKA_SECURITY_SCAN_NAME:-security-goof}"
bad_scan_name="${ORKA_SECURITY_BAD_SCAN_NAME:-security-goof-tool-transcript}"
keep_cluster="${KEEP_CLUSTER:-0}"
created_kind_cluster="0"
api_pf_pid=""

e2e_run_id="$(sanitize_image_tag "${ORKA_SECURITY_SCAN_RUN_ID:-${GITHUB_RUN_ID:-manual}-$(date -u +%Y%m%d%H%M%S)}")"
manager_image="${ORKA_MANAGER_IMAGE:-orka-controller:security-scan-e2e-${e2e_run_id}}"
general_worker_image="${ORKA_GENERAL_WORKER_IMAGE:-orka-general-worker:security-scan-e2e-${e2e_run_id}}"
fake_codex_image="${ORKA_FAKE_HARNESS_WRAPPER_IMAGE:-orka-security-fake-codex:security-scan-e2e-${e2e_run_id}}"

work_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/security-scan-e2e.XXXXXX")"
kind_config="${ORKA_SECURITY_SCAN_KIND_CONFIG:-${work_dir}/kind-config.yaml}"
fake_dockerfile="${work_dir}/Dockerfile.fake-codex"
api_pf_log="${work_dir}/api-port-forward.log"
manager_kustomization="${repo_root}/config/manager/kustomization.yaml"
manager_kustomization_backup="${work_dir}/manager-kustomization.yaml.bak"
api_base=""
api_token=""

redact() {
  sed -E \
    -e 's/(Authorization:[[:space:]]*Bearer[[:space:]]+)[A-Za-z0-9._~+\/=-]+/\1[REDACTED]/Ig' \
    -e 's/(Bearer[[:space:]]+)[A-Za-z0-9._~+\/=-]+/\1[REDACTED]/Ig' \
    -e 's/gh[opusr]_[A-Za-z0-9_]+/[REDACTED_GITHUB_TOKEN]/g' \
    -e 's/github_pat_[A-Za-z0-9_]+/[REDACTED_GITHUB_TOKEN]/g'
}

run() {
  printf '+ ' >&2
  printf '%q ' "$@" >&2
  printf '\n' >&2
  "$@"
}

run_redacted() {
  set +e
  "$@" 2>&1 | redact
  local rc=${PIPESTATUS[0]}
  set -e
  return "${rc}"
}

restore_manager_kustomization() {
  if [[ -f "${manager_kustomization_backup}" ]]; then
    cp "${manager_kustomization_backup}" "${manager_kustomization}" || true
  fi
}

cleanup_port_forward() {
  if [[ -n "${api_pf_pid}" ]]; then
    kill "${api_pf_pid}" >/dev/null 2>&1 || true
    wait "${api_pf_pid}" 2>/dev/null || true
    api_pf_pid=""
  fi
}

dump_diagnostics() {
  log "Collecting diagnostics"
  {
    echo "=== Current Kubernetes Context ==="
    kubectl config current-context 2>/dev/null || true
    echo
    echo "=== Orka Namespace Resources ==="
    kubectl -n "${orka_namespace}" get pods,svc,deploy,jobs -o wide 2>/dev/null || true
    echo
    echo "=== Test Namespace Security Resources ==="
    kubectl -n "${test_namespace}" get agents,repositoryscans,tasks,jobs,pods -o wide 2>/dev/null || true
    echo
    echo "=== RepositoryScan YAML ==="
    kubectl -n "${test_namespace}" get repositoryscan "${scan_name}" "${bad_scan_name}" -o yaml 2>/dev/null || true
    echo
    echo "=== Security Tasks YAML ==="
    kubectl -n "${test_namespace}" get tasks \
      -l "orka.ai/security-target" \
      -o yaml 2>/dev/null || true
    echo
    echo "=== Controller Logs ==="
    kubectl -n "${orka_namespace}" logs deployment/"${orka_controller_deployment}" -c manager --tail=500 2>/dev/null || true
    echo
    echo "=== Worker Logs ==="
    for job in $(kubectl -n "${test_namespace}" get jobs -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true); do
      echo "--- job/${job} ---"
      kubectl -n "${test_namespace}" logs "job/${job}" --all-containers --tail=300 --prefix 2>/dev/null || true
    done
    echo
    echo "=== API Port-forward Log ==="
    if [[ -f "${api_pf_log}" ]]; then
      cat "${api_pf_log}" 2>/dev/null || true
    fi
    if [[ -n "${api_base}" && -n "${api_token}" ]]; then
      echo
      echo "=== Security API Snapshot ==="
      curl -fsS -H "Authorization: Bearer ${api_token}" \
        "${api_base}/api/v1/security/repositories/${scan_name}/scans?namespace=${test_namespace}&limit=10" 2>/dev/null || true
      echo
      curl -fsS -H "Authorization: Bearer ${api_token}" \
        "${api_base}/api/v1/security/repositories/${scan_name}/findings?namespace=${test_namespace}&limit=50" 2>/dev/null || true
      echo
      curl -fsS -H "Authorization: Bearer ${api_token}" \
        "${api_base}/api/v1/security/repositories/${scan_name}/dropped-findings?namespace=${test_namespace}&limit=50" 2>/dev/null || true
      echo
    fi
  } 2>&1 | redact >&2
}

on_exit() {
  local status="$1"
  set +e
  if [[ "${status}" -ne 0 ]]; then
    if [[ "$(kubectl config current-context 2>/dev/null || true)" == "kind-${kind_cluster}" ]]; then
      dump_diagnostics
    else
      warn "skipping Kubernetes diagnostics because the current context is not kind-${kind_cluster}"
    fi
  fi
  cleanup_port_forward
  restore_manager_kustomization
  if [[ "${created_kind_cluster}" == "1" && "${keep_cluster}" != "1" ]]; then
    kind delete cluster --name "${kind_cluster}" >/dev/null 2>&1 || true
  elif [[ "${keep_cluster}" == "1" ]]; then
    log "KEEP_CLUSTER=1, leaving kind cluster ${kind_cluster}"
  fi
  rm -rf "${work_dir}" >/dev/null 2>&1 || true
  if [[ "${status}" -ne 0 ]]; then
    log "Security scan e2e failed"
  fi
}

duration_to_seconds() {
  local value="$1"
  local rest="$1"
  local total=0
  local number unit amount

  if [[ "${value}" =~ ^[0-9]+$ ]]; then
    printf '%s\n' "${value}"
    return
  fi

  while [[ -n "${rest}" ]]; do
    if [[ ! "${rest}" =~ ^([0-9]+)([hms])(.*)$ ]]; then
      die "unsupported duration ${value}; use digits with h, m, or s units"
    fi
    number="${BASH_REMATCH[1]}"
    unit="${BASH_REMATCH[2]}"
    rest="${BASH_REMATCH[3]}"
    amount=$((10#${number}))
    case "${unit}" in
      h) total=$((total + amount * 3600)) ;;
      m) total=$((total + amount * 60)) ;;
      s) total=$((total + amount)) ;;
    esac
  done

  [[ "${total}" -gt 0 ]] || die "duration ${value} must be positive"
  printf '%s\n' "${total}"
}

api_token_duration() {
  local timeout_seconds
  timeout_seconds="$(duration_to_seconds "${wait_timeout}")"
  printf '%ss\n' "$((timeout_seconds * 4 + 600))"
}

kind_cluster_exists() {
  kind get clusters | grep -Fxq "${kind_cluster}"
}

write_default_kind_config() {
  cat >"${kind_config}" <<'YAML'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
YAML
}

setup_kind_cluster() {
  if kind_cluster_exists; then
    log "Kind cluster ${kind_cluster} already exists; reusing it"
    return
  fi

  if [[ -z "${ORKA_SECURITY_SCAN_KIND_CONFIG:-}" ]]; then
    write_default_kind_config
  fi
  [[ -f "${kind_config}" ]] || die "Kind config not found: ${kind_config}"

  log "Creating Kind cluster ${kind_cluster}"
  run kind create cluster --name "${kind_cluster}" --config "${kind_config}"
  created_kind_cluster="1"
}

start_port_forward() {
  local namespace_arg="$1"
  local resource="$2"
  local local_port="$3"
  local remote_port="$4"
  local logfile="$5"

  kubectl -n "${namespace_arg}" port-forward "${resource}" "${local_port}:${remote_port}" >>"${logfile}" 2>&1 &
  echo $!
}

wait_for_http() {
  local url="$1"
  local description="$2"
  local attempts_remaining=90

  while (( attempts_remaining > 0 )); do
    if curl -fsS --connect-timeout 5 --max-time 10 "${url}" >/dev/null 2>&1; then
      return 0
    fi
    if [[ -n "${api_pf_pid}" ]] && ! kill -0 "${api_pf_pid}" 2>/dev/null; then
      warn "API port-forward exited while waiting for ${description}; restarting"
      wait "${api_pf_pid}" 2>/dev/null || true
      api_pf_pid="$(start_port_forward "${orka_namespace}" "svc/${orka_api_service}" "${orka_api_local_port}" "${orka_api_service_port}" "${api_pf_log}")"
    fi
    attempts_remaining=$((attempts_remaining - 1))
    sleep 2
  done

  die "${description} never became available at ${url}"
}

api_request() {
  local method="$1"
  local path="$2"
  local output="$3"
  shift 3

  curl -fsS \
    -X "${method}" \
    -H "Authorization: Bearer ${api_token}" \
    "$@" \
    "${api_base}${path}" >"${output}"
}

write_fake_codex_dockerfile() {
  cat >"${fake_dockerfile}" <<'DOCKERFILE'
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder

ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} go build -a -o /out/worker ./cmd/orka-agent-harness-wrapper

FROM node:22-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN printf '#!/bin/sh\necho "$GIT_TOKEN"\n' > /bin/echo-token \
    && chmod +x /bin/echo-token

COPY --from=builder /out/worker /worker

RUN cat > /usr/local/bin/security-scan-codex <<'NODE'
#!/usr/bin/env node
const fs = require('fs');
const path = require('path');
const childProcess = require('child_process');

const artifactDir = '/tmp/artifacts';
fs.mkdirSync(artifactDir, { recursive: true });

function argValue(name) {
  const index = process.argv.indexOf(name);
  if (index >= 0 && index + 1 < process.argv.length) {
    return process.argv[index + 1];
  }
  return '';
}

function writeArtifact(name, value) {
  fs.writeFileSync(path.join(artifactDir, path.basename(name)), value);
}

function writeLastMessage(message) {
  const outputPath = argValue('--output-last-message');
  if (outputPath) {
    fs.writeFileSync(outputPath, message);
  }
  process.stdout.write(message + '\n');
}

function git(args) {
  try {
    return childProcess.execFileSync('git', args, { encoding: 'utf8' }).trim();
  } catch (_) {
    return '';
  }
}

function parseJSON(value, fallback) {
  try {
    return JSON.parse(value);
  } catch (_) {
    return fallback;
  }
}

function sanitizeArtifactName(value) {
  return String(value).replace(/[^A-Za-z0-9.-]+/g, '-').replace(/^-+|-+$/g, '') || 'artifact';
}

function readManifest(sliceID) {
  const name = `security-review-context-${sanitizeArtifactName(sliceID)}.json`;
  return parseJSON(fs.readFileSync(path.join(artifactDir, name), 'utf8'), null);
}

function lineInfo(content, needle) {
  const lines = content.split(/\r?\n/);
  const index = lines.findIndex((line) => line.includes(needle));
  if (index < 0) {
    throw new Error(`target line not found: ${needle}`);
  }
  return { line: index + 1, quote: lines[index].trim() };
}

function baseFinding(title, evidence) {
  return {
    title,
    category: 'injection',
    severity: 'high',
    confidence: 'high',
    triage: 'recommended',
    evidence,
    summary: 'The login flow passes user-controlled input into a security-sensitive sink.',
    rootCause: 'Untrusted request data is accepted without constraining the expected primitive type or destination.',
    reproduction: 'Submit a crafted login request that uses operator-shaped password input.',
    remediation: 'Validate credential fields as strings and compare against stored password hashes outside the query selector.',
    suggestedAction: 'Patch the login handler to reject non-string credentials before querying users.',
    whyTestsDoNotAlreadyCoverThis: 'Existing tests do not exercise malicious credential object payloads.',
    suggestedRegressionTest: 'Add a login test with an operator-shaped password that must return 401.',
    minimumFixScope: 'routes/index.js loginHandler credential validation'
  };
}

function emitThreatModel() {
  const scanName = process.env.ORKA_SECURITY_REPOSITORY_SCAN || '';
  let content;
  if (scanName.includes('tool-transcript')) {
    content = '# Invalid threat model\n\n<tool_call><tool_name>shell</tool_name></tool_call>\n';
  } else {
    content = [
      '# nodejs-goof threat model',
      '',
      '- Express routes receive untrusted HTTP request bodies and query parameters.',
      '- MongoDB and template rendering are security-sensitive trust boundaries.',
      '- Authentication and redirect handling are in scope for review.',
      ''
    ].join('\n');
  }
  writeArtifact('security-threat-model.md', content);
  writeLastMessage('security threat model artifact written');
}

function emitReview() {
  const sliceID = process.env.ORKA_SECURITY_SLICE_ID || '';
  const rawSlice = parseJSON(process.env.ORKA_SECURITY_REVIEW_SLICE_JSON || '{}', {});
  const ownedFiles = new Set((rawSlice.ownedFiles || []).map((file) => file.path));
  const manifest = readManifest(sliceID);
  if (!manifest) {
    throw new Error(`missing review context manifest for ${sliceID}`);
  }

  const target = (manifest.includedFiles || []).find((file) => file.path === 'routes/index.js');
  const isTargetSlice = target && ownedFiles.has('routes/index.js');
  const findings = [];

  if (isTargetSlice) {
    const content = target.excerpt || fs.readFileSync(path.join(process.cwd(), 'routes/index.js'), 'utf8');
    const loginQuery = lineInfo(content, 'User.find({ username: req.body.username, password: req.body.password }');
    const validEvidence = [{
      path: 'routes/index.js',
      startLine: loginQuery.line,
      endLine: loginQuery.line,
      symbol: 'loginHandler',
      quote: loginQuery.quote
    }];

    findings.push(baseFinding('NoSQL injection in login lookup', validEvidence));
    findings.push(baseFinding('Traversal evidence should be rejected', [{
      path: '../routes/index.js',
      startLine: loginQuery.line,
      endLine: loginQuery.line,
      symbol: 'loginHandler',
      quote: loginQuery.quote
    }]));
    findings.push(baseFinding('Evidence outside manifest should be rejected', [{
      path: 'README.md',
      startLine: 1,
      endLine: 1,
      quote: 'A vulnerable Node.js demo application'
    }]));
    findings.push(baseFinding('Line range outside manifest should be rejected', [{
      path: 'routes/index.js',
      startLine: 99999,
      endLine: 99999,
      symbol: 'loginHandler',
      quote: loginQuery.quote
    }]));
    findings.push(baseFinding('Quote mismatch should be rejected', [{
      path: 'routes/index.js',
      startLine: loginQuery.line,
      endLine: loginQuery.line,
      symbol: 'loginHandler',
      quote: 'this quote is deliberately absent from the cited line'
    }]));
  }

  const artifact = {
    schemaVersion: 2,
    repository: {
      repoURL: process.env.ORKA_GIT_REPO || 'https://github.com/sozercan/nodejs-goof',
      branch: process.env.ORKA_GIT_BRANCH || 'main',
      subPath: process.env.ORKA_WORKSPACE_SUBPATH || '',
      baseSHA: process.env.ORKA_SECURITY_SCAN_BASE_COMMIT || '',
      headSHA: process.env.ORKA_SECURITY_SCAN_HEAD_COMMIT || git(['rev-parse', 'HEAD'])
    },
    scan: {
      mode: 'security-scan-e2e',
      sliceId: sliceID,
      summary: isTargetSlice ? 'reviewed target nodejs-goof route slice' : 'reviewed non-target slice'
    },
    findings
  };

  writeArtifact('security-findings.v2.json', JSON.stringify(artifact, null, 2) + '\n');
  writeLastMessage(`security review artifact written for ${sliceID} with ${findings.length} findings`);
}

function emitValidation() {
  const findingID = process.env.ORKA_SECURITY_FINDING_ID || 'unknown';
  writeArtifact('security-validation.json', JSON.stringify({
    version: 1,
    finding_id: findingID,
    status: 'validated',
    summary: 'deterministic validation placeholder for security-scan e2e',
    validation_steps: ['inspected deterministic fixture evidence'],
    evidence: []
  }, null, 2) + '\n');
  writeLastMessage(`security validation artifact written for ${findingID}`);
}

function main() {
  const stage = process.env.ORKA_SECURITY_STAGE || '';
  if (stage === 'threat-model') {
    emitThreatModel();
    return;
  }
  if (stage === 'review') {
    emitReview();
    return;
  }
  if (stage === 'validation') {
    emitValidation();
    return;
  }
  writeLastMessage(`security-scan fake codex no-op for stage ${stage || '<unset>'}`);
}

main();
NODE

RUN chmod 0755 /usr/local/bin/security-scan-codex \
    && ln -sf /usr/local/bin/security-scan-codex /usr/local/bin/codex \
    && mkdir -p /workspace /home/node /tmp \
    && ln -s /home/node /home/worker \
    && chown -R 1000:1000 /workspace /home/node /tmp

USER 1000:1000
ENV HOME=/home/worker
ENV CODEX_CLI_PATH=/usr/local/bin/security-scan-codex

ENTRYPOINT ["/worker"]
DOCKERFILE
}

patch_controller_images() {
  local rollout_id
  rollout_id="${e2e_run_id}"

  log "Configuring Orka controller worker images"
  kubectl -n "${orka_namespace}" get deployment "${orka_controller_deployment}" -o json |
    jq \
      --arg codexImage "${fake_codex_image}" \
      --arg generalImage "${general_worker_image}" \
      --arg rolloutID "${rollout_id}" '
      def upsert_arg($name; $value):
        . as $args
        | if any($args[]?; startswith($name + "=")) then
            map(if startswith($name + "=") then $name + "=" + $value else . end)
          else
            $args + [$name + "=" + $value]
          end;
      .spec.template.metadata.annotations = ((.spec.template.metadata.annotations // {}) + {
        "orka.ai/security-scan-e2e-run": $rolloutID
      })
      |
      .spec.template.spec.containers |= map(
        if .name == "manager" then
          .imagePullPolicy = "IfNotPresent"
          | .args = ((.args // []) | upsert_arg("--general-worker-image"; $generalImage))
        else . end
      )
    ' | kubectl apply -f -

  if kubectl -n "${orka_namespace}" get deployment "${orka_harness_wrapper_deployment}" >/dev/null 2>&1; then
    run kubectl -n "${orka_namespace}" set image deployment/"${orka_harness_wrapper_deployment}" "wrapper=${fake_codex_image}"
    run kubectl -n "${orka_namespace}" rollout status deployment/"${orka_harness_wrapper_deployment}" --timeout=5m
  fi
  run kubectl -n "${orka_namespace}" rollout status deployment/"${orka_controller_deployment}" --timeout=5m
}

reset_e2e_resources() {
  log "Resetting security scan e2e resources"
  run kubectl -n "${test_namespace}" delete repositoryscan "${scan_name}" "${bad_scan_name}" \
    --ignore-not-found=true --wait=true --timeout=2m
  run kubectl -n "${test_namespace}" delete task \
    -l "orka.ai/security-target=${scan_name}" \
    --ignore-not-found=true --wait=true --timeout=2m
  run kubectl -n "${test_namespace}" delete task \
    -l "orka.ai/security-target=${bad_scan_name}" \
    --ignore-not-found=true --wait=true --timeout=2m
  run kubectl -n "${test_namespace}" delete agent "${agent_name}" \
    --ignore-not-found=true --wait=true --timeout=2m
}

apply_agent() {
  log "Creating fake Codex runtime Agent ${agent_name}"
  kubectl apply -f - <<YAML
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: ${agent_name}
  namespace: ${test_namespace}
spec:
  runtime:
    type: codex
    defaultMaxTurns: 1
    defaultAllowBash: true
  model:
    name: fake-security-scan-analyzer
YAML
}

apply_repository_scan() {
  local name="$1"
  log "Creating RepositoryScan ${name} for ${target_repo}"
  kubectl apply -f - <<YAML
apiVersion: core.orka.ai/v1alpha1
kind: RepositoryScan
metadata:
  name: ${name}
  namespace: ${test_namespace}
spec:
  provider: github
  repoURL: ${target_repo}
  owner: sozercan
  repository: nodejs-goof
  branch: ${target_branch}
  ref: ${target_ref}
  validationMode: "off"
  maxFindingsPerRun: 20
  analysisAgentRef:
    name: ${agent_name}
YAML
}

wait_repo_phase() {
  local name="$1"
  local expected="$2"
  local timeout_seconds
  timeout_seconds="$(duration_to_seconds "${wait_timeout}")"
  local deadline=$((SECONDS + timeout_seconds))
  local phase

  log "Waiting for RepositoryScan/${name} phase ${expected}"
  while (( SECONDS < deadline )); do
    phase="$(kubectl -n "${test_namespace}" get repositoryscan "${name}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    if [[ "${phase}" == "${expected}" ]]; then
      return 0
    fi
    sleep 5
  done
  die "RepositoryScan/${name} did not reach phase ${expected}; current phase ${phase:-<empty>}"
}

scan_runs_file() {
  local name="$1"
  local output="$2"
  api_request GET "/api/v1/security/repositories/${name}/scans?namespace=${test_namespace}&limit=20" "${output}"
}

wait_latest_run_phase() {
  local name="$1"
  local expected="$2"
  local timeout_seconds
  timeout_seconds="$(duration_to_seconds "${wait_timeout}")"
  local deadline=$((SECONDS + timeout_seconds))
  local output="${work_dir}/runs-${name}.json"
  local phase run_id

  log "Waiting for latest scan run on ${name} to become ${expected}"
  while (( SECONDS < deadline )); do
    if scan_runs_file "${name}" "${output}" &&
      [[ "$(jq '.items | length' "${output}")" -gt 0 ]]; then
      phase="$(jq -r '.items[0].phase // ""' "${output}")"
      run_id="$(jq -r '.items[0].id // ""' "${output}")"
      if [[ "${phase}" == "${expected}" ]]; then
        log "scan run ${run_id}: ${phase}"
        return 0
      fi
      if [[ "${phase}" == "failed" && "${expected}" != "failed" ]]; then
        jq -c '.items[0]' "${output}" >&2
        die "latest scan run ${run_id} failed while waiting for ${expected}"
      fi
    fi
    sleep 5
  done

  scan_runs_file "${name}" "${output}" || true
  jq -c '.items[0] // {}' "${output}" >&2 || true
  die "latest scan run for ${name} did not reach phase ${expected}"
}

wait_run_phase_by_id() {
  local name="$1"
  local run_id="$2"
  local expected="$3"
  local timeout_seconds
  timeout_seconds="$(duration_to_seconds "${wait_timeout}")"
  local deadline=$((SECONDS + timeout_seconds))
  local output="${work_dir}/runs-${name}-${run_id}.json"
  local phase

  log "Waiting for scan run ${run_id} on ${name} to become ${expected}"
  while (( SECONDS < deadline )); do
    scan_runs_file "${name}" "${output}" || true
    phase="$(jq -r --arg id "${run_id}" '.items[]? | select(.id == $id) | .phase' "${output}" | head -n1)"
    if [[ "${phase}" == "${expected}" ]]; then
      return 0
    fi
    if [[ "${phase}" == "failed" && "${expected}" != "failed" ]]; then
      jq -c --arg id "${run_id}" '.items[]? | select(.id == $id)' "${output}" >&2
      die "scan run ${run_id} failed while waiting for ${expected}"
    fi
    sleep 5
  done

  scan_runs_file "${name}" "${output}" || true
  jq -c --arg id "${run_id}" '.items[]? | select(.id == $id)' "${output}" >&2 || true
  die "scan run ${run_id} did not reach phase ${expected}"
}

assert_initial_scan_state() {
  local runs="${work_dir}/runs-initial.json"
  local slices="${work_dir}/slices.json"
  local findings="${work_dir}/findings.json"
  local dropped="${work_dir}/dropped.json"
  local total

  scan_runs_file "${scan_name}" "${runs}"
  jq -e '.items[0].phase == "succeeded" and .items[0].sliceCount > 0 and .items[0].acceptedFindings == 1 and .items[0].droppedFindings >= 4' "${runs}" >/dev/null

  api_request GET "/api/v1/security/repositories/${scan_name}/slices?namespace=${test_namespace}&limit=200" "${slices}"
  jq -e '
    (.items | length) > 0 and
    any(.items[]; any(.ownedFiles[]?; .path == "routes/index.js"))
  ' "${slices}" >/dev/null

  api_request GET "/api/v1/security/repositories/${scan_name}/findings?namespace=${test_namespace}&limit=50" "${findings}"
  jq -e '
    (.items | length) == 1 and
    .items[0].state == "open" and
    .items[0].title == "NoSQL injection in login lookup" and
    .items[0].filePath == "routes/index.js"
  ' "${findings}" >/dev/null

  api_request GET "/api/v1/security/repositories/${scan_name}/dropped-findings?namespace=${test_namespace}&limit=50" "${dropped}"
  jq -e '
    (.items | length) >= 4 and
    ([.items[].reason] | any(.[]; contains("not repo-relative"))) and
    ([.items[].reason] | any(.[]; contains("not included in review context"))) and
    ([.items[].reason] | any(.[]; contains("outside included review context"))) and
    ([.items[].reason] | any(.[]; contains("quote does not match")))
  ' "${dropped}" >/dev/null

  total="$(kubectl -n "${test_namespace}" get repositoryscan "${scan_name}" -o jsonpath='{.status.findingCounts.total}')"
  [[ "${total}" == "1" ]] || die "RepositoryScan findingCounts.total = ${total:-<empty>}, want 1"

  local threat_version
  threat_version="$(kubectl -n "${test_namespace}" get repositoryscan "${scan_name}" -o jsonpath='{.status.threatModelVersion}')"
  [[ "${threat_version}" =~ ^[1-9][0-9]*$ ]] || die "threatModelVersion = ${threat_version:-<empty>}, want >= 1"

  local patch_tasks
  patch_tasks="$(kubectl -n "${test_namespace}" get tasks -l "orka.ai/security-target=${scan_name},orka.ai/security-stage=patch" -o json | jq '.items | length')"
  [[ "${patch_tasks}" == "0" ]] || die "expected no automatic patch tasks, found ${patch_tasks}"
}

assert_idempotent_manual_scan() {
  local response="${work_dir}/manual-scan.json"
  local findings="${work_dir}/findings-after-manual.json"
  local run_id count

  log "Triggering manual scan for idempotency"
  api_request POST "/api/v1/security/repositories/${scan_name}/scans?namespace=${test_namespace}" "${response}"
  run_id="$(jq -r '.id' "${response}")"
  [[ -n "${run_id}" && "${run_id}" != "null" ]] || die "manual scan response did not include id"
  wait_run_phase_by_id "${scan_name}" "${run_id}" "succeeded"
  wait_repo_phase "${scan_name}" "Ready"

  api_request GET "/api/v1/security/repositories/${scan_name}/findings?namespace=${test_namespace}&limit=50" "${findings}"
  count="$(jq '.items | length' "${findings}")"
  [[ "${count}" == "1" ]] || die "manual rescan produced ${count} findings, want 1"
}

assert_bad_threat_model_rejected() {
  local runs="${work_dir}/runs-bad-threat.json"
  wait_latest_run_phase "${bad_scan_name}" "failed"
  wait_repo_phase "${bad_scan_name}" "Error"
  scan_runs_file "${bad_scan_name}" "${runs}"
  jq -e '.items[0].errorMessage | contains("looks like tool transcript")' "${runs}" >/dev/null
}

main() {
  require_cmd make
  require_cmd go
  require_cmd docker
  require_cmd kind
  require_cmd kubectl
  require_cmd curl
  require_cmd jq

  cd "${repo_root}"
  [[ -f "${manager_kustomization}" ]] || die "missing ${manager_kustomization}"
  cp "${manager_kustomization}" "${manager_kustomization_backup}"

  trap 'status=$?; on_exit "${status}"; exit "${status}"' EXIT

  setup_kind_cluster
  run kubectl config use-context "kind-${kind_cluster}"

  log "Building manager image ${manager_image}"
  run make docker-build IMG="${manager_image}"

  log "Building general worker image ${general_worker_image}"
  run docker build -t "${general_worker_image}" -f workers/general/Dockerfile .

  write_fake_codex_dockerfile
  log "Building fake Codex worker image ${fake_codex_image}"
  run docker build -t "${fake_codex_image}" -f "${fake_dockerfile}" .

  log "Loading images into Kind cluster ${kind_cluster}"
  run kind load docker-image "${manager_image}" --name "${kind_cluster}"
  run kind load docker-image "${general_worker_image}" --name "${kind_cluster}"
  run kind load docker-image "${fake_codex_image}" --name "${kind_cluster}"

  log "Deploying Orka manager"
  run make deploy IMG="${manager_image}"
  run kubectl wait --for=condition=Established crd/repositoryscans.core.orka.ai --timeout=60s
  run kubectl -n "${orka_namespace}" rollout status deployment/"${orka_controller_deployment}" --timeout=5m
  patch_controller_images

  log "Port-forwarding Orka API service"
  api_pf_pid="$(start_port_forward "${orka_namespace}" "svc/${orka_api_service}" "${orka_api_local_port}" "${orka_api_service_port}" "${api_pf_log}")"
  api_base="http://127.0.0.1:${orka_api_local_port}"
  wait_for_http "${api_base}/readyz" "Orka API /readyz"
  api_token="$(kubectl -n "${orka_namespace}" create token orka-controller-manager --duration="$(api_token_duration)")"
  [[ -n "${api_token}" ]] || die "failed to create Orka API token"

  reset_e2e_resources
  apply_agent

  apply_repository_scan "${scan_name}"
  wait_latest_run_phase "${scan_name}" "succeeded"
  wait_repo_phase "${scan_name}" "Ready"
  assert_initial_scan_state
  assert_idempotent_manual_scan

  apply_repository_scan "${bad_scan_name}"
  assert_bad_threat_model_rejected

  log "Security scan E2E passed"
}

main "$@"
