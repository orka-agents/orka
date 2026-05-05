# Repository Security Scanning Spec

Status: Draft
Last updated: 2026-04-09

## Summary

Add a Codex Security-like workflow to Orka:

1. A user opens a new `Security` area in the embedded web UI.
2. The user registers a GitHub repository, branch, scan schedule, history window, and credentials.
3. Orka runs an initial scan that:
   - clones the repository into an isolated worker
   - generates an editable threat model
   - scans repository history and recent commits for likely vulnerabilities
   - validates likely issues when possible
   - stores structured findings and evidence
4. Orka continuously re-scans the repository for new commits.
5. From a finding detail page, the user can generate a patch and then open a pull request.

The feature should be GitHub-first in v1, human-in-the-loop for remediation, and built primarily on top of Orka's existing task, agent runtime, artifact, scheduling, and PR plumbing.

## Confirmed v1 Decisions

These decisions are locked for the first implementation:

- Source control scope: GitHub-only
- Configuration model: first-class `RepositoryScan` CRD
- Execution engine: existing agent runtimes for scanning and patching
- Default validation mode: `light`
- Remediation workflow: manual patch generation and manual PR creation
- First implementation slice: repository setup, manual scan, threat model, and findings
- Continuous scanning trigger: scheduled incremental scans only
- Monorepo scope: whole repo plus optional `subPath`

## External Reference Behavior

This design intentionally mirrors the broad Codex Security workflow described in the OpenAI docs:

- threat model first
- scan repository history and merged commits
- validate likely findings in an isolated environment
- propose a patch
- let the user review and create a PR from the findings UI

Reference docs:

- https://openai.com/index/codex-security-now-in-research-preview/
- https://developers.openai.com/codex/security/setup
- https://developers.openai.com/codex/security/threat-model
- https://developers.openai.com/codex/security/faq

## Goals

- Add a first-class `Security` web experience to the existing embedded dashboard.
- Let users configure a repository scan without writing YAML by hand.
- Generate a threat model that is editable and reused on future scans.
- Continuously scan a repository on a schedule and incrementally process new commits.
- Produce structured findings with severity, confidence, validation status, evidence, and remediation guidance.
- Support on-demand patch generation and PR creation from a finding.
- Preserve a clear audit trail of scans, findings, validation runs, and patch proposals.
- Reuse current Orka primitives wherever possible instead of introducing a parallel execution system.

## Non-goals

- Replacing SAST or dependency scanners.
- Auto-applying patches directly to protected branches.
- Supporting non-GitHub providers in v1.
- Building a perfect exploit or proof-of-concept generator.
- Creating a fully general vulnerability management platform with ticketing, SLA dashboards, or compliance reports in v1.
- Requiring a buildable repository for every scan. Validation may build when useful, but scanning should not require a successful build.

## Current Orka Capabilities We Should Reuse

These existing pieces are already strong enough to anchor the implementation:

- Embedded React dashboard and API server in `internal/api/` and `ui/`
- Scheduled tasks in `api/v1alpha1/task_types.go`
- Repo-backed agent workspaces in `api/v1alpha1/task_types.go`
- Agent runtime workers that can clone repos and return structured results in `workers/common/agent_runtime.go`
- Autonomous coordination plan persistence in `internal/store/` and `workers/ai/autonomous_prompt.go`
- Artifact upload support for AI and agent runtime tasks in `workers/common/artifacts.go`
- GitHub PR tools already registered in `internal/tools/registry.go`

Artifact handling already available:

- AI workers and agent runtime workers upload flat files from `/tmp/artifacts` through `workers/common/artifacts.go`.
- Agent runtime workers upload artifacts after successful result submission and also attempt upload on failure so partial threat models, findings, validation logs, or patch proposals can be recovered.
- Agent workspaces include a `.orka-artifacts` symlink that points to `/tmp/artifacts`, so prompts can tell agents to write artifacts from the repository working directory.
- Security scanning in v1 should rely heavily on agent runtime tasks because they already support git workspaces and artifact upload.

## High-level Product Flow

### 1. Repository setup

The user opens `Security -> New Repository` and provides:

- GitHub repository URL
- branch to scan
- optional sub-path for monorepos
- git secret reference
- environment profile or validation mode
- scan schedule
- history window
- analysis agent
- patch agent

### 2. Initial scan

Once saved, Orka immediately launches an initial scan run. That run:

- clones the target repo
- scans newest commits backward within the selected history window
- generates `security-threat-model.md`
- writes `security-findings.json`
- writes validation artifacts for reproduced findings
- writes a short task result summary

### 3. Threat model review

After the initial run completes, the repository detail page shows the generated threat model in an editor. The user can refine architecture notes, trust boundaries, auth assumptions, and risk priorities.

### 4. Continuous incremental scans

Subsequent runs execute on the configured schedule and process only commits that landed after the last completed run. Manual re-scan remains available from the UI.

### 5. Patch generation

From a finding detail page, the user clicks `Generate patch`. Orka creates a dedicated patch task against the repository, writes a diff artifact, and stores patch metadata.

### 6. PR creation

From the same finding detail page, the user clicks `Open PR`. Orka uses the latest successful patch proposal and creates a PR against the configured base branch.

## Core Design Decisions

### Decision 1: Use a new CRD for repository scan configuration

Add a new long-lived CRD, `RepositoryScan`, rather than encoding scan configuration directly in ad hoc tasks.

Why:

- It fits Orka's Kubernetes-native control model.
- Repository scan config is durable, namespace-scoped, and policy-like.
- It should have its own status, conditions, and reconciliation lifecycle.
- Dynamic outputs like findings and validation evidence still belong in SQLite, not in the CRD.

### Decision 2: Keep dynamic security data in SQLite

Threat models, scan runs, findings, and patch proposals should live in the store layer, not in CRD status blobs.

Why:

- Findings are high volume and change frequently.
- We want rich filtering and lookup by repository, severity, validation status, and patch status.
- The API/UI already use SQLite-backed data for results, plans, sessions, and artifacts.

### Decision 3: Use `type: agent` tasks for scan execution in v1

The scan task itself should be an agent runtime task with a git workspace, not an AI task.

Why:

- Agent runtime tasks already clone repositories.
- They can inspect commit history, run git commands, run project-specific validation commands, and emit structured outputs.
- This avoids depending on the unfinished ability for AI coordinators to delegate runtime-backed child tasks.

Future optimization:

- Once `delegate_task` can safely create `type: agent` children, we can decompose scans into scanner, validator, and patcher subtasks.
- The first shipping version should keep the scan pipeline inside a single repo-bound agent task to reduce moving parts.

### Decision 4: Human approval remains mandatory for remediation

Patch generation and PR creation should both be explicit user actions in v1.

Why:

- It matches the safer Codex Security interaction pattern.
- It lowers the risk of noisy or unsafe changes being pushed automatically.
- It keeps the UI understandable during the first rollout.

## Architecture Overview

```text
Browser
  |
  v
Security UI (/security/*)
  |
  v
Fiber API (/api/v1/security/*)
  |
  +--> RepositoryScan CRD CRUD
  |
  +--> SecurityStore (SQLite)
  |
  +--> Task creation for scan runs / patch runs
           |
           v
      Agent runtime worker (git workspace)
           |
           +--> result summary
           +--> security-threat-model.md
           +--> security-findings.json
           +--> security-validation.json
           +--> security-validation.txt
           +--> security-patch-*.diff
           |
           v
      Internal result/artifact APIs
           |
           v
      RepositoryScan reconciler ingests outputs
           |
           v
      SecurityStore updated
           |
           v
      UI shows repositories, findings, evidence, and patch actions
```

## New Kubernetes Resource

### `RepositoryScan`

Add `api/v1alpha1/repositoryscan_types.go` with a namespaced CRD.

Suggested shape:

```go
type RepositoryScanSpec struct {
    Provider           string                         `json:"provider,omitempty"` // "github" only in v1
    RepoURL            string                         `json:"repoURL"`
    Owner              string                         `json:"owner,omitempty"`    // derived or explicit
    Repository         string                         `json:"repository,omitempty"` // derived or explicit
    Branch             string                         `json:"branch,omitempty"`
    SubPath            string                         `json:"subPath,omitempty"`
    GitSecretRef       *corev1.LocalObjectReference   `json:"gitSecretRef,omitempty"`
    ForkRepo           string                         `json:"forkRepo,omitempty"`
    PRBaseBranch       string                         `json:"prBaseBranch,omitempty"`
    Schedule           string                         `json:"schedule,omitempty"`
    TimeZone           *string                        `json:"timeZone,omitempty"`
    HistoryDays        *int32                         `json:"historyDays,omitempty"`
    ValidationMode     string                         `json:"validationMode,omitempty"` // off, light, full
    AnalysisAgentRef   corev1alpha1.AgentReference    `json:"analysisAgentRef"`
    PatchAgentRef      *corev1alpha1.AgentReference   `json:"patchAgentRef,omitempty"`
    MaxFindingsPerRun  *int32                         `json:"maxFindingsPerRun,omitempty"`
    Suspend            *bool                          `json:"suspend,omitempty"`
}

type RepositoryScanStatus struct {
    Phase                string              `json:"phase,omitempty"` // Pending, Scanning, Ready, Error, Suspended
    LastScanID           string              `json:"lastScanID,omitempty"`
    LastScanTaskName     string              `json:"lastScanTaskName,omitempty"`
    LastSuccessfulScanAt *metav1.Time        `json:"lastSuccessfulScanAt,omitempty"`
    LastObservedHeadSHA  string              `json:"lastObservedHeadSHA,omitempty"`
    LastProcessedCommit  string              `json:"lastProcessedCommit,omitempty"`
    ThreatModelVersion   int64               `json:"threatModelVersion,omitempty"`
    FindingCounts        FindingCountsStatus `json:"findingCounts,omitempty"`
    Conditions           []metav1.Condition  `json:"conditions,omitempty"`
}

type FindingCountsStatus struct {
    Total    int32 `json:"total,omitempty"`
    Critical int32 `json:"critical,omitempty"`
    High     int32 `json:"high,omitempty"`
    Medium   int32 `json:"medium,omitempty"`
    Low      int32 `json:"low,omitempty"`
}
```

### Notes

- `provider` should default to `github`.
- `branch` defaults to the repository default branch when omitted.
- `schedule` uses the same cron format as `Task.spec.schedule`.
- `historyDays` is simpler than a custom `30d` parser and is good enough for v1.
- `forkRepo` and `prBaseBranch` map directly to existing workspace/PR concepts.

## Security Domain Storage

Add a new `SecurityStore` interface to `internal/store/store.go` and implement it in the existing SQLite store.

Suggested interface shape:

```go
type SecurityStore interface {
    CreateScanRun(ctx context.Context, run *ScanRun) error
    UpdateScanRun(ctx context.Context, run *ScanRun) error
    GetScanRun(ctx context.Context, namespace, id string) (*ScanRun, error)
    ListScanRuns(ctx context.Context, namespace, repositoryScan string, limit int, cursor string) ([]ScanRun, string, error)

    GetLatestThreatModel(ctx context.Context, namespace, repositoryScan string) (*ThreatModel, error)
    SaveThreatModel(ctx context.Context, model *ThreatModel) error

    UpsertFinding(ctx context.Context, finding *Finding) error
    GetFinding(ctx context.Context, namespace, id string) (*Finding, error)
    ListFindings(ctx context.Context, filter FindingFilter) ([]Finding, string, error)
    UpdateFindingState(ctx context.Context, namespace, id, state string) error

    CreatePatchProposal(ctx context.Context, proposal *PatchProposal) error
    UpdatePatchProposal(ctx context.Context, proposal *PatchProposal) error
    ListPatchProposals(ctx context.Context, namespace, findingID string) ([]PatchProposal, error)
}
```

### Go types

Add `internal/store/security_types.go` with the following domain objects:

- `ScanRun`
- `ThreatModel`
- `Finding`
- `FindingEvidenceRef`
- `PatchProposal`
- `FindingFeedback`

Suggested core fields:

```go
type ScanRun struct {
    ID               string
    Namespace        string
    RepositoryScan   string
    TaskName         string
    Mode             string // initial, incremental, manual, patch
    Phase            string // pending, running, succeeded, failed
    StartedAt        time.Time
    CompletedAt      *time.Time
    BaseCommit       string
    HeadCommit       string
    CommitCount      int
    Summary          string
    ErrorMessage     string
}

type ThreatModel struct {
    Namespace        string
    RepositoryScan   string
    Version          int64
    Content          string
    Source           string // generated, edited
    GeneratedByScan  string
    CreatedAt        time.Time
    UpdatedAt        time.Time
}

type Finding struct {
    ID                string
    Namespace         string
    RepositoryScan    string
    ScanRunID         string
    Fingerprint       string
    Title             string
    Summary           string
    Severity          string // critical, high, medium, low
    Confidence        string // high, medium, low
    ValidationStatus  string // unvalidated, validated, failed, skipped
    State             string // open, dismissed, patch_pending, patch_ready, pr_open, resolved
    FilePath          string
    Line              int
    CommitSHA         string
    RootCause         string
    Remediation       string
    SuggestedAction   string
    EvidenceJSON      string
    ValidationJSON    string
    PatchProposalID   string
    PRNumber          *int
    PRURL             string
    CreatedAt         time.Time
    UpdatedAt         time.Time
}

type PatchProposal struct {
    ID               string
    Namespace        string
    RepositoryScan   string
    FindingID        string
    TaskName         string
    Branch           string
    DiffArtifact     string
    SummaryArtifact  string
    Status           string // pending, succeeded, failed, pr_opened
    PRNumber         *int
    PRURL            string
    CreatedAt        time.Time
    UpdatedAt        time.Time
}
```

### SQLite tables

Add migrations in `internal/store/sqlite/sqlite.go`:

- `security_scan_runs`
- `security_threat_models`
- `security_findings`
- `security_patch_proposals`

Suggested table shape:

```sql
CREATE TABLE IF NOT EXISTS security_scan_runs (
  id                TEXT PRIMARY KEY,
  namespace         TEXT NOT NULL,
  repository_scan   TEXT NOT NULL,
  task_name         TEXT NOT NULL,
  mode              TEXT NOT NULL,
  phase             TEXT NOT NULL,
  base_commit       TEXT NOT NULL DEFAULT '',
  head_commit       TEXT NOT NULL DEFAULT '',
  commit_count      INTEGER NOT NULL DEFAULT 0,
  summary           TEXT NOT NULL DEFAULT '',
  error_message     TEXT NOT NULL DEFAULT '',
  started_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  completed_at      TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_security_scan_runs_repo
  ON security_scan_runs(namespace, repository_scan, started_at DESC);

CREATE TABLE IF NOT EXISTS security_threat_models (
  namespace         TEXT NOT NULL,
  repository_scan   TEXT NOT NULL,
  version           INTEGER NOT NULL,
  content           TEXT NOT NULL,
  source            TEXT NOT NULL,
  generated_by_scan TEXT NOT NULL DEFAULT '',
  created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  updated_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (namespace, repository_scan, version)
);

CREATE INDEX IF NOT EXISTS idx_security_threat_models_latest
  ON security_threat_models(namespace, repository_scan, version DESC);

CREATE TABLE IF NOT EXISTS security_findings (
  id                TEXT PRIMARY KEY,
  namespace         TEXT NOT NULL,
  repository_scan   TEXT NOT NULL,
  scan_run_id       TEXT NOT NULL,
  fingerprint       TEXT NOT NULL,
  title             TEXT NOT NULL,
  summary           TEXT NOT NULL,
  severity          TEXT NOT NULL,
  confidence        TEXT NOT NULL,
  validation_status TEXT NOT NULL,
  state             TEXT NOT NULL,
  file_path         TEXT NOT NULL DEFAULT '',
  line              INTEGER NOT NULL DEFAULT 0,
  commit_sha        TEXT NOT NULL DEFAULT '',
  root_cause        TEXT NOT NULL DEFAULT '',
  remediation       TEXT NOT NULL DEFAULT '',
  suggested_action  TEXT NOT NULL DEFAULT '',
  evidence_json     TEXT NOT NULL DEFAULT '',
  validation_json   TEXT NOT NULL DEFAULT '',
  patch_proposal_id TEXT NOT NULL DEFAULT '',
  pr_number         INTEGER,
  pr_url            TEXT NOT NULL DEFAULT '',
  created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  updated_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(namespace, repository_scan, fingerprint)
);

CREATE INDEX IF NOT EXISTS idx_security_findings_repo
  ON security_findings(namespace, repository_scan, severity, validation_status, state);

CREATE TABLE IF NOT EXISTS security_patch_proposals (
  id                TEXT PRIMARY KEY,
  namespace         TEXT NOT NULL,
  repository_scan   TEXT NOT NULL,
  finding_id        TEXT NOT NULL,
  task_name         TEXT NOT NULL,
  branch            TEXT NOT NULL,
  diff_artifact     TEXT NOT NULL DEFAULT '',
  summary_artifact  TEXT NOT NULL DEFAULT '',
  status            TEXT NOT NULL,
  pr_number         INTEGER,
  pr_url            TEXT NOT NULL DEFAULT '',
  created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  updated_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

### Why JSON columns are acceptable here

Use JSON text columns for evidence and validation metadata in v1 instead of building a large normalized evidence model immediately.

Why:

- We already have artifact blobs stored separately.
- The UI mainly needs structured metadata plus artifact filenames.
- It keeps the store API and migrations manageable for the first version.

## Artifact Contract

Security runs should communicate detailed outputs through artifacts, not only the task result text.

Because `workers/common/artifacts.go` uploads a flat directory under `/tmp/artifacts/`, artifact filenames in v1 must be flat and path-safe.

Required artifact filenames for scan runs:

- `security-threat-model.md`
- `security-findings.json`
- `security-validation.json`

Optional artifact filenames:

- `security-scan-summary.json`
- `security-validation.txt` (preferred human-readable validation summary)
- `security-patch-<finding-id>.diff`
- `security-patch-<finding-id>.json`

AI workers and agent runtime workers upload `/tmp/artifacts`. Agent runtime workers upload artifacts after successful result submission and attempt upload on failure for partial artifacts. In agent workspaces, `.orka-artifacts` points to `/tmp/artifacts`; security prompts should require agents to write the artifact contract there. Current upload limits are 10 MB per file and 50 MB total per task, and filenames must be flat/path-safe.

Required security artifacts can be recovered and enforced by worker prompts: if an agent produces the information in stdout but misses a file, follow-up validation or repair prompts should ask it to materialize the missing artifact before the task is considered complete.

### `security-findings.json` contract

The scanner task should write a single compact JSON payload:

```json
{
  "version": 1,
  "repository": {
    "repo_url": "https://github.com/org/repo",
    "branch": "main",
    "head_sha": "abc123",
    "base_sha": "def456"
  },
  "scan": {
    "mode": "initial",
    "commit_count": 42,
    "summary": "Validated 2 high-confidence findings"
  },
  "findings": [
    {
      "fingerprint": "sha256:...",
      "title": "Unsanitized shell execution in backup endpoint",
      "summary": "User-controlled input reaches exec.Command through the backup request path.",
      "severity": "high",
      "confidence": "high",
      "validation_status": "validated",
      "file_path": "internal/backup/handler.go",
      "line": 88,
      "commit_sha": "abc123",
      "root_cause": "Untrusted request field is concatenated into shell arguments",
      "remediation": "Pass validated arguments directly and reject shell metacharacters",
      "suggested_action": "generate_patch",
      "evidence": [
        {
          "kind": "artifact",
          "name": "security-validation.txt",
          "label": "Validation transcript"
        }
      ]
    }
  ]
}
```

The agent prompt should explicitly forbid large code excerpts in this file. Long evidence belongs in artifacts.

## Execution Model

### Scan run creation

Add `internal/controller/repositoryscan_controller.go`.

Controller responsibilities:

- reconcile `RepositoryScan` resources
- trigger an initial run when a repository is first created
- trigger incremental runs on schedule
- avoid overlapping scan runs for the same repository
- watch completion of scan and patch tasks labeled for security
- ingest artifacts into `SecurityStore`
- update `RepositoryScan.status`

### Task labels and annotations

Extend `internal/labels/labels.go` with new labels:

- `orka.ai/security-target`
- `orka.ai/security-scan-id`
- `orka.ai/security-scan-mode`
- `orka.ai/security-finding-id`

Use these labels on all security-created tasks so the reconciler can find and ingest them cleanly.

### Scan task shape

Initial and incremental scans should create `type: agent` tasks:

```yaml
spec:
  type: agent
  agentRef:
    name: security-scanner
  prompt: "<generated prompt>"
  timeout: "2h"
  priority: 700
  workspace:
    gitRepo: "https://github.com/org/repo.git"
    branch: "main"
    gitSecretRef:
      name: repo-git-creds
    subPath: "services/api"
```

### Prompt building

Do not inline a giant security prompt in the handler or controller.

Add prompt builders under a new package, for example:

- `internal/security/prompts/scan.md`
- `internal/security/prompts/patch.md`
- `internal/security/prompts.go`

The scan prompt should include:

- repo identity and branch
- scan mode: initial, incremental, manual
- threat model instructions
- required artifact contract
- validation guidance
- max findings per run
- current threat model text if one already exists
- commit range or head/base hints when doing incremental scans

### Initial scan logic

On the first run:

- scan from newest commits backward
- cap work by `historyDays`
- generate a first threat model artifact even if no findings are emitted
- persist all findings as `open`

### Incremental scan logic

On subsequent runs:

- fetch the current head SHA
- compare with `status.lastProcessedCommit`
- if unchanged, mark run as succeeded with a no-op summary
- if changed, prompt the agent to focus on commits after the last processed SHA, while still using the current threat model as context

### Patch task logic

When the user requests patch generation:

- create a dedicated `type: agent` task
- set `pushBranch` to `orka/security/<finding-id>`
- use `forkRepo` and `prBaseBranch` when configured
- prompt the agent for a minimal, reviewable fix and a diff artifact
- persist a `PatchProposal` row immediately in `pending` state
- update it to `succeeded` or `failed` when the task completes

## API Design

Add security routes in `internal/api/server.go` under `/api/v1/security`.

### Repository configuration

- `POST /api/v1/security/repositories`
- `GET /api/v1/security/repositories`
- `GET /api/v1/security/repositories/:name`
- `PUT /api/v1/security/repositories/:name`
- `DELETE /api/v1/security/repositories/:name`

These should map to `RepositoryScan` CRD CRUD, similar to current `Agent` endpoints.

### Threat model

- `GET /api/v1/security/repositories/:name/threat-model`
- `PUT /api/v1/security/repositories/:name/threat-model`

Threat model edits should create a new version in `security_threat_models`, not mutate history in place.

### Scan runs

- `GET /api/v1/security/repositories/:name/scans`
- `POST /api/v1/security/repositories/:name/scans`

`POST` launches a manual scan run immediately.

### Findings

- `GET /api/v1/security/repositories/:name/findings`
- `GET /api/v1/security/findings/:id`
- `POST /api/v1/security/findings/:id/dismiss`
- `POST /api/v1/security/findings/:id/reopen`

### Patch and PR actions

- `POST /api/v1/security/findings/:id/patch`
- `GET /api/v1/security/findings/:id/patches`
- `POST /api/v1/security/findings/:id/pull-request`

### Filtering

The findings list endpoint should support:

- `severity`
- `validationStatus`
- `state`
- `recommended=true`
- `limit`
- `cursor`

`recommended=true` should return a capped list ordered by:

1. severity
2. validation status
3. confidence
4. most recently updated

## Reusing Existing PR Plumbing

Orka already contains PR logic in built-in tools. The API layer should reuse the same GitHub helper code instead of duplicating API calls in handlers.

Recommended refactor:

- extract shared GitHub PR helper logic from:
  - `internal/tools/create_pull_request.go`
  - `internal/tools/review_pull_request.go`
  - `internal/tools/merge_pull_request.go`
- move helper code into a shared package such as `internal/githubutil/`
- call that shared package from both:
  - built-in tools
  - new security API handlers

### PR creation behavior

`POST /api/v1/security/findings/:id/pull-request` should:

- load the latest successful `PatchProposal`
- verify it has a pushed branch or branch metadata in the task result
- derive PR title/body from finding title and remediation summary
- open the PR against `RepositoryScan.spec.prBaseBranch` or the scan branch
- update `security_patch_proposals` and `security_findings`

## UI Design

### New routes

Add routes under `ui/src/routes/security/`:

- `ui/src/routes/security/index.tsx`
- `ui/src/routes/security/new.tsx`
- `ui/src/routes/security/$repoId.tsx`
- `ui/src/routes/security/findings/$findingId.tsx`

Add a new sidebar item in `ui/src/components/layout/sidebar.tsx`:

- label: `Security`
- route: `/security`

### New schemas

Add `ui/src/schemas/security.ts`:

- `repositoryScanSchema`
- `scanRunSchema`
- `threatModelSchema`
- `securityFindingSchema`
- `patchProposalSchema`

### New hooks

Add `ui/src/hooks/use-security.ts` with:

- `useRepositoryScans()`
- `useRepositoryScan(name)`
- `useThreatModel(name)`
- `useUpdateThreatModel(name)`
- `useScanRuns(name)`
- `useFindings(name, filters)`
- `useFinding(id)`
- `useGeneratePatch(id)`
- `useCreatePullRequest(id)`

Keep the implementation style consistent with `ui/src/hooks/use-tasks.ts`.

### New UI components

Add `ui/src/components/security/`:

- `repository-list.tsx`
- `repository-create-form.tsx`
- `repository-detail.tsx`
- `threat-model-editor.tsx`
- `recommended-findings.tsx`
- `finding-table.tsx`
- `finding-detail.tsx`
- `patch-proposal-card.tsx`

### Repository list page

Show:

- repository name
- branch
- last scan status
- open finding counts by severity
- last updated time
- quick action: `Scan now`

### Repository detail page

Show:

- repository metadata
- scan health summary
- editable threat model panel
- recommended findings section
- all findings table with filters
- recent scan runs

### Finding detail page

Show:

- title, severity, confidence, validation status
- file path and line
- commit info
- root cause
- remediation summary
- linked evidence artifacts
- validation output
- patch status
- actions:
  - `Generate patch`
  - `Open PR`
  - `Dismiss`
  - `Reopen`

## Scan and Patch Prompt Contracts

### Scanner prompt contract

The scanner agent should be instructed to:

- inspect current code and recent commits
- generate or update a concise threat model
- produce a bounded number of findings
- prefer high-confidence findings over broad speculation
- validate findings only when safe and practical
- write structured artifacts exactly as specified
- avoid editing or pushing code during scan runs

### Patch prompt contract

The patch agent should be instructed to:

- fix only one finding per task
- keep the diff minimal
- preserve existing behavior unless the finding requires a behavior change
- run focused tests when available
- write:
  - `security-patch-<finding-id>.diff`
  - `security-patch-<finding-id>.json`
- avoid creating a PR directly; leave PR creation to the API action in v1

## Controller Ingestion Logic

When a labeled security scan task completes:

1. Load task result and artifacts from the existing stores.
2. Parse `security-scan-summary.json` if present.
3. Parse `security-threat-model.md`.
4. Parse `security-findings.json`.
5. Upsert scan run row.
6. Create a new threat model version if generated content differs from the latest version.
7. Upsert findings by stable fingerprint.
8. Update `RepositoryScan.status`.

When a labeled security patch task completes:

1. Locate the associated `Finding`.
2. Read the patch diff artifact.
3. Upsert `PatchProposal`.
4. Update finding state to `patch_ready` or `patch_failed`.

## Metrics

Add Prometheus metrics under `internal/metrics/`:

- `orka_security_scan_runs_total{mode,status}`
- `orka_security_findings_total{severity,state}`
- `orka_security_patch_requests_total{status}`
- `orka_security_threat_model_updates_total{source}`

These should help operators understand cost, volume, and pipeline health.

## Security and Safety

- Continue to run workers in isolated pods with the existing hardened defaults.
- Require explicit `gitSecretRef` or detected credentials for private repositories.
- Never auto-open PRs without an explicit user action in v1.
- Keep artifact filenames flat and sanitized to stay within the current artifact upload model.
- Truncate or summarize oversized evidence in artifacts to stay below the current 10 MB per-file and 50 MB total upload limits.
- Treat edited threat models as user input that influences scan ranking, not as executable instructions.

## Testing Plan

### Backend unit tests

- `api/v1alpha1/repositoryscan_types_test.go`
- `internal/store/sqlite/security_store_test.go`
- `internal/api/security_handlers_test.go`
- `internal/controller/repositoryscan_controller_test.go`
- `internal/security/prompts_test.go`

### Worker tests

- `workers/common/agent_runtime_test.go` for artifact upload on success and failure
- scanner artifact contract parser tests
- patch artifact contract parser tests

### UI tests

- `ui/src/hooks/use-security.test.ts`
- route tests for:
  - repository list
  - repository detail
  - finding detail
  - threat model editing

### E2E

Add a new e2e scenario:

1. Create repository scan config.
2. Trigger manual scan.
3. Persist threat model and findings.
4. Generate patch for one finding.
5. Open PR from the patch proposal.

## Rollout Plan

### Milestone 1

- `RepositoryScan` CRD
- security API CRUD
- security UI shell
- manual scan trigger
- scan run persistence

### Milestone 2

- threat model generation and editing
- findings persistence
- recommended findings view
- incremental scan scheduling

### Milestone 3

- validation evidence artifacts
- patch generation
- patch proposal persistence

### Milestone 4

- PR creation from finding detail
- dismiss/reopen workflow
- metrics and e2e coverage

## Recommended File Plan

### Backend

- `api/v1alpha1/repositoryscan_types.go`
- `api/v1alpha1/zz_generated.deepcopy.go`
- `internal/api/security_handlers.go`
- `internal/controller/repositoryscan_controller.go`
- `internal/store/security_types.go`
- `internal/store/sqlite/security_store.go`
- `internal/security/prompts.go`
- `internal/security/parser.go`

### UI

- `ui/src/routes/security/index.tsx`
- `ui/src/routes/security/new.tsx`
- `ui/src/routes/security/$repoId.tsx`
- `ui/src/routes/security/findings/$findingId.tsx`
- `ui/src/components/security/*`
- `ui/src/hooks/use-security.ts`
- `ui/src/schemas/security.ts`

### Samples and docs

- `config/samples/security_repositoryscan.yaml`
- `config/samples/security_agents.yaml`
- `docs/api-reference.md`
- `docs/ui.md`

## Open Questions

The major product-scope decisions for `v1` are now resolved. Remaining implementation questions are narrower engineering choices:

1. Should validation remain inside the primary scan task in `v1`, or should we split it into a second targeted task for very large repositories?
2. Do we want a first-class suppression model in `v1`, or is `dismiss/reopen` state on findings enough initially?
3. Should `PatchAgentRef` be required for patch generation, or default to `AnalysisAgentRef` when omitted?
4. When a repository has no explicit branch configured, should the controller resolve and persist the default branch at creation time or lazily on first scan?

## Recommended First Implementation Slice

Start with the smallest end-to-end slice that proves the architecture:

1. Add `RepositoryScan` CRD and CRUD API.
2. Add `Security` routes and repository list/detail pages.
3. Implement manual scan only.
4. Run scan as a single repo-backed agent task.
5. Persist threat model and findings from agent artifacts.
6. Add threat model editing.

The first committed milestone stops there. After that:

7. Add scheduled incremental scans.
8. Add patch generation.
9. Add PR creation.

That sequence keeps the feature useful early while staying aligned with the long-term design.
