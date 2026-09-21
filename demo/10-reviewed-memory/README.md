# A procedure the next agent can use

A warehouse assistant reads a return procedure, proposes a shared note, and
waits for review. A different agent cannot answer the warehouse question before
publication. After the presenter accepts and applies the proposal, a fresh task
answers with the exact dock and label.

The walkthrough uses the existing chapter markers and typed commands, with a
target of about four minutes at 100 by 28 characters after idle-time compression.
`procedure.txt` is demonstration data. The proposal, review records, memory, task
results, and event history come from Orka. No answer is supplied by the script.

## Prepare and rehearse

Use the existing local demo cluster with a current Orka controller, native AI
worker, authenticated model provider, and `orka-client` ServiceAccount. The CLI
must match this checkout. `kubectl`, `orka`, `jq`, `curl`, and Python 3 are needed.
The shared helper also finds `bin/orka` when present.

Set the scoped `KUBECONFIG`, API connection, and namespace in
`demo/setup/env.sh`, using [the example](../setup/env.sh.example), then run from
the repository root:

```sh
demo/setup/reviewed-memory.sh
demo/10-reviewed-memory/demo.sh
```

These commands prepare and rehearse without recording. Setup installs two native
Agents and a separate role granting the presenter memory read, review, apply,
and disable permissions. It refuses fixed resource names owned by other work.
The model defaults are `DEMO_PROVIDER_REF=copilot` and
`DEMO_AI_MODEL=claude-opus-4.7`; set those environment variables during setup to
use another prepared provider and model with tool support.

The cluster's active memory must be empty before the walkthrough. Orka supplies
namespace memory when a native AI task starts, so an unrelated saved note could
affect this comparison. The script stops on existing active memory and leaves
those records intact. Use a dedicated demo installation when the current
namespace contains knowledge you want to keep available.

## What the run checks

- The author calls `remember` once and creates one pending proposal linked to
  its Task, preserving `Dock 3`, `RET-4827`, and the `warehouse-returns` tag.
- Each reader gets the same question in a new Task, with no Session, earlier
  task reference, repository, source note, or extra prompt. Its first model
  request contains one user message.
- Full event pagination reaches the latest sequence and includes worker
  completion. Both tool events and model-reported tool-call counts must show no
  reader tool use. Missing events or request/response pairs stop the walkthrough.
- Acceptance leaves the active memory list empty. Explicit apply creates one
  note whose source proposal and originating Task match the reviewed record.
- The final answer contains the exact dock and label. A plausible generic answer
  is insufficient. The reader configuration and shared note remain the same
  throughout the final question.

The current native AI worker automatically enables memory and transcript-search
tools. The reader instructions ask it to call no tools, and the script verifies
that observed behavior. This demonstrates a fresh answer from supplied durable
memory. It does not demonstrate enforced removal of transcript-search access.
Old proposals and transcripts are not automatically loaded into these fresh
Tasks; any reader search attempt invalidates the run.

Original CLI/API responses, every event page, submitted task manifests, and
derived evidence live in the ignored directory
`demo/setup/state/10-reviewed-memory/runs/<run-id>/`. `evidence.json` and
`evidence.txt` are written only after all checks pass.

## Repeat the walkthrough

Applying the proposal intentionally leaves a saved note. To repeat, disable only
that note using the captured source link. Set `previous_run` to the absolute run
directory, including for an attempt that stopped after applying its proposal:

```sh
source demo/lib/scenario.sh
scenario_require_cluster
scenario_connect
previous_run=/absolute/path/to/demo/setup/state/10-reviewed-memory/runs/RUN_ID
test "$(jq -r .namespace "$previous_run/run.json")" = "$ORKA_NAMESPACE"
proposal=$(jq -er .id "$previous_run/raw/proposal-applied.json")
memory=$(jq -er .appliedMemoryId "$previous_run/raw/proposal-applied.json")
orka memory get "$memory" -o json |
  jq -e --arg proposal "$proposal" \
    '.source == "memory_proposal" and .sourceProposalId == $proposal
     and .agentName == "demo-memory-author"' >/dev/null
orka memory disable "$memory"
demo/10-reviewed-memory/demo.sh
```

The proposal and source task remain available for inspection. A new walkthrough
uses new task names and a new evidence directory.

Offline checks:

```sh
for script in demo/10-reviewed-memory/demo.sh demo/setup/reviewed-memory.sh; do
  bash -n "$script"
done
python3 -m unittest discover -s demo/10-reviewed-memory -p 'test_*.py'
```

The tests exercise incomplete event histories, late searches, missing tool
events, conversation reuse, leaked source text, invented answers, and incorrect
proposal links. They use test fixtures and do not execute a model or record a cast.
