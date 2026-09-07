#!/usr/bin/env python3
"""Run the workspace E2E cleanup sequences with local command fixtures."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(os.environ.get("WORKSPACE_LIFECYCLE_SOURCE_ROOT", Path(__file__).resolve().parents[2]))
SCRIPTS = ("agent-substrate-e2e.sh", "live-agent-sandbox-e2e.sh")


def section(source, start, end):
    begin = source.index(start)
    return source[begin:source.index(end, begin)]


FIXTURE = r'''
set -Eeuo pipefail
acp_task_namespace=fixture
ORKA_NAMESPACE=fixture
ambiguous_session=fixture-ambiguous
restart_session=fixture-restart
reset_lc_sessions="fixture-first fixture-timeout fixture-cancel fixture-ambiguous fixture-restart"
log() { :; }
die() { printf '%s\n' "$*" >&2; exit 1; }
run() { "$@"; }
sleep() { :; }
date() {
  local tick
  tick=$(cat "$TEST_DIR/clock")
  printf '%s\n' "$((tick + 150))" >"$TEST_DIR/clock"
  printf '%s\n' "$tick"
}
fixture_marker_count() {
  printf '%s\n' fixture-count >>"$TEST_DIR/calls"
  if [[ "$SCENARIO" == replay ]]; then printf '2\n'; else printf '1\n'; fi
}
fixture_marker_disconnects() {
  printf '%s\n' fixture-disconnect >>"$TEST_DIR/calls"
  if [[ "$SCENARIO" == no-disconnect ]]; then printf '0\n'; else printf '1\n'; fi
}
delete_fixed_session() {
  if [[ "$1" == orka-ws-lc-cancel-session ]]; then
    [[ $(grep -c '^fixture-count$' "$TEST_DIR/calls") == 2 ]]
    grep -q '^fixture-disconnect$' "$TEST_DIR/calls"
  fi
  printf 'archive:%s\n' "$1" >>"$TEST_DIR/calls"
  [[ "$SCENARIO" != archive-conflict ]] || return 1
  touch "$TEST_DIR/archived"
}
kubectl() {
  [[ "$1" == -n && "$2" == fixture ]]
  shift 2
  case "$1 $2" in
    'get task')
      [[ ! -e "$TEST_DIR/released" ]] || return 1
      if [[ -e "$TEST_DIR/archived" && "$SCENARIO" != finalizer-retained ]]; then
        printf '%s\n' '{"metadata":{"finalizers":["acp-e2e.orka.ai/lifecycle-observer"]}}'
      else
        printf '%s\n' '{"metadata":{"finalizers":["orka.ai/cleanup","acp-e2e.orka.ai/lifecycle-observer"]}}'
      fi
      ;;
    'patch task')
      [[ -e "$TEST_DIR/archived" && "$SCENARIO" != finalizer-retained ]]
      printf '%s\n' observer-release >>"$TEST_DIR/calls"
      touch "$TEST_DIR/released"
      ;;
    'delete task')
      if [[ " $* " == *' --wait=false '* ]]; then
        printf '%s\n' cancellation-request >>"$TEST_DIR/calls"
      else
        [[ -e "$TEST_DIR/archived" ]]
        printf '%s\n' task-absence >>"$TEST_DIR/calls"
      fi
      ;;
    *) return 90 ;;
  esac
}
wait_resource_absent() {
  [[ -e "$TEST_DIR/released" ]]
  printf '%s\n' task-absence >>"$TEST_DIR/calls"
}
'''


class WorkspaceLifecycleCleanupTests(unittest.TestCase):
    def execute(self, body, scenario):
        with tempfile.TemporaryDirectory(prefix="workspace-cleanup-test-") as directory:
            folder = Path(directory)
            (folder / "clock").write_text("0\n")
            (folder / "calls").touch()
            environment = dict(os.environ, TEST_DIR=directory, SCENARIO=scenario)
            process = subprocess.run(
                ["bash", "-c", FIXTURE + "\nexercise() {\n" + body + "\n}\nexercise\n"],
                env=environment, capture_output=True, text=True, timeout=5,
            )
            return process, (folder / "calls").read_text().splitlines()

    def test_cancellation_evidence_precedes_archival_and_finalizer_wait(self):
        for script in SCRIPTS:
            source = (ROOT / "scripts" / script).read_text()
            begin = min(source.index("  # Release the observer only"), source.index("  # No-replay proof:"))
            end = source.index('  if [[ -n "${cancel_pool}" ]]; then', begin)
            # The Substrate script still uses its fixed namespace spelling.
            body = source[begin:end].replace("-n orka-system", "-n fixture")
            for scenario in ("success", "replay", "no-disconnect", "archive-conflict", "finalizer-retained"):
                with self.subTest(script=script, scenario=scenario):
                    result, calls = self.execute(body, scenario)
                    if scenario == "success":
                        self.assertEqual(result.returncode, 0, result.stderr)
                        self.assertLess(calls.index("fixture-disconnect"), calls.index("archive:orka-ws-lc-cancel-session"))
                        self.assertLess(calls.index("archive:orka-ws-lc-cancel-session"), calls.index("observer-release"))
                    else:
                        self.assertNotEqual(result.returncode, 0)
                        self.assertNotIn("observer-release", calls)
                        self.assertNotIn("task-absence", calls)
                        if scenario in ("replay", "no-disconnect"):
                            self.assertFalse(any(call.startswith("archive:") for call in calls))

    def test_final_cleanup_includes_unknown_sessions_before_task_absence(self):
        for script in SCRIPTS:
            source = (ROOT / "scripts" / script).read_text()
            begin = source.index("  # Session archival retains")
            end = source.index('delete runtimepool "${pool_name}" --ignore-not-found=true', begin)
            end = source.rfind("\n", begin, end)
            body = source[begin:end].replace("-n orka-system", "-n fixture")
            for scenario in ("success", "archive-conflict"):
                with self.subTest(script=script, scenario=scenario):
                    result, calls = self.execute(body, scenario)
                    if scenario == "success":
                        self.assertEqual(result.returncode, 0, result.stderr)
                        self.assertEqual(calls, [
                            "archive:orka-ws-lc-session", "archive:orka-ws-lc-timeout-session",
                            "archive:fixture-ambiguous", "archive:fixture-restart", "task-absence",
                        ])
                    else:
                        self.assertNotEqual(result.returncode, 0)
                        self.assertNotIn("task-absence", calls)

    def test_interrupted_run_cancels_then_archives_before_waiting(self):
        for script in SCRIPTS:
            source = (ROOT / "scripts" / script).read_text()
            body = section(source, "  # Request cancellation first", "delete agent orka-ws-lc-agent")
            body = body[:body.rfind("\n")].replace("-n orka-system", "-n fixture")
            for scenario in ("success", "archive-conflict"):
                with self.subTest(script=script, scenario=scenario):
                    result, calls = self.execute(body, scenario)
                    self.assertEqual(calls[0], "cancellation-request")
                    if scenario == "success":
                        self.assertEqual(result.returncode, 0, result.stderr)
                        self.assertEqual(calls[-1], "task-absence")
                        self.assertEqual(sum(call.startswith("archive:") for call in calls), 5)
                    else:
                        self.assertNotEqual(result.returncode, 0)
                        self.assertNotIn("task-absence", calls)


if __name__ == "__main__":
    unittest.main()
