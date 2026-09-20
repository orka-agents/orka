#!/usr/bin/env python3
"""Render the first-pass Orka title cards without using the desktop."""

import argparse
from collections import deque
import json
from pathlib import Path
import subprocess

from PIL import Image, ImageChops, ImageDraw, ImageFont


WIDTH, HEIGHT, SCALE = 1920, 1080, 2
COLORS = {
    "background": "#272822",
    "white": "#eeeeee",
    "muted": "#a5a69e",
    "pink": "#ff87ff",
    "blue": "#5fafff",
    "green": "#87d787",
    "rule": "#5f5f87",
    "subtle": "#373932",
}
FONT_PATHS = {
    "display": "/Library/Fonts/SF-Pro-Display-Semibold.otf",
    "body": "/Library/Fonts/SF-Pro-Display-Regular.otf",
    "mono": "/Library/Fonts/SF-Mono-Regular.otf",
}


def layer():
    return Image.new("RGBA", (WIDTH * SCALE, HEIGHT * SCALE), (0, 0, 0, 0))


def font(role, size):
    return ImageFont.truetype(FONT_PATHS[role], round(size * SCALE))


def text(image, position, value, size, color="white", role="body"):
    draw = ImageDraw.Draw(image)
    draw.text(tuple(round(value * SCALE) for value in position), value,
              font=font(role, size), fill=COLORS.get(color, color), anchor="lt")


def line(image, points, fill="rule", width=2):
    ImageDraw.Draw(image).line(
        [(round(x * SCALE), round(y * SCALE)) for x, y in points],
        fill=COLORS.get(fill, fill), width=round(width * SCALE),
    )


def dot(image, x, y, radius, color):
    ImageDraw.Draw(image).ellipse(
        ((x - radius) * SCALE, (y - radius) * SCALE,
         (x + radius) * SCALE, (y + radius) * SCALE), fill=COLORS[color],
    )


def dashed(image, start, end, color="muted"):
    x1, y1 = start
    x2, y2 = end
    length = ((x2 - x1) ** 2 + (y2 - y1) ** 2) ** 0.5
    for distance in range(0, int(length), 22):
        stop = min(distance + 10, length)
        points = [(x1 + (x2 - x1) * value / length,
                   y1 + (y2 - y1) * value / length) for value in (distance, stop)]
        line(image, points, color, 2)


def mascot(source):
    """Remove only the edge-connected pale background, retaining white details."""
    image = Image.open(source).convert("RGBA").crop((0, 0, 690, 685))
    pixels = image.load()
    width, height = image.size
    seen = bytearray(width * height)
    queue = deque()
    for x in range(width):
        queue.append((x, 0))
        queue.append((x, height - 1))
    for y in range(height):
        queue.append((0, y))
        queue.append((width - 1, y))
    while queue:
        x, y = queue.popleft()
        if x < 0 or y < 0 or x >= width or y >= height:
            continue
        index = y * width + x
        if seen[index]:
            continue
        seen[index] = 1
        red, green, blue, alpha = pixels[x, y]
        if min(red, green, blue) < 215:
            continue
        pixels[x, y] = (red, green, blue, 0)
        queue.extend(((x - 1, y), (x + 1, y), (x, y - 1), (x, y + 1)))
    # Isolate the mascot from the source's pale orbital illustration and
    # adjacent wordmark. The loose outline leaves the original dark border.
    silhouette = Image.new("L", image.size, 0)
    ImageDraw.Draw(silhouette).polygon([
        (116, 147), (202, 134), (223, 99), (272, 50), (327, 23),
        (386, 17), (443, 42), (471, 76), (474, 110), (502, 128),
        (496, 162), (519, 214), (545, 272), (545, 311), (585, 329),
        (586, 383), (557, 423), (470, 450), (466, 553), (350, 578),
        (282, 602), (282, 663), (220, 651), (155, 625), (111, 643),
        (49, 643), (47, 579), (102, 541), (137, 538), (139, 483),
        (139, 390), (144, 351), (163, 281), (151, 231), (123, 190),
    ], fill=255)
    ImageDraw.Draw(silhouette).rectangle((540, 295, 590, 331), fill=0)
    image.putalpha(ImageChops.multiply(image.getchannel("A"), silhouette))
    return image


def paste_mascot(image, mark, x, y, height):
    width = round(mark.width * height / mark.height)
    resized = mark.resize((width * SCALE, height * SCALE), Image.Resampling.LANCZOS)
    image.alpha_composite(resized, (x * SCALE, y * SCALE))


def bracket(image, x, top, bottom, length=42):
    line(image, [(x + length, top), (x, top), (x, bottom), (x + length, bottom)], "rule", 3)


def intro(mark):
    main = layer()
    bracket(main, 112, 176, 682)
    text(main, (174, 179), "Orka", 134, "pink", "display")
    text(main, (177, 355), "Execution and governance", 60, role="display")
    text(main, (177, 433), "for AI agents", 60, role="display")
    text(main, (178, 576), "On infrastructure you control.", 36, "muted")
    paste_mascot(main, mark, 1280, 211, 570)
    teams = layer()
    text(teams, (176, 813), "Reliability", 39, "blue", "mono")
    line(teams, [(483, 834), (632, 834)], "rule", 3)
    line(teams, [(617, 823), (632, 834), (617, 845)], "green", 3)
    text(teams, (668, 813), "Engineering", 39, "green", "mono")
    return [(main, 0.0, 0.45), (teams, 1.25, 0.45)]


def gateways(mark):
    main = layer()
    text(main, (132, 121), "Your agents. Your channels.", 78, "white", "display")
    text(main, (136, 231), "Connect the tools your teams already use.", 37, "muted")
    bracket(main, 138, 429, 771, 30)
    text(main, (194, 471), "Orka", 92, "pink", "display")
    text(main, (199, 596), "Gateway protocol", 32, "white", "mono")
    line(main, [(620, 597), (843, 597)], "blue", 3)
    dot(main, 843, 597, 5, "blue")

    available = layer()
    line(available, [(843, 437), (843, 597)], "blue", 3)
    line(available, [(843, 437), (981, 437)], "blue", 3)
    line(available, [(843, 597), (981, 597)], "blue", 3)
    dot(available, 982, 437, 6, "green")
    dot(available, 982, 597, 6, "green")
    text(available, (1024, 404), "Microsoft Teams", 49, role="display")
    text(available, (1027, 470), "Available adapter  /  shown here", 24, "green", "mono")
    text(available, (1024, 564), "Telegram", 49, role="display")
    text(available, (1027, 630), "Available adapter", 24, "green", "mono")

    possible = layer()
    dashed(possible, (843, 617), (843, 777))
    dashed(possible, (843, 777), (979, 777))
    ImageDraw.Draw(possible).ellipse((975 * SCALE, 771 * SCALE, 987 * SCALE, 783 * SCALE),
                                    outline=COLORS["muted"], width=2 * SCALE)
    text(possible, (1024, 742), "Slack", 49, role="display")
    text(possible, (1027, 808), "Adapter possibility", 24, "muted", "mono")
    return [(main, 0.0, 0.4), (available, 0.7, 0.4), (possible, 4.8, 0.4)]


def outro(mark):
    main = layer()
    text(main, (166, 158), "Orka", 70, "pink", "display")
    bracket(main, 111, 358, 651)
    text(main, (174, 366), "Run agent teams.", 94, role="display")
    text(main, (174, 492), "Stay in control.", 94, "pink", "display")
    text(main, (179, 781), "orka.ai", 44, "blue", "mono")
    paste_mascot(main, mark, 1320, 246, 500)
    return [(main, 0.0, 0.45)]


def render(scene_id, layers, frames, output_dir):
    background = Image.new("RGBA", (WIDTH, HEIGHT), COLORS["background"])
    base_path = output_dir / "background.png"
    background.convert("RGB").save(base_path)
    poster = background.copy()
    command = ["ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
               "-loop", "1", "-framerate", "30", "-i", str(base_path)]
    filters = []
    for index, (full_layer, start, duration) in enumerate(layers, start=1):
        image = full_layer.resize((WIDTH, HEIGHT), Image.Resampling.LANCZOS)
        path = output_dir / f"{scene_id}-layer-{index}.png"
        image.save(path)
        poster.alpha_composite(image)
        command.extend(["-loop", "1", "-framerate", "30", "-i", str(path)])
        previous = "0:v" if index == 1 else f"composite{index - 1}"
        filters.append(f"[{index}:v]format=rgba,fade=t=in:st={start}:d={duration}:alpha=1[fade{index}]")
        filters.append(f"[{previous}][fade{index}]overlay=0:0:format=auto[composite{index}]")
    filters.append(f"[composite{len(layers)}]format=yuv420p[video]")
    output = output_dir / f"{scene_id}.mp4"
    command.extend(["-filter_complex", ";".join(filters), "-map", "[video]", "-an",
                    "-c:v", "libx264", "-preset", "medium", "-crf", "17",
                    "-pix_fmt", "yuv420p", "-r", "30", "-frames:v", str(frames),
                    "-movflags", "+faststart", str(output)])
    poster.convert("RGB").save(output_dir / f"{scene_id}.png")
    subprocess.run(command, check=True)
    metadata = json.loads(subprocess.check_output(
        ["ffprobe", "-v", "error", "-show_entries",
         "format=duration:stream=codec_name,width,height,pix_fmt,r_frame_rate,nb_frames",
         "-of", "json", str(output)], text=True,
    ))
    metadata["id"] = scene_id
    metadata["path"] = str(output)
    (output_dir / f"{scene_id}-metadata.json").write_text(json.dumps(metadata, indent=2) + "\n")
    print(json.dumps(metadata), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--orka-source", type=Path, default=Path.home() / "projects/orka")
    parser.add_argument("--output-dir", type=Path,
                        default=Path(__file__).resolve().parents[3] / "bin/hackathon-first-pass/media")
    parser.add_argument("--scene", choices=["00-intro", "08-gateways", "09-outro"], action="append")
    args = parser.parse_args()
    args.output_dir.mkdir(parents=True, exist_ok=True)
    mark = mascot(args.orka_source / "website/static/img/orka-logo.png")
    mark.save(args.output_dir / "orka-mascot.png")
    scenes = [("00-intro", intro, 300), ("08-gateways", gateways, 300), ("09-outro", outro, 210)]
    for scene_id, draw, frames in scenes:
        if not args.scene or scene_id in args.scene:
            render(scene_id, draw(mark), frames, args.output_dir)


if __name__ == "__main__":
    main()
