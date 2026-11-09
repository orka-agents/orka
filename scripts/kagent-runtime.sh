#!/usr/bin/env bash
# Deploys, activates, and retires the runtimes of wrapped kagent Agents, from
# the deployment bundles "orka agent import kagent --bundle-dir" writes.
#
# A bundle names its runtime after the configuration it pins, so re-pinning an
# Agent (after kagent recompiles it, or after an upgrade) deploys a new
# runtime beside the old one. Orka cannot change a registered AgentRuntime's
# profile in place, and the old runtime keeps serving its sessions until it is
# retired.
set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd "${script_dir}/.." && pwd)"

usage() {
  cat <<'USAGE'
Usage:
  scripts/kagent-runtime.sh deploy <bundle>
      Build and push the runtime's images, deploy it, and register it with
      Orka under Kubernetes recovery, so Orka restarts it after an Orka
      controller restart. Waits until Orka reports the AgentRuntime Ready. A governed
      Agent's relay moves to the new runtime before registration, because
      Orka's conformance prompt calls it; until activation, governed turns on
      the old runtime fail their tool and model calls. If deploy fails or is
      interrupted before the runtime is Ready, the relay moves back.
  scripts/kagent-runtime.sh activate <bundle> -- <import arguments>
      Point the Orka Agents at their runtimes by re-running the import without
      --dry-run, with the same arguments that wrote the bundles; the import
      refuses arguments that pin other bundles. The script passes
      --bundle-dir, the directory of <bundle>, itself. It switches every Agent in
      its input, and only if all of their runtimes are Ready, so deploy them
      all first. Refuses while the relay of a governed Agent bundled beside
      <bundle> selects another runtime.
  scripts/kagent-runtime.sh retire <bundle>
      Delete the Agent's other runtimes. Orka drains each one first, so this
      waits until their sessions are done. Refuses while a governed Agent's
      relay selects another runtime. Running it again finishes an earlier
      retire that stopped.

<bundle> is a directory the import wrote, <bundle-dir>/<agent>.

Each command holds a lock per Agent it changes, the ConfigMap
<agent>-kagent-lock, so runs for one Agent never overlap. A run that was
killed leaves its lock; delete the ConfigMap once no run is left.

Environment:
  KUBECTL_CONTEXT            kubectl context (required)
  NAMESPACE                  namespace of the runtimes (default orka-system)
  CONTROLLER_NAMESPACE       namespace of the Orka controller (default NAMESPACE)
  ORKA                       orka CLI (default: orka)
  WRAPPER_SRC                orka-runtime-kagent checkout (deploy)
  IMAGE_REPO                 image repository prefix, such as ghcr.io/me (deploy)
  BUILD_PLATFORM             image platform (default linux/amd64)
  MODEL_UPSTREAM_TOKEN_FILE  file holding Orka's credential for a governed
                             Agent's model upstream (deploy, governed)
  STORAGE_CLASS              StorageClass of the broker ledger (default: cluster default)
  ORKA_API_URL               Orka API URL the supervisor calls
  SUBSTRATE_NAMESPACE        kagent v1 Substrate namespace (default ate-system)
  READY_TIMEOUT              wait for a runtime to be Ready (default 5m)
  RETIRE_TIMEOUT             wait for an old runtime to drain (default 30m)
USAGE
}

die() {
  echo "kagent-runtime: $*" >&2
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

kc() {
  kubectl --context "${KUBECTL_CONTEXT}" -n "${NAMESPACE}" "$@"
}

# Rendered objects carry their own namespaces: a v0 runtime's pod reader lives
# in the kagent Agent's namespace.
apply() {
  kubectl --context "${KUBECTL_CONTEXT}" apply -f -
}

# tostring keeps a false field from failing jq -e.
bundle_field() {
  jq -er --arg field "$1" '.[$field] | tostring' "${bundle}/runtime.json"
}

render() {
  local part=$1
  shift
  local args=(agent-runtime render-kagent "${bundle}" -n "${NAMESPACE}" --part "${part}"
    --configured-image "${configured_image}" --runtime-image "${runtime_image}")
  [ -n "${STORAGE_CLASS:-}" ] && args+=(--storage-class "${STORAGE_CLASS}")
  [ -n "${ORKA_API_URL:-}" ] && args+=(--orka-api-url "${ORKA_API_URL}")
  [ -n "${SUBSTRATE_NAMESPACE:-}" ] && args+=(--substrate-namespace "${SUBSTRATE_NAMESPACE}")
  "${ORKA}" "${args[@]}" "$@"
}

# The images a runtime runs are recorded on its Deployment, so activate and
# retire need no rebuild.
deployed_images() {
  configured_image=$(kc get deployment "${runtime}" -o jsonpath='{.spec.template.spec.containers[?(@.name=="broker")].image}')
  runtime_image=$(kc get deployment "${runtime}" -o jsonpath='{.spec.template.spec.containers[?(@.name=="supervisor")].image}')
  [ -n "${configured_image}" ] && [ -n "${runtime_image}" ] || die "runtime ${runtime} is not deployed; run deploy first"
}

build() {
  local tag=$1 metadata=$2
  shift 2
  docker buildx build --platform "${BUILD_PLATFORM}" --push --metadata-file "${metadata}" -t "${tag}" "$@" >&2
  jq -er '."containerimage.digest"' "${metadata}"
}

relay_service_of() {
  jq -er '.relay' "$1/runtime.json" | sed -E 's#^http://([^.]+)\..*#\1#'
}

# relay_selector prints a relay Service's selector, or nothing if there is no
# such Service. Any other lookup failure fails.
relay_selector() {
  kc get service "$1" --ignore-not-found -o json | jq -c '.spec.selector // {}'
}

# registration_of prints the AgentRuntime's name if it is registered. Any
# lookup failure but NotFound fails.
registration_of() {
  kc get agentruntime "$1" --ignore-not-found -o name
}

# relay_owner prints the Agent and kagent Agent a relay object is labeled and
# annotated for, as "<agent>|<apiVersion> <namespace>/<name>", or nothing if
# it is absent.
relay_owner() {
  kc get "$1" "$2" --ignore-not-found -o json |
    jq -r '"\(.metadata.labels["orka.ai/kagent-agent"] // "-")|\(.metadata.annotations["orka.ai/kagent-source"] // "-")"'
}

same_bundle_dir() {
  [ -d "$1" ] && [ "$(cd "$1" && pwd -P)" = "$2" ]
}

# Until a deployed runtime is Ready, any failure, or an interrupt, moves its
# Agent's relay back to the runtime it selected before.
#
# The restore applies only while the relay still selects this runtime: a
# concurrent deploy of the Agent may have moved it since.
restore_relay() {
  if [ -n "${relay_rollback}" ]; then
    echo "Runtime ${runtime} is not Ready; moving relay ${relay_service} back." >&2
    kc patch service "${relay_service}" --type=json -p "[{\"op\":\"test\",\"path\":\"/spec/selector\",\"value\":{\"orka.ai/kagent-runtime\":\"${runtime}\"}},{\"op\":\"replace\",\"path\":\"/spec/selector\",\"value\":${relay_rollback}},{\"op\":\"replace\",\"path\":\"/spec/ports\",\"value\":${relay_rollback_ports}}]" >/dev/null ||
      echo "kagent-runtime: relay ${relay_service} was not moved back to ${relay_rollback}; it does not select ${runtime}, or the patch failed" >&2
  fi
  # The policy goes back too, while it is still the one this deploy wrote:
  # one it created is deleted, under the Agent's lock, if it is that object.
  if [ -n "${relay_created_policy}" ]; then
    local uid
    uid=$(kc get networkpolicy "${relay_service}" --ignore-not-found -o jsonpath='{.metadata.uid}') || uid=""
    if [ "${uid}" = "${relay_created_policy}" ]; then
      kc delete networkpolicy "${relay_service}" >/dev/null ||
        echo "kagent-runtime: could not delete relay policy ${relay_service}, which this deploy created" >&2
    fi
    return 0
  fi
  [ -n "${relay_rollback_policy}" ] && [ -n "${relay_applied_policy}" ] || return 0
  kc patch networkpolicy "${relay_service}" --type=json -p "[{\"op\":\"test\",\"path\":\"/spec\",\"value\":${relay_applied_policy}},{\"op\":\"replace\",\"path\":\"/spec\",\"value\":${relay_rollback_policy}}]" >/dev/null ||
    echo "kagent-runtime: relay policy ${relay_service} was not restored; it changed since this deploy applied it, or the patch failed" >&2
}

# lock_agents holds a lock for each Agent, so one deploy, activate, or retire
# at a time changes its runtimes and relay. A lock is a ConfigMap, created
# atomically and deleted on exit.
lock_agents() {
  local name
  for name in "$@"; do
    kc create configmap "${name}-kagent-lock" --from-literal=holder="${command} pid $$ on $(hostname)" >/dev/null ||
      die "another deploy, activate, or retire of Agent ${name} holds ConfigMap ${name}-kagent-lock; if that run is gone, delete it"
    locks+=("${name}-kagent-lock")
  done
}

cleanup() {
  restore_relay
  [ -z "${work}" ] || rm -rf "${work}"
  local lock
  for lock in "${locks[@]}"; do
    kc delete configmap "${lock}" --ignore-not-found >/dev/null || echo "kagent-runtime: could not delete lock ${lock}" >&2
  done
}

# require_relay refuses unless a governed bundle's relay selects the bundle's
# runtime, which otherwise cannot make its tool and model calls.
require_relay() {
  local b=$1 governed_bundle service want got
  governed_bundle=$(jq -er '.governed | tostring' "$b/runtime.json")
  [ "${governed_bundle}" = true ] || return 0
  service=$(relay_service_of "$b")
  want=$(jq -cn --arg runtime "$(jq -r '.runtime' "$b/runtime.json")" '{"orka.ai/kagent-runtime": $runtime}')
  got=$(relay_selector "${service}")
  [ "${got}" = "${want}" ] ||
    die "relay ${service} selects ${got:-nothing}, not ${want}; move it with: $0 deploy ${b}"
}

# The epoch record lives in the Orka controller's namespace, which several
# Orka installations in one cluster each have.
controller_epoch() {
  kubectl --context "${KUBECTL_CONTEXT}" -n "${CONTROLLER_NAMESPACE}" get cepoch -o json |
    jq -er '[.items[] | select(.spec.name == "orka-controller") | .status.epoch] |
      if length == 1 and (.[0] | type == "number" and . > 0)
      then .[0] else error("expected one initialized controller epoch") end'
}

# The auth Secret is bound to one registration and endpoint. Its values are
# random and never printed; a governed Agent's upstream credential comes from
# MODEL_UPSTREAM_TOKEN_FILE.
ensure_auth_secret() {
  local secret="${runtime}-auth" found key
  found=$(kc get secret "${secret}" --ignore-not-found -o name)
  if [ -z "${found}" ]; then
    # The values go through files in the private work directory, never
    # through a command line.
    local args=()
    for key in controller-token capability-secret provider-token; do
      openssl rand -hex 32 | tr -d '\n' >"${work}/${key}"
      args+=(--from-file="${key}=${work}/${key}")
    done
    if [ "${governed}" = true ]; then
      [ -r "${MODEL_UPSTREAM_TOKEN_FILE:-}" ] || die "a governed Agent needs MODEL_UPSTREAM_TOKEN_FILE"
      # The broker refuses a credential with control characters, such as the
      # newline a file usually ends with.
      tr -d '\r\n' <"${MODEL_UPSTREAM_TOKEN_FILE}" >"${work}/model-upstream-token"
      args+=(--from-file=model-upstream-token="${work}/model-upstream-token")
    fi
    kc create secret generic "${secret}" "${args[@]}" >/dev/null
  elif [ "${governed}" = true ] && [ -n "${MODEL_UPSTREAM_TOKEN_FILE:-}" ]; then
    # The broker reads the credential once, at start, so a runtime keeps it.
    # Compare digests only; the credential is never printed.
    [ -r "${MODEL_UPSTREAM_TOKEN_FILE}" ] || die "cannot read MODEL_UPSTREAM_TOKEN_FILE"
    local stored given
    stored=$(kc get secret "${secret}" -o json | jq -er '.data["model-upstream-token"]' | base64 -d | openssl dgst -sha256 -r | cut -d' ' -f1)
    given=$(tr -d '\r\n' <"${MODEL_UPSTREAM_TOKEN_FILE}" | openssl dgst -sha256 -r | cut -d' ' -f1)
    [ "${stored}" = "${given}" ] ||
      die "runtime ${runtime} holds another upstream credential than MODEL_UPSTREAM_TOKEN_FILE; rotate it by deploying under a new --runtime-revision"
  fi
  # Orka uses the Secret only with this metadata, which a deploy that stopped
  # after creating it did not set.
  kc label --overwrite secret "${secret}" orka.ai/agent-runtime-auth=true orka.ai/agent-runtime-name="${runtime}" \
    orka.ai/kagent-runtime="${runtime}" orka.ai/kagent-agent="${agent}" >/dev/null
  kc annotate --overwrite secret "${secret}" \
    orka.ai/agent-runtime-endpoint="http://${runtime}.${NAMESPACE}.svc.cluster.local:8080" \
    orka.ai/kagent-source="${kagent_source}" >/dev/null
}

deploy() {
  need openssl
  lock_agents "${agent}"
  # One Orka Agent name may come from kagent Agents of the same name in
  # different namespaces; a deploy must not take the runtimes or relay of the
  # one the Orka Agent wraps.
  local wraps
  wraps=$(kc get agents.core.orka.ai "${agent}" --ignore-not-found -o json | jq -r '.metadata.annotations["orka.ai/kagent-source"] // empty')
  [ -z "${wraps}" ] || [ "${wraps}" = "${kagent_source}" ] ||
    die "Orka Agent ${agent} wraps kagent Agent ${wraps}, not ${kagent_source}; import ${kagent_source} into another Orka namespace"
  if [ "${governed}" = true ]; then
    # The relay Service's name comes from the Agent's relay URL, which an
    # Agent imported separately may share.
    # Both relay objects must be absent, or this Agent's for this kagent
    # Agent: before activation, no Orka Agent records which kagent Agent a
    # deployed relay serves.
    local kind owner
    relay_service=$(relay_service_of "${bundle}")
    for kind in service networkpolicy; do
      owner=$(relay_owner "${kind}" "${relay_service}")
      [ -z "${owner}" ] || [ "${owner}" = "${agent}|${kagent_source}" ] ||
        die "${kind} ${relay_service} is not ${agent}'s relay for kagent Agent ${kagent_source} (it is for ${owner%%|*}, ${owner#*|}); give ${agent} its own relay URL, or adopt it by labeling it orka.ai/kagent-agent=${agent} and annotating it orka.ai/kagent-source='${kagent_source}'"
    done
  fi
  # Only NotFound means the runtime is not deployed yet: rebuilding a deployed
  # runtime would roll new images into it under its registered profile.
  local deployed
  deployed=$(kc get deployment "${runtime}" --ignore-not-found -o name)
  if [ -z "${deployed}" ]; then
    [ -d "${WRAPPER_SRC:-}" ] || die "WRAPPER_SRC must name an orka-runtime-kagent checkout"
    grep -qE '^FROM .* AS config$' "${WRAPPER_SRC}/Dockerfile" ||
      die "${WRAPPER_SRC} predates baking a bundle (its Dockerfile has no config stage); update the checkout"
    [ -n "${IMAGE_REPO:-}" ] || die "IMAGE_REPO is required"
    need docker
  fi
  local digest
  # The directory may hold the governed upstream credential, so it goes on
  # every exit, with the relay rollback.
  work=$(mktemp -d)
  if [ -n "${deployed}" ]; then
    # The images are part of the runtime's immutable profile. A deployed
    # runtime keeps its images; roll new ones under a new name with the
    # import's --runtime-revision.
    deployed_images
    echo "Runtime ${runtime} is already deployed; reusing its images." >&2
  else
    # The wrapper's Dockerfile reads the configuration from its "config"
    # stage, which this replaces with the bundle.
    digest=$(build "${IMAGE_REPO}/kagent-configured:${runtime}" "${work}/configured.json" \
      --build-context config="${bundle}" "${WRAPPER_SRC}")
    configured_image="${IMAGE_REPO}/kagent-configured@${digest}"
    digest=$(build "${IMAGE_REPO}/kagent-runtime:${runtime}" "${work}/runtime.json" \
      -f "${repo_dir}/workers/acp/images/kagent/Dockerfile" \
      --build-arg KAGENT_RUNTIME_IMAGE="${configured_image}" \
      --build-arg KAGENT_ADAPTER_DIGEST="${configured_image##*@}" "${repo_dir}")
    runtime_image="${IMAGE_REPO}/kagent-runtime@${digest}"
  fi

  ensure_auth_secret
  render workload --controller-epoch "$(controller_epoch)" | apply
  kc rollout status "deployment/${runtime}" --timeout="${READY_TIMEOUT}"
  if [ "${governed}" = true ]; then
    # Orka's conformance prompt reaches kagent, which calls the relay, so the
    # relay must reach this runtime before registration.
    local current previous want ports
    current=$(kc get service "${relay_service}" --ignore-not-found -o json)
    want="{\"orka.ai/kagent-runtime\":\"${runtime}\"}"
    render relay >"${work}/relay.json"
    ports=$(jq -ce '.items[] | select(.kind == "Service") | .spec.ports' "${work}/relay.json")
    if [ -z "${current}" ]; then
      # Create, not apply: a deploy that created it meanwhile makes this fail.
      jq '.items |= map(select(.kind == "Service"))' "${work}/relay.json" |
        kubectl --context "${KUBECTL_CONTEXT}" create -f - >/dev/null
    else
      previous=$(jq -c '.spec.selector // {}' <<<"${current}")
      if [ "${previous}" != "${want}" ]; then
        # The handoff is one patch that holds only while the Service still
        # selects what it did and is labeled for this Agent, so it never
        # writes back a selector another deploy replaced, nor takes another
        # Agent's relay. It replaces the selector outright, since a merge with
        # an older one would select nothing.
        # Armed before the patch, since an error may follow a committed one;
        # the rollback restores only while the relay selects this runtime.
        relay_rollback=${previous}
        relay_rollback_ports=$(jq -c '.spec.ports' <<<"${current}")
        kc patch service "${relay_service}" --type=json -p "[{\"op\":\"test\",\"path\":\"/spec/selector\",\"value\":${previous}},{\"op\":\"test\",\"path\":\"/metadata/labels/orka.ai~1kagent-agent\",\"value\":\"${agent}\"},{\"op\":\"test\",\"path\":\"/metadata/annotations/orka.ai~1kagent-source\",\"value\":\"${kagent_source}\"},{\"op\":\"replace\",\"path\":\"/spec/selector\",\"value\":${want}},{\"op\":\"replace\",\"path\":\"/spec/ports\",\"value\":${ports}}]" >/dev/null ||
          die "relay ${relay_service} did not take the handoff: it changed while this deploy ran, or the patch failed; deploy again"
      fi
    fi
    # The relay is this Agent's now, so its policy is too. A rollback
    # restores the policy it replaced, which the render options, such as
    # SUBSTRATE_NAMESPACE, may have changed.
    # Like the Service, an existing policy changes only in one tested patch,
    # which replaces its whole spec, so the rollback's fence is exactly the
    # spec it wrote; an apply would keep fields the render does not set.
    local policy rendered_policy previous_policy
    rendered_policy=$(jq -ceS '.items[] | select(.kind == "NetworkPolicy") | .spec' "${work}/relay.json")
    policy=$(kc get networkpolicy "${relay_service}" --ignore-not-found -o json)
    if [ -z "${policy}" ]; then
      local created
      created=$(jq '.items[] | select(.kind == "NetworkPolicy")' "${work}/relay.json" |
        kubectl --context "${KUBECTL_CONTEXT}" create -f - -o jsonpath='{.metadata.uid}')
      # A rollback deletes a policy this deploy created, by its UID.
      relay_created_policy=${created}
    else
      previous_policy=$(jq -cS '.spec' <<<"${policy}")
      if [ "${previous_policy}" != "${rendered_policy}" ]; then
        # Armed even when the selector stays, since the policy alone can
        # shut out the serving runtime's callers.
        relay_rollback_policy=${previous_policy}
        relay_applied_policy=${rendered_policy}
        kc patch networkpolicy "${relay_service}" --type=json -p "[{\"op\":\"test\",\"path\":\"/spec\",\"value\":${previous_policy}},{\"op\":\"test\",\"path\":\"/metadata/labels/orka.ai~1kagent-agent\",\"value\":\"${agent}\"},{\"op\":\"test\",\"path\":\"/metadata/annotations/orka.ai~1kagent-source\",\"value\":\"${kagent_source}\"},{\"op\":\"replace\",\"path\":\"/spec\",\"value\":${rendered_policy}}]" >/dev/null ||
          die "relay policy ${relay_service} changed while this deploy ran, or the patch failed; deploy again"
      fi
    fi
  fi
  # The API server's service proxy reaches the runtime's public endpoint.
  kubectl --context "${KUBECTL_CONTEXT}" get --raw \
    "/api/v1/namespaces/${NAMESPACE}/services/http:${runtime}:control/proxy/v2/capabilities" >"${work}/capabilities.json"
  local deployment_uid registered owner
  deployment_uid=$(kc get deployment "${runtime}" -o jsonpath='{.metadata.uid}')
  render registration --capabilities "${work}/capabilities.json" --deployment-uid "${deployment_uid}" | apply
  # Orka recovers the Deployment, restarting it after an Orka controller
  # restart, once the Deployment names the AgentRuntime.
  registered=$(kc get agentruntime "${runtime}" -o jsonpath='{.metadata.uid}')
  owner=$(kc get deployment "${runtime}" -o jsonpath='{.metadata.annotations.orka\.ai/agent-runtime-recovery-uid}')
  if [ -z "${owner}" ]; then
    kc annotate deployment "${runtime}" "orka.ai/agent-runtime-recovery-uid=${registered}" >/dev/null
  elif [ "${owner}" != "${registered}" ]; then
    die "deployment ${runtime} is recovered by another AgentRuntime (${owner})"
  fi
  # Activation requires this registration, so it cannot switch Agents through
  # an Orka API in another cluster.
  printf '%s\n' "${registered}" >"${bundle}/registration.uid"
  kc wait --for=condition=Ready "agentruntime/${runtime}" --timeout="${READY_TIMEOUT}" ||
    die "runtime ${runtime} did not become Ready"
  # Ready alone stays set while a deleted runtime drains: require what Orka
  # admits Tasks to, the registration this deploy annotated.
  kc get agentruntime "${runtime}" -o json | jq -e --arg uid "${registered}" \
    '.metadata.uid == $uid and .metadata.deletionTimestamp == null and .status.ready == true and .status.observedGeneration == .metadata.generation' >/dev/null ||
    die "runtime ${runtime} is Ready but being deleted, not yet current, or not the registration this deploy made; deploy under a new --runtime-revision"
  relay_rollback="" relay_rollback_policy="" relay_applied_policy="" relay_created_policy=""
  echo "Runtime ${runtime} is Ready. Activate it with: $0 activate ${bundle} -- <the import arguments>"
}

activate() {
  [ "$#" -gt 0 ] || die "activate needs the import arguments after --"
  # The import activates the bundles in this bundle's directory, the ones the
  # relay checks below cover, so the script names that directory itself.
  local bundle_dir arg pending=false file_next=false args=()
  bundle_dir=$(cd "$(dirname "${bundle}")" && pwd -P)
  # The preview and the import read the same input, so standard input is
  # read once, into the private work directory.
  work=$(mktemp -d)
  for arg in "$@"; do
    if [ "${file_next}" = true ]; then
      file_next=false
      if [ "${arg}" = - ]; then
        cat >"${work}/input"
        arg=${work}/input
      fi
      args+=("${arg}")
      continue
    fi
    if [ "${pending}" = true ]; then
      pending=false
      same_bundle_dir "${arg}" "${bundle_dir}" || die "--bundle-dir ${arg} is not ${bundle_dir}, the directory of ${bundle}"
      continue
    fi
    case "${arg}" in
    --dry-run | --dry-run=*) die "activate runs the import without --dry-run" ;;
    --bundle-dir) pending=true ;;
    -f | --file)
      args+=("${arg}")
      file_next=true
      ;;
    -f=- | --file=-)
      cat >"${work}/input"
      args+=("${arg%%=*}=${work}/input")
      ;;
    --bundle-dir=*)
      same_bundle_dir "${arg#--bundle-dir=}" "${bundle_dir}" ||
        die "${arg} is not ${bundle_dir}, the directory of ${bundle}"
      ;;
    *) args+=("${arg}") ;;
    esac
  done
  [ "${pending}" = false ] || die "--bundle-dir needs a directory"
  # The import switches every Agent in its input, which wrote the bundles
  # beside this one, and refuses unless Orka reports all of their runtimes
  # Ready. A governed Agent's relay may have moved since, to a newer runtime.
  local b names=()
  for b in "$(dirname "${bundle}")"/*/; do
    if [ -f "${b}runtime.json" ]; then
      names+=("$(jq -er '.agent' "${b}runtime.json")")
    fi
  done
  lock_agents "${names[@]}"
  for b in "$(dirname "${bundle}")"/*/; do
    if [ -f "${b}runtime.json" ]; then
      require_relay "${b%/}"
    fi
  done
  # The import must include this bundle's Agent, or it would switch the
  # others and leave this one: a dry run into a scratch directory shows which
  # Agents these arguments import, without touching the bundles.
  "${ORKA}" agent import kagent "${args[@]}" --dry-run --bundle-dir "${work}/preview" -n "${NAMESPACE}" >/dev/null
  [ -f "${work}/preview/${agent}/runtime.json" ] ||
    die "the import arguments do not import Agent ${agent}; check -f"
  "${ORKA}" agent import kagent "${args[@]}" --bundle-dir "${bundle_dir}" -n "${NAMESPACE}"
  [ "$(kc get agents.core.orka.ai "${agent}" -o jsonpath='{.spec.runtime.runtimeRef.name}')" = "${runtime}" ] ||
    die "Agent ${agent} does not use ${runtime}; were the import arguments the ones that wrote this bundle?"
  echo "Agent ${agent} uses ${runtime}. Retire its other runtimes with: $0 retire ${bundle}"
}

retire() {
  lock_agents "${agent}"
  [ "$(kc get agents.core.orka.ai "${agent}" -o jsonpath='{.spec.runtime.runtimeRef.name}')" = "${runtime}" ] ||
    die "Agent ${agent} does not use ${runtime} yet; activate it first"
  # The relay may select a runtime this would delete.
  require_relay "${bundle}"
  # A runtime's objects may outlive its registration when an earlier retire
  # timed out or was interrupted, so they name runtimes to retire too.
  local old olds present registered workloads
  # Each listing must succeed on its own: a partial inventory would leave
  # runtimes behind. Only runtimes of this bundle's kagent Agent count: a
  # same-named kagent Agent of another namespace maps to the same Orka Agent.
  registered=$(kc get agentruntimes.core.orka.ai -l "orka.ai/kagent-agent=${agent}" -o json |
    jq -r --arg source "${kagent_source}" '.items[] | select(.metadata.annotations["orka.ai/kagent-source"] == $source) | .metadata.name')
  workloads=$(kc get deployment,service,persistentvolumeclaim,serviceaccount,secret -l "orka.ai/kagent-agent=${agent}" -o json |
    jq -r --arg source "${kagent_source}" '.items[] | select(.metadata.annotations["orka.ai/kagent-source"] == $source) | .metadata.labels["orka.ai/kagent-runtime"] // empty')
  olds=$(printf '%s\n%s\n' "${registered}" "${workloads}" | sort -u)
  for old in ${olds}; do
    [ "${old}" = "${runtime}" ] && continue
    present=$(registration_of "${old}")
    if [ -n "${present}" ]; then
      echo "Retiring ${old}; Orka drains it first." >&2
      kc delete agentruntime "${old}" --wait=false
      kc wait --for=delete "agentruntime/${old}" --timeout="${RETIRE_TIMEOUT}"
    fi
    # The objects serve the registration until it is gone.
    present=$(registration_of "${old}")
    [ -z "${present}" ] || die "AgentRuntime ${old} is still registered"
    kc delete deployment,service,persistentvolumeclaim,serviceaccount,secret -l "orka.ai/kagent-runtime=${old}"
    kubectl --context "${KUBECTL_CONTEXT}" delete role,rolebinding -A \
      -l "orka.ai/kagent-runtime=${old},orka.ai/kagent-runtime-namespace=${NAMESPACE}"
  done
}

[ "$#" -ge 2 ] || {
  usage >&2
  exit 2
}
command=$1
bundle=${2%/}
shift 2
[ -f "${bundle}/runtime.json" ] && [ -f "${bundle}/kagent.json" ] || die "${bundle} is not a deployment bundle"
[ -n "${KUBECTL_CONTEXT:-}" ] || die "KUBECTL_CONTEXT is required"
NAMESPACE=${NAMESPACE:-orka-system}
CONTROLLER_NAMESPACE=${CONTROLLER_NAMESPACE:-${NAMESPACE}}
ORKA=${ORKA:-orka}
BUILD_PLATFORM=${BUILD_PLATFORM:-linux/amd64}
READY_TIMEOUT=${READY_TIMEOUT:-5m}
RETIRE_TIMEOUT=${RETIRE_TIMEOUT:-30m}
need jq
need kubectl
runtime=$(bundle_field runtime)
agent=$(bundle_field agent)
governed=$(bundle_field governed)
# The kagent Agent this bundle wraps, "<apiVersion> <namespace>/<name>" as the
# Orka Agent's annotation names it; every runtime object names it too.
kagent_source=$(bundle_field source)
relay_service=""
relay_rollback=""
relay_rollback_ports=""
relay_rollback_policy=""
relay_created_policy=""
relay_applied_policy=""
work=""
locks=()
trap cleanup EXIT
trap 'exit 1' INT TERM HUP

case "${command}" in
deploy) deploy ;;
activate)
  [ "${1:-}" = -- ] && shift
  activate "$@"
  ;;
retire) retire ;;
*)
  usage >&2
  exit 2
  ;;
esac
