#!/usr/bin/env bash

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
