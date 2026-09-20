# First-pass narration

The first-pass narration matches the recorded scheduled scan, Orka Bot conversation, Engineering task, and PR #36. Recheck these claims against the actual workflow before recording a new run. The recording uses the reference voice supplied for this task.

Start the task-specific LocalAI server using the existing AIKit image:

```sh
podman run -d --name orka-hackathon-tts \
  --device /dev/dri \
  -p 127.0.0.1:18080:8080 \
  --mount type=bind,source=/private/tmp/aikit-qwen3-youtube-94iqjbdt/reference.wav,target=/models/reference.wav,readonly \
  localhost/aikit-qwen3-tts:applesilicon --config-file=/config.yaml
```

If that container already exists, inspect its status and reuse it. Do not run a second copy against the same port.

From the repository root:

```sh
python3 demo/06-hackathon/audio/generate_voiceover.py
python3 demo/06-hackathon/audio/assemble_voiceover.py
```

`generate_voiceover.py` sends one request per scene to `/v1/audio/speech`. It retains the raw 24 kHz speech, normalizes loudness to a -16 LUFS target with a -1.5 dB true-peak ceiling, and exports 48 kHz mono PCM16 WAVs for Resolve. Matching requests reuse their raw audio. Use `--scene 03-teams` to regenerate a changed scene, or `--force` for another performance of unchanged text.

Generated media, requests, and measured durations are in `bin/hackathon-first-pass/audio/`. The source WAV is mounted read-only and is not copied into the repository.

`timing.json` suggests a 118-second edit at 30 frames per second. `assemble_voiceover.py` adds silence between the scene files and exports `voiceover-timeline.wav` plus exact placement metadata. The narration remains at its original speed. Editors can instead import the individual scene WAVs and adjust scene timing in Resolve.
