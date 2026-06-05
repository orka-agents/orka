---
slug: /repository-security-scanning
---

# Repository Security Scanning

Repository security scanning gives Orka a Codex Security-like workflow: register a GitHub
repository, generate an editable threat model, scan history and new commits for likely
vulnerabilities, validate findings in an isolated worker, and remediate with generated
patches and pull requests — all human-in-the-loop.

The feature is GitHub-first and built on Orka's existing task, agent runtime, artifact,
scheduling, and PR plumbing. Remediation (patch generation and PR creation) always requires
an explicit user action.

- For the CRD field reference, see [Configuration → RepositoryScan](../concepts/configuration.md#repositoryscan).
- For the REST endpoints, see [API Reference → Security](../reference/api-reference.md#security).
- For the internal design and storage model, see [Repository Security Scanning Design](../development/security-scanning-design.md).

## How It Works

```text
Register repository (UI or RepositoryScan CRD)
  → initial scan: clone repo, generate threat model, scan history, store findings
  → scheduled incremental scans process only new commits
  → review threat model + findings in the dashboard
  → from a finding: generate a patch, then open a remediation PR
```

Each scan runs as a `type: agent` task with a git workspace. The agent writes structured
artifacts (threat model, findings JSON, validation evidence, patch diffs) that the
`RepositoryScan` controller ingests into the security store and surfaces through the
`/api/v1/security/*` API and the **Security** area of the dashboard.

## Quick Start (Dashboard)

1. Create or choose an Agent that can run repository security analysis (a `type: agent`
   runtime Agent with a git workspace, e.g. a Claude or Codex runtime agent).
2. Open the embedded dashboard and go to **Security** (`/security`).
3. Select **New Repository**, enter the GitHub repository URL, branch, optional `subPath`,
   scan schedule, validation mode, and analysis agent, then save.
4. Use **Scan Now** on the repository card or detail page to start a manual scan
   immediately, or rely on the configured cron schedule for incremental scans.
5. Review the generated threat model, scan runs, recommended findings, evidence, and
   validation status from the repository detail page.
6. From a finding detail page, optionally validate/reproduce the finding, generate a patch
   proposal, review the patch artifacts, and create a remediation pull request.

## Quick Start (GitOps / API)

You can drive the same workflow declaratively with the `RepositoryScan` CRD or the
`/api/v1/security/*` endpoints.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: RepositoryScan
metadata:
  name: example-repo
  namespace: default
spec:
  provider: github
  repoURL: "https://github.com/example/app"
  branch: main
  subPath: "services/api"        # optional monorepo scope
  gitSecretRef:                   # optional for private repositories
    name: github-credentials
  schedule: "0 2 * * *"          # optional cron for incremental scans
  validationMode: light           # off, light, or full
  analysisAgentRef:
    name: security-reviewer
  patchAgentRef:                  # optional; defaults to the analysis agent
    name: security-patcher
  maxFindingsPerRun: 25
```

See [Configuration → RepositoryScan](../concepts/configuration.md#repositoryscan) for every
spec and status field, and [API Reference → Security](../reference/api-reference.md#security)
for the endpoint and query-parameter reference, including the typical findings → validate →
patch → pull-request remediation flow.

## Scan Phases

| Phase | What happens |
|-------|--------------|
| **Initial scan** | Clones the repo, scans newest commits backward within `historyDays`, generates `security-threat-model.md`, writes `security-findings.json`, and stores a run summary. A threat model is generated even when no findings are emitted. |
| **Threat model review** | The repository detail page shows the generated threat model in an editor. Saving an edit (or a regenerated model) replaces the current threat model and influences ranking on later scans. Prior threat models are not retained as history. |
| **Incremental scans** | Run on the configured schedule and process only commits after the last completed run. Manual re-scan stays available. |
| **Patch generation** | From a finding, Orka creates a dedicated patch task that writes a diff artifact and stores patch metadata. |
| **PR creation** | Orka uses the latest successful patch proposal to open a PR against the configured base branch. |

## Validation Modes

| Mode | Behavior |
|------|----------|
| `off` | Findings are reported without validation. |
| `light` | Default. Validates likely findings when safe and practical. |
| `full` | More aggressive validation/reproduction, including builds where useful. |

Scanning never requires a buildable repository; validation may build when useful.

## Safety

- Scan and patch tasks run in isolated worker pods with Orka's hardened defaults (non-root,
  read-only rootfs, dropped capabilities).
- Private repositories require an explicit `gitSecretRef` (or detected credentials).
- Patches and PRs are never created automatically — both are explicit user actions.
- Edited threat models are treated as ranking input, not executable instructions.
- Evidence is stored as flat, sanitized artifacts within the per-file (10 MB) and total
  (50 MB) artifact upload limits.

## See Also

- [Repository Security Scanning Design](../development/security-scanning-design.md) — CRD,
  storage schema, controller ingestion, artifact contract, and prompt contracts.
- [Configuration → RepositoryScan](../concepts/configuration.md#repositoryscan)
- [API Reference → Security](../reference/api-reference.md#security)
