#!/usr/bin/env bash
# Record one demo (or all of them) to demo/casts/<name>.cast.
#
#   ./demo/record.sh                    # every demo
#   ./demo/record.sh 01-chat-to-pr      # just one
#
# Recordings are asciicast v3 at 100x28 with idle time capped at 2s. Chapter
# markers written by demo/lib/demo.sh are converted to real marker events by
# demo/lib/markers.py, so playback can pause on each chapter:
#
#   asciinema play --pause-on-markers demo/casts/01-chat-to-pr.cast
#
# space resumes, ] skips to the next chapter, . steps a frame.
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

command -v asciinema >/dev/null 2>&1 || {
  echo "asciinema 3.x is required" >&2
  exit 1
}

if [ $# -gt 0 ]; then
  demos=("$@")
else
  demos=()
  # Edited video projects also live under demo/, but have no standalone script.
  for script in demo/[0-9][0-9]-*/demo.sh; do
    [[ -x $script ]] || continue
    name=${script%/demo.sh}
    demos+=("${name#demo/}")
  done
fi
mkdir -p demo/casts

for name in "${demos[@]}"; do
  [[ $name =~ ^[0-9][0-9]-[a-z0-9-]+$ ]] || { echo "invalid demo name: $name" >&2; exit 1; }
  script=demo/$name/demo.sh
  test -x "$script" || {
    echo "no such demo: $name" >&2
    exit 1
  }
  cast=demo/casts/$name.cast
  title=$(sed -n '2s/^# //p' "$script")

  case $name in
    08-agent-to-agent|09-governed-tools|10-reviewed-memory|11-fibey-approval|12-efficiency)
      # These scripts use fresh IDs and scope their evidence to the current run.
      # Keep their saved Tasks, Sessions, and reviewed notes for inspection.
      ;;
    *)
      echo "==> resetting demo objects"
      ./demo/reset.sh "$name"
      ;;
  esac

  echo "==> recording $name"
  # --return propagates the script's exit status. Without it asciinema exits 0
  # no matter what happened inside, so a demo that aborted under `pe` would
  # still be written out as a cast and the loop would roll on to the next one.
  asciinema rec "$cast" \
    --overwrite \
    --headless \
    --return \
    --window-size 100x28 \
    --idle-time-limit 2 \
    --title "${title:-$name}" \
    --command "bash $script"

  python3 demo/lib/markers.py "$cast"
done
