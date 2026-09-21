# Orka hackathon platform cut

A 115-second introduction to Orka as an open-source platform for running and
governing AI agents. The opening explains the wider platform. A Security and
Engineering workflow then demonstrates one example, followed by compatible
agent integrations, extensible gateways, and an invitation to explore the docs.

This cut has its own source and output directories. It reuses the terminal,
card, narration, and Resolve helpers from demo 06 without changing demos 01–06.
All recordings, generated media, private state, and exports stay in gitignored
`bin/` directories.

The delivered files are:

- `bin/hackathon-platform/resolve/orka-hackathon-platform.mp4`
- `bin/hackathon-platform/resolve/orka-hackathon-platform.drp`

DaVinci Resolve assembled and rendered the final video. Its timeline is 3,450
frames at 1920×1080 and 30 fps. The H.264/AAC file is 115.072 seconds including
audio padding, below the strict two-minute limit.

## Edit

| Time | Picture | Purpose |
| --- | --- | --- |
| 00:00–00:04 | Scenario | A web app needs a security review |
| 00:04–00:23 | Platform overview | Agents, coordination, tools, skills, permissions, reviewed knowledge, and token visibility |
| 00:23–00:28 | Scope | Security and Engineering are one small example |
| 00:28–00:38 | Schedule CLI | A scheduled tick starts the scan without a human prompt |
| 00:38–00:49 | Discovery CLI | Source analysis produces a validated finding |
| 00:49–01:01 | Teams conversation | A colleague asks what needs attention and reads the saved finding |
| 01:01–01:04 | Teams request | A person chooses the fix |
| 01:04–01:08 | Handoff CLI | The custom demo bridge passes work to Engineering |
| 01:08–01:18 | Patch CLI | The coding agent reports its patch and focused tests |
| 01:18–01:25 | Delivery CLI | A real pull request has a verified publication receipt |
| 01:25–01:35 | Usage CLI | Reported tokens make work quantifiable; gaps remain visible |
| 01:35–01:45 | Integrations | Compatible local and Foundry agents, available adapters, and custom gateways |
| 01:45–01:55 | Outro | Explore the wider platform at https://orka-agents.github.io/orka/ |

The video edit is [manifest.json](manifest.json). [narration.json](narration.json)
contains the spoken text. [timing.json](timing.json) places the voice clips;
the handoff narration spans both the Teams request and CLI handoff scenes.

## Evidence and scope

This is a new recording of the retained workflow from [demo 06](../06-hackathon/README.md),
edited for time. The scan and Engineering task were not restarted for this cut.
The scheduled parent remains suspended to prevent additional ticks.

Security is the on-screen name for the existing `reliability` demo identity in
`orka-system`. Engineering uses `orka-pr647-system`. Both are on `sertac-aks`.
The display name does not rename either installation or change its permissions.

The actual scheduled child, `hackathon-scheduled-scan-1789878840`, started scan
`scan_tzxgyrjjmedksoynf7ue6bjsuz` against `sozercan/nodejs-goof` at
`bdb1aa6e8a03cb795e0012d3b6a2c80234243d31`. It reviewed 14 source areas and
retained three findings. The demo selects `fnd_0afb30da8140`, shell command
injection in `routes/index.js:168`. This intentionally vulnerable fixture
demonstrates source-based discovery. It does not establish a newly discovered
real-world zero-day or an executed proof of exploitation.

The Teams guide receives the saved finding. It does not query the scanner live
or directly delegate to Engineering. The existing custom bridge observes the
allowed message and creates Engineering task `hackathon-fix-0afb30da8140` under
that installation's permissions. The film identifies the handoff as a custom
demo integration.

The task produced [PR #36](https://github.com/sozercan/nodejs-goof/pull/36) at
`d07c0c8648249caeee857cda3248ab846f31e12d`. The delivery receipt was
`VerifiedExact`, and the PR was open at recording time. The patch report records
three focused tests. Full application and database flows were not tested, and
the PR had no attached CI checks. Separate publication keeps Git credentials
outside the coding agent; this setup uses the presenter's identity for its
distinct read and publication credential references.

Usage shows one measured Teams request with 2,018 input and 128 output tokens.
ACP scan measurements are unavailable, and model cost is `Price unavailable`.
The film does not claim complete billing, productivity measurement, or a total
across installations.

The broader narrative was checked against the Orka code and documentation:

- [External runtime contracts](../../website/docs/guides/bring-your-own-agent-runtime.md)
  require compatible, governed registrations. Local and Foundry integrations
  are qualified by that compatibility requirement.
- [Coordination](../../website/docs/reference/multi-agent-coordination.md) and
  [memory](../../website/docs/concepts/memory.md) describe reusable tools and
  knowledge. Accepting a memory proposal and applying it are separate actions.
- [Usage reporting](https://github.com/orka-agents/orka/pull/648) distinguishes
  reported tokens, missing measurements, and unavailable pricing.
- [The gateway contract](../../website/docs/reference/gateway-api.md) allows
  custom adapters. Teams and Telegram have adapters. Slack and the presenter's
  own integration are explicitly shown as custom gateway possibilities.

## Reproduce the clips

Run commands from the repository root. The retained demo 06 state, scoped CLI,
and port forwards must still be available. Inspect the existing owned resources
and refresh only expired credentials when needed; do not rerun setup over the
recorded history. See the [CLI prerequisites](../06-hackathon/terminal/README.md).
The commands below replace this cut's intermediate media.

```sh
python3 demo/07-hackathon-platform/terminal.py capture \
  01-schedule 02-discovery 04-handoff 05-patch 06-delivery 07-usage
python3 demo/07-hackathon-platform/terminal.py render \
  01-schedule 02-discovery 04-handoff 05-patch 06-delivery 07-usage
bin/hackathon-first-pass/media/.venv/bin/python demo/07-hackathon-platform/cards.py
```

The terminal keeps the original Monokai palette, SF Mono type, and demo-magic
typing. Visible commands use real Orka CLI operations with `jq` and `fold` for
selection and wrapping. The local CLI wrapper supplies private authentication.
The existing `view.py` checks evidence before capture but is not shown as an
Orka command. Raw casts and timing metadata accompany the rendered clips.

The second cut records the actual retained Orka Bot conversation in Teams.
No new messages were sent. Send any future demo messages **only to Orka Bot**,
after verifying the conversation title.

The raw capture is `bin/hackathon-platform/teams/orka-bot-second-cut.mov`.
[teams.py](teams.py) selects source intervals 2–6, 4–12, and 9–12 seconds. These
overlap because the full conversation is already visible. It crops and scales
the app pixels at normal playback speed and adds labels outside the crop.
It does not reconstruct messages or typing. Desktop changes after source
second 13 are excluded. The crop coordinates are specific to this recording;
inspect and update them for a different capture.

```sh
bin/hackathon-first-pass/media/.venv/bin/python demo/07-hackathon-platform/teams.py
```

Reuse the `orka-hackathon-tts` Podman container running
`localhost/aikit-qwen3-tts:applesilicon` on `127.0.0.1:18080`, with the supplied
reference WAV mounted read-only as `/models/reference.wav`. The
[original voice instructions](../06-hackathon/audio/README.md) document startup.

```sh
python3 demo/06-hackathon/audio/generate_voiceover.py \
  --manifest demo/07-hackathon-platform/narration.json \
  --output-dir bin/hackathon-platform/audio
python3 demo/06-hackathon/audio/assemble_voiceover.py \
  --timing demo/07-hackathon-platform/timing.json \
  --audio-dir bin/hackathon-platform/audio
```

The twelve voice clips contain 93.68 seconds of speech at their original speed.
Each begins 0.4 seconds into its scene. The master WAV is 48 kHz mono PCM16 and
lasts exactly 115 seconds.

## Assemble in Resolve

[resolve.py](resolve.py) reuses the original assembler with this cut's manifest
and output directory. It validates all input formats and durations first.

```sh
python3 demo/07-hackathon-platform/resolve.py \
  --preflight demo/07-hackathon-platform/manifest.json
python3 demo/07-hackathon-platform/resolve.py \
  --prepare-lua demo/07-hackathon-platform/manifest.json --sandbox
```

Use the installed App Store edition's Scripts menu. Put the printed
`dofile(...)` command in a Lua file named `Orka - Render revised demo.lua` under:

```text
~/Library/Containers/com.blackmagic-design.DaVinciResolveLite/Data/Library/Application Support/Fusion/Scripts/Utility/
```

Select **Workspace → Scripts → Orka - Render revised demo**. This creates a new
project, imports the clips, places the narration, exports the DRP, and starts
the MP4 render. The script refuses to overwrite an existing final export.
For another cut, use a new `output_name` in a copied manifest.

After Resolve reports completion:

```sh
python3 demo/07-hackathon-platform/resolve.py --verify-output \
  bin/hackathon-platform/resolve/orka-hackathon-platform.prepared.json
```

The validator checks 3,450 video frames, 1920×1080 at 30 fps, H.264/AAC, the
project export, and a container duration below 120 seconds. It copies verified
exports into `bin/hackathon-platform/resolve/`. Keep the staged sandbox media;
the DRP references those files.

For this export, the complete file decoded successfully, all fourteen project
media references resolved, and samples from every scene were visually checked.
All twelve narration segments matched the master at their intended positions.
The exported channels had no clipped samples and a measured peak of −1.48 dBFS.
Automated speech recognition checked narration content; it does not assess
subjective voice performance. QA artifacts remain beside the local export.
