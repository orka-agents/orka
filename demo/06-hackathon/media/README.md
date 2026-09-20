# First-pass title cards

`render_cards.py` draws three silent video clips for the hackathon edit. It uses the Monokai background from `demo/render.sh` and the pink, blue, green, and rule colors from the xterm palette in `demo/lib/demo.sh`.

The typography uses the locally installed SF Pro Display and SF Mono fonts. The mascot comes from `website/static/img/orka-logo.png` in the Orka checkout. The script isolates the mascot from the pale background, orbital decoration, and adjacent wordmark while preserving its white details.

From the repository root:

```sh
uv venv bin/hackathon-first-pass/media/.venv
uv pip install --python bin/hackathon-first-pass/media/.venv/bin/python pillow
bin/hackathon-first-pass/media/.venv/bin/python demo/06-hackathon/media/render_cards.py
```

Use `--orka-source PATH` to select another Orka checkout and `--scene 08-gateways` to render one card. FFmpeg and the listed local fonts must be installed.

The generated files are in `bin/hackathon-first-pass/media/`:

| Clip | Frames | Duration |
| --- | ---: | ---: |
| `00-intro.mp4` | 300 | 10 seconds |
| `08-gateways.mp4` | 300 | 10 seconds |
| `09-outro.mp4` | 210 | 7 seconds |

Each clip is 1920 by 1080, 30 fps, H.264 with YUV 4:2:0 pixels and no audio. PNG posters and measured metadata accompany the clips. Main text fades in once; secondary details appear in sequence and remain still for reading.

The gateway card labels Teams and Telegram as available adapters. It labels Slack as an adapter possibility and uses a dashed connection. It does not advertise a shipped Slack integration.
