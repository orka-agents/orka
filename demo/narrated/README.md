# Narrated standalone demos

This edit includes demos 01–05 and 08–11. The hackathon videos are excluded.
Each demo gets a new DaVinci Resolve project with a Story video track,
a separate Narration audio track, and chapter markers. Resolve exports the
final 1920×1080, 30 fps H.264/AAC video and a `.drp` project.

`storyboard.json` contains the scenario openings, explanations, and closing
invitations. Each ending displays `https://orka-agents.github.io/orka/`.
The narration describes the results visible in the recorded run. It does not
replace or invent command output.

## Prepare the source footage and voice

The inputs are the successful asciicast v3 recordings in `demo/casts/`.
`prepare.py` checks their exit status and chapter markers, then extracts the
selected chapters into presentation copies. Each copy starts with a clean
terminal screen and preserves the chapter's recorded output bytes. The original
captures are left unchanged. Quiet waits are capped at two seconds; long
chapters may play up to twice as fast. Shorter chapters hold their final frame
until narration finishes.

Use the requested Podman image, `localhost/aikit-qwen3-tts:applesilicon`, with
the reference WAV mounted read-only at `/models/reference.wav`. The existing
container serves `qwen3-tts` on `http://127.0.0.1:18080`. The AIKit configuration
is on branch `feat/qwen3-tts-spec` in the AIKit checkout. Do not switch a
checkout carrying other work just to read that branch.

```sh
python3 demo/narrated/prepare.py plan
python3 demo/06-hackathon/audio/generate_voiceover.py \
  --manifest bin/narrated-demos/narration.json \
  --output-dir bin/narrated-demos/audio
python3 demo/narrated/prepare.py render --jobs 2
```

The existing generator is reused as a helper; this does not produce either
hackathon video. It retains the speech request, raw WAV, normalized 48 kHz WAV,
and generation metadata. Audio is normalized to −16 LUFS with a −1.5 dB true-peak
target. Preparation adds silence only to align each clip to an exact video
frame. It never cuts speech to fit a scene.

Rendering needs Python with Pillow, FFmpeg, `agg`, and the installed SF Pro
Display and SF Mono fonts. A demo ID after `render` selects one demo. Matching
generated media is reused only when its input and media digests match.

## Assemble in Resolve

The adapter in `resolve.py` reuses the existing checked Resolve assembler.
It selects a separate output directory and a ten-minute maximum; the original
hackathon assembler retains its 119-second default. These walkthroughs are
shorter than the adapter's limit.

```sh
python3 demo/narrated/resolve.py --prepare-lua \
  bin/narrated-demos/01-chat-to-pr/manifest.json --sandbox
```

Run the printed `dofile(...)` command in Resolve's internal Lua Console,
available under Workspace > Console. The script creates a new project and
timeline, imports each scene and narration clip, checks their exact positions,
saves and exports the project, and starts the render. It does not modify the
previous project or application scripting preferences. Existing exports are
not overwritten.

The App Store edition requires staging media inside its container. Files are
staged under `~/Library/Containers/com.blackmagic-design.DaVinciResolveLite/Data/OrkaDemos/`.
Keep that directory for editable projects. A `.drp` references those media files;
it is not a self-contained media archive.

After Resolve reports completion:

```sh
python3 demo/narrated/resolve.py --verify-output \
  bin/narrated-demos/exports/01-chat-to-pr.prepared.json
```

This checks dimensions, codecs, frame count, duration, and the project export,
then copies the outputs into `bin/narrated-demos/exports/`.

## Verify the narration and exports

Optional local verification tools use a separate environment, leaving project
dependencies unchanged:

```sh
uv venv bin/narrated-demos/qa-venv --python 3.12
uv pip install --python bin/narrated-demos/qa-venv/bin/python mlx-whisper
bin/narrated-demos/qa-venv/bin/python demo/narrated/check_audio.py
bin/narrated-demos/qa-venv/bin/python demo/narrated/check_export.py 01-chat-to-pr
```

Speech recognition runs locally. Inspect flagged transcripts for omissions,
repetitions, or clipped endings. A transcript match alone is not a listening
test. The export check decodes the complete file and compares every narration
clip with the exported audio, including its timing. It also extracts middle
and ending frames from every scene for visual inspection. Check those frames
and the actual Resolve timeline for readability, scene order, and the closing
link before treating a video as complete.

All generated media and verification records stay in ignored
`bin/narrated-demos/`. The source recordings remain in ignored `demo/casts/`.
