#!/usr/bin/env bash
# Sourceable env for running the model-backed Orka demos (10/20/30/40) against
# the unified single-cluster bootstrap (make demo-cluster-up-all). It points the
# demos at the in-cluster vekil proxy + the Provider/secrets that
# install-demo-model.sh created.
#
# Usage:
#   source hack/demos/cluster/demo-env.sh
#   ./hack/demos/20-manual-workflow.sh
#   ./hack/demos/30-cron-workflow.sh
#   ./hack/demos/40-security-scanning.sh
#   ./hack/demos/10-chat-pr.sh        # also needs the `claude` CLI on PATH
#
# The workspace demos (50/60/70) have their own env; see RECORDING.md.
# Override any value before sourcing, or edit here.

# Cluster + namespace. Demos 10-60 read Agents/Tasks/secrets from DEMO_NAMESPACE
# (demo-magic); kontxt + the orka-client API SA live in `default` (handled by
# the installers). Select the unified cluster context.
export DEMO_NAMESPACE="${DEMO_NAMESPACE:-demo-magic}"
# Mint the Orka API token from the SA in DEMO_NAMESPACE (orka-client exists in
# both default and demo-magic on the unified cluster). The Anthropic/OpenAI
# compat endpoints resolve the Provider CRD from the caller token's namespace
# when no ?namespace= is given — and the `claude` CLI in Demo 10 cannot carry a
# query string on ANTHROPIC_BASE_URL. Minting from demo-magic (where the
# vekil-proxy Provider lives) makes the probe, the chat client, and the
# coordinator all resolve the Provider without URL surgery. Defaulting this to
# `default` (as the base lib does) breaks Demo 10 with "no provider
# \"vekil-proxy\" found and no 'default' Provider CRD exists".
export ORKA_TOKEN_NAMESPACE="${ORKA_TOKEN_NAMESPACE:-${DEMO_NAMESPACE}}"
if command -v kubectl >/dev/null 2>&1; then
  kubectl config use-context "kind-${KIND_CLUSTER:-orka-agent-substrate-e2e}" >/dev/null 2>&1 || true
fi

# Model: the type: ai coordinator (demos 10/20) uses the Provider; the CLI
# agents use the runtime secret. Both resolve to the in-cluster vekil proxy.
export DEMO_PROVIDER_REF="${DEMO_PROVIDER_REF:-vekil-proxy}"
export DEMO_AI_MODEL="${DEMO_AI_MODEL:-claude-opus-4.8}"
export DEMO_RUNTIME_TYPE="${DEMO_RUNTIME_TYPE:-codex}"
export DEMO_RUNTIME_MODEL="${DEMO_RUNTIME_MODEL:-gpt-5.5}"
export DEMO_RUNTIME_SECRET_REF="${DEMO_RUNTIME_SECRET_REF:-demo-runtime-key}"

# Git: the PR demos push branches + open PRs against this repo.
export DEMO_GIT_REPO="${DEMO_GIT_REPO:-https://github.com/sozercan/vekil.git}"
export DEMO_GIT_BRANCH="${DEMO_GIT_BRANCH:-main}"
export DEMO_GIT_SECRET_REF="${DEMO_GIT_SECRET_REF:-github-credentials}"

# Security demo (40) uses its own git coordinates; default them to the same repo.
export DEMO_SECURITY_GIT_REPO="${DEMO_SECURITY_GIT_REPO:-${DEMO_GIT_REPO}}"
export DEMO_SECURITY_GIT_BRANCH="${DEMO_SECURITY_GIT_BRANCH:-${DEMO_GIT_BRANCH}}"
export DEMO_SECURITY_GIT_SECRET_REF="${DEMO_SECURITY_GIT_SECRET_REF:-${DEMO_GIT_SECRET_REF}}"

# Demo 10 chat client: an external `claude` CLI on the host drives Orka's
# Anthropic-compatible API. Ensure `claude` is installed.
export DEMO_CHAT_CLIENT="${DEMO_CHAT_CLIENT:-claude-code}"
export DEMO_CLAUDE_BIN="${DEMO_CLAUDE_BIN:-claude}"

# Let the demo helpers auto port-forward the Orka API and mint a managed token.
export DEMO_AUTO_PORT_FORWARD="${DEMO_AUTO_PORT_FORWARD:-1}"

printf 'demo-env loaded: namespace=%s provider=%s model=%s runtime=%s\n' \
  "${DEMO_NAMESPACE}" "${DEMO_PROVIDER_REF}" "${DEMO_AI_MODEL}" "${DEMO_RUNTIME_TYPE}" >&2
