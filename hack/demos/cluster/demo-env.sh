#!/usr/bin/env bash

export DEMO_NAMESPACE="${DEMO_NAMESPACE:-orka-system}"
export ORKA_TOKEN_NAMESPACE="${ORKA_TOKEN_NAMESPACE:-${DEMO_NAMESPACE}}"
if command -v kubectl >/dev/null 2>&1; then
  kubectl config use-context "kind-${KIND_CLUSTER:-orka-agent-substrate-e2e}" >/dev/null 2>&1 || true
fi

export DEMO_PROVIDER_REF="${DEMO_PROVIDER_REF:-vekil-proxy}"
export DEMO_AI_MODEL="${DEMO_AI_MODEL:-claude-opus-4.8}"
export DEMO_RUNTIME_TYPE="${DEMO_RUNTIME_TYPE:-codex}"
export DEMO_RUNTIME_MODEL="${DEMO_RUNTIME_MODEL:-gpt-5.5}"
export DEMO_RUNTIME_SECRET_REF="${DEMO_RUNTIME_SECRET_REF:-demo-runtime-key}"

export DEMO_GIT_REPO="${DEMO_GIT_REPO:-https://github.com/sozercan/vekil.git}"
export DEMO_GIT_BRANCH="${DEMO_GIT_BRANCH:-main}"
export DEMO_GIT_SECRET_REF="${DEMO_GIT_SECRET_REF:-github-credentials}"

export DEMO_SECURITY_GIT_BRANCH="${DEMO_SECURITY_GIT_BRANCH:-main}"
export DEMO_SECURITY_GIT_SECRET_REF="${DEMO_SECURITY_GIT_SECRET_REF:-${DEMO_GIT_SECRET_REF}}"

export DEMO_CHAT_CLIENT="${DEMO_CHAT_CLIENT:-claude-code}"
export DEMO_CLAUDE_BIN="${DEMO_CLAUDE_BIN:-claude}"

export DEMO_AUTO_PORT_FORWARD="${DEMO_AUTO_PORT_FORWARD:-1}"

printf 'demo-env loaded: namespace=%s provider=%s model=%s runtime=%s\n' \
  "${DEMO_NAMESPACE}" "${DEMO_PROVIDER_REF}" "${DEMO_AI_MODEL}" "${DEMO_RUNTIME_TYPE}" >&2
