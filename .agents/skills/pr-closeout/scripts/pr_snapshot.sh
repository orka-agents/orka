#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: pr_snapshot.sh [--repo PATH] [--github-repo OWNER/REPO] [--pr NUMBER_OR_URL] [--summary] [--compact]

Capture a read-only GitHub PR closeout snapshot as JSON by default.

Options:
  --repo PATH              Local git checkout path (default: .)
  --github-repo OWNER/REPO GitHub repository override for --pr (default: gh repo view)
  --pr NUMBER_OR_URL       PR number or URL (default: current branch PR in local checkout)
  --summary                Print a concise human-readable summary instead of JSON
  --compact                Print compact JSON instead of pretty JSON
  -h, --help               Show this help

The snapshot includes local branch/head state, PR metadata, unresolved review
threads, and GitHub check states. It does not modify git or GitHub state.
USAGE
}

repo_path="."
github_repo=""
github_repo_explicit=0
pr_selector=""
summary=0
compact=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo)
      [[ $# -ge 2 ]] || { echo "missing value for --repo" >&2; exit 2; }
      repo_path="$2"
      shift 2
      ;;
    --github-repo)
      [[ $# -ge 2 ]] || { echo "missing value for --github-repo" >&2; exit 2; }
      github_repo="$2"
      github_repo_explicit=1
      shift 2
      ;;
    --pr)
      [[ $# -ge 2 ]] || { echo "missing value for --pr" >&2; exit 2; }
      pr_selector="$2"
      shift 2
      ;;
    --summary)
      summary=1
      shift
      ;;
    --compact)
      compact=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "required command not found: $1" >&2; exit 127; }
}

require_cmd git
require_cmd gh
require_cmd jq

git -C "$repo_path" rev-parse --show-toplevel >/dev/null
cd "$repo_path"

if [[ -n "$pr_selector" && "$pr_selector" =~ ^https?://[^/]+/([^/]+)/([^/]+)/pull/[0-9]+ ]]; then
  pr_url_repo="${BASH_REMATCH[1]}/${BASH_REMATCH[2]}"
  if [[ "$github_repo_explicit" -eq 0 ]]; then
    github_repo="$pr_url_repo"
  fi
fi

if [[ -z "$github_repo" ]]; then
  github_repo="$(gh repo view --json nameWithOwner --jq '.nameWithOwner')"
fi

owner="${github_repo%%/*}"
repo="${github_repo#*/}"
if [[ "$github_repo" != */* || -z "$owner" || -z "$repo" ]]; then
  echo "invalid GitHub repository: $github_repo" >&2
  exit 2
fi

pr_fields='number,url,title,state,isDraft,baseRefName,headRefName,headRefOid,mergeStateStatus,reviewDecision,updatedAt,author,headRepositoryOwner'
if [[ -n "$pr_selector" ]]; then
  pr_json="$(gh pr view "$pr_selector" --repo "$github_repo" --json "$pr_fields")"
else
  if [[ "$github_repo_explicit" -eq 1 ]]; then
    echo "--github-repo requires --pr; current-branch PR detection uses the local checkout" >&2
    exit 2
  fi
  pr_json="$(gh pr view --json "$pr_fields")"
fi

resolved_pr_url="$(jq -r '.url // ""' <<<"$pr_json")"
if [[ "$resolved_pr_url" =~ ^https?://[^/]+/([^/]+)/([^/]+)/pull/[0-9]+ ]]; then
  github_repo="${BASH_REMATCH[1]}/${BASH_REMATCH[2]}"
  owner="${github_repo%%/*}"
  repo="${github_repo#*/}"
fi
pr_number="$(jq -r '.number' <<<"$pr_json")"
pr_state="$(jq -r '.state // ""' <<<"$pr_json")"
head_ref="$(jq -r '.headRefName // ""' <<<"$pr_json")"
head_oid="$(jq -r '.headRefOid // ""' <<<"$pr_json")"
review_decision="$(jq -r '.reviewDecision // ""' <<<"$pr_json")"
merge_state="$(jq -r '.mergeStateStatus // ""' <<<"$pr_json")"
pr_updated_epoch="$(jq -r '(.updatedAt // empty) | fromdateiso8601' <<<"$pr_json" 2>/dev/null || echo 0)"
now_epoch="$(date +%s)"
if [[ "$pr_updated_epoch" =~ ^[0-9]+$ && "$pr_updated_epoch" -gt 0 ]]; then
  pr_updated_age_seconds=$((now_epoch - pr_updated_epoch))
else
  pr_updated_age_seconds=-1
fi

current_branch="$(git branch --show-current 2>/dev/null || true)"
local_head="$(git rev-parse HEAD 2>/dev/null || true)"
upstream="$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null || true)"
status_short="$(git status --short --branch)"
if [[ -n "$(git status --porcelain)" ]]; then
  dirty=true
else
  dirty=false
fi
if [[ "$current_branch" == "$head_ref" ]]; then
  branch_matches=true
else
  branch_matches=false
fi
if [[ -n "$head_oid" && "$local_head" == "$head_oid" ]]; then
  head_matches=true
else
  head_matches=false
fi

threads_json='[]'
threads_cursor=''
threads_pages=0
while :; do
  threads_pages=$((threads_pages + 1))
  if [[ "$threads_pages" -gt 100 ]]; then
    echo "review thread pagination exceeded 100 pages; aborting snapshot" >&2
    exit 1
  fi

  if [[ -n "$threads_cursor" ]]; then
    page_json="$(gh api graphql \
      -f owner="$owner" \
      -f repo="$repo" \
      -F number="$pr_number" \
      -f after="$threads_cursor" \
      -f query='query($owner:String!, $repo:String!, $number:Int!, $after:String) { repository(owner:$owner, name:$repo) { pullRequest(number:$number) { reviewThreads(first:100, after:$after) { nodes { id isResolved isOutdated comments(last:1) { nodes { author { login } path line url createdAt } } } pageInfo { hasNextPage endCursor } } } } }')"
  else
    page_json="$(gh api graphql \
      -f owner="$owner" \
      -f repo="$repo" \
      -F number="$pr_number" \
      -f query='query($owner:String!, $repo:String!, $number:Int!) { repository(owner:$owner, name:$repo) { pullRequest(number:$number) { reviewThreads(first:100) { nodes { id isResolved isOutdated comments(last:1) { nodes { author { login } path line url createdAt } } } pageInfo { hasNextPage endCursor } } } } }')"
  fi

  page_nodes="$(jq '.data.repository.pullRequest.reviewThreads.nodes' <<<"$page_json")"
  threads_json="$(jq -s '.[0] + .[1]' <(printf '%s' "$threads_json") <(printf '%s' "$page_nodes"))"
  has_next_page="$(jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.hasNextPage' <<<"$page_json")"
  threads_cursor="$(jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.endCursor // ""' <<<"$page_json")"
  [[ "$has_next_page" == "true" ]] || break
  if [[ -z "$threads_cursor" ]]; then
    echo "review thread pagination reported more pages but no cursor; aborting snapshot" >&2
    exit 1
  fi
done

unresolved_threads_json="$(jq '[.[] | select(.isResolved == false) | {id, isOutdated, path:((.comments.nodes | last).path // null), line:((.comments.nodes | last).line // null), url:((.comments.nodes | last).url // null), author:((.comments.nodes | last).author.login // null)}]' <<<"$threads_json")"

read_checks() {
  local mode="$1"
  local checks_tmp checks_err checks_output checks_error_text checks_exit_code checks_items checks_lookup_failed no_checks no_checks_pending
  local args=(pr checks "$pr_number" --repo "$github_repo")
  if [[ "$mode" == "required" ]]; then
    args+=(--required)
  fi
  args+=(--json name,state,bucket,link,workflow,startedAt,completedAt)

  checks_tmp="$(mktemp)"
  checks_err="$(mktemp)"
  set +e
  gh "${args[@]}" >"$checks_tmp" 2>"$checks_err"
  checks_exit_code=$?
  set -e
  checks_output="$(cat "$checks_tmp")"
  checks_error_text="$(cat "$checks_err")"
  rm -f "$checks_tmp" "$checks_err"

  checks_lookup_failed=false
  no_checks=false
  no_checks_pending=false
  if [[ -n "$checks_output" ]] && jq -e 'type == "array"' >/dev/null 2>&1 <<<"$checks_output"; then
    checks_items="$checks_output"
  elif [[ "$checks_output" == *"no required checks reported"* || "$checks_error_text" == *"no required checks reported"* || "$checks_output" == *"no checks reported"* || "$checks_error_text" == *"no checks reported"* ]]; then
    checks_items='[]'
    no_checks=true
    if [[ "$mode" == "all" && "$pr_updated_age_seconds" -ge 0 && "$pr_updated_age_seconds" -lt 900 ]]; then
      no_checks_pending=true
    fi
  else
    checks_items='[]'
    checks_lookup_failed=true
    if [[ -n "$checks_output" ]]; then
      checks_error_text="${checks_error_text}${checks_error_text:+$'\n'}$checks_output"
    fi
  fi

  if [[ "$checks_exit_code" -ne 0 && "$no_checks" != "true" && "$(jq 'length' <<<"$checks_items")" -eq 0 ]]; then
    checks_lookup_failed=true
  fi

  jq -n \
    --argjson items "$checks_items" \
    --argjson commandExitCode "$checks_exit_code" \
    --arg commandError "$checks_error_text" \
    --argjson lookupFailed "$checks_lookup_failed" \
    --argjson noChecksReported "$no_checks" \
    --argjson noChecksPending "$no_checks_pending" \
    '{items:$items, commandExitCode:$commandExitCode, commandError:$commandError, lookupFailed:$lookupFailed, noChecksReported:$noChecksReported, noChecksPending:$noChecksPending, summary:(reduce $items[] as $c ({total:0}; .total += 1 | .[$c.bucket] = ((.[$c.bucket] // 0) + 1)))}'
}

checks_result="$(read_checks all)"
required_checks_result="$(read_checks required)"
checks_json="$(jq '.items' <<<"$checks_result")"
required_checks_json="$(jq '.items' <<<"$required_checks_result")"
checks_summary_json="$(jq '.summary' <<<"$checks_result")"
required_checks_summary_json="$(jq '.summary' <<<"$required_checks_result")"
checks_exit="$(jq '.commandExitCode' <<<"$checks_result")"
required_checks_exit="$(jq '.commandExitCode' <<<"$required_checks_result")"
checks_error="$(jq -r '.commandError' <<<"$checks_result")"
required_checks_error="$(jq -r '.commandError' <<<"$required_checks_result")"
checks_lookup_failed="$(jq -r '.lookupFailed' <<<"$checks_result")"
required_checks_lookup_failed="$(jq -r '.lookupFailed' <<<"$required_checks_result")"
all_checks_no_checks_reported="$(jq -r '.noChecksReported' <<<"$checks_result")"
all_checks_no_checks_pending="$(jq -r '.noChecksPending' <<<"$checks_result")"
required_checks_no_checks_reported="$(jq -r '.noChecksReported' <<<"$required_checks_result")"
required_checks_no_checks_pending="$(jq -r '.noChecksPending' <<<"$required_checks_result")"

local_json="$(jq -n \
  --arg currentBranch "$current_branch" \
  --arg headOid "$local_head" \
  --arg upstream "$upstream" \
  --arg statusShort "$status_short" \
  --argjson prUpdatedAgeSeconds "$pr_updated_age_seconds" \
  --argjson dirty "$dirty" \
  --argjson branchMatches "$branch_matches" \
  --argjson headMatches "$head_matches" \
  '{currentBranch:$currentBranch, headOid:$headOid, upstream:$upstream, dirty:$dirty, branchMatchesPrHead:$branchMatches, headMatchesPrHeadOid:$headMatches, prUpdatedAgeSeconds:$prUpdatedAgeSeconds, statusShort:$statusShort}')"

unresolved_count="$(jq 'length' <<<"$unresolved_threads_json")"
failing_count="$(jq '[.[] | select(.bucket == "fail" or .bucket == "cancel")] | length' <<<"$required_checks_json")"
pending_count="$(jq '[.[] | select(.bucket == "pending")] | length' <<<"$required_checks_json")"

if [[ "$review_decision" == "CHANGES_REQUESTED" || "$review_decision" == "REVIEW_REQUIRED" ]]; then
  review_blocking=true
else
  review_blocking=false
fi
if [[ "$pr_state" == "OPEN" ]]; then
  pr_state_blocking=false
else
  pr_state_blocking=true
fi
case "$merge_state" in
  DIRTY|UNKNOWN|BLOCKED|BEHIND|DRAFT)
    merge_blocking=true
    ;;
  *)
    merge_blocking=false
    ;;
esac
if [[ "$failing_count" -gt 0 ]]; then
  failing_checks=true
else
  failing_checks=false
fi
if [[ "$pending_count" -gt 0 || "$all_checks_no_checks_pending" == "true" || "$required_checks_no_checks_pending" == "true" ]]; then
  pending_checks=true
else
  pending_checks=false
fi
if [[ "$unresolved_count" -gt 0 ]]; then
  unresolved_threads=true
else
  unresolved_threads=false
fi

snapshot="$(jq -n \
  --arg generatedAt "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg repository "$github_repo" \
  --argjson pr "$pr_json" \
  --argjson local "$local_json" \
  --argjson unresolvedThreads "$unresolved_threads_json" \
  --argjson checks "$checks_json" \
  --argjson checksSummary "$required_checks_summary_json" \
  --argjson allChecksSummary "$checks_summary_json" \
  --arg checksCommandError "$checks_error" \
  --arg requiredChecksCommandError "$required_checks_error" \
  --argjson checksCommandExitCode "$checks_exit" \
  --argjson requiredChecksCommandExitCode "$required_checks_exit" \
  --argjson checksLookupFailed "$checks_lookup_failed" \
  --argjson requiredChecksLookupFailed "$required_checks_lookup_failed" \
  --argjson allChecksNoChecksReported "$all_checks_no_checks_reported" \
  --argjson allChecksNoChecksPending "$all_checks_no_checks_pending" \
  --argjson requiredChecksNoChecksReported "$required_checks_no_checks_reported" \
  --argjson requiredChecksNoChecksPending "$required_checks_no_checks_pending" \
  --argjson requiredChecks "$required_checks_json" \
  --argjson prStateBlocking "$pr_state_blocking" \
  --argjson reviewDecisionBlocking "$review_blocking" \
  --argjson mergeStateBlocking "$merge_blocking" \
  --argjson failingChecks "$failing_checks" \
  --argjson pendingChecks "$pending_checks" \
  --argjson unresolvedReviewThreads "$unresolved_threads" \
  '{generatedAt:$generatedAt, repository:$repository, local:$local, pr:$pr, reviewThreads:{unresolvedCount:($unresolvedThreads|length), unresolved:$unresolvedThreads}, checks:{summary:$checksSummary, requiredSummary:$checksSummary, allSummary:$allChecksSummary, commandExitCode:$checksCommandExitCode, requiredCommandExitCode:$requiredChecksCommandExitCode, commandError:$checksCommandError, requiredCommandError:$requiredChecksCommandError, lookupFailed:($checksLookupFailed or $requiredChecksLookupFailed), requiredLookupFailed:$requiredChecksLookupFailed, allNoChecksReported:$allChecksNoChecksReported, allNoChecksPending:$allChecksNoChecksPending, requiredNoChecksReported:$requiredChecksNoChecksReported, requiredNoChecksPending:$requiredChecksNoChecksPending, items:$checks, requiredItems:$requiredChecks}, blockers:{wrongHeadBranch:($local.branchMatchesPrHead|not), wrongHeadSha:($local.headMatchesPrHeadOid|not), dirtyWorktree:$local.dirty, prStateBlocking:$prStateBlocking, mergeStateBlocking:$mergeStateBlocking, reviewDecisionBlocking:$reviewDecisionBlocking, unresolvedReviewThreads:$unresolvedReviewThreads, checksLookupFailed:($checksLookupFailed or $requiredChecksLookupFailed), failingChecks:$failingChecks, pendingChecks:$pendingChecks}}')"

if [[ "$summary" -eq 1 ]]; then
  jq -r '
    "PR #\(.pr.number): \(.pr.title)",
    "URL: \(.pr.url)",
    "Head: \(.pr.headRefName) @ \(.pr.headRefOid[0:8])",
    "Local: \(.local.currentBranch) @ \(.local.headOid[0:8]) (branch match: \(.local.branchMatchesPrHead), head match: \(.local.headMatchesPrHeadOid), dirty: \(.local.dirty))",
    "State: \(.pr.state)",
    "Merge state: \(.pr.mergeStateStatus)",
    "Review decision: \(.pr.reviewDecision // "")",
    "Unresolved review threads: \(.reviewThreads.unresolvedCount)",
    "Required checks: total=\(.checks.requiredSummary.total // 0) pass=\(.checks.requiredSummary.pass // 0) pending=\(.checks.requiredSummary.pending // 0) fail=\(.checks.requiredSummary.fail // 0) cancel=\(.checks.requiredSummary.cancel // 0) noChecksPending=\(.checks.requiredNoChecksPending) lookupFailed=\(.checks.requiredLookupFailed) exit=\(.checks.requiredCommandExitCode)",
    "All checks: total=\(.checks.allSummary.total // 0) pass=\(.checks.allSummary.pass // 0) pending=\(.checks.allSummary.pending // 0) fail=\(.checks.allSummary.fail // 0) cancel=\(.checks.allSummary.cancel // 0) noChecksPending=\(.checks.allNoChecksPending) lookupFailed=\(.checks.lookupFailed) exit=\(.checks.commandExitCode)",
    "Blockers: \(.blockers)"
  ' <<<"$snapshot"
elif [[ "$compact" -eq 1 ]]; then
  jq -c . <<<"$snapshot"
else
  jq . <<<"$snapshot"
fi
