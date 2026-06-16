#!/usr/bin/env bash
# payoff_card_security must not render scanPhase when it is an unhealthy state
# (e.g. "Error") — that reads as a broken success card. It should render the
# phase only for healthy terminal states (Ready/Succeeded/Completed).

set -Eeuo pipefail

. "$(dirname "${BASH_SOURCE[0]}")/_test_helpers.sh"

# Error phase → scanPhase row omitted, but the card still renders (PR + patches).
summarize_security_run() {
  jq -n '{
    repositoryScan: "demo-scan",
    scanPhase: "Error",
    patches: [{id: "p1"}, {id: "p2"}],
    pullRequest: {url: "https://github.com/sozercan/vekil/pull/9", status: "open"}
  }'
}

out_err="$(payoff_card_security "fnd_abc" 2>&1)"
assert_not_contains "${out_err}" "scanPhase" "Error scanPhase must be hidden on the success card"
assert_contains "${out_err}" "Security finding remediated"
assert_contains "${out_err}" "https://github.com/sozercan/vekil/pull/9"

# Ready phase → scanPhase row shown (it reinforces success).
summarize_security_run() {
  jq -n '{
    repositoryScan: "demo-scan",
    scanPhase: "Ready",
    patches: [{id: "p1"}],
    pullRequest: {url: "https://github.com/sozercan/vekil/pull/9", status: "open"}
  }'
}

out_ready="$(payoff_card_security "fnd_abc" 2>&1)"
assert_contains "${out_ready}" "scanPhase" "Ready scanPhase should be shown"
assert_contains "${out_ready}" "Ready"

printf 'PASS\n'
