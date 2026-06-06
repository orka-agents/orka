# Repository Security Scanning Implementation Plan

## Purpose

Strengthen Orka's repository security scanning by making scan scope explicit, findings evidence-backed, incremental scans cheaper, and patch proposals mechanically verifiable before pull requests are created.

This plan keeps Orka's existing architecture:

- `RepositoryScan` remains the user-facing scan configuration.
- Scan, validation, and patch work continue to run as Kubernetes-backed `Task` resources.
- Dynamic security data remains in SQLite.
- Detailed outputs continue to flow through task artifacts.
- Credentials, transaction metadata, and authorization remain governed by existing Orka worker, Secret, RBAC, OIDC, and context-token controls.

## Target Outcomes

- Every stored finding has verifiable evidence tied to repository files or explicit artifacts.
- Invalid or speculative model output is rejected or quarantined before it pollutes the finding store.
- Initial scans cover the repository through bounded review slices instead of unbounded prompts.
- Incremental scans review only slices affected by changed files when possible.
- Patch proposals are marked ready only when the recorded diff matches actual workspace changes.
- Repository security scanning uses the v2 artifact contracts directly.

## Non-Goals

- Do not replace Orka's Kubernetes execution model with a local CLI workflow.
- Do not move durable security state into repo-local files.
- Do not create automatic patch, commit, push, or PR behavior beyond Orka's explicit existing user actions.
- Do not require every repository to be buildable before scanning.
- Do not support pre-v2 findings artifacts.
- Do not add new long-lived credentials or expose raw tokens in artifacts, logs, specs, statuses, or UI responses.

## Current Baseline

Orka already has:

- `RepositoryScan` CRD for GitHub-first repository scanning.
- Threat-model, discovery, validation, and patch stages.
- SQLite-backed scan runs, threat models, findings, and patch proposals.
- Artifact contracts for threat models, findings, validation output, and patch diffs.
- Human-in-the-loop patch generation and PR creation.
- Worker isolation through non-root pods, read-only root filesystem, dropped capabilities, seccomp, optional runtime class, and writable `/tmp`, `/home/worker`, and `/workspace`.

Key gaps to close:

- Findings can be accepted with weak or non-verifiable evidence.
- Scan prompts are scoped by broad discovery lenses, not deterministic repository structure.
- Incremental scans are commit-aware but not review-slice-aware.
- Patch readiness is currently more prompt-driven than mechanically verified.
- Finding history and user decisions are preserved only through selected fields, not a full event trail.

## Architecture Overview

```text
RepositoryScan
  |
  v
mapper task or mapper phase
  |
  +--> security-slices.json
  +--> review slice rows in SQLite
  |
  v
review tasks per slice or bounded slice batch
  |
  +--> security-review-context-<slice-id>.json
  +--> security-findings.v2.json
  +--> security-dropped-findings.json
  |
  v
controller ingestion
  |
  +--> validate context and evidence
  +--> upsert accepted findings
  +--> store dropped finding diagnostics
  |
  v
validation tasks for high-value findings
  |
  +--> security-validation.json
  +--> optional transcript/evidence artifacts
  |
  v
patch tasks for user-selected findings
  |
  +--> workspace changes
  +--> security-patch-<finding-id>.diff
  +--> security-patch-<finding-id>.json
  |
  v
patch verifier
  |
  +--> patch_ready only when actual diff matches recorded diff
```

## Phase 1: Data Contracts

### Work

Define versioned internal contracts before changing execution flow.

Add documented JSON contracts for:

- `security-slices.json`
- `security-review-context-<slice-id>.json`
- `security-findings.v2.json`
- `security-dropped-findings.json`
- `security-patch-<finding-id>.json`

Recommended `ReviewSlice` shape:

```json
{
  "schemaVersion": 1,
  "id": "slice_...",
  "repositoryScan": "example",
  "source": "deterministic-go-package",
  "title": "Go package internal/security",
  "summary": "Security artifact parsing and prompt contracts.",
  "kind": "package|route|workflow|service|library|config|test-suite|unknown",
  "entrypoints": [
    {"path": "internal/security/security.go", "symbol": null, "route": null, "command": null}
  ],
  "ownedFiles": [
    {"path": "internal/security/security.go", "reason": "primary package source"}
  ],
  "contextFiles": [
    {"path": "internal/security/security_test.go", "reason": "package tests"}
  ],
  "tests": [
    {"path": "internal/security/security_test.go", "command": "go test ./internal/security"}
  ],
  "tags": ["language:go", "project-root:."],
  "trustBoundaries": ["filesystem", "serialization"],
  "confidence": "high",
  "status": "pending"
}
```

Recommended `ReviewContextManifest` shape:

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
    {"path": "internal/security/large_fixture.json", "role": "context", "reason": "maxContextFiles"}
  ],
  "promptBytes": 41000,
  "approximateTokens": 10250
}
```

Recommended `security-findings.v2.json` shape:

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

### Success Criteria

- Contracts are added to `website/docs/development/security-scanning-design.md`.
- Contracts use `schemaVersion` and are forward-extensible.
- `security-findings.v2.json` is the only findings ingestion contract.
- No raw secrets, raw tokens, or full sensitive request contexts are allowed in new contracts.
- Unit tests parse valid examples and reject malformed examples.

## Phase 2: SQLite Store Extensions

### Work

Add durable review slice and dropped-output storage.

New table:

```sql
CREATE TABLE IF NOT EXISTS security_review_slices (
  id                TEXT PRIMARY KEY,
  namespace         TEXT NOT NULL,
  repository_scan   TEXT NOT NULL,
  source            TEXT NOT NULL,
  title             TEXT NOT NULL,
  summary           TEXT NOT NULL DEFAULT '',
  kind              TEXT NOT NULL DEFAULT 'unknown',
  confidence        TEXT NOT NULL DEFAULT 'medium',
  status            TEXT NOT NULL DEFAULT 'pending',
  entrypoints_json  TEXT NOT NULL DEFAULT '[]',
  owned_files_json  TEXT NOT NULL DEFAULT '[]',
  context_files_json TEXT NOT NULL DEFAULT '[]',
  tests_json        TEXT NOT NULL DEFAULT '[]',
  tags_json         TEXT NOT NULL DEFAULT '[]',
  trust_boundaries_json TEXT NOT NULL DEFAULT '[]',
  last_scan_run_id  TEXT NOT NULL DEFAULT '',
  last_reviewed_at  TIMESTAMP,
  created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  updated_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(namespace, repository_scan, id)
);

CREATE INDEX IF NOT EXISTS idx_security_review_slices_repo
  ON security_review_slices(namespace, repository_scan, status, updated_at DESC);
```

Optional table:

```sql
CREATE TABLE IF NOT EXISTS security_dropped_findings (
  id                TEXT PRIMARY KEY,
  namespace         TEXT NOT NULL,
  repository_scan   TEXT NOT NULL,
  scan_run_id       TEXT NOT NULL,
  task_name         TEXT NOT NULL,
  slice_id          TEXT NOT NULL DEFAULT '',
  reason            TEXT NOT NULL,
  sample_json       TEXT NOT NULL DEFAULT '',
  created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

Store interface additions:

- `UpsertReviewSlice(ctx, slice)`
- `ListReviewSlices(ctx, filter)`
- `GetReviewSlice(ctx, namespace, repositoryScan, id)`
- `UpdateReviewSliceStatus(ctx, namespace, repositoryScan, id, status)`
- `CreateDroppedFinding(ctx, dropped)`
- `ListDroppedFindings(ctx, filter)`

### Success Criteria

- Migration is idempotent.
- Store tests cover insert, update, list, filtering, JSON round trip, and namespace isolation.
- Fresh databases initialize all scan, slice, dropped-output, finding, patch, and validation tables.
- Scan flow handles empty slice tables by running the mapper before review.

## Phase 3: Deterministic Review Slice Mapper

### Work

Implement a deterministic mapper package under `internal/security/slices` or similar.

Initial language/framework support:

- Go:
  - Prefer `go list ./...` when available and safe.
  - Fallback to package directories containing `.go` files.
  - Map package tests and command packages.
- Node/TypeScript:
  - Detect root and workspace `package.json`.
  - Map package scripts for `build`, `test`, `lint`, `typecheck`.
  - Detect `apps/*`, `packages/*`, `services/*`.
  - Detect Next `app/` and `pages/` routes where local evidence exists.
  - Detect server/API directories and common route files.
- Python:
  - Detect `pyproject.toml`, `setup.cfg`, `setup.py`, `requirements.txt`.
  - Map source roots and tests.
  - Detect Flask/FastAPI/Django routes conservatively.
- Shell and workflows:
  - Map `.github/workflows/*`, `scripts/*`, Dockerfiles, release files, Makefiles.
  - Treat shell/YAML run blocks as process-execution surfaces.
- Generic fallback:
  - Bounded source groups by directory for recognized code extensions.

Mapper safety rules:

- Do not execute arbitrary project scripts.
- Do not follow symlinked directories.
- Skip dependency, generated, build, and cache directories.
- Validate every path is relative to the repository checkout and inside the workspace.
- Do not include likely secret files as owned review files.
- Preserve `subPath` scoping.

### Success Criteria

- A mapper task can generate `security-slices.json` without model access.
- Orka's own repository maps to useful Go, UI, workflow, script, and config slices.
- Fixture tests cover Go, Node/TS monorepo, Python service, workflows, and generic fallback.
- Mapper output is stable across repeated runs on unchanged input.
- Mapper output never includes paths outside the repository or under excluded directories.

## Phase 4: Scan Pipeline Integration

### Work

Integrate slices into the repository scan lifecycle.

Recommended sequence:

1. Threat model stage runs first as today.
2. Mapper stage produces and ingests review slices.
3. Review tasks run for selected slices.

Task labeling:

- `orka.ai/security-stage=mapper`
- `orka.ai/security-stage=review`
- `orka.ai/security-slice-id=<sliceID>`

New artifacts:

- `security-slices.json`
- `security-review-context-<sliceID>.json`
- `security-findings.v2.json`
- `security-dropped-findings.json`

### Success Criteria

- Initial scan creates review slices before slice review tasks.
- If mapper fails, the scan reports mapper failure clearly.
- If mapper produces no selected slices, the scan completes with a no-op summary.
- Controller can ingest slice review tasks independently and update scan status.
- Scan run summary includes slice counts and accepted/dropped finding counts.

## Phase 5: Bounded Context Builder

### Work

Build bounded review prompts from slice data.

Context rules:

- Include owned files first.
- Include context files next.
- Include tests as first-class evidence.
- Cap file count and bytes.
- Number file lines in prompt excerpts.
- Record included line ranges in the context manifest.
- Record omitted files with reasons.
- Include the exact valid evidence path list in the prompt.

Prompt rules:

- Prefer a small number of high-signal findings.
- Require structured evidence from valid paths.
- Require line ranges or quotes.
- Require test coverage analysis.
- Require minimum fix scope.
- Ask for deduplication of sibling/root-cause issues.

### Success Criteria

- Review prompts are bounded by configured max file count and byte budget.
- Context manifest matches the prompt content exactly.
- Findings citing omitted files are rejected by the validator.
- Tests verify truncated files can only be cited within included ranges.
- Prompt generation is deterministic for the same slice and file contents.

## Phase 6: Evidence Validation and Partitioned Ingestion

### Work

Add a validator for v2 finding artifacts.

Validation rules:

- `findings` must be an array.
- Each finding must have title, category, severity, confidence, summary, remediation, and evidence.
- Evidence array must not be empty.
- Evidence paths must be repo-relative and safe.
- Evidence paths must be present in the review context manifest.
- Evidence files must be readable inside the workspace or explicitly reference an artifact.
- Line ranges must include both start and end.
- Line ranges must not be inverted.
- Line ranges must be within included manifest ranges.
- Quotes, when present, must match file contents within the cited range or match after whitespace compaction.
- Invalid findings are dropped individually.

Dropped finding record:

```json
{
  "index": 2,
  "reason": "evidence file was not included in review context",
  "sample": {"title": "..."},
  "layer": "validation"
}
```

### Success Criteria

- A mixed artifact with valid and invalid findings stores valid findings and records invalid ones.
- Invalid evidence path traversal is rejected.
- Missing evidence is rejected.
- Stale line ranges are rejected.
- Quote mismatch is rejected.
- Dropped finding diagnostics are visible through artifacts and API.
- Scan does not fail solely because one finding is invalid unless all findings are invalid and no useful output remains.

## Phase 7: Stable Finding Identity and State Preservation

### Work

For v2 findings, compute Orka-owned fingerprints instead of trusting model-provided fingerprints.

Fingerprint inputs:

- Namespace
- Repository scan name
- Repo URL
- Effective branch
- SubPath
- Slice ID
- Category
- Normalized title
- Canonical sorted evidence refs

Canonical evidence ref:

```json
{
  "path": "internal/archive/extract.go",
  "startLine": 42,
  "endLine": 58,
  "symbol": "Extract",
  "quote": null
}
```

Preserve across rediscovery:

- User-visible state
- Validation status when already validated or pending
- Validation JSON
- Patch proposal ID
- PR number and URL
- Created timestamp
- Existing evidence refs, merged without duplicates

Optional enhancement:

- Add a finding event table for triage, validation, patch, PR, and rediscovery events.

### Success Criteria

- Reordered evidence produces the same finding ID.
- Repeated scan of unchanged issue preserves finding ID and user state.
- Dismissed, fixed, suppressed, and false-positive states are not overwritten by rediscovery.
- New materially different evidence creates a distinct finding.
- Tests cover rediscovery, state preservation, evidence merge, and validation preservation.

## Phase 8: Incremental Slice Selection

### Work

Use changed files to select review slices for incremental scans.

Algorithm:

1. Determine `baseCommit` and `headCommit`.
2. Compute changed files inside the scan `subPath`.
3. Select slices whose owned files changed.
4. Include slices whose context files changed if confidence is high enough or config enables context-triggered review.
5. If changed files cannot be computed, fall back to normal scheduled review behavior.
6. If no slices match, mark scan succeeded with a no-op summary.

### Success Criteria

- A one-file change reviews only affected slices.
- A test-only change can revalidate or review associated slices based on policy.
- A workflow-only change reviews workflow slices.
- Empty changed-file set produces no review tasks and no false failure.
- Scan run records reviewed slice count and skipped slice count.

## Phase 9: Validation Task Improvements

### Work

Feed structured v2 finding data and evidence context into validation tasks.

Validation prompt should include:

- Finding JSON.
- Primary evidence snippets.
- Context manifest summary.
- Relevant tests.
- Existing validation status and history if any.

Validation output should:

- Use `validated`, `failed`, or `skipped`.
- Include assumptions, controls, blindspots, likelihood, impact, and evidence refs.
- Never search for unrelated findings.

### Success Criteria

- Validation tasks focus on one finding.
- Validation can lower confidence or mark failed without deleting the original finding.
- Validation evidence is merged into the finding.
- Validation result is persisted and visible in UI/API.

## Phase 10: Patch Proposal Verification

### Work

Make patch readiness deterministic.

Before marking a patch proposal `succeeded` or finding state `patch_ready`:

- Confirm patch task completed successfully.
- Confirm result includes pushed branch information when applicable.
- Load declared patch summary artifact.
- Load declared diff artifact.
- Compute actual workspace diff from the task output path or structured result if available.
- Compare actual changed files to declared changed files.
- Verify diff artifact corresponds to actual changes.
- Reject unexpected changed files unless an explicit force path exists.

Patch summary artifact shape:

```json
{
  "schemaVersion": 1,
  "findingId": "fnd_...",
  "summary": "Added archive path containment check.",
  "changedFiles": ["internal/archive/extract.go", "internal/archive/extract_test.go"],
  "testsRun": [
    {"command": "go test ./internal/archive", "exitCode": 0}
  ],
  "risk": "low"
}
```

### Success Criteria

- Patch task that edits undeclared files is not marked ready.
- Missing diff artifact prevents `patch_ready`.
- Stale or mismatched diff artifact prevents `patch_ready`.
- Successful patch proposal records exact changed files, tests run, branch, and artifacts.
- PR creation can only use the latest successful verified patch proposal.

## Phase 11: API and UI

### Work

Expose new scan details.

API additions:

- List review slices for a repository scan.
- Get review slice detail.
- List dropped findings by scan run.
- Return structured v2 evidence on finding detail.
- Return accepted/dropped counts on scan run summaries.

UI additions:

- Repository detail:
  - slice count
  - reviewed slice count
  - last reviewed time
  - accepted/dropped finding counts
- Finding detail:
  - structured evidence with file and line
  - category and triage
  - test analysis
  - suggested regression test
  - minimum fix scope
  - validation and patch history
- Scan run detail:
  - per-stage status
  - slice review status
  - dropped finding diagnostics

### Success Criteria

- User can inspect why a finding was accepted.
- User can inspect why model output was dropped.
- User can filter by slice, category, state, severity, validation status.
- Repository and finding pages show v2 evidence and dropped-output diagnostics.

## Phase 12: Configuration

### Work

Add conservative knobs without making configuration mandatory.

Possible `RepositoryScanSpec` additions, only if needed:

- `sliceMode`: `off|deterministic|auto`
- `maxSlicesPerRun`
- `maxFilesPerSlice`
- `maxContextFiles`
- `maxPromptBytes`
- `incrementalSliceSelection`: `owned|ownedAndContext`
- `evidenceValidationMode`: `audit|enforce`

Prefer controller defaults and SQLite/internal config before CRD fields unless users need per-repository control.

### Success Criteria

- Defaults improve quality without new required config.
- Documentation explains each knob and its production tradeoff.

## Implementation Stages

### Stage A: Passive Contracts

- Add stores and parsers.
- Add tests and docs.

Success:

- Contract and store tests pass.
- Scan tests cover the mapper-first pipeline.

### Stage B: Mapper Enabled, Review Unchanged

- Generate and ingest slices during scans.
- Persist slice rows for API/UI inspection.

Success:

- UI/API can show slices.
- Mapper output is stable and scoped to the repository workspace.

### Stage C: v2 Ingestion Enabled in Audit Mode

- Accept v2 artifacts from new prompts.
- Validate and record dropped findings.

Success:

- Invalid v2 findings are observable but do not break scans.
- Valid v2 findings are stored.

### Stage D: Slice-Based Review Enabled

- Review selected slices.

Success:

- Initial scans produce slice-backed findings.
- Incremental scans run fewer tasks.

### Stage E: Evidence Enforcement

- Treat invalid evidence as rejected finding output.
- Keep dropped diagnostics.

Success:

- Stored findings are evidence-backed.
- No invalid evidence reaches the finding store.

### Stage F: Patch Verification Enforcement

- Require patch verification before `patch_ready`.

Success:

- PR creation never uses an unverified patch proposal.

## Testing Plan

### Unit Tests

- JSON contract parsing and validation.
- Mapper path safety.
- Mapper fixture coverage.
- Review context manifest generation.
- Evidence validator accepted/dropped partitioning.
- Fingerprint canonicalization.
- Store migrations and JSON round trips.
- Patch summary/diff verification.

### Controller Tests

- Mapper task ingestion.
- Slice review task creation.
- v2 finding ingestion.
- Mixed valid/invalid finding artifact ingestion.
- Dropped finding persistence.
- Rediscovery state preservation.
- Incremental slice selection.
- Patch task verification outcomes.

### E2E Tests

- Initial scan with deterministic slices.
- Incremental scan with one changed file.
- Invalid evidence artifact is dropped and visible.
- Valid high-severity finding triggers validation according to mode.
- Patch proposal with matching diff becomes ready.
- Patch proposal with unexpected changed files stays failed/open.

### Manual Verification

- Run a scan against Orka itself.
- Confirm slices are meaningful.
- Confirm high-signal findings include verifiable evidence.
- Confirm UI can inspect evidence and dropped output.
- Confirm no raw secrets or tokens appear in artifacts, logs, or API responses.

## Operational Metrics

Add or extend metrics:

- `orka_security_review_slices_total{namespace,repository_scan,status}` with low-cardinality labels only if repository labels are acceptable in the deployment; otherwise avoid repository labels.
- `orka_security_findings_ingested_total{schema_version,result}`
- `orka_security_findings_dropped_total{reason}`
- `orka_security_review_context_bytes`
- `orka_security_patch_verification_total{result,reason}`

Avoid high-cardinality labels such as raw repo URL, finding ID, file path, branch, subject, token, or transaction ID.

## Documentation Updates

Update:

- `website/docs/guides/repository-security-scanning.md`
- `website/docs/development/security-scanning-design.md`
- `website/docs/reference/api-reference.md`
- `website/docs/development/testing.md`

Document:

- Slice lifecycle.
- v2 artifact contracts.
- Evidence validation.
- Dropped finding diagnostics.
- Incremental scan behavior.
- Patch verification behavior.
- Configuration flags.

## Risks and Mitigations

### Risk: Mapper misses important code paths

Mitigation:

- Track source coverage.
- Add generic fallback slices.
- Allow manual re-scan and future user-tuned slice settings.

### Risk: Evidence validator drops true positives

Mitigation:

- Start in audit mode.
- Store dropped diagnostics.
- Permit artifact-backed evidence for validation outputs.
- Tune prompts and validator together.

### Risk: Too many slice tasks increase Kubernetes load

Mitigation:

- Batch small slices.
- Cap `maxSlicesPerRun`.
- Prioritize high-risk/trust-boundary slices.
- Use incremental selection for scheduled scans.

### Risk: Fingerprint changes create duplicate findings

Mitigation:

- Version fingerprinting.
- Merge findings with matching evidence-derived fingerprints.

### Risk: Patch verification lacks access to actual workspace diff

Mitigation:

- Require worker structured result to include changed files and diff digest.
- Add a worker-side diff artifact generated after agent execution.
- Treat missing verification data as not ready.

## Implementation Order

1. Add docs for v2 contracts and rollout states.
2. Add store tables and store tests.
3. Add contract structs and parsers.
4. Add evidence validator with fixture tests.
5. Add mapper package with Go/workflow/generic support.
6. Add mapper ingestion and slice store updates.
7. Add context manifest builder.
8. Add v2 finding ingestion in audit mode.
9. Add deterministic fingerprinting and state preservation tests.
10. Enable slice review tasks behind default-off flag.
11. Add incremental slice selection.
12. Add patch summary/diff verification.
13. Add API and UI surfaces.
14. Enable evidence enforcement by default after tests and manual scan pass.

## Definition of Done

- `make lint-fix && make test` passes.
- `make manifests generate` passes if any CRD type or marker changes are made.
- UI lint and tests pass if UI surfaces are changed.
- Repository scan E2E covers initial scan, incremental scan, invalid evidence, validation, and patch verification.
- Documentation describes the v2 artifact contracts.
- No raw secrets, credentials, tokens, raw transaction tokens, or sensitive contexts appear in persisted artifacts, Task specs/statuses, logs, metrics, or UI responses.
- Stored v2 findings have validated evidence or are not stored.
- Patch proposals cannot reach `patch_ready` without deterministic verification.
