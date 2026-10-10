#!/usr/bin/env bash
set -Eeuo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
# shellcheck source=scripts/aikit-e2e.sh
source "${root}/scripts/aikit-e2e.sh"

kubectl() {
  case "$1 ${2:-} ${3:-}" in
    'config current-context ')
      if [[ "${wrong_context:-false}" == true ]]; then printf '%s\n' production;
      else printf '%s\n' "kind-${kind_cluster}"; fi
      ;;
    '--request-timeout=5s get pods')
      printf '%s\n' '{"items":[{"metadata":{"uid":"model-pod"},"spec":{"nodeName":"e2e-node"},"status":{"phase":"Running"}}]}'
      ;;
    '--request-timeout=10s get --raw')
      [[ "$*" == *'/api/v1/nodes/e2e-node/proxy/stats/summary' ]]
      printf '%s\n' '{"pods":[{"podRef":{"uid":"foreign-pod","namespace":"private"},"cpu":{"usageNanoCores":99},"memory":{"rssBytes":99}},{"podRef":{"uid":"model-pod","namespace":"aikit-system"},"cpu":{"usageNanoCores":1234},"memory":{"rssBytes":3000000000}}]}'
      ;;
    'get deployment orka-provider-auth-proxy')
      if [[ "${missing_flag:-false}" == true ]]; then
        printf '%s\n' '{"spec":{"template":{"spec":{"containers":[{"name":"proxy","args":["--token-file=/token"]}]}}}}'
      else
        printf '%s\n' '{"spec":{"template":{"spec":{"containers":[{"name":"proxy","args":["--listen-address=:8080","--upstream-base-url=http://vekil.vekil-system.svc:1337","--token-file=/token","--token-reload-interval=5s"]}]}}}}'
      fi
      ;;
    'get deployment orka-controller-manager')
      printf '%s\n' '{"spec":{"template":{"spec":{"containers":[{"name":"manager","args":["--enable-acp","--chat-max-duration=30m","--chat-max-concurrent=10"]}]}}}}'
      ;;
    'patch deployment orka-controller-manager') printf '%s\n' "${@: -1}" >"${work}/controller-patch.json" ;;
    'rollout status deployment/orka-controller-manager') printf '%s\n' controller-rollout >>"${work}/calls" ;;
    'get networkpolicy orka-provider-auth-proxy')
      printf '%s\n' '{"spec":{"egress":[{"to":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"kube-system"}},"podSelector":{"matchLabels":{"k8s-app":"kube-dns"}}}],"ports":[{"protocol":"UDP","port":53},{"protocol":"TCP","port":53}]},{"to":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"vekil-system"}},"podSelector":{"matchLabels":{"app.kubernetes.io/name":"vekil"}}}],"ports":[{"protocol":"TCP","port":1337}]}]}}'
      ;;
    'patch deployment orka-provider-auth-proxy') printf '%s\n' "${@: -1}" >"${work}/deployment-patch.json" ;;
    'patch networkpolicy orka-provider-auth-proxy') printf '%s\n' "${@: -1}" >"${work}/network-patch.json" ;;
    'rollout status deployment/orka-provider-auth-proxy') printf '%s\n' rollout >>"${work}/calls" ;;
    'delete namespace vekil-system') printf '%s\n' delete-namespace >>"${work}/calls" ;;
    'create configmap aikit-configuration')
      [[ "$*" == *"--from-file=config.yaml="* && "$*" == *"--from-file=chat-template.jinja="* ]]
      printf '%s\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: aikit-configuration'
      ;;
    'apply -f -')
      input="$(cat)"
      if [[ "${input}" == *'kind: ConfigMap'* ]]; then printf '%s\n' "${input}" >"${work}/configuration.yaml";
      else printf '%s\n' "${input}" >"${work}/aikit.yaml"; fi
      ;;
    'rollout status deployment/aikit') printf '%s\n' model-rollout >>"${work}/calls" ;;
    *) echo "unexpected kubectl invocation: $*" >&2; return 1 ;;
  esac
}

# Verify the fixture lookup against the actual name-prefixed installation,
# rather than trusting the unrendered base resource name.
[[ -x "${root}/bin/kustomize" ]] || { echo 'run make kustomize before this test' >&2; exit 1; }
"${root}/bin/kustomize" build "${root}/config/acp-production" >"${work}/production.yaml"
  python3 - "${work}/production.yaml" <<'PYNAME'
import sys,re
objects=open(sys.argv[1]).read().split('\n---\n')
assert any(re.search(r'^kind: NetworkPolicy$',obj,re.M) and
           re.search(r'^  name: orka-provider-auth-proxy$',obj,re.M) and
           re.search(r'^  namespace: orka-system$',obj,re.M) for obj in objects)
PYNAME

export E2E_LOCAL_MODEL="${aikit_model}"
if (wrong_context=true configure_provider_proxy) >/dev/null 2>&1; then
  echo 'non-E2E cluster context was accepted' >&2; exit 1
fi
[[ ! -e "${work}/deployment-patch.json" ]]
configure_provider_proxy
jq -e '.spec.template.spec.containers[0] | .name == "proxy" and
  .args == ["--listen-address=:8080","--upstream-base-url=http://aikit.aikit-system.svc:8080","--token-file=/token","--token-reload-interval=5s"]' \
  "${work}/deployment-patch.json" >/dev/null
jq -e '.spec.egress | length == 2 and
  .[0].to[0].namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == "kube-system" and
  .[0].ports == [{protocol:"UDP",port:53},{protocol:"TCP",port:53}] and
  .[1].to == [{namespaceSelector:{matchLabels:{"kubernetes.io/metadata.name":"aikit-system"}},podSelector:{matchLabels:{"app.kubernetes.io/name":"aikit"}}}] and
  .[1].ports == [{protocol:"TCP",port:8080}]' "${work}/network-patch.json" >/dev/null
jq -e '.spec.template.spec.containers[0] | .name == "manager" and
  .args == ["--enable-acp","--chat-max-concurrent=10","--chat-max-duration=170s"]' "${work}/controller-patch.json" >/dev/null
printf '%s\n' 'ok - local upstream keeps auth flags and restricted DNS/model egress'
printf '%s\n' 'ok - CI server work ends before the unchanged 180s client deadline'
sample_model_resources >"${work}/model-resources.log"
jq -e '.cpu.usageNanoCores == 1234 and .memory.rssBytes == 3000000000 and
  (has("podRef") | not) and (tostring | contains("private") | not)' "${work}/model-resources.log" >/dev/null
if (wrong_context=true sample_model_resources) >/dev/null 2>&1; then
  echo 'resource sampling accepted a non-E2E context' >&2; exit 1
fi
printf '%s\n' 'ok - model diagnostics capture only counters on the verified E2E context'

if (E2E_LOCAL_MODEL=wrong configure_provider_proxy) >/dev/null 2>&1; then
  echo 'mismatched local model was accepted' >&2; exit 1
fi
rm "${work}/deployment-patch.json"
if (missing_flag=true configure_provider_proxy) >/dev/null 2>&1; then
  echo 'missing upstream flag was accepted' >&2; exit 1
fi
[[ ! -e "${work}/deployment-patch.json" ]]
printf '%s\n' 'ok - missing upstream flag and mismatched model fail closed'

deploy_aikit
python3 - "${work}/aikit.yaml" <<'PY'
import sys,re
manifest=open(sys.argv[1]).read()
assert 'image: ghcr.io/kaito-project/aikit/qwen3.5:4b@sha256:525dfb8b5ccc1c180f0eab633bcf459d2a574c14bfab01c03535191c810d121d' in manifest
assert 'automountServiceAccountToken: false' in manifest
assert 'args: ["--config-file=/etc/orka-aikit/config.yaml"]' in manifest
assert 'mountPath: /etc/orka-aikit' in manifest
assert 'readOnly: true' in manifest
for name,value in [('LOCALAI_THREADS','"4"'),('LOCALAI_CONTEXT_SIZE','"32768"'),('LOCALAI_LOAD_TO_MEMORY','qwen-3.5-4b')]:
    assert re.search(r'name: '+name+r'\s+value: '+re.escape(value),manifest)
assert 'limits:\n              cpu: "4"\n              memory: 6Gi' in manifest
assert 'policyTypes: [Ingress]' in manifest
assert 'kubernetes.io/metadata.name: orka-system' in manifest
assert 'namespace: vekil' not in manifest
assert len(re.findall(r'^kind: ',manifest,re.M)) == 4
PY
printf '%s\n' 'ok - pinned Qwen deploy is CPU bounded and restricts model ingress'
python3 - "${root}/scripts/fixtures/aikit/config.yaml" "${root}/.github/workflows/live-copilot-proxy-e2e.yml" <<'PYCONFIG'
import sys,re
configuration=open(sys.argv[1]).read()
workflow=open(sys.argv[2]).read()
assert re.search(r'^- name: qwen-3\.5-4b$',configuration,re.M)
assert 'model: Qwen3.5-4B-Q4_K_M.gguf' in configuration
for key,value in [('temperature','0.7'),('top_p','0.8'),('top_k','20'),('min_p','0.0'),('presence_penalty','1.5'),('repeat_penalty','1.0')]:
    assert re.search(r'^    '+key+r': '+re.escape(value)+r'$',configuration,re.M)
assert 'enable_thinking: false' in configuration
assert '"n_ubatch:128"' in configuration
assert 'flash_attention: "off"' in configuration
assert re.search(r'^  reasoning:\n    disable: true$',configuration,re.M)
assert re.search(r'^    runs-on: ubuntu-latest$',workflow,re.M)
assert re.search(r'^    timeout-minutes: 120$',workflow,re.M)
assert 'AIKIT_MODEL: qwen-3.5-4b' in workflow
assert 'AIKIT_IMAGE: ghcr.io/kaito-project/aikit/qwen3.5:4b@sha256:525dfb8b5ccc1c180f0eab633bcf459d2a574c14bfab01c03535191c810d121d' in workflow
PYCONFIG
printf '%s\n' 'ok - 4B image, model config, sampling, and free Actions runner agree'
require_cmd() { :; }
if (aikit_image=mutable-tag main) >/dev/null 2>&1; then
  echo 'mutable AIKit image was accepted' >&2; exit 1
fi
if grep -Eq 'secrets\.|COPILOT_GITHUB_TOKEN|retrying' "${root}/.github/workflows/live-copilot-proxy-e2e.yml"; then
  echo 'AIKit CI still depends on cloud credentials or whole-suite retries' >&2; exit 1
fi
printf '%s\n' 'ok - CI requires immutable images and no cloud credentials or retries'
python3 - "${root}/scripts/aikit-e2e.sh" <<'PYEMBED'
import sys
script = open(sys.argv[1]).read()
assert script.index("make ensure-ui-embed") < script.index("AIKIT_PROBE_URL=")
PYEMBED
printf '%s\n' 'ok - fresh checkouts initialize UI embed before the full-prompt Go probe'

work_dir="${work}"
curl() {
  local payload="" arg result_value text
  for arg in "$@"; do [[ "${arg}" != @* ]] || payload="${arg#@}"; done
  [[ -f "${payload}" ]] || { echo 'missing synthetic preflight payload' >&2; return 1; }
  if [[ "${payload}" == */tools-warmup.json ]]; then
    jq -e '.stream == true and .max_output_tokens == 128 and
      .tool_choice == {type:"function",name:"ci_echo"} and .tools[0].name == "ci_echo"' "${payload}" >/dev/null || return 1
    [[ "${stream_failure:-false}" != true ]] || return 28
    printf '%s\n\n' 'event: response.created' 'data: {"type":"response.created"}'
    if [[ "${missing_terminal:-false}" != true ]]; then
      printf 'data: %s\n\n' "$(jq -nc --arg name "${tool_name:-ci_echo}" --arg arguments "${tool_arguments:-{\"text\":\"ORKA_TOOL_READY\"}}" \
        '{type:"response.completed",response:{status:"completed",output:[{type:"function_call",name:$name,call_id:"call_echo",arguments:$arguments}]}}')"
    fi
    printf '%s\n\n' 'data: [DONE]'
  else
    [[ "${payload}" == */tool-result-warmup.json ]] || return 1
    result_value="$(jq -er '.input[2].output | select(type == "string")' "${payload}")" || return 1
    jq -e --arg result_value "${result_value}" '.input[1].type == "function_call" and
      .input[2] == {type:"function_call_output",call_id:"call_echo",output:$result_value} and
      ($result_value | test("^ORKA_TOOL_OUTPUT_[0-9a-f]{32}$")) and
      .input[3] == {role:"developer",content:"Reply with exactly the tool output text and nothing else."} and
      (del(.input[2].output) | all(.. | strings; contains($result_value) | not))' "${payload}" >/dev/null || return 1
    case "${tool_result_mode:-exact}" in
      exact) text="${result_value}" ;;
      wrong) text=wrong_marker ;;
      prompt) text="$(jq -r '.input[0].content' "${payload}")" ;;
      arguments) text="$(jq -r '.input[1].arguments | fromjson | .text' "${payload}")" ;;
      arguments_json) text="$(jq -r '.input[1].arguments' "${payload}")" ;;
      developer) text="$(jq -r '.input[3].content' "${payload}")" ;;
      prefix) text="Tool result: ${result_value}" ;;
      suffix) text="${result_value} done" ;;
      whitespace) text=" ${result_value} " ;;
      newline) text="$(printf '%s\nextra line' "${result_value}")" ;;
      *) echo "unexpected synthetic tool result mode: ${tool_result_mode}" >&2; return 1 ;;
    esac
    jq -nc --arg text "${text}" \
      '{output:[{type:"message",content:[{type:"output_text",text:$text}]}]}'
  fi
}
qualify_responses_tools http://synthetic.invalid
first_tool_result="$(jq -r '.input[2].output' "${work}/tool-result-warmup.json")"
qualify_responses_tools http://synthetic.invalid
[[ "$(jq -r '.input[2].output' "${work}/tool-result-warmup.json")" != "${first_tool_result}" ]] || {
  echo 'tool-result preflight reused its previous value' >&2; exit 1;
}
printf '%s\n' 'ok - tool-result preflight exposes a fresh value only in function_call_output'
for failure in stream_failure missing_terminal tool_name tool_arguments; do
  case "${failure}" in
    stream_failure|missing_terminal) value=true ;;
    tool_name) value=wrong_tool ;;
    tool_arguments) value='{"text":"wrong"}' ;;
  esac
  if (export "${failure}=${value}"; qualify_responses_tools http://synthetic.invalid) >/dev/null 2>&1; then
    echo "streamed tool preflight accepted ${failure}" >&2; exit 1
  fi
done
printf '%s\n' 'ok - streamed Responses requires completion and the exact tool call arguments'
for mode in wrong prompt arguments arguments_json developer prefix suffix whitespace newline; do
  if (tool_result_mode="${mode}"; qualify_responses_tools http://synthetic.invalid) >/dev/null 2>&1; then
    echo "tool-result preflight accepted ${mode}" >&2; exit 1
  fi
done
printf '%s\n' 'ok - tool-result preflight rejects prompt/argument echoes and surrounding text'
