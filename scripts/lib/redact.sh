#!/usr/bin/env bash
# Shared secret redaction for E2E diagnostics output.
# Source this file; do not execute it.
#
# redact reads stdin and writes a redacted copy to stdout. The pattern set is
# the union of every redaction previously maintained by the individual E2E
# scripts (auth/cookie/API-key headers, Bearer/Basic values, GitHub
# gh*_/github_pat_ tokens, token/api-key assignments, env-style token and
# webhook-secret assignments, JSON name/value credential pairs, webhook
# signatures, and JWTs). It intentionally over-redacts: when a header or
# token-like assignment is seen, the remainder of the line is dropped.
#
# Callers that hold literal secret values can list the NAMES of the shell
# variables carrying them in the ORKA_REDACT_SECRET_VARS array, for example:
#   ORKA_REDACT_SECRET_VARS=(webhook_secret)
# The value of each named variable is read at call time, so variables that are
# assigned or rotated after sourcing are still redacted.

redact() {
  local text var value
  text="$(cat)"
  for var in ${ORKA_REDACT_SECRET_VARS[@]+"${ORKA_REDACT_SECRET_VARS[@]}"}; do
    value="${!var:-}"
    if [[ -n "${value}" ]]; then
      text="${text//"${value}"/[REDACTED]}"
    fi
  done
  printf '%s\n' "${text}" | sed -E \
    -e 's/((^|[^[:alnum:]_-])(Authorization|Proxy-Authorization|Cookie|Set-Cookie|X-API-Key|API-Key|[A-Za-z0-9_-]*Token)[[:space:]"'\'']*[:=][[:space:]"'\'']*).*/\1[REDACTED]/I' \
    -e 's/((Bearer|Basic)[[:space:]]+).*/\1[REDACTED]/I' \
    -e 's/("name":[[:space:]]*"[A-Za-z0-9_-]*(TOKEN|SECRET|KEY|PASSWORD)[A-Za-z0-9_-]*",[[:space:]]*"value":[[:space:]]*")[^"]*/\1[REDACTED]/Ig' \
    -e 's/(X-Hub-Signature-256:[[:space:]]*sha256=)[A-Fa-f0-9]+/\1[REDACTED_SIGNATURE]/g' \
    -e 's/(ORKA_GITHUB_WEBHOOK_SECRET=)[^[:space:]]+/\1[REDACTED_WEBHOOK_SECRET]/g' \
    -e 's/(ACTIONS_ID_TOKEN_REQUEST_TOKEN=)[^[:space:]]+/\1[REDACTED]/g' \
    -e 's/gh[opusr]_[A-Za-z0-9_]+/[REDACTED_GITHUB_TOKEN]/g' \
    -e 's/github_pat_[A-Za-z0-9_]+/[REDACTED_GITHUB_TOKEN]/g' \
    -e 's/(api[_-]?key[=:][[:space:]]*)[A-Za-z0-9._~+\/=:-]+/\1[REDACTED]/Ig' \
    -e 's/(token[=:][[:space:]]*)[A-Za-z0-9._~+\/=:-]+/\1[REDACTED]/Ig' \
    -e 's/eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}/[REDACTED-JWT]/g'
}
