# shellcheck shell=bash
# Shared preparation and evidence capture for demos 08-11. No recording here.
# shellcheck source=demo/lib/demo.sh
source "$(dirname "${BASH_SOURCE[0]}")/demo.sh"

if [[ -x $repo_root/bin/orka ]]; then
  export PATH="$repo_root/bin:$PATH"
fi

scenario_require_commands() {
  local name
  for name in "$@"; do
    if ! type -P "$name" >/dev/null; then
      printf 'Missing command: %s\n' "$name" >&2
      return 1
    fi
  done
}

scenario_require_cluster() {
  scenario_require_commands kubectl jq python3
  if [[ -z ${KUBECONFIG:-} || $KUBECONFIG == *:* || ! -f $KUBECONFIG ]]; then
    printf 'Set KUBECONFIG to one scoped demo cluster file, or configure the selected demo environment file.\n' >&2
    return 1
  fi
  local context
  context=$(kubectl config current-context)
  if [[ -n ${DEMO_KUBE_CONTEXT:-} ]]; then
    if [[ $context != "$DEMO_KUBE_CONTEXT" ]]; then
      printf 'DEMO_KUBE_CONTEXT is %s, but the selected kubeconfig context is %s.\n' "$DEMO_KUBE_CONTEXT" "$context" >&2
      return 1
    fi
  elif [[ $context != kind-* ]]; then
    printf 'The default requires a local kind demo cluster; explicitly set DEMO_KUBE_CONTEXT to use %s.\n' "$context" >&2
    return 1
  fi
  kubectl get namespace "$ORKA_NAMESPACE" -o name >/dev/null
}

scenario_init() {
  demo_name=$1
  scenario_require_cluster
  scenario_require_commands orka curl
  umask 077
  run_id=${DEMO_RUN_ID:-$(date -u +%m%d%H%M%S)-$(python3 -c 'import secrets; print(secrets.token_hex(3))')}
  if [[ ! $run_id =~ ^[a-z0-9][a-z0-9-]*$ || ${#run_id} -gt 32 ]]; then
    printf 'DEMO_RUN_ID must be 1-32 lowercase letters, digits, or hyphens, starting with a letter or digit.\n' >&2
    return 1
  fi
  run_dir=$demo_root/setup/state/$demo_name/runs/$run_id
  raw_dir=$run_dir/raw
  mkdir -p "$(dirname "$run_dir")"
  # Reusing a run directory could mix old evidence with a new attempt.
  mkdir "$run_dir"
  mkdir "$raw_dir"
  export demo_name run_id run_dir raw_dir
  cd "$run_dir" || return
}

scenario_connect() {
  ensure_port_forward
  orka config set-server "$ORKA_API" >/dev/null
  orka config set-namespace "$ORKA_NAMESPACE" >/dev/null
  orka_token | orka config set-token --file - >/dev/null
}

# Read every event page, keeping the original responses next to the combined
# view. A short first page alone cannot prove that no tool was called.
scenario_collect_events() {
  local task=$1 output=$2 after=0 next latest page=0 file pages
  pages=${output%.json}-pages
  mkdir "$pages"
  while true; do
    page=$((page + 1))
    printf -v file '%s/%05d.json' "$pages" "$page"
    orka task events "$task" --after "$after" --limit 100 -o json >"$file"
    jq -e --arg task "$task" --argjson after "$after" '
      .streamID == $task and .streamType == "task" and .afterSeq == $after
      and (.events | type == "array") and (.latestSeq | type == "number")
      and all(.events[]; .seq > $after and .streamID == $task)
    ' "$file" >/dev/null
    next=$(jq '[.events[].seq] | max // 0' "$file")
    latest=$(jq '.latestSeq' "$file")
    if (( next >= latest && latest > 0 )); then
      break
    fi
    if (( next <= after )); then
      printf 'Incomplete event history for %s. Refusing to infer missing evidence.\n' "$task" >&2
      return 1
    fi
    after=$next
  done
  jq -s '
    .[0] as $first | .[-1] as $last |
    {namespace: $first.namespace, streamID: $first.streamID, streamType: "task",
     afterSeq: 0, latestSeq: $last.latestSeq, events: [.[].events[]]}
  ' "$pages"/*.json >"$output"
  jq -e '
    (.events | length) > 0
    and ([.events[].seq] == [range(1; .latestSeq + 1)])
    and any(.events[]; .type == "TaskSucceeded" or .type == "TaskFailed" or .type == "TaskCancelled")
  ' "$output" >/dev/null
}
