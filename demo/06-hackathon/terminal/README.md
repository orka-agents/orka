# First-pass terminal scenes

These scripts record live, read-only CLI queries. They reuse `demo/lib/demo.sh` without changing it. Before sourcing the helper, `common.sh` sets `ORKA_DEMO_ENV=/dev/null` and a task-owned config directory. It immediately replaces the helper's `orka()` function with the scoped `demo/06-hackathon/cli.py` wrapper. The presenter's `HOME` and Orka configuration are never changed.

Record and render the two completed scan scenes:

```sh
python3 demo/06-hackathon/terminal/capture.py 01-schedule 02-discovery
python3 demo/06-hackathon/terminal/render.py 01-schedule 02-discovery
```

The recording is asciicast v3 at 100 columns by 28 rows. The original raw cast is retained. Chapter-marker conversion changes only the separate playback copy. `agg` renders the Monokai theme with the original helper colors. FFmpeg fits that terminal into a 1920 by 1080, 30 fps, silent H.264 clip. The final frame is held to meet the scene budget. If a capture exceeds its budget, rendering condenses its timing and records the speed factor in metadata.

`view.py` is a local display formatter, not an Orka command. It selects and labels fields returned by the real CLI. It never supplies task outcomes, findings, token counts, or PR results. Only the selected command-injection finding is read; other finding contents are excluded.

| Scene | Frames | Seconds | Required evidence |
| --- | ---: | ---: | --- |
| `01-schedule` | 420 | 14 | Scheduled parent, first child logs, completed scan |
| `02-discovery` | 510 | 17 | Live validated finding `fnd_0afb30da8140` |
| `04-handoff` | 210 | 7 | `state/handoff.json` and matching Engineering task |
| `05-patch` | 390 | 13 | Succeeded Engineering task and its recorded result |
| `06-delivery` | 420 | 14 | Exact publication, PR receipt, matching open GitHub PR |
| `07-usage` | 210 | 7 | Measured source Teams request on the live usage page |

The remaining three seconds of the handoff scene come from Teams footage in Resolve. Later scenes are prepared but are not captured by default. Once their real records exist, pass their names explicitly to both scripts. The preconditions reject missing handoff records, unfinished work, mismatched publication, and unavailable Teams measurements.

The patch scene labels the coding agent's result as an excerpt. Review the real result and narration before recording it; the excerpt is not independent proof of test correctness. The delivery scene verifies the remote PR against the recorded commit. The usage scene shows one measured Teams request, visible missing ACP measurements, and the recorded pricing status. It does not combine team installations or invent dollar costs.

Generated recordings, GIFs, MP4s, frame previews, and metadata stay under `bin/hackathon-first-pass/terminal/`.
