# Orka platform overview

A 118-second overview for viewers who have never used Orka. The opening walks
through agents, coordination, tools and skills, permissions, reviewed knowledge,
and task results and usage. A security workflow illustrates these capabilities:
a scheduled source-code scan, a human request in Microsoft Teams, and an
engineering agent's patch and pull request.

Orka opens the video. The security-review example appears on "Two teams. One
workflow." A progress line follows Scan, Finding, Human request, Fix, and Review.
The closing diagram shows five gateway inputs and four distinct agent options,
with shared Teams chats clearly marked as coming soon for a multiplayer experience.
The final card returns to the wider platform: "One example. Many ways to work."

## Delivered edit

- Video: `bin/hackathon-platform-overview/resolve/orka-hackathon-platform-overview.mp4`
- Resolve export: `bin/hackathon-platform-overview/resolve/orka-hackathon-platform-overview.drp`
- Project: `Orka Hackathon Platform Overview 20260920-212851`
- Timeline: `Your agents, working together - Overview`

The delivered project is a separate import of the verified
`Orka Hackathon Platform Introduction 20260920-205705` export. Its new timeline
and media use independent paths. The original
`Orka Hackathon Platform 20260920-162246`, both previous revisions, and their
exports are retained. Preservation hashes accompany
the new export in `bin/hackathon-platform-overview/preservation.json`.

The timeline is 3,540 frames at 1920×1080 and 30 fps. Resolve renders the final
H.264/AAC MP4. The container stays below two minutes, including audio padding.
All generated media and private evidence stay in gitignored `bin/` directories.

| Time | Picture | What the viewer learns |
| --- | --- | --- |
| 00:00–00:18 | Orka feature overview | Agents, coordination, tools and skills, permissions, reviewed knowledge, results and usage |
| 00:18–00:29.5 | Two teams. One workflow. | A scheduled source-code scan leads to a fix requested from Teams |
| 00:29.5–00:34.5 | Enlarged schedule evidence | Orka starts scheduled work |
| 00:34.5–00:42.5 | Enlarged source finding | An image URL can become a server command |
| 00:42.5–00:51 | Actual Teams conversation | A person reviews the finding and chooses the next action |
| 00:51–00:54 | Actual Teams request | The person requests the fix |
| 00:54–00:58.5 | Team handoff | A custom demo integration passes work under Engineering's permissions |
| 00:58.5–01:10.5 | Actual GitHub diff and retained test report | The agent proposes a code change and reports three passing focused tests |
| 01:10.5–01:17.5 | Actual GitHub pull request | The patch and test results are ready for human review |
| 01:17.5–01:24.5 | Usage command and output | The CLI query and one task's reported tokens, with missing measurements explicit |
| 01:24.5–01:46.5 | Channels and agents | Five gateway inputs, four agent options, and shared Teams chats coming soon |
| 01:46.5–01:58 | Platform and docs | One example of the workflows teams can build with Orka |

[manifest.json](manifest.json) defines the edit. [narration.json](narration.json)
contains the spoken text, and [timing.json](timing.json) places the voice clips.
The handoff voice segment spans the request and handoff shots. The patch shot
contains six seconds of the actual diff followed by six seconds of the test report.

## Gateway and agent diagram

Gateway inputs appear separately on the left, in this order:

1. Microsoft Teams
2. Microsoft Scout
3. Telegram
4. Slack
5. Bring-your-own gateway

The Teams entry marks planned support for shared chats as "Coming soon" and
explains the multiplayer experience as working with Orka together in the same
conversation. The recording uses a one-to-one Orka Bot conversation.

The diagram identifies Slack as a custom gateway. Teams and Telegram have
[Teams](https://github.com/orka-agents/orka-gateway-teams) and
[Telegram](https://github.com/orka-agents/orka-gateway-telegram) adapters. The
[Scout adapter](https://github.com/orka-agents/orka-gateway-scout) requires
compatible Scout builds; its README documents the supported local-login profile
and remaining limitations. The film does not imply support for every Scout release.
The [gateway protocol](../../website/docs/reference/gateway-api.md) provides the
contract for custom applications and adapters.

Agent options appear separately on the right:

- Native agents running in Kubernetes, optionally with
  [Agent Sandbox](../../website/docs/concepts/agent-sandbox.md) and
  [Agent Substrate](../../website/docs/concepts/substrate.md).
- Local agents running in containers on your laptop.
- Foundry-hosted agents running in Microsoft Foundry, through the
  [Hosted Agents adapter](https://github.com/orka-agents/agent-runtime-foundry).
- Bring-your-own agent through a compatible runtime.

Local, Foundry-hosted, and custom agents remain subject to
[compatible runtime registration](../../website/docs/guides/bring-your-own-agent-runtime.md).
The diagram explains connection options. The recorded workflow does not exercise
all of these integrations.

## Evidence and scope

This edit reuses the retained workflow from [demo 06](../06-hackathon/README.md).
The scan and Engineering task were not restarted, and no Teams messages were sent.
The scheduled parent remains suspended after the recorded scan.

Security is the display name for the existing `reliability` identity in
`orka-system`. Engineering uses `orka-pr647-system`. Both are on `sertac-aks`.
These labels do not rename either installation or change its permissions.

The scheduled child `hackathon-scheduled-scan-1789878840` started scan
`scan_tzxgyrjjmedksoynf7ue6bjsuz` against `sozercan/nodejs-goof` at
`bdb1aa6e8a03cb795e0012d3b6a2c80234243d31`. It reviewed 14 source areas and
retained three findings. The film selects `fnd_0afb30da8140`, shell command
injection in `routes/index.js:168`. This intentionally vulnerable fixture
demonstrates source-based discovery; it does not establish a new real-world
zero-day or an executed exploit.

Orka Bot receives the saved finding. A custom demo bridge observes the allowed
Teams request and creates Engineering task `hackathon-fix-0afb30da8140` under
that installation's permissions. The film names the custom integration.

The task produced [PR #36](https://github.com/sozercan/nodejs-goof/pull/36) at
`d07c0c8648249caeee857cda3248ab846f31e12d`. The PR remains open at capture time.
The actual browser captures show its description and the change in
`routes/index.js`. The task report records three focused tests. Full application
and database flows were not tested, and the PR has no attached CI checks.
Separate publication keeps Git credentials outside the coding agent.

Usage shows the retained `orka usage other unassociated` command and its `jq`
selection together with the result: one measured Teams request with 2,018 input
and 128 output tokens.
The scan's token measurements and model pricing are unavailable. The film does
not claim complete billing or a total across installations.

[story.py](story.py) crops and scales the retained terminal and Teams clips.
Selected intervals play at their retained speed, then hold the last frame when
needed. GitHub images are screenshots of the real page held as stills. Captions
and the progress indicator appear outside the evidence crop. The script does not
reconstruct messages, commands, code, results, or application UI. Source hashes,
crop bounds, selected intervals, and render commands are stored beside each clip.

## Reproduce the media

The following retained inputs must exist locally:

- `bin/hackathon-platform-revised/terminal/` and `teams/`, from the preceding cut.
- `bin/hackathon-platform-overview/evidence/pr-overview.png`, the actual PR overview.
- `bin/hackathon-platform-overview/evidence/pr-diff.png`, the actual changed-code view.

The GitHub crop coordinates are specific to the captured windows. When replacing
a capture, update the crop in `story.py` and inspect the output. Capture only the
existing PR; these steps require no comments, reviews, merges, or live task runs.

Use the existing Pillow environment and local narration service described in
[the voice instructions](../06-hackathon/audio/README.md). The reference WAV
remains read-only. From the repository root:

```sh
bin/hackathon-first-pass/media/.venv/bin/python demo/07-hackathon-platform/cards.py
bin/hackathon-first-pass/media/.venv/bin/python demo/07-hackathon-platform/story.py
python3 demo/06-hackathon/audio/generate_voiceover.py \
  --manifest demo/07-hackathon-platform/narration.json \
  --output-dir bin/hackathon-platform-overview/audio
python3 demo/06-hackathon/audio/assemble_voiceover.py \
  --timing demo/07-hackathon-platform/timing.json \
  --audio-dir bin/hackathon-platform-overview/audio
```

The narration stays at its generated speed. The assembler rejects speech that
overruns its scene. The master is 48 kHz mono PCM16 and lasts exactly 118 seconds.
The earlier `terminal.py` and `teams.py` helpers describe the retained source
recording and are not needed to render this edit.

## Assemble in Resolve

```sh
python3 demo/07-hackathon-platform/resolve.py \
  --preflight demo/07-hackathon-platform/manifest.json
python3 demo/07-hackathon-platform/resolve.py \
  --prepare-lua demo/07-hackathon-platform/manifest.json --sandbox
```

The helper stages independent media in the App Store edition's container and
prints a Lua entry point. Its standard assembler creates a new project. For this
delivery, the guarded `orka-hackathon-platform-overview.copy.lua` driver instead
imports the existing introduction DRP under a new name, verifies the inherited clip
sequence, builds the new timeline, and removes inherited media references from
the copy. The original project exports remain intact. That driver and its
preparation script are retained under `bin/hackathon-platform-overview/resolve/`.

Run the copy driver through **Workspace → Scripts → Orka - Render overview copy**.
It clears inherited render destinations in the copy and renders to the new output
directory. It refuses to overwrite an existing project or final
export. For another revision, use a new output name and matching output paths.

After Resolve finishes:

```sh
python3 demo/07-hackathon-platform/resolve.py --verify-output \
  bin/hackathon-platform-overview/resolve/orka-hackathon-platform-overview.prepared.json
```

Keep the staged sandbox media; the DRP references those files. QA includes the
frame count, complete export decode, narration placement, speech transcription,
scene samples, and original-file preservation checks. Speech recognition checks
content and does not assess subjective voice performance.
