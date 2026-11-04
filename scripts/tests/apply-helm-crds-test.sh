#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="${APPLY_HELM_CRDS_SCRIPT:-${root}/scripts/apply-helm-crds.sh}"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/apply-helm-crds-test.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT
real_jq="$(command -v jq)"
real_mktemp="$(command -v mktemp)"
mkdir "${test_root}/bin"

# JSON is valid YAML; this structural schema has the same shape as a large
# generated CRD without relying on a live API server or a YAML parser.
python3 - "${test_root}/target.json" <<'PY'
import json
import sys

spec = {
    "group": "fixtures.orka.ai",
    "names": {"plural": "examples", "singular": "example", "kind": "Example"},
    "scope": "Namespaced",
    "versions": [{"name": "v1", "served": True, "storage": True,
                  "schema": {"openAPIV3Schema": {"type": "object", "properties": {
                      "spec": {"type": "object", "properties": {
                          "field%d" % i: {"type": "string", "description": "schema documentation " * 60}
                          for i in range(180)
                      }}
                  }}}}]
}
assert len(json.dumps(spec, separators=(",", ":")).encode()) > 131072
with open(sys.argv[1], "w") as output:
    json.dump({"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
               "metadata": {"name": "examples.fixtures.orka.ai"}, "spec": spec}, output)
PY

cat >"${test_root}/bin/fixture-cli" <<'PY'
#!/usr/bin/env python3
import copy
import json
import os
from pathlib import Path
import sys

command = Path(sys.argv[0]).name
args = sys.argv[1:]
# Model Linux's per-string execve limit even when these tests run on macOS.
if any(len(arg.encode()) >= 131072 for arg in args):
    print("%s: Argument list too long (simulated Linux argv limit)" % command, file=sys.stderr)
    sys.exit(126)
work = Path(os.environ["TEST_WORK"])
mode = os.environ["TEST_MODE"]
if command == "jq":
    os.execv(os.environ["REAL_JQ"], [os.environ["REAL_JQ"], *args])
if command == "mktemp":
    count_file = work / "mktemp-count"
    count = int(count_file.read_text()) if count_file.exists() else 0
    count_file.write_text(str(count + 1))
    if mode == "mktemp-failure" and count == 1:
        sys.exit(37)
    os.execv(os.environ["REAL_MKTEMP"], [os.environ["REAL_MKTEMP"], *args])
target = json.loads(Path(os.environ["TEST_TARGET"]).read_text())
if command == "helm":
    assert args == ["show", "crds", "fixture-chart"], args
    if mode == "helm-failure":
        sys.exit(23)
    print(json.dumps(target))
    sys.exit(0)
assert command == "kubectl", command
assert args[:2] == ["--context", "fixture-context"], args
args = args[2:]
with (work / "events.jsonl").open("a") as output:
    output.write(json.dumps(args[0]) + "\n")
name = target["metadata"]["name"]
if args[0] == "apply":
    assert args[1:5] == ["--server-side", "--force-conflicts", "--field-manager=orka-crd-lifecycle", "-f"], args
    assert json.loads(Path(args[5]).read_text()) == target
elif args[0] == "create":
    assert args[1:3] == ["--dry-run=client", "-f"] and args[4:] == ["-o", "json"], args
    assert json.loads(Path(args[3]).read_text()) == target
    print(json.dumps(target))
elif args[0] == "get":
    assert args[1:] == ["customresourcedefinition", name, "-o", "jsonpath={.metadata.resourceVersion}"], args
    print("42", end="")
elif args[0] == "patch":
    assert args[1:5] == ["customresourcedefinition", name, "--type=json", "--patch-file"], args
    patch = json.loads(Path(args[5]).read_text())
    assert patch == [
        {"op": "test", "path": "/metadata/resourceVersion", "value": "42"},
        {"op": "replace", "path": "/spec", "value": target["spec"]},
    ], "exact resourceVersion test and complete spec replacement are required"
    if mode == "conflict":
        print("resourceVersion test failed", file=sys.stderr)
        sys.exit(1)
    state = json.loads((work / "state.json").read_text())
    state["metadata"]["resourceVersion"] = "43"
    state["spec"] = copy.deepcopy(patch[1]["value"])
    (work / "state.json").write_text(json.dumps(state))
elif args[0] == "wait":
    assert args[1:] == ["--for=condition=Established", "--timeout=60s", "customresourcedefinition/" + name], args
else:
    raise AssertionError(args)
PY
chmod +x "${test_root}/bin/fixture-cli"
for command in helm kubectl jq mktemp; do
  ln -s fixture-cli "${test_root}/bin/${command}"
done

for mode in success conflict helm-failure mktemp-failure; do
  work="${test_root}/${mode}"
  mkdir -p "${work}/tmp"
  python3 - "${test_root}/target.json" "${work}/state.json" <<'PY'
import json
import sys

state = json.load(open(sys.argv[1]))
state["metadata"]["resourceVersion"] = "42"
state["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]["spec"]["properties"]["staleReviewField"] = {"type": "string"}
with open(sys.argv[2], "w") as output:
    json.dump(state, output)
PY
  status=0
  env PATH="${test_root}/bin:${PATH}" TMPDIR="${work}/tmp" \
    REAL_JQ="${real_jq}" REAL_MKTEMP="${real_mktemp}" \
    TEST_WORK="${work}" TEST_MODE="${mode}" TEST_TARGET="${test_root}/target.json" \
    bash "${script}" fixture-chart fixture-context >"${work}/output.log" 2>&1 || status=$?
  if [[ "${mode}" == success && ${status} -ne 0 ]]; then
    cat "${work}/output.log" >&2
    echo "large CRD apply failed with status ${status}" >&2
    exit 1
  fi
  if [[ "${mode}" != success && ${status} -eq 0 ]]; then
    echo "CRD helper ignored ${mode}" >&2
    exit 1
  fi
  python3 - "${work}" "${test_root}/target.json" "${mode}" <<'PY'
import json
from pathlib import Path
import sys

work, fixture, mode = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3]
assert list((work / "tmp").iterdir()) == [], "temporary CRD or patch files leaked"
events_file = work / "events.jsonl"
events = [json.loads(line) for line in events_file.read_text().splitlines()] if events_file.exists() else []
state = json.loads((work / "state.json").read_text())
target = json.loads(fixture.read_text())
if mode == "success":
    assert events == ["apply", "create", "get", "patch", "wait"], events
    assert state["spec"] == target["spec"], "large schema was truncated or stale fields survived"
    assert state["metadata"]["resourceVersion"] == "43"
elif mode == "conflict":
    assert events == ["apply", "create", "get", "patch"], events
    assert state["metadata"]["resourceVersion"] == "42"
    assert "staleReviewField" in state["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]["spec"]["properties"]
else:
    assert events == [], events
PY
  echo "PASS: ${mode}"
done
