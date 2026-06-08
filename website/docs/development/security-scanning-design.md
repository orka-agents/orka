---
slug: /security-scanning-design
---

# Repository Security Scanning Design

This page documents the internal design of repository security scanning: the storage model,
controller ingestion flow, artifact contract, and agent prompt contracts. For the
user-facing workflow, see [Repository Security Scanning](../guides/repository-security-scanning.md).

The feature is GitHub-first, human-in-the-loop for remediation, and built on top of Orka's
existing task, agent runtime, artifact, scheduling, and PR plumbing rather than a parallel
execution system.

## Design Decisions

| Decision | Rationale |
|----------|-----------|
| **`RepositoryScan` is a first-class CRD**, not config embedded in ad hoc tasks | Scan config is durable, namespace-scoped, and policy-like, with its own status, conditions, and reconciliation lifecycle. Dynamic outputs (findings, evidence) stay in SQLite. |
| **Dynamic security data lives in SQLite**, not CRD status | Findings are high-volume and change frequently; the store enables filtering by repository, severity, validation status, and patch status, consistent with results/plans/sessions/artifacts. |
| **Scans run as Kubernetes-backed tasks** with a git workspace | Threat-model, review, validation, and patch work run as agent tasks. The deterministic mapper runs as a container task using the managed general worker, so slice generation does not require model access. |
| **Human approval is mandatory for remediation** | Patch generation and PR creation are explicit user actions, matching the safer Codex Security interaction pattern and reducing the risk of noisy or unsafe automated changes. |

This design mirrors the broad Codex Security workflow (threat model first; scan history and
merged commits; validate likely findings in isolation; propose a patch; let the user review
and create a PR). Reference: [OpenAI Codex Security](https://developers.openai.com/codex/security/setup).

### Scope (v1)

- Source control: GitHub only.
- Continuous scanning: scheduled incremental scans only.
- Monorepo: whole repo plus optional `subPath`.
- Default validation mode: `light`.
- Remediation: manual patch generation and manual PR creation.

Non-goals include replacing SAST/dependency scanners, auto-applying patches to protected
branches, non-GitHub providers, exploit/PoC generation, full vulnerability-management
tooling (ticketing/SLA/compliance), and requiring a buildable repo for every scan.

## Architecture

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
      Agent/general worker (git workspace)
           |
           +--> result summary
           +--> security-slices.json
           +--> security-review-context-<slice-id>.json
           +--> security-threat-model.md
           +--> security-findings.v2.json
           +--> security-dropped-findings.json
           +--> security-validation-*.txt
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

## `RepositoryScan` CRD

Defined in `api/v1alpha1/repositoryscan_types.go` as a namespaced CRD. Core spec/status
shape:

```go
type RepositoryScanSpec struct {
    Provider           string                         `json:"provider,omitempty"` // "github" only in v1
    RepoURL            string                         `json:"repoURL"`
    Owner              string                         `json:"owner,omitempty"`      // derived or explicit
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
```

Notes:

- `provider` defaults to `github`.
- `branch` currently defaults to the literal `main` when omitted (`security.EffectiveBranch`); it is **not** resolved from the repository's actual default branch. Set `spec.branch` explicitly for repositories whose default branch is not `main` (e.g. `master`, `trunk`).
- `schedule` uses the same cron format as `Task.spec.schedule`.
- `historyDays` is intentionally simpler than a custom `30d` duration parser.
- `forkRepo` and `prBaseBranch` map directly to existing workspace/PR concepts.

## Storage Model

The `SecurityStore` interface (`internal/store/store.go`, SQLite implementation under
`internal/store/sqlite/`) persists dynamic security data. Domain types live in
`internal/store/security_types.go`: `ScanRun`, `ThreatModel`, `Finding`,
`FindingEvidenceRef`, `ReviewSlice`, `DroppedFinding`, and `PatchProposal`.

```go
type SecurityStore interface {
    CreateScanRun(ctx context.Context, run *ScanRun) error
    UpdateScanRun(ctx context.Context, run *ScanRun) error
    GetScanRun(ctx context.Context, namespace, id string) (*ScanRun, error)
    ListScanRuns(ctx context.Context, namespace, repositoryScan string, limit int, cursor string) ([]ScanRun, string, error)

    UpsertReviewSlice(ctx context.Context, slice *ReviewSlice) error
    ListReviewSlices(ctx context.Context, filter ReviewSliceFilter) ([]ReviewSlice, string, error)
    GetReviewSlice(ctx context.Context, namespace, repositoryScan, id string) (*ReviewSlice, error)
    UpdateReviewSliceStatus(ctx context.Context, namespace, repositoryScan, id, lastScanRunID, status string) error

    GetLatestThreatModel(ctx context.Context, namespace, repositoryScan string) (*ThreatModel, error)
    SaveThreatModel(ctx context.Context, model *ThreatModel) error

    UpsertFinding(ctx context.Context, finding *Finding) error
    GetFinding(ctx context.Context, namespace, id string) (*Finding, error)
    ListFindings(ctx context.Context, filter FindingFilter) ([]Finding, string, error)
    GetFindingCounts(ctx context.Context, namespace, repositoryScan string) (FindingCounts, error)
    UpdateFindingState(ctx context.Context, namespace, id, state string) error

    CreatePatchProposal(ctx context.Context, proposal *PatchProposal) error
    UpdatePatchProposal(ctx context.Context, proposal *PatchProposal) error
    ListPatchProposals(ctx context.Context, namespace, findingID string) ([]PatchProposal, error)

    CreateDroppedFinding(ctx context.Context, dropped *DroppedFinding) error
    ListDroppedFindings(ctx context.Context, filter DroppedFindingFilter) ([]DroppedFinding, string, error)
}
```

### SQLite tables

Six tables back the security store: `security_scan_runs`, `security_threat_models`,
`security_review_slices`, `security_findings`, `security_dropped_findings`, and
`security_patch_proposals`. Existing databases are upgraded idempotently with additive
columns for slice counts, accepted/dropped counts, v2 finding metadata, and structured
evidence references.

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

Evidence and validation metadata use JSON text columns rather than a fully normalized
evidence model: artifact blobs are stored separately, the UI mainly needs structured
metadata plus artifact filenames, and JSON keeps the store API and migrations manageable.

Review slices store JSON arrays for entrypoints, owned files, context files, tests, tags,
and trust boundaries. Dropped finding diagnostics store only a reason and compact sample
JSON; they must not contain secrets, raw tokens, raw transcripts, or full request contexts.

> **Threat-model history:** although `security_threat_models` carries a `version` column,
> `SaveThreatModel` is currently replace-only — it deletes existing rows for the repository
> before inserting the new model, so only the latest threat model is retained. The versioned
> schema leaves room to preserve history later, but no prior versions are kept today.

## Artifact Contract

Security runs communicate detailed outputs through artifacts, not just the task result
text. Because `workers/common/artifacts.go` uploads a flat directory under
`/tmp/artifacts/`, artifact filenames must be flat and path-safe.

Current scan artifacts:

- `security-threat-model.md` — required from the threat-model stage.
- `security-slices.json` — deterministic mapper output, schema version 1.
- `security-review-context-<slice-id>.json` — bounded prompt/context manifest, schema version 1.
- `security-findings.v2.json` — evidence-backed v2 findings payload, schema version 2.
- `security-dropped-findings.json` — controller-written diagnostics for invalid v2 findings.

Optional:

- `security-validation.json`
- `security-validation.txt`
- `security-patch-<finding-id>.diff`
- `security-patch-<finding-id>.json`

Agent runtime tasks call `common.UploadArtifacts()` after result submission on both the
success path and the failure path where partial artifacts still exist, so the threat model,
findings payload, validation evidence, and patch diff persist reliably.

### `security-slices.json`

The mapper writes stable, deterministic review slices:

```json
{
  "schemaVersion": 1,
  "slices": [
    {
      "schemaVersion": 1,
      "id": "slice_...",
      "repositoryScan": "example",
      "source": "deterministic-go-package",
      "title": "Go package internal/security",
      "summary": "Security artifact parsing and prompt contracts.",
      "kind": "package",
      "entrypoints": [{"path": "internal/security/security.go", "reason": "primary package source"}],
      "ownedFiles": [{"path": "internal/security/security.go", "reason": "primary package source"}],
      "contextFiles": [{"path": "internal/security/security_test.go", "reason": "package tests"}],
      "tests": [{"path": "internal/security/security_test.go", "command": "go test ./internal/security"}],
      "tags": ["language:go", "project-root:."],
      "trustBoundaries": ["filesystem", "serialization"],
      "confidence": "high",
      "status": "pending"
    }
  ]
}
```

Mapper output must use repo-relative paths, must not follow symlinked directories, and must
skip dependency/build/cache/generated directories and likely secret files.

### `security-review-context-<slice-id>.json`

The context manifest records exactly which files and line ranges were included in the
bounded review prompt:

```json
{
  "schemaVersion": 1,
  "sliceId": "slice_...",
  "includedFiles": [
    {
      "path": "internal/security/security.go",
      "role": "owned",
      "bytes": 18200,
      "includedBytes": 18200,
      "includedLineRanges": [{"startLine": 1, "endLine": 420}],
      "truncated": false,
      "readable": true,
      "skippedReason": null
    }
  ],
  "omittedFiles": [
    {"path": "internal/security/large_fixture.json", "role": "context", "reason": "maxFiles"}
  ],
  "promptBytes": 41000,
  "approximateTokens": 10250
}
```

### `security-findings.v2.json`

Review output uses schema version 2 and must cite evidence from the review context manifest:

```json
{
  "schemaVersion": 2,
  "repository": {
    "repoURL": "https://github.com/example/app",
    "branch": "main",
    "subPath": "",
    "baseSHA": "",
    "headSHA": ""
  },
  "scan": {
    "mode": "initial",
    "sliceId": "slice_...",
    "summary": "Reviewed one bounded slice."
  },
  "findings": [
    {
      "title": "Untrusted archive path can escape extraction directory",
      "category": "path-traversal",
      "severity": "high",
      "confidence": "high",
      "triage": "confirmed-risk",
      "evidence": [
        {
          "path": "internal/archive/extract.go",
          "startLine": 42,
          "endLine": 58,
          "symbol": "Extract",
          "quote": null
        }
      ],
      "summary": "Archive entry names are joined without checking the resolved destination.",
      "rootCause": "The extraction code trusts archive-controlled paths.",
      "reproduction": "A tar entry named ../../tmp/pwn writes outside the destination.",
      "remediation": "Clean and resolve each destination path, then require it to remain under the extraction root.",
      "suggestedAction": "Generate a patch with a path containment check and regression test.",
      "whyTestsDoNotAlreadyCoverThis": "Existing extraction tests cover normal relative paths only.",
      "suggestedRegressionTest": "Add an archive entry with ../ and assert extraction fails.",
      "minimumFixScope": "Update extraction path resolution and add one focused test."
    }
  ]
}
```

Controller ingestion validates v2 findings independently. A valid finding is stored with an
Orka-owned fingerprint derived from namespace, repository scan, repo URL, branch, subPath,
slice ID, category, normalized title, and canonical sorted evidence refs. Invalid findings
are dropped individually and recorded in `security_dropped_findings` plus
`security-dropped-findings.json`.

Validation rejects missing required fields, empty evidence, unsafe/path-traversal evidence,
evidence files omitted from the context manifest, inverted or stale line ranges, line
ranges outside included manifest ranges, and quote mismatches when workspace content is
available for quote verification.

### `security-patch-<finding-id>.json`

Patch tasks must write a summary artifact that matches the actual structured workspace
diff:

```json
{
  "schemaVersion": 1,
  "findingId": "fnd_...",
  "summary": "Added archive path containment check.",
  "changedFiles": ["internal/archive/extract.go", "internal/archive/extract_test.go"],
  "testsRun": [{"command": "go test ./internal/archive", "exitCode": 0}],
  "risk": "low"
}
```

## Execution Model

The `RepositoryScan` controller (`internal/controller/repositoryscan_controller.go`):

- reconciles `RepositoryScan` resources;
- triggers an initial run when a repository is first created;
- triggers incremental runs on schedule and avoids overlapping runs for the same repository;
- watches completion of security-labeled scan and patch tasks;
- ingests artifacts into `SecurityStore`;
- updates `RepositoryScan.status`.

Security-created tasks carry labels so the reconciler can find and ingest them:
`orka.ai/security-target`, `orka.ai/security-scan-id`, `orka.ai/security-scan-mode`,
`orka.ai/security-stage`, `orka.ai/security-scope`, `orka.ai/security-slice-id`, and
`orka.ai/security-finding-id`.

### Scan task shape

```yaml
spec:
  type: agent
  agentRef:
    name: security-scanner
  prompt: "<generated prompt>"
  timeout: "2h"
  priority: 700
  agentRuntime:
    workspace:
      gitRepo: "https://github.com/org/repo.git"
      branch: "main"
      gitSecretRef:
        name: repo-git-creds
      subPath: "services/api"
```

### Scan logic

- **Initial**: scan newest commits backward, cap by `historyDays`, generate a first threat
  model artifact even with zero findings, run the deterministic mapper, persist review
  slices, then run selected review tasks. If the mapper produces no selected slices, the
  run completes with a no-op summary.
- **Incremental**: fetch the current head SHA and compare with `status.lastProcessedCommit`.
  If unchanged, mark the run succeeded with a no-op summary; if changed, focus the agent on
  commits after the last processed SHA while still using the current threat model as
  context and slice-aware changed-file selection.
- **Patch**: create a dedicated `type: agent` task with `pushBranch` set to
  `orka/security/<finding-id>` (using `forkRepo`/`prBaseBranch` when configured), prompt for
  a minimal reviewable fix, a diff artifact, and a patch summary artifact. A
  `PatchProposal` transitions to `succeeded` only after Orka confirms the task completed,
  branch metadata is present, the summary changed-file list matches the structured
  workspace result, and the diff artifact matches the actual workspace diff.

Prompt builders live under `internal/security/` (e.g. `internal/security/prompts.go` with
markdown templates) rather than inline in handlers. The scan prompt includes repo identity
and branch, scan mode, threat-model instructions, the required artifact contract, validation
guidance, `maxFindingsPerRun`, the current threat model when one exists, and commit-range
hints for incremental scans.

### Controller ingestion

When a labeled security **mapper** task completes, the controller parses
`security-slices.json`, upserts review slices, and records slice counts on the scan run.

When a labeled security **review** task completes, the controller loads the task result and
artifacts. It loads the matching `security-review-context-<slice-id>.json`, validates each
`security-findings.v2.json` finding against the manifest, upserts accepted findings by
Orka-owned stable fingerprint, records dropped diagnostics for rejected findings, and
updates accepted/dropped counts on the scan run.

When a labeled security **patch** task completes, the controller locates the associated
finding, parses the structured worker result, loads `security-patch-<finding-id>.diff` and
`security-patch-<finding-id>.json`, verifies both artifacts against actual workspace
changes, upserts the `PatchProposal`, and updates finding state to `patch_ready` only when
verification succeeds.

## Prompt Contracts

**Scanner agent** is instructed to: inspect current code and recent commits; generate or
update a concise threat model; produce a bounded number of findings; prefer high-confidence
findings over broad speculation; validate only when safe and practical; write structured
artifacts exactly as specified; and avoid editing or pushing code during scan runs.

**Patch agent** is instructed to: fix only one finding per task; keep the diff minimal;
preserve existing behavior unless the finding requires a change; run focused tests when
available; write `security-patch-<finding-id>.diff` and `security-patch-<finding-id>.json`;
and avoid creating a PR directly (PR creation is the API action).

## Reusing PR Plumbing

The security API reuses the shared GitHub helper code that backs the built-in PR tools
(`internal/tools/create_pull_request.go`, `review_pull_request.go`, `merge_pull_request.go`)
rather than duplicating GitHub API calls in handlers. `POST /api/v1/security/findings/:id/pull-request`
loads the latest successful `PatchProposal`, verifies it has a pushed branch, derives the PR
title/body from the finding title and remediation summary, opens the PR against
`RepositoryScan.spec.prBaseBranch` (or the scan branch), and updates the patch proposal and
finding rows.

## Metrics

The following repository-security Prometheus metrics are **planned but not yet registered**.
They do not exist in `internal/metrics/` today; treat them as a design target, not a series
you can scrape. (For metrics Orka actually exposes, see
[Configuration → Prometheus Metrics](../concepts/configuration.md#prometheus-metrics).)

- `orka_security_scan_runs_total{mode,status}`
- `orka_security_review_slices_total{status}`
- `orka_security_findings_ingested_total{schema_version,result}`
- `orka_security_findings_dropped_total{reason}`
- `orka_security_review_context_bytes`
- `orka_security_patch_verification_total{result,reason}`
- `orka_security_threat_model_updates_total{source}`

## Safety

- Workers run in isolated pods with the existing hardened defaults.
- Private repositories require an explicit `gitSecretRef` or detected credentials.
- PRs are never opened without an explicit user action.
- Artifact filenames stay flat and sanitized within the artifact upload model.
- Oversized evidence is truncated/summarized to stay below the 10 MB per-file and 50 MB
  total upload limits.
- Edited threat models are treated as ranking input, not executable instructions.
