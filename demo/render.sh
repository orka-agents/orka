#!/usr/bin/env bash
# Render recorded casts to animated GIFs for docs and posts.
#
#   ./demo/render.sh                    # every cast in demo/casts
#   ./demo/render.sh 02-agent-sandbox   # just one
#
# Uses agg (https://github.com/asciinema/agg). GIFs land next to the casts as
# demo/casts/<name>.gif and are gitignored like the casts. Idle time is
# already capped at record time, so the GIF length matches playback.
# RENDER_SPEED, RENDER_FONT_SIZE (16), and RENDER_LAST_FRAME (seconds the
# final frame holds, 3) adjust the output.
set -eu -o pipefail

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

command -v agg >/dev/null 2>&1 || {
  echo "agg is required: brew install agg (or cargo install --git https://github.com/asciinema/agg)" >&2
  exit 1
}

if [ $# -gt 0 ]; then
  names=$*
else
  names=$(cd demo/casts && ls *.cast | sed 's/\.cast$//' | tr '\n' ' ')
fi

for name in $names; do
  cast=demo/casts/$name.cast
  test -f "$cast" || {
    echo "no such cast: $cast" >&2
    exit 1
  }
  echo "==> rendering $name"
  # Render at the size the cast was recorded with.
  read -r cols rows < <(head -n 1 "$cast" | jq -r '[.term.cols // 100, .term.rows // 28] | @tsv')
  agg "$cast" "demo/casts/$name.gif" \
    --cols "$cols" --rows "$rows" \
    --font-size "${RENDER_FONT_SIZE:-16}" \
    --theme monokai \
    --speed "${RENDER_SPEED:-1.0}" \
    --last-frame-duration "${RENDER_LAST_FRAME:-3}" 2>&1 | tr "\r" "\n" | tail -n 1
done
