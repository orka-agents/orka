#!/usr/bin/env bash

set -Eeuo pipefail

emit_block() {
  local indent="$1"
  local text="$2"
  while IFS= read -r line; do
    printf '%s%s\n' "${indent}" "${line}"
  done <<< "${text}"
}

pr_repo_details_block() {
  local push_branch="$1"
  cat <<EOF
Repository details for delegated agent tasks:
- gitRepo: ${DEMO_GIT_REPO}
- branch: ${DEMO_GIT_BRANCH}
- gitSecretRef: ${DEMO_GIT_SECRET_REF}
- pushBranch: ${push_branch}
EOF
  if [[ -n "${DEMO_GIT_FORK_REPO:-}" ]]; then
    printf '%s\n' "- forkRepo: ${DEMO_GIT_FORK_REPO}"
  fi
  if [[ -n "${DEMO_PR_BASE_BRANCH:-}" ]]; then
    printf '%s\n' "- prBaseBranch: ${DEMO_PR_BASE_BRANCH}"
  fi
}

render_pr_agents_manifest() {
  cat <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: ${DEMO_CODER_AGENT_NAME}
  namespace: ${DEMO_NAMESPACE}
  labels:
    demo.orka.ai/name: ${DEMO_LABEL_VALUE}
    demo.orka.ai/scenario: pr-workflow
spec:
  runtime:
    type: ${DEMO_RUNTIME_TYPE}
    defaultMaxTurns: 100
    defaultAllowBash: true
    defaultAllowedTools:
      - Read
      - Write
      - Edit
      - Bash
      - Glob
      - Grep
  model:
    name: ${DEMO_RUNTIME_MODEL}
  systemPrompt:
    inline: |
      You are the implementation agent for a live Orka demo.
      Work only in the repository workspace provided by the task.
      Keep the diff focused and run the smallest relevant validation you can.
      Do not manually commit, push, or open a pull request unless the task explicitly asks you to repair git state.
      Leave the final file changes in the workspace; Orka will capture the diff and push it to ORKA_PUSH_BRANCH automatically.
      If git reports dubious ownership, mark /workspace as a safe.directory before continuing.
      If you receive FEEDBACK FROM REVIEW, address every item before handing work back.
  resources:
    requests:
      cpu: ${DEMO_AGENT_CPU_REQUEST}
      memory: ${DEMO_AGENT_MEMORY_REQUEST}
    limits:
      cpu: ${DEMO_AGENT_CPU_LIMIT}
      memory: ${DEMO_AGENT_MEMORY_LIMIT}
  secretRef:
    name: ${DEMO_RUNTIME_SECRET_REF}
---
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: ${DEMO_SECURITY_REVIEWER_NAME}
  namespace: ${DEMO_NAMESPACE}
  labels:
    demo.orka.ai/name: ${DEMO_LABEL_VALUE}
    demo.orka.ai/scenario: pr-workflow
spec:
  runtime:
    type: ${DEMO_RUNTIME_TYPE}
    defaultMaxTurns: 60
    defaultAllowBash: true
    defaultAllowedTools:
      - Read
      - Bash
      - Glob
      - Grep
  model:
    name: ${DEMO_RUNTIME_MODEL}
  systemPrompt:
    inline: |
      You are the security reviewer for a live Orka demo.
      Review only the proposed changes and reply with exactly one verdict heading:
      APPROVED
      or
      CHANGES_NEEDED
      If the working tree is clean, fetch origin and inspect the diff between the base branch named in the task prompt and the current checked-out branch or HEAD before you decide.
      The task workspace already contains the checked-out repository and branch. Do not run git clone or create nested repositories inside the workspace.
      Run git commands to ground your review. First fetch origin, then list changed files with git diff --name-only origin/<base-branch>...HEAD, then inspect only that diff with git diff origin/<base-branch>...HEAD.
      Scope decisions must come only from that diff. Ignore repository files that already exist on the branch or in history if they are not part of the current diff.
      Your final answer must not mention any file that is absent from git diff --name-only origin/<base-branch>...HEAD.
      If the diff contains only README.md and/or CONTRIBUTING.md changes, treat the change as documentation-only.
      Follow that heading with concise, actionable feedback.
      Approve when the requested change is already correct, clearly scoped, and safe.
      Do not request preference-only wording changes for documentation unless they materially affect security, policy accuracy, or the user's explicit requirements.
      Never commit, push, or open a pull request.
  resources:
    requests:
      cpu: ${DEMO_AGENT_CPU_REQUEST}
      memory: ${DEMO_AGENT_MEMORY_REQUEST}
    limits:
      cpu: ${DEMO_AGENT_CPU_LIMIT}
      memory: ${DEMO_AGENT_MEMORY_LIMIT}
  secretRef:
    name: ${DEMO_RUNTIME_SECRET_REF}
---
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: ${DEMO_QUALITY_REVIEWER_NAME}
  namespace: ${DEMO_NAMESPACE}
  labels:
    demo.orka.ai/name: ${DEMO_LABEL_VALUE}
    demo.orka.ai/scenario: pr-workflow
spec:
  runtime:
    type: ${DEMO_RUNTIME_TYPE}
    defaultMaxTurns: 60
    defaultAllowBash: true
    defaultAllowedTools:
      - Read
      - Bash
      - Glob
      - Grep
  model:
    name: ${DEMO_RUNTIME_MODEL}
  systemPrompt:
    inline: |
      You are the quality reviewer for a live Orka demo.
      Review only the proposed changes and reply with exactly one verdict heading:
      APPROVED
      or
      CHANGES_NEEDED
      If the working tree is clean, fetch origin and inspect the diff between the base branch named in the task prompt and the current checked-out branch or HEAD before you decide.
      The task workspace already contains the checked-out repository and branch. Do not run git clone or create nested repositories inside the workspace.
      Run git commands to ground your review. First fetch origin, then list changed files with git diff --name-only origin/<base-branch>...HEAD, then inspect only that diff with git diff origin/<base-branch>...HEAD.
      Scope decisions must come only from that diff. Ignore repository files that already exist on the branch or in history if they are not part of the current diff.
      Your final answer must not mention any file that is absent from git diff --name-only origin/<base-branch>...HEAD.
      If the diff contains only README.md and/or CONTRIBUTING.md changes, treat the change as documentation-only.
      Follow that heading with concise, actionable feedback.
      Focus on correctness, clarity, and testability.
      Approve when the requested change is already correct and reviewable.
      Do not request preference-only wording or placement changes unless they materially improve correctness, clarity of the explicit request, or maintainability.
      Never commit, push, or open a pull request.
  resources:
    requests:
      cpu: ${DEMO_AGENT_CPU_REQUEST}
      memory: ${DEMO_AGENT_MEMORY_REQUEST}
    limits:
      cpu: ${DEMO_AGENT_CPU_LIMIT}
      memory: ${DEMO_AGENT_MEMORY_LIMIT}
  secretRef:
    name: ${DEMO_RUNTIME_SECRET_REF}
---
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: ${DEMO_PR_COORDINATOR_NAME}
  namespace: ${DEMO_NAMESPACE}
  labels:
    demo.orka.ai/name: ${DEMO_LABEL_VALUE}
    demo.orka.ai/scenario: pr-workflow
spec:
  providerRef:
    name: ${DEMO_PROVIDER_REF}
  model:
    name: ${DEMO_AI_MODEL}
  systemPrompt:
    inline: |
      You are the coordinator for a live Orka demo.
      Follow this workflow exactly:
      1. Read the repository details from the task prompt.
      2. Delegate implementation to ${DEMO_CODER_AGENT_NAME} with that workspace and a pushBranch. Tell the coder to edit files only and let Orka push the final diff automatically.
      3. Wait for the coder to finish.
      4. Delegate parallel review to ${DEMO_SECURITY_REVIEWER_NAME} and ${DEMO_QUALITY_REVIEWER_NAME} without prior_task. For review tasks, set workspace.gitRepo, workspace.gitSecretRef, and workspace.branch = pushBranch, and set maxTurns to at least 60. NEVER include workspace.pushBranch on review tasks. In each review prompt, explicitly tell reviewers the workspace is already checked out for them, so they must not clone again. Tell them to fetch origin, run git diff --name-only origin/<base-branch>...HEAD, inspect only that diff, and ignore unchanged repository files outside that diff. If the coder result includes a files list, include an "Expected changed files" section in the review prompt and tell reviewers their final answer must not mention files outside that list or outside git diff --name-only origin/<base-branch>...HEAD. Reviewers should inspect the current branch diff against the base branch from the prompt; they do not need prior_task.
      5. Wait for both reviewers.
      6. If either reviewer returns CHANGES_NEEDED and you have not already iterated twice, summarize all feedback and delegate a follow-up fix to ${DEMO_CODER_AGENT_NAME}. For follow-up coder tasks, set workspace.gitRepo, workspace.gitSecretRef, workspace.branch = pushBranch, and workspace.pushBranch = pushBranch. Do NOT use prior_task for follow-up coder tasks after the first push, because that creates a fresh local history that cannot be pushed back to the same branch cleanly. Make it explicit that the workspace is already checked out for the coder, so the coder must edit files in place and must not clone the repo again. Tell the coder to preserve the explicit original request while addressing review feedback; if a reviewer flags a broken newly-added documentation link, prefer creating the minimal supporting file instead of deleting requested content. Tell the coder to edit files only and let Orka push the incremental update automatically. Then repeat the review step.
      7. When both reviewers approve, create a pull request with create_pull_request using the coder task name, the pushed branch, and the base branch from the prompt.
      8. Report the pull request URL and a brief summary of the result.
  coordination:
    enabled: true
    maxDepth: 3
    maxConcurrentChildren: 3
    allowedAgents:
      - name: ${DEMO_CODER_AGENT_NAME}
      - name: ${DEMO_SECURITY_REVIEWER_NAME}
      - name: ${DEMO_QUALITY_REVIEWER_NAME}
EOF
}

render_chat_request_file() {
  emit_block "" "Create an AI task in namespace ${DEMO_NAMESPACE} using the existing ${DEMO_PR_COORDINATOR_NAME} agent in that namespace.
Do not create, update, or delete agents or providers for this demo.
Wait for the workflow to finish and report the pull request URL.
"
  printf '\n'
  pr_repo_details_block "${DEMO_CHAT_PUSH_BRANCH}"
  printf '\n\n'
  emit_block "" "Change request:
${DEMO_CHAT_REQUEST}"
}

render_manual_task_manifest() {
  cat <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: ${DEMO_MANUAL_TASK_NAME}
  namespace: ${DEMO_NAMESPACE}
  labels:
    demo.orka.ai/name: ${DEMO_LABEL_VALUE}
    demo.orka.ai/scenario: manual-workflow
spec:
  type: ai
  agentRef:
    name: ${DEMO_PR_COORDINATOR_NAME}
  timeout: 45m
  priority: 800
  prompt: |
EOF
  emit_block "    " "${DEMO_MANUAL_REQUEST}"
  printf '\n'
  emit_block "    " "$(pr_repo_details_block "${DEMO_MANUAL_PUSH_BRANCH}")"
}

render_cron_agent_manifest() {
  cat <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: ${DEMO_CRON_AGENT_NAME}
  namespace: ${DEMO_NAMESPACE}
  labels:
    demo.orka.ai/name: ${DEMO_LABEL_VALUE}
    demo.orka.ai/scenario: cron-workflow
spec:
  runtime:
    type: ${DEMO_RUNTIME_TYPE}
    defaultMaxTurns: 40
    defaultAllowBash: true
    defaultAllowedTools:
      - Read
      - Bash
      - Glob
      - Grep
      - Write
  model:
    name: ${DEMO_RUNTIME_MODEL}
  systemPrompt:
    inline: |
      You are the scheduled repository reporter for a live Orka demo.
      Read the repository, produce a short report in the task result, and stop.
      Do not commit, push, or open a pull request.
  resources:
    requests:
      cpu: ${DEMO_AGENT_CPU_REQUEST}
      memory: ${DEMO_AGENT_MEMORY_REQUEST}
    limits:
      cpu: ${DEMO_AGENT_CPU_LIMIT}
      memory: ${DEMO_AGENT_MEMORY_LIMIT}
  secretRef:
    name: ${DEMO_RUNTIME_SECRET_REF}
EOF
}

render_cron_task_manifest() {
  cat <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: ${DEMO_CRON_TASK_NAME}
  namespace: ${DEMO_NAMESPACE}
  labels:
    demo.orka.ai/name: ${DEMO_LABEL_VALUE}
    demo.orka.ai/scenario: cron-workflow
spec:
  type: agent
  agentRef:
    name: ${DEMO_CRON_AGENT_NAME}
  schedule: "${DEMO_CRON_SCHEDULE}"
  concurrencyPolicy: Forbid
  successfulRunsHistoryLimit: 2
  failedRunsHistoryLimit: 1
  timeout: 20m
  prompt: |
EOF
  emit_block "    " "${DEMO_CRON_REQUEST}"
  printf '\n'
  cat <<EOF
  agentRuntime:
    workspace:
      gitRepo: ${DEMO_GIT_REPO}
      branch: ${DEMO_GIT_BRANCH}
EOF
  if [[ -n "${DEMO_GIT_SECRET_REF:-}" ]]; then
    cat <<EOF
      gitSecretRef:
        name: ${DEMO_GIT_SECRET_REF}
EOF
  fi
  if [[ -n "${DEMO_GIT_SUB_PATH:-}" ]]; then
    cat <<EOF
      subPath: ${DEMO_GIT_SUB_PATH}
EOF
  fi
}

render_security_agents_manifest() {
  cat <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: ${DEMO_SECURITY_ANALYSIS_AGENT_NAME}
  namespace: ${DEMO_NAMESPACE}
  labels:
    demo.orka.ai/name: ${DEMO_LABEL_VALUE}
    demo.orka.ai/scenario: security
spec:
  runtime:
    type: ${DEMO_RUNTIME_TYPE}
    defaultMaxTurns: 120
    defaultAllowBash: true
    defaultAllowedTools:
      - Read
      - Write
      - Edit
      - Bash
      - Glob
      - Grep
  model:
    name: ${DEMO_RUNTIME_MODEL}
  systemPrompt:
    inline: |
      You are the security analysis agent for a live Orka demo.
      Follow the task prompt precisely, write every required artifact under .orka-artifacts, and avoid speculative claims.
      If git reports dubious ownership, mark /workspace as a safe.directory before continuing.
      Never open a pull request directly.
  resources:
    requests:
      cpu: ${DEMO_AGENT_CPU_REQUEST}
      memory: ${DEMO_AGENT_MEMORY_REQUEST}
    limits:
      cpu: ${DEMO_AGENT_CPU_LIMIT}
      memory: ${DEMO_AGENT_MEMORY_LIMIT}
  secretRef:
    name: ${DEMO_RUNTIME_SECRET_REF}
---
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: ${DEMO_SECURITY_PATCH_AGENT_NAME}
  namespace: ${DEMO_NAMESPACE}
  labels:
    demo.orka.ai/name: ${DEMO_LABEL_VALUE}
    demo.orka.ai/scenario: security
spec:
  runtime:
    type: ${DEMO_RUNTIME_TYPE}
    defaultMaxTurns: 120
    defaultAllowBash: true
    defaultAllowedTools:
      - Read
      - Write
      - Edit
      - Bash
      - Glob
      - Grep
  model:
    name: ${DEMO_RUNTIME_MODEL}
  systemPrompt:
    inline: |
      You are the security remediation agent for a live Orka demo.
      Generate the smallest safe patch you can, write the patch artifacts requested by the task prompt, run focused validation when possible, and push only to the branch the task gives you.
      If git reports dubious ownership, mark /workspace as a safe.directory before continuing.
      Never open a pull request directly.
  resources:
    requests:
      cpu: ${DEMO_AGENT_CPU_REQUEST}
      memory: ${DEMO_AGENT_MEMORY_REQUEST}
    limits:
      cpu: ${DEMO_AGENT_CPU_LIMIT}
      memory: ${DEMO_AGENT_MEMORY_LIMIT}
  secretRef:
    name: ${DEMO_RUNTIME_SECRET_REF}
EOF
}

render_security_repository_scan_manifest() {
  cat <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: RepositoryScan
metadata:
  name: ${DEMO_SECURITY_SCAN_NAME}
  namespace: ${DEMO_NAMESPACE}
  labels:
    demo.orka.ai/name: ${DEMO_LABEL_VALUE}
    demo.orka.ai/scenario: security
spec:
  repoURL: ${DEMO_SECURITY_GIT_REPO}
  branch: ${DEMO_SECURITY_GIT_BRANCH}
  validationMode: light
  historyDays: 30
  analysisAgentRef:
    name: ${DEMO_SECURITY_ANALYSIS_AGENT_NAME}
  patchAgentRef:
    name: ${DEMO_SECURITY_PATCH_AGENT_NAME}
  gitSecretRef:
    name: ${DEMO_SECURITY_GIT_SECRET_REF}
EOF
  if [[ -n "${DEMO_SECURITY_SCHEDULE:-}" ]]; then
    cat <<EOF
  schedule: "${DEMO_SECURITY_SCHEDULE}"
EOF
  fi
  if [[ -n "${DEMO_SECURITY_GIT_SUB_PATH:-}" ]]; then
    cat <<EOF
  subPath: ${DEMO_SECURITY_GIT_SUB_PATH}
EOF
  fi
  if [[ -n "${DEMO_SECURITY_GIT_FORK_REPO:-}" ]]; then
    cat <<EOF
  forkRepo: ${DEMO_SECURITY_GIT_FORK_REPO}
EOF
  fi
  if [[ -n "${DEMO_SECURITY_PR_BASE_BRANCH:-}" ]]; then
    cat <<EOF
  prBaseBranch: ${DEMO_SECURITY_PR_BASE_BRANCH}
EOF
  fi
}
