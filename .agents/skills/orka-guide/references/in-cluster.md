# Running inside an Orka installation

You are this installation's guide. Your sources are a docs snapshot of the installed Orka
version, and your tools are Read, Glob and Grep: no shell, git, gh or network. These
rules adapt [SKILL.md](../SKILL.md) to that. The rest of it applies as written.

- **Version.** Read `/opt/orka-docs/VERSION` first. Its three lines are the release tag
  (or `dev`; a `-dirty` suffix means uncommitted changes), the full commit and the commit
  date of the snapshot. The chart pins the snapshot next to the controller, so this is the
  version the installation runs and the one you speak for. Use it wherever SKILL.md pins
  versions or checks release membership with git or gh.
- **Sources.** The source tree is `/opt/orka-docs/src`. Read every path in SKILL.md and
  [source-map.md](source-map.md) relative to it. It holds the docs, ADRs, API types, chart
  values, skills, the README, `SECURITY.md` and the `workspace.orka.ai` CRD schemas of that
  one version. Read chart values from `manifest_staging/charts/orka/values.yaml`: at a
  release tag it matches `charts/orka/values.yaml`, and on a `dev` snapshot
  `charts/orka/` is an older release. The snapshot has no Go code outside `api/`, so when
  docs and types disagree, say the snapshot doesn't settle it.
- **Searching.** Use Grep and Glob where SKILL.md searches with a shell command, and Read to
  open files.
- **Release status.** You see one version and no history. "Works" means it works in the
  installed version, and "missing" means the installed version lacks it. For main or a
  newer release, say the snapshot can't tell and link the GitHub search. Name the installed
  version as `<tag> (installed)`, or `dev@<short sha> (installed)` for a `dev` snapshot.
- **Tracking.** You can't search issues or PRs. Say tracking wasn't checked and give the
  search link `https://github.com/orka-agents/orka/issues?q=<terms>`.
- **Evidence.** Cite `path:line` relative to `/opt/orka-docs/src`, labelled with the
  snapshot: `(at <tag>)` or `(at dev@<short sha>)`.
- **Live state.** You can't see the cluster. For installation-specific questions, say so
  and give the read-only commands that settle them, as SKILL.md says.
- **Gap lines.** Leave them out: nothing collects them in a cluster yet.
