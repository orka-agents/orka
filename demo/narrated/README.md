# Narrated standalone demos

This edit includes demos 01–05 and 08–12. The hackathon videos are excluded.
Each demo gets a new DaVinci Resolve project with a Story video track,
a separate Narration audio track, and chapter markers. Resolve exports the
final 1920×1080, 30 fps H.264/AAC video and a `.drp` project.

`storyboard.json` contains the scenario openings, explanations, and closing
invitations. Each ending displays `https://orka-agents.github.io/orka/`.
The narration describes the results visible in the recorded run. It does not
replace or invent command output.

Demo 12 follows twenty customers reporting duplicate charges after retrying a
frozen checkout. A support assistant drafts individual factual replies, while payments engineering fixes
the cause in a sample repository and runs its tests. The terminal shows real
Orka CLI and `kubectl` commands. Both runs keep their requests, instructions,
and repository revision fixed, first using hosted GPT-5.5 and then enabling
Vekil's semantic routing. The native support Agent connects to Vekil through
the Orka Provider named `semantic-router`. The Codex coding agent's model
connection uses the platform's model proxy to reach the same Vekil gateway.
Narration follows observed routes, outcomes, usage, timings, and dated cost
estimates. It calls out raw amounts and percentage changes, and omits the
cluster name. Record it only after the live checks succeed.

## Prepare the source footage and voice

The inputs are the successful asciicast v3 recordings in `demo/casts/`.
`prepare.py` checks their exit status and chapter markers, then extracts the
selected chapters into presentation copies. Each copy starts with a clean
terminal screen and preserves the chapter's recorded output bytes. The original
captures are left unchanged. Quiet waits are capped at two seconds; long
chapters normally play up to twice as fast. Demo 12's two repeated customer
batches allow up to four times playback speed. The storyboard's `max_speed`
setting controls that limit per chapter. Shorter chapters hold their final
frame until narration finishes.

Use the requested Podman image, `localhost/aikit-qwen3-tts:applesilicon`, with
the reference WAV mounted read-only at `/models/reference.wav`. The existing
container serves `qwen3-tts` on `http://127.0.0.1:18080`. The AIKit configuration
is on branch `feat/qwen3-tts-spec` in the AIKit checkout. Do not switch a
checkout carrying other work just to read that branch.

For a continuous performance, pass the whole script to Qwen once per video.
Keep the voice reference and its identity file in durable, ignored production
storage. The identity file records the reference path/hash, container image ID,
and model configuration path/hash. A new reference or changed script needs a
new take directory.

```sh
python3 demo/narrated/continuous_voice.py generate \
  --output-dir bin/narrated-demos/continuous/EDITION \
  --identity bin/PRODUCTION/voice-reference/identity.json
bin/narrated-demos/qa-venv/bin/python demo/narrated/align_voice.py \
  bin/narrated-demos/continuous/EDITION/01-chat-to-pr
python3 demo/narrated/prepare.py render 01-chat-to-pr \
  --continuous-dir bin/narrated-demos/continuous/EDITION --jobs 2
```

Replace `EDITION` and `PRODUCTION` with the current production paths. Add demo
IDs after `generate` to select particular videos. Generation is sequential and
sends one complete script in each request. It retains the request, source WAV,
normalized 48 kHz WAV, reference/model identities, and generation metadata.
Audio is normalized to −16 LUFS with a −1.5 dB true-peak target.

The alignment command transcribes locally and proposes chapter boundaries in
measured silence. Inspect the expected text, transcript, figures, ending, and
boundary positions before marking `alignment.json`'s `review.status` as
`accepted`. Record the review method and evidence there. Resolve uncertain
recognition results with listening or a second local recognizer. The renderer
rejects unreviewed or stale alignment. It copies every sample of the complete
performance in order and adds silence only at scene boundaries for reading
time. The final Resolve timeline has one narration clip. Speech is never
accelerated or trimmed to fit the terminal.

The earlier per-chapter workflow remains available through `prepare.py plan`,
the shared `demo/06-hackathon/audio/generate_voiceover.py` helper, and
`prepare.py render` without `--continuous-dir`. Using that shared helper does
not add the hackathon videos to the collection.

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

Run the printed `dofile(...)` command through a uniquely named Lua wrapper
in Resolve's Fusion `Scripts/Utility` directory, using Workspace > Scripts.
The internal Lua Console is an alternative when it is accessible. The script creates a new project and
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

For continuous takes, use `align_voice.py` and the accepted alignment review
instead of `check_audio.py`, which checks the earlier per-chapter layout.
Speech recognition runs locally. Inspect flagged transcripts for omissions,
repetitions, or clipped endings. A transcript match alone is not a listening
test. The export check decodes the complete file and compares every narration
clip with the exported audio, including its timing. It also extracts middle
and ending frames from every scene for visual inspection. Check those frames
and the actual Resolve timeline for readability, scene order, and the closing
link before treating a video as complete.

All generated media and verification records stay in ignored
`bin/narrated-demos/`. The source recordings remain in ignored `demo/casts/`.
