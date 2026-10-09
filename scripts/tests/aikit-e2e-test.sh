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
    'get deployment orka-provider-auth-proxy')
      if [[ "${missing_flag:-false}" == true ]]; then
        printf '%s\n' '{"spec":{"template":{"spec":{"containers":[{"name":"proxy","args":["--token-file=/token"]}]}}}}'
      else
        printf '%s\n' '{"spec":{"template":{"spec":{"containers":[{"name":"proxy","args":["--listen-address=:8080","--upstream-base-url=http://vekil.vekil-system.svc:1337","--token-file=/token","--token-reload-interval=5s"]}]}}}}'
      fi
      ;;
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
printf '%s\n' 'ok - local upstream keeps auth flags and restricted DNS/model egress'

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
assert 'image: ghcr.io/kaito-project/aikit/qwen3.5:2b@sha256:d838c5eebf533b5b73f67cc7f4984921f87d64d28e4a52d482740b667f46b8b4' in manifest
assert 'automountServiceAccountToken: false' in manifest
assert 'args: ["--config-file=/etc/orka-aikit/config.yaml"]' in manifest
assert 'mountPath: /etc/orka-aikit' in manifest
assert 'readOnly: true' in manifest
for name,value in [('LOCALAI_THREADS','"4"'),('LOCALAI_CONTEXT_SIZE','"32768"'),('LOCALAI_LOAD_TO_MEMORY','qwen-3.5-2b')]:
    assert re.search(r'name: '+name+r'\s+value: '+re.escape(value),manifest)
assert 'limits:\n              cpu: "4"\n              memory: 6Gi' in manifest
assert 'policyTypes: [Ingress]' in manifest
assert 'kubernetes.io/metadata.name: orka-system' in manifest
assert 'namespace: vekil' not in manifest
assert len(re.findall(r'^kind: ',manifest,re.M)) == 4
PY
printf '%s\n' 'ok - pinned Qwen deploy is CPU bounded and restricts model ingress'
require_cmd() { :; }
if (aikit_image=mutable-tag main) >/dev/null 2>&1; then
  echo 'mutable AIKit image was accepted' >&2; exit 1
fi
if grep -Eq 'secrets\.|COPILOT_GITHUB_TOKEN|retrying' "${root}/.github/workflows/live-copilot-proxy-e2e.yml"; then
  echo 'AIKit CI still depends on cloud credentials or whole-suite retries' >&2; exit 1
fi
printf '%s\n' 'ok - CI requires immutable images and no cloud credentials or retries'
