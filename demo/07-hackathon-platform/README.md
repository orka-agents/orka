# Orka hackathon platform cut

A 118-second introduction to Orka as an open-source platform for running and
governing AI agents. Orka opens the video. The web-app security-review scenario
is introduced on the "Two teams. One workflow." card. Input gateways then lead
to Orka and its native Kubernetes or compatible external agents.

This cut has its own source and output directories. It reuses the terminal,
card, narration, and Resolve helpers from demo 06 without changing demos 01–06.
All recordings, generated media, private state, and exports stay in gitignored
`bin/` directories.

The delivered files are:

- `bin/hackathon-platform-revised/resolve/orka-hackathon-platform-revised.mp4`
- `bin/hackathon-platform-revised/resolve/orka-hackathon-platform-revised.drp`

The installed Resolve project is `Orka Hackathon Platform Revised 20260920`.
It was copied from `Orka Hackathon Platform 20260920-162246` before editing.
The original project, media, and exports remain unchanged. All media referenced
by the revised project has a separate path under `orka-hackathon-platform-revised`.

DaVinci Resolve assembled and rendered the final video. Its timeline is 3,540
frames at 1920×1080 and 30 fps. The H.264/AAC file is 118.080 seconds including
audio padding, below the strict two-minute limit.

## Edit

| Time | Picture | Purpose |
| --- | --- | --- |
| 00:00–00:19 | Orka overview | Agents, coordination, tools, skills, permissions, reviewed knowledge, and token visibility |
| 00:19–00:27 | Two teams. One workflow. | A web app needs a security review; Security finds vulnerabilities and Engineering prepares a fix |
| 00:27–00:37 | Schedule CLI | A scheduled tick starts the scan without a human prompt |
| 00:37–00:48 | Discovery CLI | Source analysis produces a validated finding |
| 00:48–01:00 | Teams conversation | A colleague asks what needs attention and reads the saved finding |
| 01:00–01:03 | Teams request | A person chooses the fix |
| 01:03–01:07 | Handoff CLI | The custom demo bridge passes work to Engineering |
| 01:07–01:17 | Patch CLI | The coding agent reports its patch and focused tests |
| 01:17–01:24 | Delivery CLI | A real pull request has a verified publication receipt |
| 01:24–01:34 | Usage CLI | Reported tokens make work quantifiable; gaps remain visible |
| 01:34–01:48 | Integrations | Gateways on the left feed Orka; native Kubernetes and compatible local or Foundry agents are on the right |
| 01:48–01:58 | Outro | Explore the wider platform at https://orka-agents.github.io/orka/ |

The video edit is [manifest.json](manifest.json). [narration.json](narration.json)
contains the spoken text. [timing.json](timing.json) places the voice clips;
the handoff narration spans both the Teams request and CLI handoff scenes.

## Evidence and scope

This revision reuses the platform cut's recording of the retained workflow from
[demo 06](../06-hackathon/README.md). Only two title cards, the schedule clip's
editorial captions, three narration segments, and the edit timing changed.
The scan and Engineering task were not restarted. The scheduled parent remains
suspended to prevent additional ticks.

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
- Native agents run in Kubernetes. [Agent Sandbox](../../website/docs/concepts/agent-sandbox.md)
  and [Agent Substrate](../../website/docs/concepts/substrate.md) are optional
  execution-workspace providers, separate from gateway inputs.

## Reproduce the clips

Run commands from the repository root. This revision needs the retained files
under `bin/hackathon-platform`, not live cluster access or new Teams messages.
Copy the original inputs into the revision's separate output directory:

```sh
mkdir -p bin/hackathon-platform-revised/{terminal,teams,audio}
cp -n bin/hackathon-platform/terminal/*.cast bin/hackathon-platform-revised/terminal/
cp -n bin/hackathon-platform/teams/03-teams.mp4 bin/hackathon-platform-revised/teams/
cp -n bin/hackathon-platform/teams/04-request.mp4 bin/hackathon-platform-revised/teams/
cp -n bin/hackathon-platform/audio/*.wav bin/hackathon-platform-revised/audio/
```

Update only the two editorial captions in the copied schedule casts. Commands,
timestamps, and command output remain the recorded data:

```sh
python3 - <<'PY'
from pathlib import Path

for path in Path("bin/hackathon-platform-revised/terminal").glob("01-schedule*.cast"):
    text = path.read_text()
    text = text.replace("Find vulnerabilities hidden in source code.",
                        "Find undiscovered security vulnerabilities in source code.")
    text = text.replace("Recorded workflow. Future ticks paused after the scan.",
                        "Future ticks paused after the scan.")
    path.write_text(text)
PY

python3 demo/07-hackathon-platform/terminal.py render \
  01-schedule 02-discovery 04-handoff 05-patch 06-delivery 07-usage
bin/hackathon-first-pass/media/.venv/bin/python demo/07-hackathon-platform/cards.py
```

The terminal keeps the original Monokai palette, SF Mono type, and demo-magic
typing. Visible commands use real Orka CLI operations with `jq` and `fold` for
selection and wrapping. The local CLI wrapper supplies private authentication.
The existing `view.py` checks evidence before capture but is not shown as an
Orka command. Raw casts and timing metadata accompany the rendered clips.

The original platform cut records the actual retained Orka Bot conversation in Teams.
No new messages were sent. Send any future demo messages **only to Orka Bot**,
after verifying the conversation title.

The raw capture is `bin/hackathon-platform/teams/orka-bot-second-cut.mov`.
[teams.py](teams.py) selects source intervals 2–6, 4–12, and 9–12 seconds. These
overlap because the full conversation is already visible. It crops and scales
the app pixels at normal playback speed and adds labels outside the crop.
It does not reconstruct messages or typing. Desktop changes after source
second 13 are excluded. The crop coordinates are specific to this recording;
inspect and update them for a different capture.

The revision copies its finished Teams clips unchanged. `teams.py` still owns
the original cut's output directory and is not needed for this revision.

Reuse the `orka-hackathon-tts` Podman container running
`localhost/aikit-qwen3-tts:applesilicon` on `127.0.0.1:18080`, with the supplied
reference WAV mounted read-only as `/models/reference.wav`. The
[original voice instructions](../06-hackathon/audio/README.md) document startup.

```sh
python3 demo/06-hackathon/audio/generate_voiceover.py \
  --manifest demo/07-hackathon-platform/narration.json \
  --output-dir bin/hackathon-platform-revised/audio \
  --scene 00-scope --scene 01-schedule --scene 08-gateways
python3 demo/06-hackathon/audio/assemble_voiceover.py \
  --timing demo/07-hackathon-platform/timing.json \
  --audio-dir bin/hackathon-platform-revised/audio
```

The eleven voice clips contain 97.36 seconds of speech at their original speed.
Each begins 0.4 seconds into its scene. The master WAV is 48 kHz mono PCM16 and
lasts exactly 118 seconds. Check newly generated speech against the scene budgets;
the assembler rejects clips that do not fit.

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
`dofile(...)` command in a new Lua file named `Orka - Render platform revision.lua` under:

```text
~/Library/Containers/com.blackmagic-design.DaVinciResolveLite/Data/Library/Application Support/Fusion/Scripts/Utility/
```

Select **Workspace → Scripts → Orka - Render platform revision**. This creates a new
project, imports the clips, places the narration, exports the DRP, and starts
the MP4 render. The script refuses to overwrite an existing final export.
For another cut, use a new `output_name` in a copied manifest.

The delivered project instead started from an export/import of the exact
original project. Its original timeline and media references were removed from
the copy only after the revised timeline passed the frame checks. The one-time,
project-name-guarded script and untouched project snapshot are retained in
`bin/hackathon-platform-revised/resolve/`.

After Resolve reports completion:

```sh
python3 demo/07-hackathon-platform/resolve.py --verify-output \
  bin/hackathon-platform-revised/resolve/orka-hackathon-platform-revised.prepared.json
```

The validator checks 3,540 video frames, 1920×1080 at 30 fps, H.264/AAC, the
project export, and a container duration below 120 seconds. It copies verified
exports into `bin/hackathon-platform-revised/resolve/`. Keep the staged sandbox media;
the DRP references those files.

QA checks include decoding the complete export, resolving all thirteen project
media references, checking the scene order and frame counts, sampling every
scene, and verifying narration placement. Automated speech recognition checks
the three regenerated segments; it does not assess subjective voice performance.
SHA-256 comparisons verify that the original project and media are unchanged.
QA artifacts remain beside the local export.
