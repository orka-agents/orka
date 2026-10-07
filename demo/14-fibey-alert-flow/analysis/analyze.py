"""Compare pump-1's readings since maintenance with its prior 24 hours.

Runs as the data-analysis job Orka starts on AKS. The sensor history is
synthetic and baked into the image; the analysis is real and prints a short
report that becomes the job's result.
"""

import csv
import statistics
import sys
from pathlib import Path

HISTORY = Path(__file__).with_name("sensor-history.csv")
SENSORS = {
    "pt101_bar": ("PT-101 (replaced)", "bar"),
    "pt102_bar": ("PT-102", "bar"),
    "flow_m3h": ("Flow", "m3/h"),
    "motor_current_a": ("Motor current", "A"),
    "vibration_mm_s": ("Vibration", "mm/s"),
}
CHANGE = 0.15  # a sensor changed if its median moved more than 15%


def main():
    with HISTORY.open(newline="") as handle:
        rows = [{k: (v if k == "timestamp" else float(v)) for k, v in row.items()} for row in csv.DictReader(handle)]
    stopped = [i for i, row in enumerate(rows) if row["flow_m3h"] == 0]
    if not stopped:
        sys.exit("no maintenance stop in the sensor history")
    before, after = rows[: stopped[0]], rows[stopped[-1] + 1 :]
    print(f"pump-1 sensor history: {len(rows)} readings, {rows[0]['timestamp']} to {rows[-1]['timestamp']}")
    print(f"Maintenance stop {clock(rows[stopped[0]])}-{clock(after[0])} (flow 0). Since then, against the prior 24 h:")
    changed = []
    for key, (label, unit) in SENSORS.items():
        baseline = statistics.median(row[key] for row in before)
        now = statistics.median(row[key] for row in after[-6:])
        if abs(now - baseline) / baseline > CHANGE:
            since = next(row for row in after if abs(row[key] - baseline) / baseline > CHANGE)
            changed.append(label)
            print(f"- {label:<20} {now:>5.1f} {unit:<5} since {clock(since)}, baseline {baseline:.1f}  CHANGED")
        else:
            print(f"- {label:<20} {now:>5.1f} {unit:<5} baseline {baseline:.1f}  normal")
    if changed == [SENSORS["pt101_bar"][0]]:
        print("Finding: only the replaced transmitter changed. The pump runs normally,")
        print("so the PT-101 reading is suspect, not the pump.")
    else:
        print("Finding: " + (", ".join(changed) if changed else "no sensor") + " changed.")


def clock(row):
    return row["timestamp"][11:16]


if __name__ == "__main__":
    main()
