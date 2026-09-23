#!/usr/bin/env python3
"""Transcribe a complete narration locally and propose silent scene boundaries."""

import argparse
from difflib import SequenceMatcher
import hashlib
import json
from pathlib import Path
import re
import wave

import numpy as np


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def tokens(text):
    text = text.lower().replace("orca", "orka")
    # Keep one-to-one spelling normalization so every token retains its timestamp.
    words = re.findall(r"[a-z0-9]+", text)
    aliases = {"0": "zero", "1": "one", "2": "two", "3": "three", "4": "four",
               "5": "five", "6": "six", "7": "seven", "8": "eight", "9": "nine",
               "16": "sixteen", "18": "eighteen", "20": "twenty", "24": "twentyfour",
               "32": "thirtytwo", "cpu": "cpu", "cpus": "cpu", "github": "github"}
    return [aliases.get(word, word) for word in words]


def edit_distance(first, second):
    previous = list(range(len(second) + 1))
    for row, a in enumerate(first, 1):
        current = [row]
        for column, b in enumerate(second, 1):
            current.append(min(current[-1] + 1, previous[column] + 1,
                               previous[column - 1] + (a != b)))
        previous = current
    return previous[-1]


def propose_boundaries(plan, transcription, samples, rate):
    expected = [token for scene in plan["scenes"] for token in tokens(scene["text"])]
    timed = []
    for segment in transcription["segments"]:
        for word in segment["words"]:
            timed.extend({"token": token, "start": word["start"], "end": word["end"]}
                         for token in tokens(word["word"]))
    actual = [word["token"] for word in timed]
    if not actual:
        raise ValueError("The narration has no recognized words")
    matcher = SequenceMatcher(None, expected, actual, autojunk=False)
    mapping = {}
    for a, b, size in matcher.get_matching_blocks():
        mapping.update((a + offset, b + offset) for offset in range(size))
    boundaries, word_boundaries, details = [0], [0], []
    position = 0
    for scene in plan["scenes"][:-1]:
        position += len(tokens(scene["text"]))
        # Scene transitions must be grounded in recognized words on both sides.
        left = next((mapping[index] for index in range(position - 1, max(-1, position - 5), -1)
                     if index in mapping), None)
        right = next((mapping[index] for index in range(position, min(len(expected), position + 4))
                      if index in mapping), None)
        if left is None or right is None or right <= left:
            raise ValueError("No reliable transition after " + scene["id"])
        start, end = timed[left]["end"], timed[right]["start"]
        if not 0 <= end - start <= 2:
            raise ValueError("Ambiguous speech transition after " + scene["id"])
        lo, hi = round(start * rate), round(end * rate)
        window = max(1, round(0.02 * rate))
        if hi - lo < window:
            raise ValueError("No twenty-millisecond pause after " + scene["id"])
        energy = np.convolve(samples[lo:hi].astype(np.float64) ** 2,
                             np.ones(window) / window, mode="valid")
        eligible = np.flatnonzero(energy < (32768 * 10 ** (-40 / 20)) ** 2)
        if not len(eligible):
            raise ValueError("No quiet boundary after " + scene["id"])
        center = (hi - lo - window) / 2
        chosen = int(eligible[np.argmin(np.abs(eligible - center))])
        boundary = lo + chosen + window // 2
        if not boundaries[-1] < boundary < len(samples):
            raise ValueError("Scene boundary is outside the audio")
        boundaries.append(boundary)
        word_boundaries.append(right)
        details.append({"after": scene["id"], "sample": boundary,
                        "seconds": boundary / rate, "speech_gap": [start, end],
                        "boundary_rms_dbfs": float(10 * np.log10(max(energy[chosen], 1e-12) / 32768 ** 2))})
    boundaries.append(len(samples))
    word_boundaries.append(len(actual))
    scenes, reviews = [], []
    for index, scene in enumerate(plan["scenes"]):
        scenes.append({**scene, "start_sample": boundaries[index], "end_sample": boundaries[index + 1]})
        recognized = actual[word_boundaries[index]:word_boundaries[index + 1]]
        wanted = tokens(scene["text"])
        reviews.append({"id": scene["id"], "expected": scene["text"],
                        "recognized_tokens": " ".join(recognized),
                        "word_error_rate": edit_distance(wanted, recognized) / max(1, len(wanted)),
                        "ending_distance": edit_distance(wanted[-8:], recognized[-8:])})
    return scenes, details, reviews


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("--model", default="mlx-community/whisper-small.en-mlx")
    args = parser.parse_args()
    directory = args.directory.resolve()
    plan = json.loads((directory / "plan.json").read_text())
    metadata_path = directory / "metadata.json"
    metadata = json.loads(metadata_path.read_text())
    audio = directory / "narration.wav"
    sha = digest(audio)
    if sha != metadata["final"]["sha256"] or metadata["text"] != plan["text"]:
        raise ValueError("Narration differs from its recorded script or hash")
    if plan["text"] != "\n\n".join(scene["text"] for scene in plan["scenes"]):
        raise ValueError("Scene text does not match the complete request")
    if metadata["near_frame_cap"]:
        raise ValueError("Narration may have reached its generation cap")
    with wave.open(str(audio), "rb") as source:
        if (source.getframerate(), source.getnchannels(), source.getsampwidth()) != (48000, 1, 2):
            raise ValueError("Expected 48kHz mono PCM16 narration")
        samples = np.frombuffer(source.readframes(source.getnframes()), dtype="<i2")
    transcript_path = directory / ("transcript-" + args.model.rsplit("/", 1)[-1] + ".json")
    old = json.loads(transcript_path.read_text()) if transcript_path.exists() else {}
    if old.get("audio_sha256") == sha and old.get("model") == args.model:
        transcription = old["transcription"]
    else:
        import mlx_whisper
        transcription = mlx_whisper.transcribe(
            str(audio), path_or_hf_repo=args.model, language="en", verbose=None,
            temperature=0, condition_on_previous_text=False, word_timestamps=True,
            initial_prompt="Orka. Fibey. Vekil. Jev. AIKit. Qwen. gVisor. Agent Substrate. Kubernetes.")
        transcript_path.write_text(json.dumps({"audio_sha256": sha, "model": args.model,
                                               "transcription": transcription}, indent=2) + "\n")
    scenes, boundaries, reviews = propose_boundaries(plan, transcription, samples, 48000)
    report = {"version": 1, "demo_id": plan["demo_id"], "text": plan["text"],
              "audio": {"path": str(audio), "sha256": sha}, "scenes": scenes,
              "metadata": {"path": str(metadata_path), "sha256": digest(metadata_path)},
              "sources": [{"path": str(transcript_path), "sha256": digest(transcript_path)}],
              "method": "Local speech recognition word timestamps; scene transitions placed in measured silence",
              "boundaries": boundaries, "content_checks": reviews,
              "review": {"status": "pending", "audio_sha256": sha}}
    destination = directory / "alignment.json"
    if destination.exists():
        previous = json.loads(destination.read_text())
        if previous.get("review", {}).get("status") == "accepted":
            raise ValueError("Preserve the accepted alignment before replacing it")
    destination.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({"demo": plan["demo_id"], "seconds": len(samples) / 48000,
                      "scenes": len(scenes), "review": "pending",
                      "content_checks": [{k: v for k, v in review.items() if k not in ("expected", "recognized_tokens")}
                                         for review in reviews]}), flush=True)


if __name__ == "__main__":
    main()
