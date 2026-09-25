#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/lib/release-chart-acceptance.sh
. "${root}/scripts/lib/release-chart-acceptance.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/release-chart-acceptance-test.XXXXXX")"
trap 'rm -rf -- "${test_root}"' EXIT

fail() { printf 'error: %s\n' "$*" >&2; exit 1; }
for command in helm jq kubectl openssl python3; do
  command -v "${command}" >/dev/null 2>&1 || fail "missing required command: ${command}"
done

# Generate actual certificates. A Kustomize Service certificate cannot pass
# the packaged chart's admission requests.
mkdir "${test_root}/tls"
live_acp_release_chart_tls "${test_root}/tls"
openssl verify -CAfile "${test_root}/tls/ca.crt" -verify_hostname orka-webhook.orka-system.svc \
  "${test_root}/tls/tls.crt" >/dev/null 2>&1
openssl verify -CAfile "${test_root}/tls/ca.crt" -verify_hostname orka-webhook.orka-system.svc.cluster.local \
  "${test_root}/tls/tls.crt" >/dev/null 2>&1
if openssl verify -CAfile "${test_root}/tls/ca.crt" -verify_hostname orka-admission.orka-system.svc \
  "${test_root}/tls/tls.crt" >/dev/null 2>&1; then
  fail "chart certificate unexpectedly authenticates the Kustomize Service"
fi
printf '%s\n' 'ok - admission certificate authenticates the packaged chart Service'

python3 - "${test_root}/candidate.json" <<'PY'
import json
from pathlib import Path
import sys

roles = ("controller", "ai-worker", "general-worker", "agent-harness-wrapper", "acp-codex-runtime",
         "acp-claude-runtime", "acp-copilot-runtime", "acp-opencode-runtime", "workspace-publisher")
images = {role: "ghcr.io/orka-agents/orka" + ("" if role == "controller" else "/" + role)
          + "@sha256:" + format(index, "064x") for index, role in enumerate(roles, 1)}
Path(sys.argv[1]).write_text(json.dumps({"candidateSHA": "a" * 40, "version": "v0.2.0", "images": images,
    "chart": {"file": "orka-0.2.0.tgz", "sha256": "b" * 64}}))
PY
live_acp_release_chart_values "${test_root}/candidate.json" "${test_root}/tls/ca.crt" "${test_root}/values.json"
jq -e '
  .controller.mode == "harness-v2" and .controller.watchNamespace == "orka-system"
  and .controller.acpRuntime.namespace == "orka-runtimes"
  and .store.persistence.enabled == true and .providerProxy.enabled == true
  and .providerProxy.upstreamBaseURL == "http://vekil.vekil-system.svc:1337"
  and .providerProxy.egress == [{
    to: [{
      namespaceSelector: {matchLabels: {"kubernetes.io/metadata.name": "vekil-system"}},
      podSelector: {matchLabels: {"app.kubernetes.io/name": "vekil"}}
    }],
    ports: [{protocol: "TCP", port: 1337}]
  }]
  and .publisher.auth.existingSecret == "release-chart-publisher"
  and .controller.agentExecutionSnapshot.existingSecret == "release-chart-snapshot"
  and .webhooks.tls.existingSecret == "release-chart-webhook"
  and ([.controller.image, .publisher.image, .workers.ai.image, .workers.general.image, .harnessV1.image]
    | all(.[]; .digest | test("^sha256:[a-f0-9]{64}$")))
  and ([.. | objects | keys[] | select(. == "token" or . == "controllerToken" or . == "capabilitySecret" or . == "secret")] | length == 0)
' "${test_root}/values.json" >/dev/null

# Render the actual chart from an archive with the helper's values. This
# catches mismatched value keys, Deployment names, and worker digest wiring.
helm package "${root}/cmd/build/helmify/static" --destination "${test_root}" >/dev/null
archives=("${test_root}"/orka-*.tgz)
[[ "${#archives[@]}" == 1 ]] || fail "expected exactly one test chart archive"
helm template orka "${archives[0]}" -n orka-system --values "${test_root}/values.json" >"${test_root}/render.yaml"
for role in controller ai-worker general-worker acp-codex-runtime acp-claude-runtime acp-copilot-runtime acp-opencode-runtime workspace-publisher; do
  image_ref="$(jq -r --arg role "${role}" '.images[$role]' "${test_root}/candidate.json")"
  grep -F -- "${image_ref}" "${test_root}/render.yaml" >/dev/null || fail "render omitted ${role} digest"
done
for name in orka-controller orka-workspace-publisher orka-provider-auth-proxy orka-scm-egress-proxy orka-webhook; do
  grep -F -- "name: ${name}" "${test_root}/render.yaml" >/dev/null || fail "render omitted ${name}"
done
if grep -F 'kind: Secret' "${test_root}/render.yaml" >/dev/null; then
  fail "chart values unexpectedly render Secret contents into Helm release history"
fi
for mutation in \
  '.images["general-worker"] = "ghcr.io/orka-agents/orka/general-worker:v0.2.0"' \
  'del(.images["acp-claude-runtime"])' \
  '.images.controller = .images["ai-worker"]'; do
  jq "${mutation}" "${test_root}/candidate.json" >"${test_root}/invalid-candidate.json"
  if live_acp_release_chart_values "${test_root}/invalid-candidate.json" "${test_root}/tls/ca.crt" \
      "${test_root}/invalid-values.json" >"${test_root}/invalid-values.log" 2>&1; then
    fail "chart values accepted missing, mutable, or wrong-role candidate images"
  fi
done
printf '%s\n' 'ok - actual packaged chart renders each candidate digest and uses existing Secrets'

cat >"${test_root}/before.json" <<'JSON'
{
  "pvcs":[
    {"name":"orka-store","uid":"store-uid","volumeName":"store-volume","phase":"Bound"},
    {"name":"orka-workspace-publisher","uid":"publisher-uid","volumeName":"publisher-volume","phase":"Bound"}
  ],
  "controller":{"podUID":"controller-before","claim":"orka-store"},
  "publisher":{"podUID":"publisher-pod","claim":"orka-workspace-publisher"},
  "task":{"namespace":"orka-system","name":"release-chart-acceptance","uid":"task-uid","phase":"Succeeded",
    "attempts":1,"jobName":"acceptance-job","jobUID":"job-uid","completionTime":"2026-09-12T00:00:00Z","resultAvailable":true},
  "jobs":[{"name":"acceptance-job","uid":"job-uid","taskUID":"task-uid"}],
  "workerPods":[{"uid":"worker-pod","jobUID":"job-uid","phase":"Succeeded","restarts":0}]
}
JSON
jq '.controller.podUID = "controller-after" | .jobs = [] | .workerPods = []' \
  "${test_root}/before.json" >"${test_root}/after.json"
live_acp_release_chart_assert_recovery "${test_root}/before.json" "${test_root}/after.json"
for mutation in \
  '.pvcs[0].uid = "replacement"' \
  '.pvcs[1].volumeName = "replacement-volume"' \
  '.controller.claim = "new-empty-store"' \
  '.controller.podUID = "controller-before"' \
  '.task.uid = "replacement-task"' \
  '.task.attempts = 2' \
  '.task.resultAvailable = false' \
  '.jobs = [{"name":"replayed-job","uid":"replayed-uid"}]' \
  '.workerPods = [{"uid":"replayed-pod"}]'; do
  jq "${mutation}" "${test_root}/after.json" >"${test_root}/invalid-after.json"
  if live_acp_release_chart_assert_recovery "${test_root}/before.json" "${test_root}/invalid-after.json"; then
    fail "recovery accepted changed storage, task identity, attempt, or worker execution"
  fi
done
# Empty evidence must never qualify, even if both inputs happen to agree.
printf '{}\n' >"${test_root}/empty.json"
if live_acp_release_chart_assert_recovery "${test_root}/empty.json" "${test_root}/empty.json" 2>/dev/null; then
  fail "recovery accepted empty evidence"
fi
printf '%s\n' 'ok - recovery requires durable PVCs, controller replacement, and no recreated execution'

jq -n '{release:{revision:1,status:"deployed"},namespace:{mode:"harness-v2"},
  objects:[{uid:"same",generation:2}],secrets:[range(6) | {uid:("secret-" + tostring),resourceVersion:"1"}]}' \
  >"${test_root}/mode-before.json"
cp "${test_root}/mode-before.json" "${test_root}/mode-after.json"
printf 'Error: controller mode identity is missing or incompatible; namespace must already claim harness-v1\n' >"${test_root}/mode-error.log"
live_acp_release_chart_assert_mode_rejection "${test_root}/mode-before.json" "${test_root}/mode-after.json" "${test_root}/mode-error.log" 1
if live_acp_release_chart_assert_mode_rejection "${test_root}/mode-before.json" "${test_root}/mode-after.json" "${test_root}/mode-error.log" 0; then
  fail "mode rejection accepted successful upgrade"
fi
if live_acp_release_chart_assert_mode_rejection "${test_root}/empty.json" "${test_root}/empty.json" "${test_root}/mode-error.log" 1; then
  fail "mode rejection accepted empty state evidence"
fi
jq '.objects[0].generation = 3' "${test_root}/mode-before.json" >"${test_root}/mode-after.json"
if live_acp_release_chart_assert_mode_rejection "${test_root}/mode-before.json" "${test_root}/mode-after.json" "${test_root}/mode-error.log" 1; then
  fail "mode rejection accepted a mutated resource"
fi
cp "${test_root}/mode-before.json" "${test_root}/mode-after.json"
printf 'Error: Kubernetes cluster unreachable\n' >"${test_root}/mode-error.log"
if live_acp_release_chart_assert_mode_rejection "${test_root}/mode-before.json" "${test_root}/mode-after.json" "${test_root}/mode-error.log" 1; then
  fail "mode rejection accepted an unrelated Helm error"
fi
printf '%s\n' 'ok - mode rejection requires the expected guard and unchanged release state'

# Exercise real kubectl namespace resolution against a local Kubernetes API
# fixture, including resources that inherit Helm's release namespace.
(
  chart_work_dir="${test_root}/stable-state"
  mkdir "${chart_work_dir}"
  cat >"${chart_work_dir}/server.py" <<'PY'
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
from pathlib import Path
import sys
from urllib.parse import urlsplit

directory = Path(sys.argv[1])
resources = {}
for namespace, name in (("orka-system", "inherited"), ("orka-runtimes", "explicit")):
    resources[f"/api/v1/namespaces/{namespace}/configmaps/{name}"] = {
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"namespace": namespace, "name": name, "uid": name, "resourceVersion": "1"}}
for name in ("orka-system", "orka-runtimes"):
    resources[f"/api/v1/namespaces/{name}"] = {
        "apiVersion": "v1", "kind": "Namespace",
        "metadata": {"name": name, "uid": name, "labels": {"orka.ai/controller-mode": "harness-v2"}}}
for suffix in ("snapshot", "webhook", "publisher", "provider", "scm", "artifact"):
    name = "release-chart-" + suffix
    resources[f"/api/v1/namespaces/orka-system/secrets/{name}"] = {
        "apiVersion": "v1", "kind": "Secret",
        "metadata": {"namespace": "orka-system", "name": name, "uid": name, "resourceVersion": "1"}}
resources["/api"] = {"apiVersion": "v1", "kind": "APIVersions", "versions": ["v1"]}
resources["/apis"] = {"apiVersion": "v1", "kind": "APIGroupList", "groups": []}
resources["/api/v1"] = {
    "apiVersion": "v1", "kind": "APIResourceList", "groupVersion": "v1", "resources": [
        {"name": name, "singularName": "", "namespaced": namespaced, "kind": kind, "verbs": ["get", "list"]}
        for name, kind, namespaced in (("configmaps", "ConfigMap", True), ("namespaces", "Namespace", False),
                                      ("secrets", "Secret", True))]}

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_GET(self):
        resource = resources.get(urlsplit(self.path).path)
        self.send_response(200 if resource is not None else 404)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(resource or {
            "apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": "NotFound", "code": 404
        }).encode())

server = HTTPServer(("127.0.0.1", 0), Handler)
(directory / "port").write_text(str(server.server_port))
server.serve_forever()
PY
  python3 "${chart_work_dir}/server.py" "${chart_work_dir}" >"${chart_work_dir}/server.log" 2>&1 &
  state_server_pid=$!
  trap 'kill "${state_server_pid}" >/dev/null 2>&1 || true; wait "${state_server_pid}" >/dev/null 2>&1 || true' EXIT
  deadline=$((SECONDS + 10))
  while [[ ! -s "${chart_work_dir}/port" ]] && (( SECONDS < deadline )); do sleep 0.1; done
  [[ -s "${chart_work_dir}/port" ]] || fail "Kubernetes API fixture did not start"

  export LIVE_ACP_CONTEXT=kind-chart-state LIVE_ACP_KUBECONFIG="${chart_work_dir}/original-kubeconfig.json"
  export KUBECONFIG="${LIVE_ACP_KUBECONFIG}"
  jq -n --arg server "http://127.0.0.1:$(cat "${chart_work_dir}/port")" --arg context "${LIVE_ACP_CONTEXT}" '
    {apiVersion:"v1",kind:"Config",clusters:[{name:"fixture",cluster:{server:$server}}],
     contexts:[{name:$context,context:{cluster:"fixture",namespace:"unrelated"}}],"current-context":$context}' \
    >"${LIVE_ACP_KUBECONFIG}"
  cp "${LIVE_ACP_KUBECONFIG}" "${chart_work_dir}/original-before.json"
  jq -n '{apiVersion:"v1",kind:"List",items:[
    {apiVersion:"v1",kind:"ConfigMap",metadata:{name:"inherited"}},
    {apiVersion:"v1",kind:"ConfigMap",metadata:{name:"explicit",namespace:"orka-runtimes"}},
    {apiVersion:"v1",kind:"Namespace",metadata:{name:"orka-runtimes"}}]}' >"${chart_work_dir}/manifest.json"
  kubectl() { command kubectl --cache-dir="${chart_work_dir}/cache" "$@"; }
  helm() {
    case "$*" in
      *" status orka -o json") printf '{"version":1,"info":{"status":"deployed"}}\n' ;;
      *" get manifest orka") cat "${chart_work_dir}/manifest.json" ;;
      *) fail "unexpected Helm operation in state observation" ;;
    esac
  }
  _live_acp_release_chart_stable_state "${chart_work_dir}/state.json"
  jq -e '(.objects | map([.kind,.namespace,.name])) == [
    ["ConfigMap","orka-runtimes","explicit"], ["ConfigMap","orka-system","inherited"],
    ["Namespace",null,"orka-runtimes"]] and (.secrets | length) == 6' "${chart_work_dir}/state.json" >/dev/null
  cmp -s "${LIVE_ACP_KUBECONFIG}" "${chart_work_dir}/original-before.json" || fail "state observation changed the scoped kubeconfig"
  jq '(.items[] | select(.metadata.name == "explicit")).metadata.namespace = "missing-namespace"' \
    "${chart_work_dir}/manifest.json" >"${chart_work_dir}/missing.json"
  mv "${chart_work_dir}/missing.json" "${chart_work_dir}/manifest.json"
  if _live_acp_release_chart_stable_state "${chart_work_dir}/missing-state.json" >"${chart_work_dir}/missing.log" 2>&1; then
    fail "state observation accepted a missing resource"
  fi
)
printf '%s\n' 'ok - state observation preserves explicit namespaces and defaults inherited namespaces without editing kubeconfig'

# A local HTTP server stands in for kubectl port-forward. Check the real curl
# and PID cleanup path without creating a cluster or using provider access.
mkdir "${test_root}/bin" "${test_root}/api"
cat >"${test_root}/bin/kubectl" <<'PY'
#!/usr/bin/env python3
from http.server import BaseHTTPRequestHandler, HTTPServer
import os
from pathlib import Path

directory = Path(os.environ["ACP_CHART_TEST_SERVER_DIR"])
class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_GET(self):
        if self.path == "/healthz":
            body, status = b"ok", 200
        elif self.headers.get("Authorization") == "Bearer synthetic-chart-test-token":
            body, status = (directory / "response.json").read_bytes(), 200
        else:
            body, status = b"", 401
        self.send_response(status)
        self.end_headers()
        self.wfile.write(body)

server = HTTPServer(("127.0.0.1", 0), Handler)
(directory / "server.pid").write_text(str(os.getpid()))
print("Forwarding from 127.0.0.1:%d -> 8080" % server.server_port, flush=True)
server.serve_forever()
PY
chmod +x "${test_root}/bin/kubectl"
(
  chart_work_dir="${test_root}/api"
  chart_api_pid=""
  export LIVE_ACP_CONTEXT=kind-chart-test ACP_CHART_TEST_SERVER_DIR="${chart_work_dir}"
  export PATH="${test_root}/bin:${PATH}"
  trap '_live_acp_release_chart_stop_api' EXIT
  printf 'Authorization: Bearer synthetic-chart-test-token\n' >"${chart_work_dir}/api-header"
  printf '{"result":"expected-chart-result"}\n' >"${chart_work_dir}/response.json"
  _live_acp_release_chart_start_api
  _live_acp_release_chart_result acceptance expected-chart-result
  printf '{"result":"changed-result-must-not-be-printed"}\n' >"${chart_work_dir}/response.json"
  if _live_acp_release_chart_result acceptance expected-chart-result >"${chart_work_dir}/result-check.log" 2>&1; then
    fail "result assertion accepted changed durable data"
  fi
  [[ ! -s "${chart_work_dir}/result-check.log" ]] || fail "result assertion printed response contents"
  _live_acp_release_chart_stop_api
  [[ -z "${chart_api_pid}" ]] || fail "API cleanup retained a stale process PID"
  if kill -0 "$(cat "${chart_work_dir}/server.pid")" >/dev/null 2>&1; then
    fail "API cleanup orphaned the port-forward process"
  fi
)
printf '%s\n' 'ok - authenticated result comparison rejects changed data and stops its port-forward'

# Guard failures occur before any command can touch an unrelated cluster.
kubectl() { fail "unowned context reached kubectl"; }
if LIVE_ACP_KIND_CREATED=0 LIVE_ACP_CONTEXT=production live_acp_kind_deploy_release_chart >"${test_root}/guard.log" 2>&1; then
  fail "helper accepted an unowned context"
fi
grep -F 'bootstrap-owned Kind context' "${test_root}/guard.log" >/dev/null
if LIVE_ACP_KIND_CREATED=1 LIVE_ACP_KIND_CLUSTER=test LIVE_ACP_CONTEXT=kind-test \
  LIVE_ACP_KUBECONFIG=/tmp/scoped-config KUBECONFIG=/tmp/global-config \
  live_acp_kind_deploy_release_chart >"${test_root}/guard.log" 2>&1; then
  fail "helper accepted the global kubeconfig instead of the scoped one"
fi
grep -F 'bootstrap-owned Kind context' "${test_root}/guard.log" >/dev/null
printf '%s\n' 'ok - cluster mutations require the bootstrap-owned scoped Kind context'
