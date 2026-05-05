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
      Do not modify .github/workflows/* or CI, release, Dockerfile, Makefile, Goreleaser, or other build automation unless the task explicitly requests workflow/build/release changes.
      Do not install language runtimes or large toolchains into the agent workspace; the coordinator runs full validation in a separate container task.
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
      Do not block on unrelated existing files, generated artifacts, or any file that is not listed by git diff --name-only origin/<base-branch>...HEAD.
      Blocking feedback must cite only changed files and exact diff lines from git diff origin/<base-branch>...HEAD.
      If you cannot point to diff evidence for a concern, do not return CHANGES_NEEDED; approve or include the concern as a non-blocking note.
      Your final answer must not mention any file that is absent from git diff --name-only origin/<base-branch>...HEAD.
      Follow that heading with concise, actionable feedback.
      For code changes, look for secret exposure, command injection, unsafe shell invocation, unsafe file or network access, and credential handling issues.
      Approve when the requested change is already correct, clearly scoped, and safe.
      Do not request preference-only changes unless they materially affect security, correctness, or the user's explicit requirements.
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
      Do not block on unrelated existing files, generated artifacts, or any file that is not listed by git diff --name-only origin/<base-branch>...HEAD.
      Blocking feedback must cite only changed files and exact diff lines from git diff origin/<base-branch>...HEAD.
      If you cannot point to diff evidence for a concern, do not return CHANGES_NEEDED; approve or include the concern as a non-blocking note.
      Your final answer must not mention any file that is absent from git diff --name-only origin/<base-branch>...HEAD.
      Follow that heading with concise, actionable feedback.
      Focus on feature behavior, correctness, clarity, testability, and fit with project conventions.
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
      2. Delegate implementation to ${DEMO_CODER_AGENT_NAME} with that workspace, a pushBranch, timeout "40m", and maxTurns 60. Tell the coder to edit files only and let Orka push the final diff automatically.
      3. Wait for the coder to finish.
      If the coder task fails or times out, do not create a pull request, and report the failure clearly.
      4. Validate the pushed change with create_container_task before review. First determine the validation environment from repository evidence, not from demo defaults. Every discovery and validation container task that inspects repository files MUST set workspace.gitRepo, workspace.gitSecretRef, and either workspace.ref=headSHA or workspace.branch=pushBranch. Do not run repo validation without workspace. Prefer immutable validation: if the latest coder result includes headSHA, set workspace.ref = headSHA; otherwise set workspace.branch = pushBranch. Do not set workspace.pushBranch for validation. If the environment is not already clear, create a read-only discovery container task with the default worker image and that workspace to inspect project files such as .github/workflows, go.mod, package.json, pyproject.toml, Cargo.toml, Makefile, Dockerfile, and .devcontainer. Choose and report the validation image, command, workspace ref/branch, and evidence. For Go repositories, prefer the go.mod toolchain directive when present, otherwise the go directive, choose a matching golang:<major.minor> image, and prepend: export PATH=/usr/local/go/bin:\$PATH; export GOCACHE=/tmp/go-cache; export GOMODCACHE=/tmp/go-mod-cache; export CGO_ENABLED=0; Use command ["sh", "-lc"] and args [the selected validation command]. If validation fails because the repo is missing, go is not on PATH, or caches are unwritable, retry validation with corrected configuration or report VALIDATION_CONFIG_BLOCKED rather than treating it as a code failure. If the validation environment cannot be determined confidently, report VALIDATION_CONFIG_BLOCKED and do not create a pull request. Wait for the validation task. If validation fails, summarize its result and delegate a focused repair task to ${DEMO_CODER_AGENT_NAME} with workspace.gitRepo, workspace.gitSecretRef, workspace.branch = pushBranch, workspace.pushBranch = pushBranch, timeout "40m", and maxTurns 60. Tell the coder to fix only validation failures. Repeat validation. Use at most ${DEMO_VALIDATION_REPAIR_LIMIT} validation repair tasks; if validation still fails, report VALIDATION_BLOCKED and do not create a pull request.
      5. Delegate parallel review to ${DEMO_SECURITY_REVIEWER_NAME} and ${DEMO_QUALITY_REVIEWER_NAME} without prior_task. For review tasks, set workspace.gitRepo, workspace.gitSecretRef, workspace.branch = pushBranch, timeout "20m", and maxTurns 40. NEVER include workspace.pushBranch on review tasks. Include the latest validation task name and validation summary in each review prompt. In each review prompt, explicitly tell reviewers the workspace is already checked out for them, so they must not clone again. Tell them to fetch origin, run git diff --name-only origin/<base-branch>...HEAD, inspect only that diff with git diff origin/<base-branch>...HEAD, ignore unchanged repository files outside that diff, avoid blocking on concerns without diff evidence, and cite only changed files/lines in feedback. Explicitly include the original change request acceptance criteria in each review prompt. Reviewers must judge the change against the original request and acceptance criteria, not hidden demo expectations. They may require bounded labels, clear documentation/warnings, and no user/key/prompt label values when those criteria apply. If the coder result includes a files list, include an "Expected changed files" section in the review prompt and tell reviewers their final answer must not mention files outside that list or outside git diff --name-only origin/<base-branch>...HEAD. Reviewers should inspect the current branch diff against the base branch from the prompt; they do not need prior_task.
      6. Wait for both reviewers.
      7. If either reviewer returns CHANGES_NEEDED, summarize all review feedback and delegate a focused repair task to ${DEMO_CODER_AGENT_NAME}. For repair tasks, set workspace.gitRepo, workspace.gitSecretRef, workspace.branch = pushBranch, workspace.pushBranch = pushBranch, timeout "40m", and maxTurns 60. Tell the coder to preserve the original request and fix only the review issues. Prefer additional focused repair iterations over stopping early when reviewers identify concrete diff-backed security, correctness, or acceptance-criteria issues. Then repeat validation and review. Use at most ${DEMO_REVIEW_REPAIR_LIMIT} review repair tasks; if reviewers still request changes, report REVIEW_BLOCKED and do not create a pull request.
      8. When validation passes and both reviewers approve, create a pull request with create_pull_request using the latest successful coder task name, the pushed branch, and the base branch from the prompt.
      9. After the pull request exists, call check_pull_request_ci with the latest coder task and PR number. If CI is pending, keep checking for up to 30 minutes. If CI fails, summarize the failed check details and delegate a focused CI repair task to ${DEMO_CODER_AGENT_NAME} with workspace.branch = pushBranch and workspace.pushBranch = pushBranch. Tell the coder to fix only build, lint, formatting, dependency, or test failures. After each CI repair, repeat validation and review before checking CI again. Use at most ${DEMO_CI_REPAIR_LIMIT} CI repair tasks; if CI still fails, report CI_BLOCKED.
      10. Report the pull request URL, final validation status, final review status, CI status, child-task count, and a brief summary. Do not report the PR as ready unless validation passes, reviewers approve, and CI passes.
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
  emit_block "" "Claude Code is the local client. Orka is the server-side orchestrator.
Start exactly one coordinator task for this demo, but first create the coordinator and specialist Agents through Orka's chat tool path.

Create the Agents by translating the Agent specs below into create_agent tool calls. This YAML is the source of truth for the four demo Agents; do not apply it with kubectl and do not create any extra Agents.

Critical tool-use constraints:
- The first four Orka tool calls MUST be direct create_agent tool calls.
- Do not use create_ai_task, create_agent_task, or create_container_task to create Agents.
- A Task whose prompt starts with "create_agent" is incorrect and must not be created.
- Only after all four create_agent calls return success may you call create_ai_task once for the coordinator.

Create-agent mapping rules:
- Call create_agent exactly four times before creating the coordinator task, one call for each Agent object in this order: ${DEMO_CODER_AGENT_NAME}, ${DEMO_SECURITY_REVIEWER_NAME}, ${DEMO_QUALITY_REVIEWER_NAME}, ${DEMO_PR_COORDINATOR_NAME}.
- Pass metadata.name as name and metadata.namespace as namespace.
- Pass spec.providerRef.name as providerRef when present.
- Pass spec.model.name as model.name.
- Pass spec.systemPrompt.inline as systemPrompt verbatim.
- Pass spec.runtime as runtime. For runtime Agents, map spec.secretRef.name to runtime.secretRef.
- Preserve runtime.defaultMaxTurns, runtime.defaultAllowedTools, and runtime.defaultAllowBash.
- Pass spec.resources as resources, including requests and limits, when present.
- Pass spec.coordination as coordination, including allowedAgents, maxDepth, and maxConcurrentChildren.
- Do not use create_agent initialPrompt. Agent creation must not start any task.
- create_agent does not need labels for this chat demo path; ignore metadata.labels if they are not supported by the tool.

---BEGIN AGENT SPECS---"
  render_pr_agents_manifest
  emit_block "" "---END AGENT SPECS---

After all four create_agent calls succeed, use Orka's create_ai_task tool exactly once with these arguments:
- name: ${DEMO_CHAT_SESSION}
- namespace: ${DEMO_NAMESPACE}
- agentRef: ${DEMO_PR_COORDINATOR_NAME}
- providerRef: ${DEMO_PROVIDER_REF}
- sessionRef: ${DEMO_CHAT_SESSION}
- timeout: ${DEMO_PR_WORKFLOW_TIMEOUT}
- priority: 700
- prompt: use the entire Coordinator task prompt section below verbatim

Do not create, update, or delete tools or providers in this chat turn.
Do not create any task except the one coordinator create_ai_task call described above.
After creating the coordinator task, capture the returned task name, use wait_for_task until it reaches Succeeded or Failed, then use fetch_task_output and report only a concise final status.

Coordinator task prompt (verbatim):
---BEGIN COORDINATOR TASK PROMPT---"
  pr_repo_details_block "${DEMO_CHAT_PUSH_BRANCH}"
  printf '\n\n'
  emit_block "" "Change request:
${DEMO_CHAT_REQUEST}
---END COORDINATOR TASK PROMPT---"
}

render_chat_story_file() {
  emit_block "" "Scenario:
A maintainer gives Orka a live change request through an Anthropic-compatible chat client. Orka should turn that request into an auditable coordinator Task, specialist child Tasks, validation, review, and a PR handoff.

What to watch:
- Claude Code sends one chat request to Orka's Anthropic-compatible endpoint.
- The chat request creates the coordinator, coder, and reviewer Agents through the create_agent tool path.
- The chat request then creates one coordinator Task in Kubernetes.
- The chat-created coordinator delegates to coder and reviewer Agents.
- Child Tasks implement the current request, run validation, and perform parallel review.
- The final result points to the PR handoff with review and CI status.

Current change request:
${DEMO_CHAT_REQUEST}

Repository details:"
  pr_repo_details_block "${DEMO_CHAT_PUSH_BRANCH}"
}

render_manual_story_file() {
  emit_block "" "Scenario:
The platform team submits the same kind of work as declarative Kubernetes YAML instead of a chat turn. The request can be the default Vekil metrics slice or a live request supplied with DEMO_MANUAL_REQUEST, DEMO_REQUEST_FILE, or DEMO_MANUAL_REQUEST_FILE.

What to watch:
- The coordinator, coder, and reviewer Agents are applied up front.
- The Task CR starts a bounded workflow from the rendered prompt.
- Orka records child Tasks, runtime logs, validation, review, CI repair if needed, and the final PR status.

Current change request:
${DEMO_MANUAL_REQUEST}

Repository details:"
  pr_repo_details_block "${DEMO_MANUAL_PUSH_BRANCH}"
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
  ai:
    providerRef:
      name: ${DEMO_PROVIDER_REF}
  agentRef:
    name: ${DEMO_PR_COORDINATOR_NAME}
  timeout: ${DEMO_PR_WORKFLOW_TIMEOUT}
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
