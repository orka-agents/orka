# Orka hackathon first pass

A 118-second introduction to Orka as an execution and governance layer for AI
agents. Reliability discovers a source-code vulnerability, a person chooses the
work in Teams, and Engineering prepares a pull request. Security is the example;
the product story is agent teams doing work under explicit permissions.

This directory is independent of demos 01–05. It reuses their terminal helper
without changing it. Generated media, private credentials, and captured evidence
belong under the gitignored `bin/hackathon-first-pass/` directory.

The first pass is rendered in DaVinci Resolve with Qwen3 narration. The MP4 and
editable `.drp` are in `bin/hackathon-first-pass/resolve/`, named
`orka-hackathon-first-pass`. The actual workflow produced
[PR #36](https://github.com/sozercan/nodejs-goof/pull/36), verified at commit
`d07c0c8648249caeee857cda3248ab846f31e12d`. Its three focused tests passed; no CI
checks were attached at recording time. The PR remains open for review.

## Edit

| Time | Picture | Point |
| --- | --- | --- |
| 00:00–00:10 | Intro card | What Orka does |
| 00:10–00:24 | CLI schedule and scan records | Work starts on a real scheduled tick |
| 00:24–00:41 | CLI finding and source evidence | Reliability produces an actionable finding |
| 00:41–00:57 | Orka Bot in Teams | A person asks what needs attention |
| 00:57–01:00 | Teams fix request | The person chooses the work |
| 01:00–01:07 | CLI handoff receipt | Engineering receives the finding |
| 01:07–01:20 | CLI coding result | The agent prepares the patch and runs checks |
| 01:20–01:34 | CLI delivery receipt and PR | Publication is independently recorded |
| 01:34–01:41 | CLI usage | Reported usage and missing measurements are visible |
| 01:41–01:51 | Gateway card | Other channels can connect through adapters |
| 01:51–01:58 | Outro card | Run agent teams. Stay in control. |

The scene manifest is [resolve/manifest.json](resolve/manifest.json). Narration
and timing live in [audio](audio/README.md). Terminal capture and title-card
generation are documented in [terminal](terminal/README.md) and
[media](media/README.md).

## Workflow and evidence

The target is the existing `sertac-aks` cluster. Reliability uses `orka-system`;
Engineering uses `orka-pr647-system`. `cluster.py` verifies the exact cluster API
endpoint and refuses to replace resources without this demo's ownership label.
It creates task-owned identities, private kubeconfigs, agents, and distinct Git
credential references. It does not rebuild or replace the installations.

`cluster.py schedule` creates a scheduled container Task. Its next cron tick
creates a RepositoryScan against a pinned commit of `sozercan/nodejs-goof`.
`cluster.py suspend` stops subsequent ticks after evidence has been captured.
The initial scan is not started by a Teams prompt.

The first recorded run is `scan_tzxgyrjjmedksoynf7ue6bjsuz`, created by scheduled
Task `hackathon-scheduled-scan-1789878840`. It reviewed 14 source areas and
retained three findings. This edit selects `fnd_0afb30da8140`, shell command
injection in `routes/index.js:168`, from commit
`bdb1aa6e8a03cb795e0012d3b6a2c80234243d31`.

`workflow.py prepare-teams` supplies that saved finding to a concise guide agent
and adds a binding for the already allowed sender and conversation. The guide
does not have live scanner access or a tool for starting Engineering. The
original binding remains available.

`workflow.py bridge` watches for an admitted event from that exact binding,
sender, account, conversation, and guide. Only this message starts Engineering:

> Ask Engineering to fix this and open a pull request.

The bridge creates `hackathon-fix-0afb30da8140` in the Engineering installation
and records the source event and selected finding. The coding agent receives
a write-intent workspace with a small allowed path set. The separate Orka
Publisher receives the publication credentials and handles the branch and PR.

The CLI scenes read live records. Their preconditions reject missing handoffs,
unfinished tasks, unverified publication, mismatched PRs, and absent usage
measurements. `view.py` formats selected fields and labels excerpts; it does not
invent command output.

## Teams capture

Send messages **only to Orka Bot**. Verify the conversation title immediately
before every send. Do not use a channel, another chat, or a general Teams search
box for these messages.

Record the actual conversation with macOS recording controls. Ask what needs
attention, wait for the real reply, then send the exact fix request above while
the bridge is running. Keep the raw capture under
`bin/hackathon-first-pass/teams/`. Normalize selected footage to silent
1920×1080, 30 fps clips named `03-teams.mp4` and `04-request.mp4`, lasting 16 and
3 seconds respectively. Exclude unrelated chats, notifications, and desktop
audio. Preserve the original capture so the edit can be checked.

Use DaVinci Resolve for the final scene assembly, narration placement, project
export, and MP4 render. [The Resolve instructions](resolve/README.md) describe
the installed App Store edition and its embedded Lua console. The planned edit
is 3,540 frames. Verify the exported file is below 120 seconds and watch it for
readability and narration alignment before delivery.

## What this demonstrates

- The vulnerability is in an intentionally vulnerable public fixture. This is
  source-based discovery, not a claim of a newly discovered zero-day.
- The retained finding has source and attack-path validation. The saved evidence
  does not establish an executed proof of exploitation.
- Cross-installation handoff uses the explicit demo bridge. It is not native
  cross-installation delegation or an Orka approval-and-resume mechanism.
- The person selects a fix; no merge or production deployment is automatic.
- Separate Git credential references use the presenter's existing identity in
  this setup. The demo does not establish four independently scoped identities.
- The usage scene shows one reported Teams request. ACP scan measurements and
  pricing may be unavailable; those states remain visible. It does not present
  an estimated cost as a measured bill or combine the installations' totals.
- Teams and Telegram have adapters. Slack is shown as an adapter possibility,
  not as a shipped integration.

Before reusing the environment, inspect the existing task-owned resources and
refresh only expired short-lived credentials. Do not rerun setup over recorded
history or replace the saved evidence with another scan. Never commit tokens,
private state, media binaries, or the voice reference.
