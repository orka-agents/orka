#!/usr/bin/env bash
# kontxt-caller — used by Demo 50.
#
# Reads a projected ServiceAccount token, exchanges it at the TTS for a
# TxToken, then attaches the TxToken to an Orka API call and prints a
# small audit trail.
#
# SECURITY CONTRACT (AGENTS.md): never log JWTs, never echo the
# subject token contents, never print Authorization or Txn-Token header
# values. We never echo a token directly; we also pipe every output line
# through a JWT-prefix redactor as belt-and-braces.

set -Eeuo pipefail

# Redact JWT-ish strings from any string before printing.
__redact() {
  sed -E 's/eyJ[A-Za-z0-9_=-]{20,}/eyJ\xE2\x80\xA6[REDACTED]/g'
}

emit() {
  # Print a line of audit text with JWT-prefix redaction. Buffer-flushed
  # synchronously so kubectl logs sees it before the script exits.
  printf '%s\n' "$*" | __redact
}

: "${SUBJECT_TOKEN_PATH:=/var/run/orka/token}"
: "${ORKA_CONTEXT_TOKEN_TTS_URL:?ORKA_CONTEXT_TOKEN_TTS_URL is required}"
: "${ORKA_API_URL:?ORKA_API_URL is required}"
: "${TARGET_NAMESPACE:=default}"

if [[ ! -r "${SUBJECT_TOKEN_PATH}" ]]; then
  emit "1/3 subject token: missing (${SUBJECT_TOKEN_PATH})"
  exit 1
fi
emit "1/3 subject token: present (${SUBJECT_TOKEN_PATH})"

# 2/3 — exchange the subject token at the TTS for a TxToken. The TTS
# server in scripts/live-kontxt-e2e (main.go:137) registers /token_endpoint
# and expects id_token subject_token_type + txn_token requested_token_type.
: "${KONTXT_TTS_PARENT_SCOPE:=orka:tasks:list orka:tasks:get}"
tts_response="$(curl -sS -m 15 \
  -X POST "${ORKA_CONTEXT_TOKEN_TTS_URL}/token_endpoint" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'grant_type=urn:ietf:params:oauth:grant-type:token-exchange' \
  --data-urlencode 'subject_token_type=urn:ietf:params:oauth:token-type:id_token' \
  --data-urlencode 'requested_token_type=urn:ietf:params:oauth:token-type:txn_token' \
  --data-urlencode "scope=${KONTXT_TTS_PARENT_SCOPE}" \
  --data-urlencode "subject_token=$(cat "${SUBJECT_TOKEN_PATH}")" || true)"

if ! tx_token="$(printf '%s' "${tts_response}" | jq -r '.access_token // empty')" \
   || [[ -z "${tx_token}" ]]; then
  emit "2/3 TTS exchange: FAILED (no access_token in response)"
  exit 1
fi
scope_value="$(printf '%s' "${tts_response}" | jq -r '.scope // "-"')"
emit "2/3 TTS exchange: ok (TxToken minted, scope=${scope_value})"

# 3/3 — call the Orka API with the TxToken attached. Use Txn-Token header
# (the default kontxt mode); never log the value. /api/v1/tasks is a
# namespace-scoped GET that the controller authorizes against the TxToken.
code="$(curl -sS -m 15 -o /tmp/orka-response.json -w '%{http_code}' \
  -H "Txn-Token: ${tx_token}" \
  "${ORKA_API_URL}/api/v1/tasks?namespace=${TARGET_NAMESPACE}" || printf '000')"

case "${code}" in
  2*)
    emit "3/3 orka api call: ok status=${code} namespace=${TARGET_NAMESPACE}"
    exit 0
    ;;
  401|403)
    emit "3/3 orka api call: denied status=${code} namespace=${TARGET_NAMESPACE}"
    exit 1
    ;;
  *)
    emit "3/3 orka api call: error status=${code} namespace=${TARGET_NAMESPACE}"
    exit 1
    ;;
esac
