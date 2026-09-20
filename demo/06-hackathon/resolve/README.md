# Resolve assembly

`assemble.py` builds a new DaVinci Resolve project from individual video scenes
and narration clips, exports the project, and renders H.264/AAC MP4. It does not
edit an existing project or change application preferences.

The first pass uses 1920x1080, constant 30 fps, and a maximum of 119 seconds. The
source scene videos should already have their desired framing, titles, and
timing. Resolve performs the final scene assembly, audio placement, and export.
Scene audio is excluded so desktop notifications cannot leak into the mix.

The first-pass manifest is checked in as `demo/06-hackathon/resolve/manifest.json`.
It references generated clips under `bin/hackathon-first-pass/` and defines the
118-second edit. A custom manifest can use the same format:

```json
{
  "project_name": "Orka Hackathon First Pass",
  "timeline_name": "Orka First pass",
  "output_name": "orka-hackathon-first-pass",
  "scenes": [
    {
      "id": "intro",
      "title": "Run agent teams. Stay in control.",
      "path": "../scenes/01-intro.mp4",
      "frames": 240,
      "source_start_frame": 0
    },
    {
      "id": "discovery",
      "title": "A scheduled scan discovers work",
      "path": "../scenes/02-discovery.mp4",
      "frames": 600
    }
  ],
  "audio": [
    {
      "path": "../audio/narration.wav",
      "record_frame": 0,
      "frames": 810,
      "track": 1
    }
  ]
}
```

All frame counts use the 30 fps timeline. `source_start_frame` defaults to zero.
Scene clips are placed consecutively. Audio uses `record_frame` for placement and
can be supplied as a full narration track or separate paragraphs. Audio clips
must fit within the video and cannot overlap on the same track. Paths are
relative to the manifest, or absolute.

Run a media preflight before opening the console:

```bash
python3 demo/06-hackathon/resolve/assemble.py --preflight demo/06-hackathon/resolve/manifest.json
```

The installed Resolve 21.1.0 includes its SDK under
`/Applications/DaVinci Resolve.app/Contents/Resources/Developer/Scripting`.
The external Python SDK loads, but currently cannot connect to the running app.
The embedded Lua 5.1 console is available and is the preferred route. Generate a
Lua driver after the media is ready:

```bash
python3 demo/06-hackathon/resolve/assemble.py --prepare-lua demo/06-hackathon/resolve/manifest.json --sandbox
```

The installed 21.1 SDK imports through `MediaStorage.AddItemsToMediaPool` and
uses an exclusive `endFrame` for `AppendClipInfo`. The assembler verifies each
inserted duration and position, then the exported frame count. A four-second
test export verified both video scenes and the audio track before the full edit.

The App Store edition is sandboxed. `--sandbox` copies the selected media and
self-contained driver into a task-named directory under
`~/Library/Containers/com.blackmagic-design.DaVinciResolveLite/Data/OrkaHackathon/`.
Resolve renders there. Output validation copies the verified MP4 and project
export back into `bin/hackathon-first-pass/resolve/`, preserving the originals.
The exported project references the staged media, which remains available for
editing. No application preferences change.

Without `--sandbox`, use Resolve's normal Import Media Folder dialog to select
the task's `bin/hackathon-first-pass` directory if Lua reports `Operation not
permitted`. This access lasts for the current app session. Use a task-owned
project for the import.

This edition also omits Lua's `io` library. The preflight therefore writes a
`.prepared.json` file. Lua prints the live render job and project information in
the console, while Resolve's own SDK writes the `.drp` and MP4. After rendering,
pass the `.prepared.json` file to `--verify-output`; it creates the final
`.report.json` from the exported media. Do not change scripting security settings.

In Workspace > Console, use the Lua prompt and run the printed `dofile(...)`
command. For the default output name it is:

```lua
dofile("/Users/sozercan/Library/Containers/com.blackmagic-design.DaVinciResolveLite/Data/OrkaHackathon/orka-hackathon-first-pass/orka-hackathon-first-pass.console.lua")
```

Check progress in the same Lua console:

```lua
_orka_assembler.status(_orka_render_report)
```

Resolve saves a new project with a timestamped name, writes a `.drp` export, then
starts the render. The script does not wait in the console. It does not save or
edit the previously open project. Existing output names are rejected, so use a
new `output_name` for a new attempt.

Once Resolve reports completion, validate the exported media from the shell:

```bash
python3 demo/06-hackathon/resolve/assemble.py --verify-output bin/hackathon-first-pass/resolve/orka-hackathon-first-pass.prepared.json
```

Validation checks the exported codecs, dimensions, frame rate, exact video frame
count, duration below 120 seconds, and presence of the Resolve project export.
Also watch the rendered video to check scene continuity, narration alignment,
readability, and audio quality.

The installed scripting README documents internal console execution, media
import, frame-accurate timeline placement, and the render workflow. Its examples
`1_sorted_timeline_from_folder.py` and `3_grade_and_render_all_timelines.py` provide
the corresponding project, timeline, and render calls.
